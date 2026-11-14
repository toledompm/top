package dokku

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/toledompm/top/internal/exec"
)

func TestZZFastExecNoChanges(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "dokku")
	if err := os.WriteFile(script, []byte("#!/bin/bash\necho \"!     No changes detected, skipping git commit\" >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	fe := &exec.FastExec{}
	err := New(fe).DeployImage(context.Background(), ctlRef, theImage())
	t.Logf("RESULT err=%v", err)
	if err != nil {
		t.Errorf("FastExec-path no-changes deploy must classify as success: %v", err)
	}
}
