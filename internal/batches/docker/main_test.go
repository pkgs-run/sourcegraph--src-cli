package docker

import (
	"os"
	"testing"

	"github.com/pkgs-run/sourcegraph--src-cli/internal/exec/expect"
)

func TestMain(m *testing.M) {
	code := expect.Handle(m)
	os.Exit(code)
}
