//go:build conformance

// TestMain builds the tiny helper binaries the exec probe needs (same
// pattern as internal/exec's TestMain): hermetic, portable helper-echo —
// see internal/exec/exectest/cmd/helper-echo.
package conformance_check

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

var helperEcho string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "top-conformance")
	if err != nil {
		fmt.Fprintf(os.Stderr, "temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	// cwd of a go test run is this package's dir; the module root is one up.
	dest := filepath.Join(dir, "helper-echo")
	build := exec.Command("go", "build", "-o", dest, "./internal/exec/exectest/cmd/helper-echo")
	build.Dir = ".."
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build helper-echo: %v\n%s\n", err, out)
		os.Exit(1)
	}
	helperEcho = dest
	os.Exit(m.Run())
}
