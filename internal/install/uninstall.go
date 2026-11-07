// SPDX-License-Identifier: MIT

package install

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/toledompm/top/internal/backend"
	"github.com/toledompm/top/internal/config"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/inventory"
	"github.com/toledompm/top/internal/secrets"
	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/store/sqlite"
)

// Uninstall — the ownership-safe teardown of docs/contracts/installer.md
// ("Teardown") and ADR-0009's Teardown section. Defaults KEEP data:
// without the purge flags nothing store- or secret-bearing is deleted.
//
// Order (the contract's list, one idempotent body):
//
//	1. open the state store — a MISSING top.db is typed
//	   ErrNothingInstalled: TOP tears down only what it can prove it
//	    owns, and a lost store proves nothing.
//	2. lease acquire — the serialization lease stops accepting triggers
//	   while teardown runs (one-writer law; released on any exit).
//	3. fresh ownership snapshot; every ledger entry goes through the
//	   two-key rule (inventory.Deletable): ledger match AND fresh-
//	   snapshot match. Ledgered managed apps are removed via the backend
//	   (Destroy) in REVERSE namespace order — the controller (a core
//	   app) dies last. Networks/volumes/volumes answer per the snapshot;
//	   dokku-created networks carry no top.dev labels, so the two-key
//	   law refuses them with the printed manual command (the safe
//	   failure mode, per ADR-0009 "touching any resource not in the
//	   ledger" gate).
//	4. WITH --purge-data: destroy the storage entry, then the data-dir
//	   tree MINUS secrets/ and git/ (keys and secrets retained unless
//	   --purge-secrets are separately flagged — ADR-0009: no
//	   yes-to-everything). Without the flag: everything stays.
//	5. WITH --purge-secrets: delete each hostfile secret (names only;
//	   values never surface) and the git key material.
//	6. WITH --uninstall-dokku: print what Dokku's uninstaller leaves
//	   behind and the exact manual purge commands (executed runbook is
//	   M3 hardening; the M2 slice prints and refuses to auto-run —
//	   documented deviation).
//	7. Report: Removed / Skipped (with-reason refusals) / Kept — the
//	   ledger-diff proof that pre-existing resources survived.
//
// A ledger row is removed ONLY after its resource was removed
// (store.RemoveLedgerEntry); a failed removal leaves the row and the
// re-run redoes the tier. Dry-run prints the same decision list with
// zero mutations.

// UninstallOptions parameterize Uninstall (same seam discipline as the
// installer's Options).
type UninstallOptions struct {
	Runner  exec.Runner             // nil → exec.NewFastExec()
	Backend backend.PlatformBackend // REQUIRED when not DryRun
	Store   store.Store             // nil → sqlite at <dataDir>/top.db (owned)

	DataDir string

	// The tier flags: each IS its own ADR-0009 approval marker; there is
	// no flag that turns them all on (no yes-to-everything).
	PurgeData      bool
	PurgeSecrets   bool
	UninstallDokku bool

	Confirm bool // the ADR-0009 approval marker (CLI's requireConfirm passes it)
	DryRun  bool

	// Namespaces (nil → config.NamespacePolicyOrder) and Now (clock
	// seam) — mirrors the installer's Options.
	Namespaces []string
	Now        func() time.Time
}

func (o *UninstallOptions) applyDefaults() {
	if o.Runner == nil {
		o.Runner = exec.NewFastExec()
	}
	if o.DataDir == "" {
		o.DataDir = DataDirDefault
	}
	if len(o.Namespaces) == 0 {
		o.Namespaces = config.NamespacePolicyOrder
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
}

// leaseName is the serialization lease uninstall holds while running
// ("stop accepting triggers" — the reconciler's controller lease is
// held FOR the uninstall; ADR-0009/0007 share the store's lease).
const leaseName = "controller"

// Uninstall runs the teardown. On any failure the partial Result comes
// back with the typed error; the lease (when held) is released either
// way.
func Uninstall(ctx context.Context, opts UninstallOptions) (*Result, error) {
	opts.applyDefaults()
	if !opts.DryRun && !opts.Confirm {
		return nil, fmt.Errorf("uninstall mutates the host (destructive): %w", ErrGateRequired)
	}
	if !opts.DryRun && opts.Backend == nil {
		return nil, fmt.Errorf("uninstall: %w: Options.Backend is required when not --dry-run", domain.ErrInvalid)
	}

	res := &Result{Notes: []string{}}

	st, own, err := openUninstallStore(ctx, opts)
	if err != nil {
		return nil, err
	}
	if own {
		defer func() { _ = st.Close() }()
	}

	// Dry-run: the decision list, zero mutations, no lease.
	if opts.DryRun {
		if err := teardownPass(ctx, st, opts, res, false); err != nil {
			return res, err
		}
		res.Notes = append(res.Notes,
			"dry-run: nothing was removed; this list is exactly what the real run executes",
			"default teardown keeps the data (SQLite state, secrets, git keys) — pass --purge-data/--purge-secrets to remove them (ADR-0009 tiers)")
		return res, nil
	}

	holder := "uninstall-" + opts.Now().UTC().Format("20060102150405")
	ok, lerr := st.AcquireLease(ctx, leaseName, holder, 30*time.Minute, false)
	switch {
	case lerr != nil:
		return res, fmt.Errorf("%w: lease acquire failed: %v", ErrLeaseHeld, lerr)
	case !ok:
		return res, fmt.Errorf("%w: another writer holds the %q lease (is the controller mid-reconcile? re-run `topctl uninstall` after it concludes)", ErrLeaseHeld, leaseName)
	}
	defer func() { _ = st.ReleaseLease(ctx, leaseName, holder) }()

	if err := uninstallOpWrap(ctx, st, opts, res); err != nil {
		return res, err
	}

	res.NextCommands = []string{"topctl doctor"}
	return res, nil
}

// uninstallOpWrap is uninstall's phase-operation wrapper (kind
// "uninstall"; details fixed pre-insert since the flags are static; the
// lifecycle pending → running → terminal brackets the destroy loop — a
// failed teardown records the redacted error and stays for the re-run).
func uninstallOpWrap(ctx context.Context, st store.Store, opts UninstallOptions, res *Result) error {
	op, err := domain.NewOperation(domain.OpUninstall, domain.ActorUser, "", "")
	if err != nil {
		return fmt.Errorf("uninstall: create op: %w", err)
	}
	op.Details = map[string]any{
		"purgeData":      opts.PurgeData,
		"purgeSecrets":   opts.PurgeSecrets,
		"uninstallDokku": opts.UninstallDokku,
		"dataDir":        opts.DataDir,
	}
	if err := st.InsertOperation(ctx, op); err != nil {
		return fmt.Errorf("uninstall: record insert: %w", err)
	}
	if err := st.UpdateOperationState(ctx, op.ID, domain.OpRunning, "", opts.Now()); err != nil {
		return fmt.Errorf("uninstall: record running: %w", err)
	}
	terr := teardownPass(ctx, st, opts, res, true)
	var terminal error
	if terr != nil {
		terminal = st.UpdateOperationState(ctx, op.ID, domain.OpFailed, strings.TrimSpace(terr.Error()), opts.Now())
	} else {
		terminal = st.UpdateOperationState(ctx, op.ID, domain.OpSucceeded, "", opts.Now())
	}
	if terminal != nil {
		return fmt.Errorf("uninstall: record terminal: %w", terminal)
	}
	res.PhasesRun = append(res.PhasesRun, "uninstall")
	return terr
}

// openUninstallStore opens the state store; a MISSING top.db is the typed
// nothing-installed error (NEVER a fresh empty store — uninstalling
// against a new empty ledger would silently do "everything" done and lie
// by omission about ownership).
func openUninstallStore(ctx context.Context, opts UninstallOptions) (store.Store, bool, error) {
	if opts.Store != nil {
		return opts.Store, false, nil
	}
	path := opts.DataDir + "/" + "top.db"
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, false, fmt.Errorf("%w: no record at %s — if the store was lost, nothing here proves TOP owns anything (operations are history, not state)",
			ErrNothingInstalled, path)
	}
	s, oerr := sqlite.Open(ctx, path)
	if oerr != nil {
		return nil, false, fmt.Errorf("%w: %s: %v", ErrStoreUnavailable, path, oerr)
	}
	return s, true, nil
}

// teardownPass executes (or, with mutate=false, plans) the destroy loop.
// Decisions are computed ONCE; mutate only changes whether host/store
// removals actually run.
func teardownPass(ctx context.Context, st store.Store, opts UninstallOptions, res *Result, mutate bool) error {
	fresh, err := inventory.Snapshot(ctx, opts.Runner, inventory.SnapshotOptions{
		Namespaces: opts.Namespaces,
		Now:        opts.Now(),
	})
	if err != nil {
		return fmt.Errorf("fresh ownership snapshot: %w", err)
	}

	entries, err := st.ListLedger(ctx, "")
	if err != nil {
		return fmt.Errorf("ledger read: %w", err)
	}

	// Destroy order: apps in REVERSE tier/policy order (namespace order
	// reversed so the controller — core — dies last); every other kind
	// trails.
	order := reversePolicyOrder(opts.Namespaces)
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if (a.Kind == inventory.KindApp) != (b.Kind == inventory.KindApp) {
			return a.Kind == inventory.KindApp
		}
		if a.Kind != inventory.KindApp || b.Kind != inventory.KindApp {
			return a.Identifier < b.Identifier
		}
		ai, aok := appTierIndex(a.Identifier, order)
		bi, bok := appTierIndex(b.Identifier, order)
		if aok && bok && ai != bi {
			return ai < bi // ascending along the reversed order → default, obs, core
		}
		return a.Identifier < b.Identifier
	})

	for _, e := range entries {
		dec, derr := inventory.Deletable(ctx, st, e.Kind, e.Identifier, fresh, opts.Namespaces)
		if derr != nil {
			return fmt.Errorf("two-key check for %s %s: %w", e.Kind, e.Identifier, derr)
		}
		if !dec.Deletable {
			res.Skipped = append(res.Skipped,
				fmt.Sprintf("%s %s: not removed (%s)", e.Kind, e.Identifier, dec.String()))
			continue
		}
		switch e.Kind {
		case inventory.KindApp:
			if !mutate {
				res.Removed = append(res.Removed, "app "+e.Identifier+" (would remove via backend; ledger row would drop)")
				continue
			}
			if err := removeApp(ctx, st, opts, e, res); err != nil {
				return err
			}
		default:
			// Networks/volumes/containers: the M2 adapter slice has no
			// teardown verbs for them. The two-key law already refuses
			// dokku-created networks (no top.dev labels); refuse-with-
			// reason here too, printing the manual command.
			res.Skipped = append(res.Skipped,
				fmt.Sprintf("%s %s: no teardown verb in the M2 adapter slice (manual: `dokku network:destroy %s` for networks; review by hand)",
					e.Kind, e.Identifier, e.Identifier))
		}
	}

	// --- data tiers -----------------------------------------------------------
	if opts.PurgeData {
		if mutate {
			if err := opts.Backend.DestroyStorageEntry(ctx, StorageEntryName); err != nil {
				return fmt.Errorf("destroy storage entry %s: %w", StorageEntryName, err)
			}
			res.Removed = append(res.Removed, "storage-entry "+StorageEntryName)
			kept, rmErr := removeAllExcept(opts.DataDir, []string{"secrets", "git"})
			if rmErr != nil {
				return rmErr
			}
			res.Removed = append(res.Removed, kept...)
		} else {
			res.Removed = append(res.Removed, "storage-entry "+StorageEntryName+" (would destroy; host dir stays for the secrets/git tiers)")
		}
	} else {
		res.LeftAlone = append(res.LeftAlone, "SQLite state under "+opts.DataDir+" KEPT (pass --purge-data to delete; ADR-0009: kept by default)")
	}

	if opts.PurgeSecrets {
		if mutate {
			removed, sErr := purgeSecrets(opts.DataDir)
			if sErr != nil {
				return sErr
			}
			res.Removed = append(res.Removed, removed...)
			// With the data purged as well, the emptied containers and
			// the data dir itself go last (purge-data removed everything
			// else already; nothing here survives either flag).
			if opts.PurgeData {
				for _, sub := range []string{"secrets", "git"} {
					if err := os.RemoveAll(filepath.Join(opts.DataDir, sub)); err != nil {
						return fmt.Errorf("purge %s dir: %w", sub, err)
					}
				}
				if children, rerr := os.ReadDir(opts.DataDir); rerr == nil && len(children) == 0 {
					if err := os.Remove(opts.DataDir); err != nil {
						return fmt.Errorf("purge %s: %w", opts.DataDir, err)
					}
					res.Removed = append(res.Removed, "removed "+opts.DataDir)
				}
			}
		} else {
			res.Removed = append(res.Removed, "secrets/* + git key material (would delete)")
		}
	} else {
		res.LeftAlone = append(res.LeftAlone, "secrets and git keys under "+opts.DataDir+"/{secrets,git} KEPT (pass --purge-secrets to delete; ADR-0009) ")
	}

	if opts.UninstallDokku {
		leftovers := []string{
			"apt/dpkg package files (dokku, herokuish, plugn, sshcommand)",
			"/var/lib/dokku (plugin data, storage dirs, nginx state)",
			"/etc/nginx config rendered by dokku",
			"the `dokku` unix user and its sshcommand.authorized_keys",
		}
		res.Notes = append(res.Notes, "uninstalling dokku — its own uninstaller leaves (check each): "+strings.Join(leftovers, "; "))
		res.Commands = append(res.Commands,
			"sudo apt-get --purge -y remove dokku",
			"sudo deluser --remove-home dokku",
			"sudo rm -rf /var/lib/dokku")
		res.Skipped = append(res.Skipped, "dokku itself: NOT uninstalled in the M2 slice — the printed commands are the manual runbook (auto-run is M3 hardening, ADR-0009 separate confirmation)")
	} else {
		res.Checked = append(res.Checked, "dokku itself untouched (pass --uninstall-dokku for the documented purge)")
	}

	return nil
}

// removeApp destroys one deleted-ledger app THROUGH THE BACKEND and drops
// the ledger row after success. With --purge-data, the storage entry's
// mount is detached first (data is being destroyed; the mount would point
// at a vanished dir otherwise).
func removeApp(ctx context.Context, st store.Store, opts UninstallOptions, e *store.Resource, res *Result) error {
	app := e.Identifier
	ns, name, ok := splitAppName(app)
	if !ok {
		return fmt.Errorf("app %s: refusing teardown: the name cannot be derived into a TOP ref (two-key defense-in-depth)", app)
	}
	ref := domain.Ref(domain.Namespace(ns), domain.Name(name))

	if opts.PurgeData {
		if err := opts.Backend.DetachStorage(ctx, ref, StorageEntryName, opts.DataDir); err != nil {
			return fmt.Errorf("detach storage %s/%s from %s: %w", StorageEntryName, opts.DataDir, app, err)
		}
	}
	if err := opts.Backend.Destroy(ctx, ref); err != nil {
		return fmt.Errorf("destroy app %s: %w", app, err)
	}
	if err := st.RemoveLedgerEntry(ctx, e.Kind, e.Identifier); err != nil {
		return fmt.Errorf("ledger drop %s %s after removal: %w", e.Kind, e.Identifier, err)
	}
	byOp := e.CreatedByOp
	if byOp == "" {
		byOp = "-"
	}
	res.Removed = append(res.Removed, fmt.Sprintf("app %s (was %s, created by op %s)", app, e.State, byOp))
	return nil
}

// splitAppName back-derives "<ns>-<name>" — preferring the LONGEST known
// namespace (registered namespaces may collide as prefixes of each other).
func splitAppName(app string) (string, string, bool) {
	for _, ns := range configNamespaceOrder() {
		rest, ok := strings.CutPrefix(app, ns+"-")
		if ok && rest != "" {
			return ns, rest, true
		}
	}
	return "", "", false
}

func configNamespaceOrder() []string { return config.NamespacePolicyOrder }

// appTierIndex locates an app's namespace in the destroy order (ok=false
// → unknown namespace, sorts after known ones, tie-broken by name).
func appTierIndex(app string, order []string) (int, bool) {
	for i, ns := range order {
		if strings.HasPrefix(app, ns+"-") {
			return i, true
		}
	}
	return -1, false
}

// reversePolicyOrder renders the (operator's) namespace order reversed —
// the contract's "reverse namespace order" for destroys.
func reversePolicyOrder(namespaces []string) []string {
	out := make([]string, len(namespaces))
	for i, ns := range namespaces {
		out[len(out)-1-i] = ns
	}
	return out
}

// removeAllExcept deletes every entry under dir except the listed basenames
// (the purge-data tier's "everything but secrets/git keys"), then dir
// itself if nothing was kept. Returns the removed-bytes list for the
// report.
func removeAllExcept(dir string, keep []string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("purge %s: read dir: %w", dir, err)
	}
	removed := []string{}
	for _, e := range entries {
		if containsString(keep, e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(path); err != nil {
			return removed, fmt.Errorf("purge %s: %w", path, err)
		}
		removed = append(removed, "removed "+path)
	}
	// Keep the data dir itself when a tier preserved something in it;
	// remove only when empty (an empty dir keeps nothing to lose).
	children, rerr := os.ReadDir(dir)
	if rerr == nil && len(children) == 0 {
		if err := os.Remove(dir); err == nil {
			removed = append(removed, "removed "+dir)
		}
	}
	return removed, nil
}

// purgeSecrets deletes every hostfile secret (names via List; values
// never surface) and the git key material.
func purgeSecrets(dataDir string) ([]string, error) {
	removed := []string{}
	hf := secrets.NewHostFile(dataDir + "/secrets")
	names, err := hf.List()
	if err != nil {
		return removed, fmt.Errorf("purge secrets: %w", err)
	}
	for _, n := range names {
		if err := hf.Delete(n.Name); err != nil && !errors.Is(err, secrets.ErrMissing) {
			return removed, fmt.Errorf("purge secret %s: %w", secrets.SafeRef(n.Name), err)
		}
		removed = append(removed, "removed secret "+secrets.SafeRef(n.Name))
	}
	for _, p := range []string{"known_hosts", "deploy_key"} {
		path := filepath.Join(dataDir, "git", p)
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, fmt.Errorf("purge git key %s: %w", path, err)
		}
		removed = append(removed, "removed "+path)
	}
	return removed, nil
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
