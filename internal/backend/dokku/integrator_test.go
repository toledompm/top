// SPDX-License-Identifier: MIT

// Tier-1 golden-argv tests for the integrator-surface methods
// (internal/install's seam; extend-not-fork, ADR-0006). Fixture values
// fictional throughout.
package dokku

import (
	"context"
	"testing"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec/exectest"
)

// ctlRef is the platform's own controller ref (ADR-0009 naming); its app
// name is core-top-controller, derived by domain.AppName like everywhere.
var ctlRef = domain.Ref("core", "top-controller")

func theImage() string { return "ghcr.io/example/top/topctl:bootstrap-pinned" }

func TestDeployImageGoldenArgv(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok("Deploying"), "git:from-image", "core-top-controller", theImage())
	if err := newC(f).DeployImage(context.Background(), ctlRef, theImage()); err != nil {
		t.Fatalf("DeployImage: %v", err)
	}
	assertArgv(t, f, "dokku git:from-image core-top-controller "+theImage())
}

// TestDeployImageValidation: empty image refs and bad refs are refused
// without spawning argv.
func TestDeployImageValidation(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	if err := newC(f).DeployImage(context.Background(), ctlRef, ""); err == nil {
		t.Fatal("empty image ref must be refused")
	}
	if err := newC(f).DeployImage(context.Background(), domain.Ref("core", "NOT_valid"), "img"); err == nil {
		t.Fatal("invalid ref must be refused")
	}
	if got := recordedArgv(f); len(got) != 0 {
		t.Fatalf("validation refusals must not spawn argv: %v", got)
	}
}

func TestEnsureStorageEntryGoldenArgv(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Absent → created with --chown false (TOP owns the host dir).
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", fail(1, ""), "storage:info", "top-metadata")
	f.SetOutput("dokku", ok("Created"), "storage:create", "top-metadata", "/var/lib/top", "--chown", "false")
	if err := newC(f).EnsureStorageEntry(ctx, "top-metadata", "/var/lib/top"); err != nil {
		t.Fatalf("EnsureStorageEntry: %v", err)
	}
	assertArgv(t, f,
		"dokku storage:info top-metadata",
		"dokku storage:create top-metadata /var/lib/top --chown false")

	// Exists → probe only.
	f2 := exectest.NewFakeRunner()
	f2.SetOutput("dokku", ok("info"), "storage:info", "top-metadata")
	if err := newC(f2).EnsureStorageEntry(ctx, "top-metadata", "/var/lib/top"); err != nil {
		t.Fatalf("resume EnsureStorageEntry: %v", err)
	}
	assertArgv(t, f2, "dokku storage:info top-metadata")

	// Bad names refused without spawning.
	f3 := exectest.NewFakeRunner()
	if err := newC(f3).EnsureStorageEntry(ctx, "Top entry", "/x"); err == nil {
		t.Fatal("bad entry name must be refused")
	}
	if err := newC(f3).EnsureStorageEntry(ctx, "core-top-controllerr-with-a-super-much-too-long-name-beyond-45", "/x"); err == nil {
		t.Fatal("over-long entry name must be refused")
	}
	if got := recordedArgv(f3); len(got) != 0 {
		t.Fatalf("validation refusals must not spawn argv: %v", got)
	}
}

func TestAttachDetachStorageGoldenArgv(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok("Mounted"), "storage:mount", "core-top-controller", "top-metadata", "--container-dir", "/var/lib/top")
	if err := newC(f).AttachStorage(ctx, ctlRef, "top-metadata", "/var/lib/top"); err != nil {
		t.Fatalf("AttachStorage: %v", err)
	}
	assertArgv(t, f, "dokku storage:mount core-top-controller top-metadata --container-dir /var/lib/top")

	// Detach: app exists, mount requested.
	f2 := exectest.NewFakeRunner()
	f2.SetOutput("dokku", ok(existsYes), "apps:exists", "core-top-controller")
	f2.SetOutput("dokku", ok("Unmounted"), "storage:unmount", "core-top-controller", "top-metadata", "--container-dir", "/var/lib/top")
	if err := newC(f2).DetachStorage(ctx, ctlRef, "top-metadata", "/var/lib/top"); err != nil {
		t.Fatalf("DetachStorage: %v", err)
	}
	assertArgv(t, f2,
		"dokku apps:exists core-top-controller",
		"dokku storage:unmount core-top-controller top-metadata --container-dir /var/lib/top")

	// Detach: app gone → no-op success (apps:destroy already cleaned it).
	f3 := exectest.NewFakeRunner()
	f3.SetOutput("dokku", fail(20, existsNo), "apps:exists", "core-top-controller")
	if err := newC(f3).DetachStorage(ctx, ctlRef, "top-metadata", "/var/lib/top"); err != nil {
		t.Fatalf("DetachStorage no-op path: %v", err)
	}
	assertArgv(t, f3, "dokku apps:exists core-top-controller")

	// Validation: relative container path refused, no spawn.
	f4 := exectest.NewFakeRunner()
	if err := newC(f4).AttachStorage(ctx, ctlRef, "top-metadata", "data"); err == nil {
		t.Fatal("relative container path must be refused")
	}
	if got := recordedArgv(f4); len(got) != 0 {
		t.Fatalf("validation refusals must not spawn argv: %v", got)
	}
}

func TestDestroyStorageEntryGoldenArgv(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok("Destroyed"), "storage:destroy", "top-metadata", "--force")
	if err := newC(f).DestroyStorageEntry(context.Background(), "top-metadata"); err != nil {
		t.Fatalf("DestroyStorageEntry: %v", err)
	}
	assertArgv(t, f, "dokku storage:destroy top-metadata --force")

	// Missing entry on re-run: exit 1 tolerated (idempotent teardown).
	f2 := exectest.NewFakeRunner()
	f2.SetOutput("dokku", fail(1, ""), "storage:destroy", "top-metadata", "--force")
	if err := newC(f2).DestroyStorageEntry(context.Background(), "top-metadata"); err != nil {
		t.Fatalf("re-run DestroyStorageEntry must tolerate absence: %v", err)
	}
}
