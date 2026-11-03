// SPDX-License-Identifier: MIT

package reconcile

// Tier-1 hermetic tests for the T-403 provisioning sync (provision.go):
// tree convergence against real fs temp dirs (the sync IS filesystem
// work — no runner seam), the dash-kebab secret→env rule (secretenv.go),
// and the run-level no-op guarantees (empty/un-set ProvisionSrc).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/toledompm/top/internal/domain"
)

// writeTree lays a source tree under srcRoot (paths + contents + mode).
func writeTree(t *testing.T, root string, files map[string]string, mode os.FileMode) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

func TestProvisioningSyncConvergesTree(t *testing.T) {
	t.Parallel()
	rev := t.TempDir() // "materialized revision" (ProvisionSrc)
	vol := t.TempDir() // VolumeRoot
	src := filepath.Join(rev, "observability", "prometheus-config")
	writeTree(t, src, map[string]string{
		"prometheus.yml": "global:\n  scrape_interval: 15s\n",
		"rules/base.yml": "groups: []\n",
	}, 0o644)

	dest := filepath.Join(vol, "observability-prometheus-config")
	in := RunInput{
		Specs: []domain.Service{{
			Ref:     domain.Ref(domain.ObservabilityNamespace, "prometheus"),
			Storage: []domain.Storage{{Name: "config", MountPath: "/etc/prometheus"}},
		}},
		ProvisionSrc: rev,
		VolumeRoot:   vol,
	}
	r, err := New(Options{Backend: newFakeBackend(), Store: newFakeStore()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var res RunResult
	if err := r.syncProvisioning(context.Background(), in, &res); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Synced) != 1 || res.Synced[0].Entry != "config" {
		t.Fatalf("Synced = %+v, want one config entry", res.Synced)
	}
	b, err := os.ReadFile(filepath.Join(dest, "prometheus.yml"))
	if err != nil || string(b) != "global:\n  scrape_interval: 15s\n" {
		t.Fatalf("synced prometheus.yml: %q, err %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "rules", "base.yml")); err != nil || string(b) != "groups: []\n" {
		t.Fatalf("synced rules/base.yml: %q, err %v", b, err)
	}
	fi, err := os.Stat(filepath.Join(dest, "prometheus.yml"))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("synced mode = %v, want 0644 (mode is part of the identity)", fi.Mode())
	}

	// Idempotency: a converged second run copies nothing.
	var res2 RunResult
	if err := r.syncProvisioning(context.Background(), in, &res2); err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if len(res2.Synced) != 0 {
		t.Fatalf("second run synced %+v, want nothing (idempotent convergence)", res2.Synced)
	}
}

func TestProvisioningSyncPrunesStaleFiles(t *testing.T) {
	t.Parallel()
	rev := t.TempDir()
	vol := t.TempDir()
	src := filepath.Join(rev, "observability", "grafana-provisioning")
	dest := filepath.Join(vol, "observability-grafana-provisioning")
	// A stale file left from yesteryear's tree…
	if err := os.MkdirAll(filepath.Join(dest, "datasources"), 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	stale := "datasources/removed.yaml"
	if err := os.WriteFile(filepath.Join(dest, stale), []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	// …and a live tree that no longer carries it.
	writeTree(t, src, map[string]string{
		"datasources/prometheus.yaml": "apiVersion: 1\n",
	}, 0o644)

	in := RunInput{
		Specs: []domain.Service{{
			Ref:     domain.Ref(domain.ObservabilityNamespace, "grafana"),
			Storage: []domain.Storage{{Name: "provisioning", MountPath: "/etc/grafana/provisioning"}},
		}},
		ProvisionSrc: rev,
		VolumeRoot:   vol,
	}
	r, err := New(Options{Backend: newFakeBackend(), Store: newFakeStore()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var res RunResult
	if err := r.syncProvisioning(context.Background(), in, &res); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, stale)); !os.IsNotExist(err) {
		t.Fatalf("stale file survived the sync (GitOps tree convergence): %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "datasources", "prometheus.yaml")); err != nil || string(b) != "apiVersion: 1\n" {
		t.Fatalf("live file not synced: %q %v", b, err)
	}
}

func TestProvisioningSyncSkipsDataAndUnaware(t *testing.T) {
	t.Parallel()
	rev := t.TempDir()
	vol := t.TempDir()
	// A DATA tree happens to exist (operator data dir), plus a tenant
	// service with the same entry shape — both must stay untouched…
	dataDir := filepath.Join(vol, "observability-prometheus-data")
	writeTree(t, dataDir, map[string]string{"\x01wal": "host data"}, 0o600)
	// …and an empty ProvisionSrc/VolumeRoot pair is a pure no-op.
	in := RunInput{
		Specs: []domain.Service{{
			Ref: domain.Ref(domain.ObservabilityNamespace, "prometheus"),
			Storage: []domain.Storage{
				{Name: "data", MountPath: "/prometheus", Size: "2Gi"}, // no <svc>-data src tree anyway
			},
		}},
		ProvisionSrc: rev,
		VolumeRoot:   vol,
	}
	r, err := New(Options{Backend: newFakeBackend(), Store: newFakeStore()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var res RunResult
	if err := r.syncProvisioning(context.Background(), in, &res); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Synced) != 0 {
		t.Fatalf("data-style entry must never sync: %+v", res.Synced)
	}
	// Provisioning-unaware composition (fields empty): no-op regardless.
	var res3 RunResult
	if err := r.syncProvisioning(context.Background(), RunInput{Specs: in.Specs}, &res3); err != nil {
		t.Fatalf("unaware sync: %v", err)
	}
}

func TestProvisioningSyncSkipsFileTreeLeaf(t *testing.T) {
	t.Parallel()
	rev := t.TempDir()
	vol := t.TempDir()
	// Same leaf exists as a FILE: skipped silently (not a tree).
	f := filepath.Join(rev, "observability", "prometheus-config")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(f, []byte("not a dir\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	in := RunInput{
		Specs: []domain.Service{{
			Ref:     domain.Ref(domain.ObservabilityNamespace, "prometheus"),
			Storage: []domain.Storage{{Name: "config", MountPath: "/etc/prometheus"}},
		}},
		ProvisionSrc: rev,
		VolumeRoot:   vol,
	}
	r, err := New(Options{Backend: newFakeBackend(), Store: newFakeStore()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var res RunResult
	if err := r.syncProvisioning(context.Background(), in, &res); err != nil {
		t.Fatalf("file-instead-of-dir must SKIP silently (not a provisioning tree): %v", err)
	}
}

func TestSecretEnvKeyDashKebabRule(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"gf-security-admin-password": "GF_SECURITY_ADMIN_PASSWORD",
		"smtp-password":              "SMTP_PASSWORD",
		"api-token":                  "API_TOKEN",
		"x":                          "X",
	} {
		got, err := SecretEnvKey(name)
		if err != nil || got != want {
			t.Errorf("SecretEnvKey(%q) = %q,%v want %q,nil", name, got, err, want)
		}
	}
	if _, err := SecretEnvKey("has_underscore"); err == nil {
		t.Error("underscore in name must fail (names are [a-z0-9-], ADR-0008; use dash-kebab of the env var)")
	} else if !strings.Contains(err.Error(), "dash-kebab") {
		t.Errorf("error text should carry the fix hint, got: %v", err)
	}
}
