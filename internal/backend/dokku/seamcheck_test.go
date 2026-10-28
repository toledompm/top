package dokku

// SPDX-License-Identifier: MIT

// Integration check: the adapter satisfies the reconciler's Backend seam
// (seam_test in internal/reconcile also pins this from the consumer side;
// the dual pinning keeps both surfaces honest).
import (
	"testing"

	"github.com/toledompm/top/internal/exec/exectest"
	"github.com/toledompm/top/internal/reconcile"
)

func TestAdapterSatisfiesReconcilerSeam(t *testing.T) {
	var _ reconcile.Backend = New(exectest.NewFakeRunner())
}
