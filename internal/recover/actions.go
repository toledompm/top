// SPDX-License-Identifier: MIT

package recover

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store/sqlite"
)

// Restart, Redeploy, Restore — the mutating recovery actions of the
// backup-recovery contract's "Recovery" list (ADR-0005; the ADR-0009
// `--confirm` gate is enforced by the CLI before any of these runs).
//
// Shared rules, per the contract's failure behavior:
//
//   - every step is idempotent (re-running a finished action is safe);
//   - every result carries the exact manual command equivalent —
//     "recovery output doubles as a runbook";
//   - the actions depend ONLY on the Dokku CLI, the filesystem, and the
//     SQLite file — never on the controller, Prometheus, or Grafana
//     being healthy (ADR-0005).
//
// M2-slice scope: these bodies are working-but-minimal. Milestone 5
// (T-503) hardens them — restore especially, which is scaffold-only
// until the ADR-0007 safeguards exist.

// controllerRef is the controller's service ref ("core" is
// immutable-reserved in every legal config, ADR-0004/0005). The Dokku
// app name is derived from it, not from a bare constant, so the
// namespace-policy decision has exactly one author.
var controllerRef = domain.ServiceRef{Namespace: "core", Name: "top-controller"}

// resolved names what Redeploy decided to deploy and where that
// decision came from (runbook provenance: the printed output says why).
type resolved struct {
	// GitRevision is a repo commit sha to git:sync ("" when image).
	GitRevision string
	// ImageRef is an image reference to git:from-image ("" when rev).
	ImageRef string
	// Source names the decision origin ("--revision flag", "last-known-
	// good revision from the state-store checkpoint", …).
	Source string
	// Notes carry context for humans (e.g. store-unavailable fallback).
	Notes []string
}

// Restart restarts the controller via Dokku and waits bounded for
// health (ps:report deployed && running). With dryRun, it prints the
// exact commands and executes nothing (the CLI contract's invariant:
// every mutating command supports --dry-run printing the plan).
func Restart(ctx context.Context, opts Options, dryRun bool) (*Result, error) {
	opts.applyDefaults()
	app := controllerApp()
	res := &Result{
		App:      app,
		Commands: []string{"dokku ps:restart " + app},
		Source:   "direct Dokku restart (ADR-0005: no TOP process involved)",
	}
	if dryRun {
		res.Notes = append(res.Notes, "dry-run: the command above is what a real run would issue, followed by a bounded health wait")
		return res, nil
	}

	if _, _, err := opts.Runner.Run(ctx, "dokku", "ps:restart", app); err != nil {
		return res, fmt.Errorf("restart %s: dokku ps:restart failed: %w (exact retry: `dokku ps:restart %s`; then verify health: `dokku ps:report %s --format json`)",
			app, err, app, app)
	}
	if err := waitHealthy(ctx, opts, app); err != nil {
		return res, err
	}
	return res, nil
}

// waitHealthy polls ps:report until the app reports deployed+running or
// opts.HealthTimeout elapses (contract cap: 3 minutes). Read-only.
func waitHealthy(ctx context.Context, opts Options, app string) error {
	deadline := time.Now().Add(opts.HealthTimeout)
	for {
		deployed, running, err := healthSnapshot(ctx, opts, app)
		if err == nil && deployed && running {
			return nil
		}
		if deadlineExpired(time.Now(), deadline) {
			last := fmt.Sprintf("deployed=%t running=%t", deployed, running)
			if err != nil {
				last += fmt.Sprintf(" (last ps:report error: %v)", err)
			}
			return fmt.Errorf("waiting for %s health: not healthy within %s: %s — manual path: `dokku ps:report %s --format json`, `dokku logs %s --num 200`, then `topctl recover diagnose`",
				app, opts.HealthTimeout, last, app, app)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s health: %w — manual check: `dokku ps:report %s --format json`", app, ctx.Err(), app)
		case <-time.After(opts.PollInterval):
		}
	}
}

// deadlineExpired keeps the bounded-wait comparison behind a trivially
// testable helper (the contract's 3-minute cap is behavior).
func deadlineExpired(now, deadline time.Time) bool { return now.After(deadline) }

// healthSnapshot reads ps:report deployed/running (read-only).
func healthSnapshot(ctx context.Context, opts Options, app string) (bool, bool, error) {
	out, _, err := opts.Runner.Run(ctx, "dokku", "ps:report", app, "--format", "json")
	if err != nil {
		return false, false, err
	}
	m, jerr := parseReportObject(out)
	if jerr != nil {
		return false, false, jerr
	}
	deployed := reportBool(m, "deployed", "ps-deployed", "deployed-state")
	running := reportBool(m, "running", "ps-running", "running-state")
	return deployed, running, nil
}

// Redeploy redeploys the controller directly via Dokku:
//
//  1. resolve what to deploy (--revision flag > store checkpoint
//     last-known-good, git sha or image ref > pinned bootstrap image);
//  2. run its commands (git:sync+ps:rebuild for a sha — which requires
//     the config's git.url — or git:from-image for an image ref);
//  3. wait bounded for health.
//
// A broken or absent store does NOT abort (ADR-0005: recovery survives
// a broken store): we fall back to the pinned bootstrap image and carry
// the store failure in the result's notes.
func Redeploy(ctx context.Context, opts Options, revisionFlag string, dryRun bool) (*Result, error) {
	opts.applyDefaults()
	app := controllerApp()

	choice := resolveDeploySource(ctx, opts, revisionFlag, controllerRef)
	res := &Result{
		App:      app,
		Revision: choice.GitRevision,
		Image:    choice.ImageRef,
		Source:   choice.Source,
		Notes:    choice.Notes,
		Commands: planCommands(choice, app, opts.GitURL),
	}
	if dryRun {
		res.Notes = append(res.Notes, "dry-run: the command(s) above are what a real run would issue, followed by a bounded health wait")
		return res, nil
	}

	if choice.GitRevision != "" && opts.GitURL == "" {
		return res, fmt.Errorf("cannot git:sync revision %s: no platform repo URL is configured (git.url in top-config.yaml or TOP_GIT_URL); manual equivalent: `dokku git:sync %s <repo-url> %s && dokku ps:rebuild %s`",
			choice.GitRevision, app, choice.GitRevision, app)
	}

	if err := runDeployChoice(ctx, opts, app, choice, opts.GitURL); err != nil {
		return res, err
	}
	if err := waitHealthy(ctx, opts, app); err != nil {
		return res, err
	}
	return res, nil
}

// planCommands renders the manual command equivalent for a resolution
// (substituting the configured repo URL, or the <repo-url> literal when
// none is configured — honest in both cases).
func planCommands(choice resolved, app, gitURL string) []string {
	commands := make([]string, 0, 2)
	if choice.GitRevision != "" {
		url := gitURL
		if url == "" {
			url = "<repo-url>"
		}
		commands = append(commands,
			fmt.Sprintf("dokku git:sync %s %s %s", app, url, choice.GitRevision),
			fmt.Sprintf("dokku ps:rebuild %s", app))
		return commands
	}
	return append(commands, fmt.Sprintf("dokku git:from-image %s %s", app, choice.ImageRef))
}

// runDeployChoice executes the chosen deploy path.
func runDeployChoice(ctx context.Context, opts Options, app string, choice resolved, gitURL string) error {
	var argv [][]string
	if choice.GitRevision != "" {
		argv = [][]string{
			{"git:sync", app, gitURL, choice.GitRevision},
			{"ps:rebuild", app},
		}
	} else {
		argv = [][]string{{"git:from-image", app, choice.ImageRef}}
	}
	for _, args := range argv {
		if _, _, err := opts.Runner.Run(ctx, "dokku", args...); err != nil {
			return fmt.Errorf("redeploy %s: dokku %s failed: %w (exact retry: `dokku %s`; then `topctl recover diagnose`)",
				app, args[0], err, strings.Join(args, " "))
		}
	}
	return nil
}

// resolveDeploySource picks what to deploy:
//
//   - revisionFlag (already validated as a sha or "kind=value" form —
//     the CLI validates plain shas; an explicit sha wins outright);
//   - else the store checkpoint: repo-wide last-known-good (git sha),
//     or the controller's per-service last-known-good (which may be an
//     image reference for image deploys — that deploys via from-image);
//   - else the pinned bootstrap image, noting why the store gave
//     nothing.
func resolveDeploySource(ctx context.Context, opts Options, revisionFlag string, ref domain.ServiceRef) resolved {
	switch {
	case revisionFlag != "":
		return resolved{GitRevision: revisionFlag, Source: "--revision flag"}
	}
	rev, image, source, notes := lastKnownGoodFromStore(ctx, opts, ref)
	if rev == "" && image == "" {
		return resolved{
			ImageRef: opts.BootstrapImage,
			Source:   "pinned bootstrap image (ADR-0005 fallback)",
			Notes:    append(notes, source),
		}
	}
	return resolved{GitRevision: rev, ImageRef: image, Source: source, Notes: notes}
}

// lastKnownGoodFromStore reads the state file (<dataDir>/top.db):
// repo-wide checkpoint last-known-good first, else the per-service
// last-known-good for the controller (a git sha or an image reference).
// Any failure returns a note only — treat the store as evidence, never
// as a hard dependency.
func lastKnownGoodFromStore(ctx context.Context, opts Options, ref domain.ServiceRef) (sha, image, source string, notes []string) {
	st, err := sqlite.Open(ctx, dbPath(opts))
	if err != nil {
		return "", "", "bootstrap fallback", []string{fmt.Sprintf("state store %s could not be opened (%v) — no last-known-good to redeploy from", dbPath(opts), err), err.Error()}
	}
	defer func() { _ = st.Close() }()

	cp, err := st.GetCheckpoint(ctx)
	if err != nil {
		return "", "", "bootstrap fallback", []string{fmt.Sprintf("could not read the checkpoint from %s (%v)", dbPath(opts), err), err.Error()}
	}
	if cp.LastKnownGood != "" {
		return cp.LastKnownGood, "", fmt.Sprintf("last-known-good revision from the state-store checkpoint (%s, repo-wide)", dbPath(opts)), nil
	}
	if ref, ok := cp.LastKnownGoodPerService[ref]; ok && ref.Value != "" {
		switch ref.Kind {
		case domain.RevisionGit:
			return ref.Value, "", fmt.Sprintf("last-known-good revision for %s from the state-store checkpoint (%s)", controllerRef, dbPath(opts)), nil
		case domain.RevisionImage:
			return "", ref.Value, fmt.Sprintf("last-known-good IMAGE for %s from the state-store checkpoint (%s)", controllerRef, dbPath(opts)), nil
		}
	}
	return "", "", "bootstrap fallback", []string{fmt.Sprintf("state store %s has no last-known-good checkpoint yet (fresh install or a never-green reconcile)", dbPath(opts))}
}

// Restore is SCAFFOLD ONLY in the M2 slice: it refuses to run.
//
// WHY (docs/contracts/backup-recovery.md + ADR-0007): restore overwrites
// live metadata, so the contract demands the safeguard order FIRST —
// controller stopped → live watermark vs backup watermark (refuse
// without --force) → safety snapshot of the current DB → replace + run
// migrations + restart controller + verify health → reconciliation
// hint. That engine is Milestone 5 (T-503); the CLI surface exists now
// so agents get an honest refusal (exit 3, code not-implemented)
// instead of a stub pretending.
func Restore(_ context.Context, _ Options, _ string) error {
	return ErrRestoreDeferred
}

// ErrRestoreDeferred marks the not-yet-M5 restore path; the CLI matches
// it with errors.Is and maps it to exit 3 + a runbook hint.
var ErrRestoreDeferred = errors.New("restore lands in Milestone 5 (T-503): the ADR-0007 safeguard order — stop controller, watermark check, safety snapshot, replace + migrate + restart + verify — must exist before a restore may run; the manual path is documented in docs/operations/recovery.md")
