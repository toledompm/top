// SPDX-License-Identifier: MIT

// provision.go — T-403 provisioning sync (separate file, additive): each
// run syncs the platform repo's well-named provisioning trees into the
// TOP-owned storage-entry host dirs BEFORE any apply, so config /
// provisioning files (prometheus.yml; grafana datasources + dashboards)
// reach the container through the `storage` mounts the reconciler
// attaches. Mechanism-choice rationale is documented in the platform
// repo README ("T-403 wiring" items 1 and 4).
//
// Conventions (pinned here and in the repo README):
//
//   - The platform repo carries a provisioning tree for a service's
//     storage entry `<entry>` at `<ns>/<svc>-<entry>` (e.g.
//     `observability/prometheus-config`,
//     `observability/grafana-provisioning`).
//   - If that source tree exists, the entry is a PROVISIONING entry: its
//     host dir — the reconciler's own VolumeRoot formula
//     (`storageHostDir`: `<VolumeRoot>/<app>-<entry>`, ADR-0009) —
//     converges to the tree.
//   - Other entries (data-style) sync NOTHING: their host dirs are app
//     data, never overwritten by reconcile (ADR-0009 data lifecycle).
//
// Semantics: hash-verified copy (bytes + mode), stale files pruned, so
// the dir converges to the repo tree — GitOps for a mounted dir.
// Idempotent: a second green run with unchanged content writes nothing.
// A missing source tree is a no-op (tenant apps; operator walk-backs).
// A sync failure FAILS THE RUN before any mutation: ProvisionSrc is this
// run's materialized snapshot dir (valid for the whole run), and the
// container must never mount a half-converged tree.

package reconcile

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/toledompm/top/internal/domain"
)

// PhaseProvision names the T-403 provisioning-sync stage in run errors
// (the provision file owns the stage; seam.go's pipeline block stays
// untouched — additive by composition).
const PhaseProvision Phase = "provision"

// provisioningNameRe constrains entry names the sync will act on (the
// same DNS-1123-label discipline the adapter's storage verbs use); wider
// shapes stay spec-validation's job, never the sync's.
const provisioningNameMax = 64

// SyncedEntry is one provisioning entry this run changed (paths only —
// never file contents; ADR-0008 values-never records discipline).
type SyncedEntry struct {
	Ref     domain.ServiceRef // owning spec
	Entry   string            // storage entry name
	FromDir string            // source tree (this run's materialized snapshot)
	ToDir   string            // TOP-owned host dir backing the entry
}

// syncProvisioning converges every provisioning entry of in.Specs. No-op
// when the composition is provisioning-unaware (ProvisionSrc or
// VolumeRoot empty — e.g. the diff-only path, which owns no host writes,
// or callers not yet wired to pass the snapshot dir).
func (r *Reconciler) syncProvisioning(ctx context.Context, in RunInput, res *RunResult) error {
	if in.ProvisionSrc == "" || in.VolumeRoot == "" {
		return nil
	}
	for _, svc := range in.Specs {
		for _, st := range svc.Storage {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !provisioningEntryName(st.Name) {
				continue // spec validation's job upstream, not the sync's
			}
			src := filepath.Join(in.ProvisionSrc,
				string(svc.Ref.Namespace), string(svc.Ref.Name)+"-"+st.Name)
			info, err := os.Stat(src)
			if os.IsNotExist(err) {
				continue // not a provisioning entry for this tree — skip
			}
			if err != nil {
				return fmt.Errorf("provision %s: %s: source tree: %w", svc.Ref, st.Name, err)
			}
			if !info.IsDir() {
				continue
			}
			dest := storageHostDir(in.VolumeRoot, svc.Ref, st.Name)
			if dest == "" {
				continue // VolumeRoot-less runs never mount (ADR-0009)
			}
			changed, err := syncTree(src, dest)
			if err != nil {
				return fmt.Errorf("provision %s: %s: %w", svc.Ref, st.Name, err)
			}
			if changed {
				res.Synced = append(res.Synced, SyncedEntry{
					Ref: svc.Ref, Entry: st.Name, FromDir: src, ToDir: dest,
				})
			}
		}
	}
	return nil
}

// provisioningEntryName accepts the label-shape entry names only.
func provisioningEntryName(name string) bool {
	if name == "" || len(name) > provisioningNameMax {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-':
		default:
			return false
		}
	}
	return name[0] != '-' && name[len(name)-1] != '-'
}

// syncTree converges dest to the source tree; true = THEN-ONLY-IF the
// tree changed on disk (reporting is per entry, never per file — records
// stay bounded).
func syncTree(src, dest string) (bool, error) {
	// Source pass: relative path → bytes + mode (trees are small).
	type syncFile struct {
		bytes []byte
		mode  os.FileMode // perm bits only
	}
	want := map[string]syncFile{}
	if err := filepath.WalkDir(src, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil // symlinks/specials never sync (spec-load discipline too)
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		want[rel] = syncFile{bytes: b, mode: fi.Mode().Perm()}
		return nil
	}); err != nil {
		return false, fmt.Errorf("walk source tree: %w", err)
	}

	// Directories on demand (0755 on the dir bits: the container's app
	// user must read through; files keep the repo's 0644).
	for rel := range want {
		dir := filepath.Join(dest, filepath.Dir(rel))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	// Prune pass: drop files present on disk but absent from the tree.
	if err := filepath.WalkDir(dest, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if p == dest || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dest, p)
		if err != nil {
			return err
		}
		if _, ok := want[rel]; !ok {
			if err := os.Remove(p); err != nil {
				return fmt.Errorf("prune stale %s: %w", rel, err)
			}
		}
		return nil
	}); err != nil {
		return false, fmt.Errorf("walk dest tree: %w", err)
	}

	// Copy pass: compare (bytes hash + mode); write only on drift.
	changed := false
	for rel, f := range want {
		p := filepath.Join(dest, rel)
		same, err := fileMatches(p, f.bytes, f.mode)
		if err != nil {
			return false, err
		}
		if same {
			continue
		}
		if err := os.WriteFile(p, f.bytes, f.mode); err != nil {
			return false, fmt.Errorf("write %s: %w", rel, err)
		}
		changed = true
	}
	return changed, nil
}

// fileMatches reports whether the on-disk file's bytes and mode match the
// tree's file. Missing = drift (copy); unreadable machine errors go to
// the caller — silent divergence is the failure mode to avoid.
func fileMatches(p string, bytes []byte, mode os.FileMode) (bool, error) {
	fi, err := os.Stat(p)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", filepath.Base(p), err)
	}
	cur, err := os.ReadFile(p)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", filepath.Base(p), err)
	}
	if fi.Mode().Perm() != mode || sha256.Sum256(cur) != sha256.Sum256(bytes) {
		return false, nil
	}
	return true, nil
}
