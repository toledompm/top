// SPDX-License-Identifier: MIT

package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/toledompm/top/internal/doctor"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/inventory"
	"github.com/toledompm/top/internal/store"
)

// Phases 1, 2, 3, 4 — the mutating bodies of docs/contracts/installer.md,
// each wrapped by runPhase (the phase's operation record) and governed by
// ADR-0009: everything TOP creates is ledgered bootstrap-owned via
// inventory.MarkCreated; everything pre-existing was recorded in phase 1
// and is left alone.

// --- phase 1: ownership inventory ----------------------------------------------

// runInventory snapshots the host (read-only) and walks it into the
// ownership ledger as pre-existing rows, anchored on THIS phase's install
// operation (created_by). The full snapshot JSON is embedded in the op's
// details (size-capped) and ALSO written to <dataDir>/inventory-<opid>.json
// when the data dir already exists — never anywhere outside TOP dirs
// (writing outside would be an un-owned host mutation).
func runInventory(ctx context.Context, opts Options, st store.Store, opID string, res *Result, extra phaseExtras) error {
	snap, err := inventory.Snapshot(ctx, opts.Runner, inventory.SnapshotOptions{
		Namespaces: opts.Namespaces,
		Now:        opts.Now(),
	})
	if err != nil {
		return fmt.Errorf("snapshot host state: %w", err)
	}

	report, inserted, err := inventory.CreateLedgerFromSnapshot(ctx, st, snap, opts.Namespaces, opID, opts.Now())
	if err != nil {
		return fmt.Errorf("ledger walk of snapshot: %w", err)
	}
	for _, e := range report {
		if e.Skipped == "" {
			res.Checked = append(res.Checked, "inventory: recorded pre-existing "+e.Kind+" "+e.Identifier)
		} else {
			res.Checked = append(res.Checked, "inventory: "+e.Kind+" "+e.Identifier+" already recorded, skipped")
		}
	}
	if inserted > 0 {
		res.LeftAlone = append(res.LeftAlone, fmt.Sprintf("%d pre-existing resource(s) recorded in the ownership ledger", inserted))
	} else {
		res.Checked = append(res.Checked, "inventory: nothing pre-existing was TOP-shaped; ledger unchanged by the snapshot walk")
	}

	// Details embed (size-capped) + optional on-disk copy inside TOP dirs.
	snapJSON, jerr := snap.JSON()
	if jerr != nil {
		return fmt.Errorf("marshal snapshot: %w", jerr)
	}
	if len(snapJSON) <= maxDetailsSnapshotBytes {
		var payload any
		if perr := json.Unmarshal(snapJSON, &payload); perr == nil {
			extra["inventorySnapshot"] = payload
		}
	} else {
		extra["inventorySnapshotNote"] = fmt.Sprintf("snapshot is %d bytes, above the %d-byte details cap; recorded summarized",
			len(snapJSON), maxDetailsSnapshotBytes)
	}
	if dst, stErr := os.Stat(opts.DataDir); stErr == nil && dst.IsDir() {
		fiPath := filepath.Join(opts.DataDir, "inventory-"+opID+".json")
		if wErr := os.WriteFile(fiPath, snapJSON, 0o600); wErr != nil {
			return fmt.Errorf("write %s: %w", fiPath, wErr)
		}
	}
	return nil
}

// --- phase 2: gated Dokku setup --------------------------------------------------

// runDokkuSetup is the dokku presence decision and the gated install
// path. The approval semantics (ADR-0009): the CLI's confirmation
// (interactive on a TTY by default, --confirm otherwise) IS the consent
// for the change list printed in the dry-run plan and in the refusal
// messages — apt packages, nginx on 80/443, the dokku user, sshcommand
// hooks. Sudo is run NON-interactively (`sudo -n`): when the password is
// required, TOP prints the exact command for the operator's own terminal
// and returns a typed refusal — never a password prompt through captured
// exec, never a passwordless assumption.
func runDokkuSetup(ctx context.Context, opts Options, res *Result) error {
	vout, _, err := opts.Runner.Run(ctx, "dokku", "version")
	switch {
	case errors.Is(err, exec.ErrNotFound):
		// Absent: the decision point.
		if !opts.InstallDokku {
			return fmt.Errorf("%w: dokku is not installed; either re-run with --install-dokku (approval-gated, changes apt packages + nginx on 80/443 + the dokku user + sshcommand hooks) or install Dokku %s+ yourself per https://dokku.com/docs/getting-started/installation/ and re-run",
				ErrDokkuRequired, doctor.MinDokkuVersion)
		}
		return runDokkuBootstrap(ctx, opts, res)
	case err != nil:
		return fmt.Errorf("dokku is present but `dokku version` failed: %w (fix: `sudo -E dokku` / add yourself to the dokku group; reinstall per docs.dokku.com if worse)", err)
	default:
		version := strings.TrimSpace(string(vout))
		if !atLeastVersion(version, doctor.MinDokkuVersion) {
			return fmt.Errorf("dokku reports %q, below the supported floor %s (hint: upgrade per docs.dokku.com and re-run)", version, doctor.MinDokkuVersion)
		}
		res.Checked = append(res.Checked, "dokku "+version+" ≥ "+doctor.MinDokkuVersion+" (present, healthy)")
		return nil
	}
}

// runDokkuBootstrap runs the pinned upstream install script per the
// documented bootstrap: wget the pinned script (plain exec, no sudo), then
// `sudo -n DOKKU_TAG=<pin> bash bootstrap.sh`. Non-interactive sudo
// refusals print the exact fallback commands and return ErrSudoRefused —
// a clean exit with instructions is a SUCCESS path here in spirit (the
// operator takes one step in their terminal, then re-runs install;
// phases 0-1 are idempotent).
func runDokkuBootstrap(ctx context.Context, opts Options, res *Result) error {
	if _, _, err := opts.Runner.Run(ctx, "wget", "-NP", ".", dokkuBootstrapScriptURL); err != nil {
		return fmt.Errorf("fetching the pinned dokku bootstrap script failed: %w (manual path: %s)", err, wgetScriptCommand())
	}
	// `sudo -n` never prompts: passwordless sudo proceeds, anything else
	// refuses fast and lands on the printed fallback (ADR-0009).
	if _, _, err := opts.Runner.Run(ctx, "sudo", "-n", "DOKKU_TAG="+dokkuBootstrapTag, "bash", bootstrapScriptFileName); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			res.Commands = append(res.Commands, sudoScriptCommands()...)
			return fmt.Errorf("%w: sudo is not on PATH; run the documented bootstrap by hand, then re-run `topctl install`:\n  %s",
				ErrSudoRefused, strings.Join(sudoScriptCommands(), "\n  "))
		}
		res.Commands = append(res.Commands, sudoScriptCommands()...)
		return fmt.Errorf("%w: sudo requires your password — TOP does not prompt through captured output (ADR-0009: print, not escalate). Run these in your terminal, then re-run `topctl install`: %s",
			ErrSudoRefused, strings.Join(sudoScriptCommands(), "; "))
	}
	out, _, verr := opts.Runner.Run(ctx, "dokku", "version")
	if verr != nil {
		return fmt.Errorf("dokku install finished but `dokku version` failed (post-install verification): %w", verr)
	}
	version := strings.TrimSpace(string(out))
	if !atLeastVersion(version, doctor.MinDokkuVersion) {
		return fmt.Errorf("post-install dokku reports %q, below the floor %s", version, doctor.MinDokkuVersion)
	}
	res.Checked = append(res.Checked, "dokku "+version+" installed via the pinned "+dokkuBootstrapTag+" bootstrap (≥ "+doctor.MinDokkuVersion+")")
	// DOKKU_VHOST/NGINX port detection is deliberately out of scope for
	// this task (proxy property wiring is M3+); noted, not probed.
	return nil
}

// atLeastVersion is semverish floor comparison ("maj.min" floor vs
// "maj.min[.patch]-or-v-prefixed" actual). Doctor's engineAtLeast stays
// unexported; this bounded local twin is re-verifiable against doctor's
// tests and documented as such.
func atLeastVersion(actual, floor string) bool {
	a, f := verParts(actual), verParts(floor)
	for i := 0; i < len(f); i++ {
		av := 0
		if i < len(a) {
			av = a[i]
		}
		if av < f[i] {
			return false
		}
		if av > f[i] {
			return true
		}
	}
	return true
}

func verParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	fields := strings.Split(v, ".")
	out := []int{}
	for _, fld := range fields {
		n := 0
		for i := 0; i < len(fld); i++ {
			if fld[i] < '0' || fld[i] > '9' {
				break
			}
			n = n*10 + int(fld[i]-'0')
		}
		out = append(out, n)
	}
	return out
}

// --- phase 3: platform wiring ----------------------------------------------------

// dir targets TOP creates (contract: dataDir itself + secrets, git;
// 0700 dirs).
var wiringDirs = []struct{ sub string }{
	{sub: ""},
	{sub: "secrets"},
	{sub: "git"},
}

// runWiring creates the data-dir tree, provisions key material, creates
// the shared internal network, and registers the git remote URL on the
// operation record. Directory creation is host mutation — gated by
// Confirm at the runner level (Install refuses without it) and skipped by
// dry-run before ever reaching here.
func runWiring(ctx context.Context, opts Options, st store.Store, opID string, res *Result, extra phaseExtras) error {
	created, err := ensureDataDirs(opts.DataDir)
	if err != nil {
		return err
	}
	res.Created = append(res.Created, created...)

	// Deploy key + known_hosts: operator-supplied paths (a) copied in at
	// 0600. Never read the KEY content into argv/logs — file copy only.
	// The key lands under gitsource's ADR-0008 name (id_ed25519 — the
	// reader gitsource.New hardcodes); provisioning "deploy_key" made
	// phase 5's first reconcile ls-remote with a nonexistent identity
	// (live homelab install, 2026-11-14: two M2-era components each
	// invented their own name).
	gitDir := filepath.Join(opts.DataDir, "git")
	for _, p := range []struct{ src, dst string }{
		{opts.DeployKey, filepath.Join(gitDir, "id_ed25519")},
		{opts.KnownHosts, filepath.Join(gitDir, "known_hosts")},
	} {
		if err := copyFile0600(p.src, p.dst); err != nil {
			return fmt.Errorf("provision %s: %w", filepath.Base(p.dst), err)
		}
		res.Created = append(res.Created, "file "+p.dst+" (0600)")
	}

	// Shared internal network, via the backend (extend-not-fork).
	if err := opts.Backend.EnsureNetwork(ctx, InternalNetwork); err != nil {
		return fmt.Errorf("ensure network %s: %w", InternalNetwork, err)
	}
	// Ledger anchor: bootstrap-owned row for the network. NOTE the
	// two-key consequence: dokku's network:create cannot carry
	// top.dev/* labels, so uninstall's fresh-snapshot check refuses this
	// row (skip-with-reason + printed manual command) — recorded
	// deliberately, the refusal is the safer failure mode.
	if err := inventory.MarkCreated(ctx, st, inventory.KindNetwork, InternalNetwork, opID, topLabels("core", InternalNetwork), opts.Now()); err != nil {
		return fmt.Errorf("ledger network %s: %w", InternalNetwork, err)
	}
	res.Created = append(res.Created, "network "+InternalNetwork)

	// Repo initialization (--init, ADR-0005's contract clause in phase 3):
	// when the flag passed and the remote resolves EMPTY (freshly probed
	// here), seed the embedded platform-repo template and push one commit
	// (internal/install/repo_init.go for the credential model). Without
	// --init this is a no-op BY DESIGN — the existing-repo path touches
	// the remote with nothing but phase 0's read-only ls-remote.
	if opts.Init {
		if err := runRepoInit(ctx, opts, st, opID, res, extra); err != nil {
			return err
		}
	}

	// Git remote registration: recorded on the phase op (details above,
	// via extra) — the full store-side registration + Resolve check is
	// T-212/M3 (documented deviation).
	extra["gitRemote"] = opts.RepoURL
	extra["gitBranch"] = opts.Branch
	note := " recorded on the install op; the store-side registration + Resolve check stays with the controller (M3)"
	if opts.Init {
		note = " recorded on the install op (--init seeded it: the first reconcile finds " + opts.RepoURL + " at the pushed seed revision)"
	}
	res.Checked = append(res.Checked, "git remote "+opts.RepoURL+" (branch "+opts.Branch+")"+note)
	return nil
}

// topLabels is the top.dev/* label set for bootstrap-created resources
// (ADR-0009 layer 2; TOP never writes dokku/com.dokku.* labels — ADR-0001).
func topLabels(namespace, service string) map[string]string {
	return map[string]string{
		inventory.LabelManaged:   inventory.ManagedTrue,
		inventory.LabelNamespace: namespace,
		inventory.LabelService:   service,
	}
}

// ensureDataDirs creates dataDir + the three subdirs at 0700 (chmod AFTER
// MkdirAll so an inherited-umask creation still ends up 0700) and returns
// what it created. Idempotent: existing dirs are re-tightened (contract:
// "TOP re-tightens this during install").
func ensureDataDirs(dataDir string) ([]string, error) {
	created := []string{}
	for _, wd := range wiringDirs {
		path := dataDir
		if wd.sub != "" {
			path = filepath.Join(dataDir, wd.sub)
		}
		existed := true
		if st, err := os.Stat(path); err != nil || !st.IsDir() {
			existed = false
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", path, err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return nil, fmt.Errorf("chmod 700 %s: %w", path, err)
		}
		if !existed {
			created = append(created, "dir "+path+" (0700)")
		} else {
			created = append(created, "dir re-tightened "+path+" (0700)")
		}
	}
	return created, nil
}

// copyFile0600 copies src → dst with mode 0600 (secret material-deploy
// keys go 0600, never wider; never logged).
func copyFile0600(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // operator-supplied path is the feature
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close() //nolint:errcheck // read-only
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

// --- phase 4: bootstrap deploy ----------------------------------------------------

// runBootstrapDeploy is ADR-0005's one-time direct deploy, doc'ed here:
//
//  1. EnsureStorageEntry(StorageEntryName, dataDir)   — SQLite's mount
//  2. EnsureApp(core/top-controller)                   — create or RESUME-adopt
//  3. AttachStorage(app, entry, containerPath=host path) — mount for deploy
//  4. DeployImage(app, pinned image ref)               — git:from-image
//  5. bounded Inspect health wait (deployed && running)
//
// Every created asset gets its bootstrap-owned ledger row (created_by =
// this phase's op). On ANY failure the phase's op records
// "bootstrap-failed: …" (the contract's recovery marker — failed ops
// are visible via `topctl history`) and the partial state stays: re-runs
// adopt what exists and finish.
func runBootstrapDeploy(ctx context.Context, opts Options, st store.Store, opID string, res *Result, extra phaseExtras) error {
	app := opts.ControllerAppName()
	ref := ControllerRef()

	// 1. Storage entry (the SQLite state's bind mount; ADR-0007's
	// top.db-on-mount requirement). The adapter owns the idempotency
	// decision (storage:info probe); the result lands in Checked, not
	// Created, so the report never claims a fresh creation on resume.
	if err := opts.Backend.EnsureStorageEntry(ctx, StorageEntryName, opts.DataDir); err != nil {
		return fmt.Errorf("ensure storage entry %s: %w", StorageEntryName, err)
	}
	res.Checked = append(res.Checked, "storage-entry "+StorageEntryName+" ensured → host "+opts.DataDir)

	// Anchor the created app + storage decision in the ledger BEFORE the
	// deploy so a failed deploy still leaves the honest paper trail —
	// then the deploy. EnsureApp ADOPTS (creates only when absent), so
	// the ledger distinguishes: adopt happens only on the resume path
	// where phase 4 already recorded the app (or phase 0 would have
	// failed on a foreign app).
	//
	// The create is retried on dokku's verified first-boot poison window
	// (tier-3 e2etest, 2026-10-28): right after the host's dokku daemon
	// boots, `apps:create` can transiently exit 1 with "! No web
	// listeners specified" and the flake is sticky for a failed-create
	// window. Nothing else is retried: every other failure surfaces.
	ensureDeadline := time.Now().Add(60 * time.Second)
	for {
		ensureErr := opts.Backend.EnsureApp(ctx, ref)
		if ensureErr == nil {
			break
		}
		if !strings.Contains(ensureErr.Error(), "No web listeners specified") || time.Now().After(ensureDeadline) {
			return fmt.Errorf("ensure app %s: %w", app, ensureErr)
		}
		time.Sleep(2 * time.Second)
	}
	rowMarked, rerr := st.ListLedger(ctx, inventory.KindApp)
	if rerr != nil {
		return fmt.Errorf("ledger app scan: %w", rerr)
	}
	known := false
	for _, e := range rowMarked {
		if e.Identifier == app {
			known = true
			break
		}
	}
	if !known {
		if err := inventory.MarkCreated(ctx, st, inventory.KindApp, app, opID, topLabels(domain.CoreNamespace.String(), domain.Name("top-controller").String()), opts.Now()); err != nil {
			return fmt.Errorf("ledger app %s: %w", app, err)
		}
		res.Created = append(res.Created, "app "+app)
	} else {
		res.Adopted = append(res.Adopted, "app "+app+" (resume: deploy failure left it; adopting to finish)")
	}

	if err := opts.Backend.AttachStorage(ctx, ref, StorageEntryName, opts.DataDir); err != nil {
		return fmt.Errorf("attach storage %s to %s: %w", StorageEntryName, app, err)
	}
	res.Created = append(res.Created, "mount "+StorageEntryName+":"+opts.DataDir+" → "+app)

	// Docker-socket mount (deploy stage): the controller container has no
	// dokku CLI — its PATH shim drives the HOST's dokku through the Docker
	// daemon socket (`docker exec <runtime> dokku …`, cmd/topctl/dokku-shim;
	// the tier-3/4 e2e harnesses verified this shape 2026-11-03, the live
	// homelab install forced it into the product 2026-11-13). Idempotent:
	// only add when the deploy-stage options don't already carry the mount.
	sockMount := "-v /var/run/docker.sock:/var/run/docker.sock"
	if out, _, rerr := opts.Runner.Run(ctx, "dokku", "docker-options:report", app, "--format", "json"); rerr == nil {
		if !strings.Contains(string(out), "/var/run/docker.sock") {
			if _, _, aerr := opts.Runner.Run(ctx, "dokku", "docker-options:add", app, "deploy", sockMount); aerr != nil {
				return fmt.Errorf("docker-options add socket mount: %w", aerr)
			}
			res.Created = append(res.Created, "docker-options deploy "+sockMount+" → "+app+" (controller dokku-shim transport)")
		}
	} else {
		return fmt.Errorf("docker-options report %s: %w (needed to wire the controller's dokku-shim transport)", app, rerr)
	}

	// The deploy body. DeployImage failure surfaces its own adapter error
	// text; the type-carrying layer above marks the phase op bootstrap-failed.
	if err := opts.Backend.DeployImage(ctx, ref, opts.ImageRef()); err != nil {
		return fmt.Errorf("bootstrap deploy of %s: %w (stderr: %s) (manual path: `dokku git:from-image %s %s`)",
			opts.ImageRef(), err, stderrExcerpt(err, 400), app, opts.ImageRef())
	}
	res.Created = append(res.Created, "bootstrap-deploy "+app+" ← "+opts.ImageRef())

	// Bounded health wait — Inspect until deployed && running.
	if err := waitHealthy(ctx, opts, ref, app); err != nil {
		return err
	}
	res.Checked = append(res.Checked, "health: deployed && running ("+app+")")
	extra["app"] = app
	extra["image"] = opts.ImageRef()
	return nil
}

// The comment above is retired with the removed
// storageEntryExists draft; the phase defers entry/creation idempotency
// to the adapter (Ensure*, never probe-then-act from the caller side).

// waitHealthy polls Inspect until deployed && running or timeout,
// through the FULL backend (richer report re-use is the point of the
// interface-inspect seam).
func waitHealthy(ctx context.Context, opts Options, ref domain.ServiceRef, app string) error {
	deadline := time.Now().Add(opts.HealthTimeout)
	for {
		obs, err := opts.Backend.Inspect(ctx, ref)
		if err == nil && obs.Deployed && obs.Running {
			return nil
		}
		last := "inspect failed: " + fmt.Sprint(err)
		if err == nil {
			last = fmt.Sprintf("deployed=%t running=%t", obs.Deployed, obs.Running)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("waiting for %s health: not deployed+running within %s: %s — manual path: `dokku ps:report %s --format json`, `dokku logs %s --num 200`; re-run `topctl install` to resume",
				app, opts.HealthTimeout, last, app, app)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s health: %w (re-run `topctl install` to resume)", app, ctx.Err())
		case <-time.After(opts.PollInterval):
		}
	}
}

// stderrExcerpt returns a bounded, single-line excerpt of the failed
// exec's stderr (the evidence the op record otherwise loses — the
// 2026-11-13/14 homelab debug loop ran blind on "exit 1" alone). ~400
// bytes; redaction already applied by the exec layer.
func stderrExcerpt(err error, max int) string {
	var ee *exec.ExitError
	if !errors.As(err, &ee) || len(ee.Stderr) == 0 {
		return "none captured"
	}
	s := strings.ReplaceAll(string(ee.Stderr), "\n", " | ")
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// writeRuntimeConfigIfAbsent seeds <dataDir>/top-config.yaml on first
// install (0600): the git remote install just seeded, the reserved
// namespaces, and the daemon cadence — the operator edits it, but should
// never have to author it (T-600, live homelab 2026-11-14: the daemon
// polled an EMPTY git url until the operator hand-wrote a config it
// could not have known the shape of). The controller reads this same
// file (cli --config default resolution), so the daemon and every CLI
// verb see the install's own repo from the first start.
func writeRuntimeConfigIfAbsent(opts Options, res *Result) error {
	path := opts.DataDir + "/top-config.yaml"
	if _, err := os.Stat(path); err == nil {
		return nil // operator-owned once it exists
	}
	// A resumed run may reach phase 6 with the DataDir not yet created
	// (a phase-1 skip over a pre-filesystem interruption) — the config
	// write must not be the thing that fails then.
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return fmt.Errorf("datadir %s: %w", opts.DataDir, err)
	}
	body := fmt.Sprintf(`# Written by topctl install (TOP %s) — the runtime config the
# controller daemon and every CLI verb read. Edit freely; install never
# overwrites an existing file.
apiVersion: top/v1alpha1
git:
  url: %s
  branch: %s
controller:
  pollIntervalSeconds: 60
  reconcileIntervalSeconds: 300
secretsProvider:
  kind: hostfile
namespaces:
  reserved: [%s]
`, opts.InitVersion, opts.RepoURL, opts.Branch, strings.Join(opts.Namespaces, ", "))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write runtime config %s: %w", path, err)
	}
	// 0644 deliberately: the file carries NO secrets (ADR-0008 keeps
	// those in <dataDir>/secrets, 0600 root) — a non-root `topctl diff`
	// must be able to read the git URL (live homelab 2026-11-14: 0600
	// root made every non-sudo CLI verb resolve an EMPTY git url).
	res.Created = append(res.Created, "file "+path+" (0644; runtime config)")
	return nil
}
