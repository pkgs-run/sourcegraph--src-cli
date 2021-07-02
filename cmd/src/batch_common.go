package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/neelance/parallel"
	"github.com/pkg/errors"
	"github.com/sourcegraph/sourcegraph/lib/output"
	"github.com/sourcegraph/src-cli/internal/api"
	"github.com/sourcegraph/src-cli/internal/batches"
	"github.com/sourcegraph/src-cli/internal/batches/executor"
	"github.com/sourcegraph/src-cli/internal/batches/graphql"
	"github.com/sourcegraph/src-cli/internal/batches/service"
	"github.com/sourcegraph/src-cli/internal/batches/workspace"
)

var (
	batchPendingColor = output.StylePending
	batchSuccessColor = output.StyleSuccess
	batchSuccessEmoji = output.EmojiSuccess
)

type batchExecuteFlags struct {
	allowUnsupported bool
	allowIgnored     bool
	api              *api.Flags
	apply            bool
	cacheDir         string
	tempDir          string
	clearCache       bool
	file             string
	keepLogs         bool
	namespace        string
	parallelism      int
	timeout          time.Duration
	workspace        string
	cleanArchives    bool
	skipErrors       bool

	// EXPERIMENTAL
	textOnly bool
}

func newBatchExecuteFlags(flagSet *flag.FlagSet, cacheDir, tempDir string) *batchExecuteFlags {
	caf := &batchExecuteFlags{
		api: api.NewFlags(flagSet),
	}

	flagSet.BoolVar(
		&caf.textOnly, "text-only", false,
		"No TUI, only print text",
	)
	flagSet.BoolVar(
		&caf.allowUnsupported, "allow-unsupported", false,
		"Allow unsupported code hosts.",
	)
	flagSet.BoolVar(
		&caf.allowIgnored, "force-override-ignore", false,
		"Do not ignore repositories that have a .batchignore file.",
	)
	flagSet.BoolVar(
		&caf.apply, "apply", false,
		"Ignored.",
	)
	flagSet.StringVar(
		&caf.cacheDir, "cache", cacheDir,
		"Directory for caching results and repository archives.",
	)
	flagSet.BoolVar(
		&caf.clearCache, "clear-cache", false,
		"If true, clears the execution cache and executes all steps anew.",
	)
	flagSet.StringVar(
		&caf.tempDir, "tmp", tempDir,
		"Directory for storing temporary data, such as log files. Default is /tmp. Can also be set with environment variable SRC_BATCH_TMP_DIR; if both are set, this flag will be used and not the environment variable.",
	)
	flagSet.StringVar(
		&caf.file, "f", "",
		"The batch spec file to read.",
	)
	flagSet.BoolVar(
		&caf.keepLogs, "keep-logs", false,
		"Retain logs after executing steps.",
	)
	flagSet.StringVar(
		&caf.namespace, "namespace", "",
		"The user or organization namespace to place the batch change within. Default is the currently authenticated user.",
	)
	flagSet.StringVar(&caf.namespace, "n", "", "Alias for -namespace.")

	flagSet.IntVar(
		&caf.parallelism, "j", runtime.GOMAXPROCS(0),
		"The maximum number of parallel jobs. Default is GOMAXPROCS.",
	)
	flagSet.DurationVar(
		&caf.timeout, "timeout", 60*time.Minute,
		"The maximum duration a single batch spec step can take.",
	)
	flagSet.BoolVar(
		&caf.cleanArchives, "clean-archives", true,
		"If true, deletes downloaded repository archives after executing batch spec steps.",
	)
	flagSet.BoolVar(
		&caf.skipErrors, "skip-errors", false,
		"If true, errors encountered while executing steps in a repository won't stop the execution of the batch spec but only cause that repository to be skipped.",
	)

	flagSet.StringVar(
		&caf.workspace, "workspace", "auto",
		`Workspace mode to use ("auto", "bind", or "volume")`,
	)

	flagSet.BoolVar(verbose, "v", false, "print verbose output")

	return caf
}

func batchCreatePending(out *output.Output, message string) output.Pending {
	return out.Pending(output.Line("", batchPendingColor, message))
}

func batchCompletePending(p output.Pending, message string) {
	p.Complete(output.Line(batchSuccessEmoji, batchSuccessColor, message))
}

func batchDefaultCacheDir() string {
	uc, err := os.UserCacheDir()
	if err != nil {
		return ""
	}

	// Check if there's an old campaigns cache directory but not a new batch
	// directory: if so, we should rename the old directory and carry on.
	//
	// TODO(campaigns-deprecation): we can remove this migration shim after June
	// 2021.
	old := path.Join(uc, "sourcegraph", "campaigns")
	dir := path.Join(uc, "sourcegraph", "batch")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if _, err := os.Stat(old); os.IsExist(err) {
			// We'll just try to do this without checking for an error: if it
			// fails, we'll carry on and let the normal cache directory handling
			// logic take care of it.
			os.Rename(old, dir)
		}
	}

	return dir
}

// batchDefaultTempDirPrefix returns the prefix to be passed to ioutil.TempFile.
// If one of the environment variables SRC_BATCH_TMP_DIR or
// SRC_CAMPAIGNS_TMP_DIR is set, that is used as the prefix. Otherwise we use
// "/tmp".
func batchDefaultTempDirPrefix() string {
	// TODO(campaigns-deprecation): we can remove this migration shim in
	// Sourcegraph 4.0.
	for _, env := range []string{"SRC_BATCH_TMP_DIR", "SRC_CAMPAIGNS_TMP_DIR"} {
		if p := os.Getenv(env); p != "" {
			return p
		}
	}

	// On macOS, we use an explicit prefix for our temp directories, because
	// otherwise Go would use $TMPDIR, which is set to `/var/folders` per
	// default on macOS. But Docker for Mac doesn't have `/var/folders` in its
	// default set of shared folders, but it does have `/tmp` in there.
	if runtime.GOOS == "darwin" {
		return "/tmp"
	}

	return os.TempDir()
}

func batchOpenFileFlag(flag *string) (io.ReadCloser, error) {
	if flag == nil || *flag == "" || *flag == "-" {
		return os.Stdin, nil
	}

	file, err := os.Open(*flag)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot open file %q", *flag)
	}
	return file, nil
}

type executeBatchSpecOpts struct {
	flags *batchExecuteFlags

	applyBatchSpec bool

	out    *output.Output
	client api.Client
}

// executeBatchSpec performs all the steps required to upload the batch spec to
// Sourcegraph, including execution as needed and applying the resulting batch
// spec if specified.
func executeBatchSpec(ctx context.Context, opts executeBatchSpecOpts) error {
	svc := service.New(&service.Opts{
		AllowUnsupported: opts.flags.allowUnsupported,
		AllowIgnored:     opts.flags.allowIgnored,
		Client:           opts.client,
	})

	if err := svc.DetermineFeatureFlags(ctx); err != nil {
		return err
	}

	if err := checkExecutable("git", "version"); err != nil {
		return err
	}

	if err := checkExecutable("docker", "version"); err != nil {
		return err
	}

	// Parse flags and build up our service and executor options.
	pending := batchCreatePending(opts.out, "Parsing batch spec")
	batchSpec, rawSpec, err := batchParseSpec(opts.out, &opts.flags.file, svc)
	if err != nil {
		return err
	}
	batchCompletePending(pending, "Parsing batch spec")

	pending = batchCreatePending(opts.out, "Resolving namespace")
	namespace, err := svc.ResolveNamespace(ctx, opts.flags.namespace)
	if err != nil {
		return err
	}
	batchCompletePending(pending, "Resolving namespace")

	imageProgress := opts.out.Progress([]output.ProgressBar{{
		Label: "Preparing container images",
		Max:   1.0,
	}}, nil)
	err = svc.SetDockerImages(ctx, batchSpec, func(perc float64) {
		imageProgress.SetValue(0, perc)
	})
	if err != nil {
		return err
	}
	imageProgress.Complete()

	pending = batchCreatePending(opts.out, "Determining workspace type")
	workspaceCreator := workspace.NewCreator(ctx, opts.flags.workspace, opts.flags.cacheDir, opts.flags.tempDir, batchSpec.Steps)
	if workspaceCreator.Type() == workspace.CreatorTypeVolume {
		_, err = svc.EnsureImage(ctx, workspace.DockerVolumeWorkspaceImage)
		if err != nil {
			return err
		}
	}

	pending.VerboseLine(output.Linef("🚧", output.StyleSuccess, "Workspace creator: %T", workspaceCreator))
	batchCompletePending(pending, "Set workspace type")

	pending = batchCreatePending(opts.out, "Resolving repositories")
	repos, err := svc.ResolveRepositories(ctx, batchSpec)
	if err != nil {
		if repoSet, ok := err.(batches.UnsupportedRepoSet); ok {
			batchCompletePending(pending, "Resolved repositories")

			block := opts.out.Block(output.Line(" ", output.StyleWarning, "Some repositories are hosted on unsupported code hosts and will be skipped. Use the -allow-unsupported flag to avoid skipping them."))
			for repo := range repoSet {
				block.Write(repo.Name)
			}
			block.Close()
		} else if repoSet, ok := err.(batches.IgnoredRepoSet); ok {
			batchCompletePending(pending, "Resolved repositories")

			block := opts.out.Block(output.Line(" ", output.StyleWarning, "The repositories listed below contain .batchignore files and will be skipped. Use the -force-override-ignore flag to avoid skipping them."))
			for repo := range repoSet {
				block.Write(repo.Name)
			}
			block.Close()
		} else {
			return errors.Wrap(err, "resolving repositories")
		}
	} else {
		batchCompletePending(pending, fmt.Sprintf("Resolved %d repositories", len(repos)))
	}

	pending = batchCreatePending(opts.out, "Determining workspaces")
	tasks, err := svc.BuildTasks(ctx, repos, batchSpec)
	if err != nil {
		return err
	}
	batchCompletePending(pending, fmt.Sprintf("Found %d workspaces with steps to execute", len(tasks)))

	// EXECUTION OF TASKS
	coord := svc.NewCoordinator(executor.NewCoordinatorOpts{
		Creator:       workspaceCreator,
		CacheDir:      opts.flags.cacheDir,
		ClearCache:    opts.flags.clearCache,
		SkipErrors:    opts.flags.skipErrors,
		CleanArchives: opts.flags.cleanArchives,
		Parallelism:   opts.flags.parallelism,
		Timeout:       opts.flags.timeout,
		KeepLogs:      opts.flags.keepLogs,
		TempDir:       opts.flags.tempDir,
	})

	pending = batchCreatePending(opts.out, "Checking cache for changeset specs")
	uncachedTasks, cachedSpecs, err := coord.CheckCache(ctx, tasks)
	if err != nil {
		return err
	}
	var specsFoundMessage string
	if len(cachedSpecs) == 1 {
		specsFoundMessage = "Found 1 cached changeset spec"
	} else {
		specsFoundMessage = fmt.Sprintf("Found %d cached changeset specs", len(cachedSpecs))
	}
	switch len(uncachedTasks) {
	case 0:
		batchCompletePending(pending, fmt.Sprintf("%s; no tasks need to be executed", specsFoundMessage))
	case 1:
		batchCompletePending(pending, fmt.Sprintf("%s; %d task needs to be executed", specsFoundMessage, len(uncachedTasks)))
	default:
		batchCompletePending(pending, fmt.Sprintf("%s; %d tasks need to be executed", specsFoundMessage, len(uncachedTasks)))
	}

	p := newBatchProgressPrinter(opts.out, *verbose, opts.flags.parallelism)
	freshSpecs, logFiles, err := coord.Execute(ctx, uncachedTasks, batchSpec, p.PrintStatuses)
	if err != nil && !opts.flags.skipErrors {
		return err
	}
	p.Complete()
	if err != nil && opts.flags.skipErrors {
		printExecutionError(opts.out, err)
		opts.out.WriteLine(output.Line(output.EmojiWarning, output.StyleWarning, "Skipping errors because -skip-errors was used."))
	}

	if len(logFiles) > 0 && opts.flags.keepLogs {
		func() {
			block := opts.out.Block(output.Line("", batchSuccessColor, "Preserving log files:"))
			defer block.Close()

			for _, file := range logFiles {
				block.Write(file)
			}
		}()
	}

	specs := append(cachedSpecs, freshSpecs...)

	err = svc.ValidateChangesetSpecs(repos, specs)
	if err != nil {
		return err
	}

	ids := make([]graphql.ChangesetSpecID, len(specs))

	if len(specs) > 0 {
		var label string
		if len(specs) == 1 {
			label = "Sending changeset spec"
		} else {
			label = fmt.Sprintf("Sending %d changeset specs", len(specs))
		}

		progress := opts.out.Progress([]output.ProgressBar{
			{Label: label, Max: float64(len(specs))},
		}, nil)

		for i, spec := range specs {
			id, err := svc.CreateChangesetSpec(ctx, spec)
			if err != nil {
				return err
			}
			ids[i] = id
			progress.SetValue(0, float64(i+1))
		}
		progress.Complete()
	} else {
		if len(repos) == 0 {
			opts.out.WriteLine(output.Linef(output.EmojiWarning, output.StyleWarning, `No changeset specs created`))
		}
	}

	pending = batchCreatePending(opts.out, "Creating batch spec on Sourcegraph")
	id, url, err := svc.CreateBatchSpec(ctx, namespace, rawSpec, ids)
	batchCompletePending(pending, "Creating batch spec on Sourcegraph")
	if err != nil {
		return prettyPrintBatchUnlicensedError(opts.out, err)
	}

	if opts.applyBatchSpec {
		pending = batchCreatePending(opts.out, "Applying batch spec")
		batch, err := svc.ApplyBatchChange(ctx, id)
		if err != nil {
			return err
		}
		batchCompletePending(pending, "Applying batch spec")

		opts.out.Write("")
		block := opts.out.Block(output.Line(batchSuccessEmoji, batchSuccessColor, "Batch change applied!"))
		defer block.Close()

		block.Write("To view the batch change, go to:")
		block.Writef("%s%s", cfg.Endpoint, batch.URL)

	} else {
		opts.out.Write("")
		block := opts.out.Block(output.Line(batchSuccessEmoji, batchSuccessColor, "To preview or apply the batch spec, go to:"))
		defer block.Close()

		block.Writef("%s%s", cfg.Endpoint, url)
	}

	return nil
}

// TODO: This is a straight up copy of the other function
func textOnlyExecuteBatchSpec(ctx context.Context, opts executeBatchSpecOpts) error {
	svc := service.New(&service.Opts{
		AllowUnsupported: opts.flags.allowUnsupported,
		AllowIgnored:     opts.flags.allowIgnored,
		Client:           opts.client,
	})

	if err := svc.DetermineFeatureFlags(ctx); err != nil {
		return err
	}

	if err := checkExecutable("git", "version"); err != nil {
		return err
	}

	if err := checkExecutable("docker", "version"); err != nil {
		return err
	}

	// Parse flags and build up our service and executor options.
	fmt.Println("Parsing batch spec")
	batchSpec, rawSpec, err := batchParseSpec(opts.out, &opts.flags.file, svc)
	if err != nil {
		return err
	}
	fmt.Println("Parsing batch spec DONE")

	fmt.Println("Resolving namespace")
	namespace, err := svc.ResolveNamespace(ctx, opts.flags.namespace)
	if err != nil {
		return err
	}
	fmt.Println("Resolving namespace DONE")

	fmt.Println("Preparing Docker images")
	err = svc.SetDockerImages(ctx, batchSpec, func(perc float64) {
		fmt.Printf("Preparing Docker images: %f\n", perc)
	})
	if err != nil {
		return err
	}
	fmt.Println("Preparing Docker images DONE")

	fmt.Println("Determining workspace type")
	workspaceCreator := workspace.NewCreator(ctx, opts.flags.workspace, opts.flags.cacheDir, opts.flags.tempDir, batchSpec.Steps)
	if workspaceCreator.Type() == workspace.CreatorTypeVolume {
		_, err = svc.EnsureImage(ctx, workspace.DockerVolumeWorkspaceImage)
		if err != nil {
			return err
		}
	}

	fmt.Println("Determined workspace type")

	fmt.Println("Resolving repositories")
	repos, err := svc.ResolveRepositories(ctx, batchSpec)
	if err != nil {
		if repoSet, ok := err.(batches.UnsupportedRepoSet); ok {
			fmt.Println("Resolved repositories")

			block := opts.out.Block(output.Line(" ", output.StyleWarning, "Some repositories are hosted on unsupported code hosts and will be skipped. Use the -allow-unsupported flag to avoid skipping them."))
			for repo := range repoSet {
				block.Write(repo.Name)
			}
			block.Close()
		} else if repoSet, ok := err.(batches.IgnoredRepoSet); ok {
			fmt.Println("Resolved repositories")

			block := opts.out.Block(output.Line(" ", output.StyleWarning, "The repositories listed below contain .batchignore files and will be skipped. Use the -force-override-ignore flag to avoid skipping them."))
			for repo := range repoSet {
				block.Write(repo.Name)
			}
			block.Close()
		} else {
			return errors.Wrap(err, "resolving repositories")
		}
	} else {
		fmt.Println(fmt.Sprintf("Resolved %d repositories", len(repos)))
	}

	fmt.Println("Determining workspaces")
	tasks, err := svc.BuildTasks(ctx, repos, batchSpec)
	if err != nil {
		return err
	}
	fmt.Println(fmt.Sprintf("Found %d workspaces with steps to execute", len(tasks)))

	// EXECUTION OF TASKS
	coord := svc.NewCoordinator(executor.NewCoordinatorOpts{
		Creator:       workspaceCreator,
		CacheDir:      opts.flags.cacheDir,
		ClearCache:    opts.flags.clearCache,
		SkipErrors:    opts.flags.skipErrors,
		CleanArchives: opts.flags.cleanArchives,
		Parallelism:   opts.flags.parallelism,
		Timeout:       opts.flags.timeout,
		KeepLogs:      opts.flags.keepLogs,
		TempDir:       opts.flags.tempDir,
	})

	fmt.Println("Checking cache for changeset specs")
	uncachedTasks, cachedSpecs, err := coord.CheckCache(ctx, tasks)
	if err != nil {
		return err
	}
	var specsFoundMessage string
	if len(cachedSpecs) == 1 {
		specsFoundMessage = "Found 1 cached changeset spec"
	} else {
		specsFoundMessage = fmt.Sprintf("Found %d cached changeset specs", len(cachedSpecs))
	}
	switch len(uncachedTasks) {
	case 0:
		fmt.Println(fmt.Sprintf("%s; no tasks need to be executed", specsFoundMessage))
	case 1:
		fmt.Println(fmt.Sprintf("%s; %d task needs to be executed", specsFoundMessage, len(uncachedTasks)))
	default:
		fmt.Println(fmt.Sprintf("%s; %d tasks need to be executed", specsFoundMessage, len(uncachedTasks)))
	}

	// p := newBatchProgressPrinter(opts.out, *verbose, opts.flags.parallelism)
	fmt.Println("Executing tasks")
	freshSpecs, logFiles, err := coord.Execute(ctx, uncachedTasks, batchSpec, func(ts []*executor.TaskStatus) {
		fmt.Printf("number of task statuses: %d\n", len(ts))
	})
	if err != nil && !opts.flags.skipErrors {
		return err
	}
	fmt.Println("Executing tasks DONE")
	if err != nil && opts.flags.skipErrors {
		fmt.Printf("execution error: %s\n", err)
		fmt.Println("Skipping errors because -skip-errors was used.")
	}

	if len(logFiles) > 0 && opts.flags.keepLogs {
		for _, file := range logFiles {
			fmt.Printf("log file: %s\n", file)
		}
	}

	specs := append(cachedSpecs, freshSpecs...)

	err = svc.ValidateChangesetSpecs(repos, specs)
	if err != nil {
		return err
	}

	ids := make([]graphql.ChangesetSpecID, len(specs))

	if len(specs) > 0 {
		var label string
		if len(specs) == 1 {
			label = "Sending changeset spec"
		} else {
			label = fmt.Sprintf("Sending %d changeset specs", len(specs))
		}

		fmt.Println(label)

		for i, spec := range specs {
			id, err := svc.CreateChangesetSpec(ctx, spec)
			if err != nil {
				return err
			}
			ids[i] = id
			fmt.Printf("done: %d/%d\n", i+1, len(specs))
		}
		fmt.Printf("%s DONE\n", label)
	} else {
		if len(repos) == 0 {
			fmt.Println("No changeset specs created")
		}
	}

	fmt.Println("Creating batch spec on Sourcegraph")
	id, url, err := svc.CreateBatchSpec(ctx, namespace, rawSpec, ids)
	fmt.Println("Creating batch spec on Sourcegraph")
	if err != nil {
		return prettyPrintBatchUnlicensedError(opts.out, err)
	}

	if opts.applyBatchSpec {
		fmt.Println("Applying batch spec")
		batch, err := svc.ApplyBatchChange(ctx, id)
		if err != nil {
			return err
		}
		fmt.Println("Applying batch spec")

		fmt.Println("Batch change applied!")

		fmt.Println("To view the batch change, go to:")
		fmt.Printf("%s%s\n", cfg.Endpoint, batch.URL)

	} else {
		opts.out.Write("")
		fmt.Println("To preview or apply the batch spec, go to:")
		fmt.Printf("%s%s\n", cfg.Endpoint, url)
	}

	return nil
}

// batchParseSpec parses and validates the given batch spec. If the spec has
// validation errors, the errors are output in a human readable form and an
// exitCodeError is returned.
func batchParseSpec(out *output.Output, file *string, svc *service.Service) (*batches.BatchSpec, string, error) {
	f, err := batchOpenFileFlag(file)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()

	spec, raw, err := svc.ParseBatchSpec(f)
	if err != nil {
		if merr, ok := err.(*multierror.Error); ok {
			block := out.Block(output.Line("\u274c", output.StyleWarning, "Batch spec failed validation."))
			defer block.Close()

			for i, err := range merr.Errors {
				block.Writef("%d. %s", i+1, err)
			}

			return nil, "", &exitCodeError{
				error:    nil,
				exitCode: 2,
			}
		} else {
			// This shouldn't happen; let's just punt and let the normal
			// rendering occur.
			return nil, "", err
		}
	}

	return spec, raw, nil
}

// printExecutionError is used to print the possible error returned by
// batchExecute.
func printExecutionError(out *output.Output, err error) {
	// exitCodeError shouldn't generate any specific output, since it indicates
	// that this was done deeper in the call stack.
	if _, ok := err.(*exitCodeError); ok {
		return
	}

	out.Write("")

	writeErrs := func(errs []error) {
		var block *output.Block

		if len(errs) > 1 {
			block = out.Block(output.Linef(output.EmojiFailure, output.StyleWarning, "%d errors:", len(errs)))
		} else {
			block = out.Block(output.Line(output.EmojiFailure, output.StyleWarning, "Error:"))
		}

		for _, e := range errs {
			if taskErr, ok := e.(executor.TaskExecutionErr); ok {
				block.Write(formatTaskExecutionErr(taskErr))
			} else {
				if err == context.Canceled {
					block.Writef("%sAborting", output.StyleBold)
				} else {
					block.Writef("%s%s", output.StyleBold, e.Error())
				}
			}
		}

		if block != nil {
			block.Close()
		}
	}

	switch err := err.(type) {
	case parallel.Errors, *multierror.Error, api.GraphQlErrors:
		writeErrs(flattenErrs(err))

	default:
		writeErrs([]error{err})
	}

	out.Write("")

	block := out.Block(output.Line(output.EmojiLightbulb, output.StyleSuggestion, "The troubleshooting documentation can help to narrow down the cause of the errors:"))
	block.WriteLine(output.Line("", output.StyleSuggestion, "https://docs.sourcegraph.com/batch_changes/references/troubleshooting"))
	block.Close()
}

func flattenErrs(err error) (result []error) {
	switch errs := err.(type) {
	case parallel.Errors:
		for _, e := range errs {
			result = append(result, flattenErrs(e)...)
		}

	case *multierror.Error:
		for _, e := range errs.Errors {
			result = append(result, flattenErrs(e)...)
		}

	case api.GraphQlErrors:
		for _, e := range errs {
			result = append(result, flattenErrs(e)...)
		}

	default:
		result = append(result, errs)
	}

	return result
}

func formatTaskExecutionErr(err executor.TaskExecutionErr) string {
	if ee, ok := errors.Cause(err).(*exec.ExitError); ok && ee.String() == "signal: killed" {
		return fmt.Sprintf(
			"%s%s%s: killed by interrupt signal",
			output.StyleBold,
			err.Repository,
			output.StyleReset,
		)
	}

	return fmt.Sprintf(
		"%s%s%s:\n%s\nLog: %s\n",
		output.StyleBold,
		err.Repository,
		output.StyleReset,
		err.Err,
		err.Logfile,
	)
}

// prettyPrintBatchUnlicensedError introspects the given error returned when
// creating a batch spec and ascertains whether it's a licensing error. If it
// is, then a better message is output. Regardless, the return value of this
// function should be used to replace the original error passed in to ensure
// that the displayed output is sensible.
func prettyPrintBatchUnlicensedError(out *output.Output, err error) error {
	// Pull apart the error to see if it's a licensing error: if so, we should
	// display a friendlier and more actionable message than the usual GraphQL
	// error output.
	if gerrs, ok := err.(api.GraphQlErrors); ok {
		// A licensing error should be the sole error returned, so we'll only
		// pretty print if there's one error.
		if len(gerrs) == 1 {
			if code, cerr := gerrs[0].Code(); cerr != nil {
				// We got a malformed value in the error extensions; at this
				// point, there's not much sensible we can do. Let's log this in
				// verbose mode, but let the original error bubble up rather
				// than this one.
				out.Verbosef("Unexpected error parsing the GraphQL error: %v", cerr)
			} else if code == "ErrCampaignsUnlicensed" || code == "ErrBatchChangesUnlicensed" {
				// OK, let's print a better message, then return an
				// exitCodeError to suppress the normal automatic error block.
				// Note that we have hand wrapped the output at 80 (printable)
				// characters: having automatic wrapping some day would be nice,
				// but this should be sufficient for now.
				block := out.Block(output.Line("🪙", output.StyleWarning, "Batch Changes is a paid feature of Sourcegraph. All users can create sample"))
				block.WriteLine(output.Linef("", output.StyleWarning, "batch changes with up to 5 changesets without a license. Contact Sourcegraph"))
				block.WriteLine(output.Linef("", output.StyleWarning, "sales at %shttps://about.sourcegraph.com/contact/sales/%s to obtain a trial", output.StyleSearchLink, output.StyleWarning))
				block.WriteLine(output.Linef("", output.StyleWarning, "license."))
				block.Write("")
				block.WriteLine(output.Linef("", output.StyleWarning, "To proceed with this batch change, you will need to create 5 or fewer"))
				block.WriteLine(output.Linef("", output.StyleWarning, "changesets. To do so, you could try adding %scount:5%s to your", output.StyleSearchAlertProposedQuery, output.StyleWarning))
				block.WriteLine(output.Linef("", output.StyleWarning, "%srepositoriesMatchingQuery%s search, or reduce the number of changesets in", output.StyleReset, output.StyleWarning))
				block.WriteLine(output.Linef("", output.StyleWarning, "%simportChangesets%s.", output.StyleReset, output.StyleWarning))
				block.Close()
				return &exitCodeError{exitCode: graphqlErrorsExitCode}
			}
		}
	}

	// In all other cases, we'll just return the original error.
	return err
}

func checkExecutable(cmd string, args ...string) error {
	if err := exec.Command(cmd, args...).Run(); err != nil {
		return fmt.Errorf(
			"failed to execute \"%s %s\":\n\t%s\n\n'src batch' require %q to be available.",
			cmd,
			strings.Join(args, " "),
			err,
			cmd,
		)
	}
	return nil
}

func contextCancelOnInterrupt(parent context.Context) (context.Context, func()) {
	ctx, ctxCancel := context.WithCancel(parent)
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	go func() {
		select {
		case <-c:
			ctxCancel()
		case <-ctx.Done():
		}
	}()

	return ctx, func() {
		signal.Stop(c)
		ctxCancel()
	}
}
