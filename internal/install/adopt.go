// SPDX-License-Identifier: MIT

package install

// T-402 — contract phase 5: FIRST RECONCILIATION + the ADR-0005 ownership
// handoff. docs/contracts/installer.md: "the bootstrapped controller loads
// the repo, adopts its own deployment (same app name; converts ledger
// entry to `managed`), and reconciles `core` then `observability`".
// ADR-0005: "First reconciliation must adopt the bootstrap controller
// deployment (same app name) rather than error on drift."
//
// Order inside the phase (deliberate):
//
//  1. LEDGER GUARD — the handoff converts ONLY a recorded bootstrap-owned
//     row (the ADR-0009 two-key spirit; MarkManaged itself refuses
//     pre-existing). Missing row / no controller spec → info finding, the
//     deployment STAYS bootstrap-owned, the phase records success with
//     details "adopted": false — and its re-run REDOES (scanPhases keys
//     this phase's completion on the adoption record), so a controller
//     spec pushed later still gets adopted.
//  2. MARK MANAGED — the store.MarkManaged flip FIRST, so the very first
//     reconciler pass already sees the managed row and behaves
//     adoption-aware (field drift converges; image drift reports
//     only — T-402 in internal/reconcile). Nothing is redeployed here:
//     the phase itself issues no host verb at all, and the reconciler's
//     noop-diff-for-matching-spec path is the anti-redeploy invariant's
//     proof (TestAdoptSelfNoRedeployWhenSpecMatches over the pipeline).
//  3. FIRST RUN — through the FirstRunner seam (composition root wires
//     gitsource+spec+reconcile; internal/reconcile's own tests prove the
//     pipeline half). A non-green run FAILS the phase and the run's
//     error; the manager already flipped stays (MarkManaged is
//     idempotent, the next `topctl install` redoes phase 5 only).
//
// Already-managed rows are always re-adopted NOTHING: the op-record
// resume guard skips a successful phase 5 entirely, and even a redone
// phase only re-verifies (MarkManaged is idempotent, details record
// "already managed").
//
// Idempotency anchors: the ADOPTED-BY-OP annotation is the phase-5
// operation record (details "adoptedByOp"), and the first run's own
// reconciliation operation id is recorded beside it ("firstRunOp") —
// the ledger row itself carries no annotation because MarkManaged's
// surface is (kind, identifier) only (zero store changes, T-402 scope).

import (
	"context"
	"fmt"

	"github.com/toledompm/top/internal/inventory"
	"github.com/toledompm/top/internal/store"
)

// FirstRunReport is what the reconciliation handoff reports to the
// installer's phase 5 (plain data, JSON-shapeable for op details).
type FirstRunReport struct {
	// Revision is the resolved git sha the run reconciled toward
	// ("" when no run happened).
	Revision string
	// RunID is the reconciliation's operation id ("op_<uuid>"; "" when
	// no run happened).
	RunID string
	// ControllerSpec reports whether the repo defines
	// core/top-controller (the adoption precondition). False → the
	// deployment stays bootstrap-owned (info finding, never a failure).
	ControllerSpec bool
	// Succeeded reports the run recorded green (contract: install exits
	// 0 only when Readiness(core) is green and the first run is
	// recorded; T-402 keys the handoff on true).
	Succeeded bool
	// Status is the run's own status string (recorded on the phase op
	// for diagnosis when Succeeded is false; "" when no run happened).
	Status string
	// Notes are operator-facing lines from the handoff (e.g. generated
	// secrets), surfaced on the install envelope. Values NEVER ride
	// here — file paths only (ADR-0008).
	Notes []string
}

// FirstRunner is the phase-5 seam: ONE reconciliation pass over the
// seeded platform repo's specs. The composition root (internal/cli,
// mirroring orReconciler) wires the production implementation:
//
//	gitsource (resolve seeded revision → fetch snapshot)
//	→ spec.LoadDir(WithPlatformRepo) (strict)
//	→ reconcile.Reconciler.Run (the ADR-0005 adopt-aware pipeline,
//	  ControllerSpec = the bundle defines install.ControllerRef())
//
// and reports ControllerSpec even when the run itself fails (the missing
// controller spec is the interesting no-spec finding). Tests fake it;
// the tier-1 integration test wires the REAL gitsource+spec+reconcile
// over a local file:// repo (split runner, internal/cli's pattern).
type FirstRunner interface {
	FirstRun(ctx context.Context) (FirstRunReport, error)
}

// runFirstReconcile is phase 5's body (contract's first reconciliation +
// adoption), wrapped by runPhase like every other phase.
func runFirstReconcile(ctx context.Context, opts Options, st store.Store, opID string, res *Result, extra phaseExtras) error {
	app := opts.ControllerAppName()

	// 1. Ledger guard: the exact ADR-0009 posture.
	entries, lerr := st.ListLedger(ctx, inventory.KindApp)
	if lerr != nil {
		return fmt.Errorf("adoption ledger scan %s: %w", app, lerr)
	}
	var row *store.Resource
	for _, e := range entries {
		if e.Identifier == app {
			row = e
			break
		}
	}
	switch {
	case row == nil:
		return adoptSkipped(res, extra, app, "no bootstrap-owned ledger row for the controller app; nothing to adopt (ADR-0009: nothing is claimed without a recorded row)")
	case row.State == store.ResourceManaged:
		extra["adopted"] = true
		extra["adoptReason"] = "already managed (idempotent re-verify; re-adopts nothing)"
		res.Checked = append(res.Checked, "ownership "+app+" already managed — adoption skipped (idempotent re-run)")
		return nil
	case row.State == store.ResourcePreExisting:
		// Cannot happen in sane installs (phase 0 refuses foreign
		// apps), but the typed refusal is the honest answer: like
		// MarkManaged, adoption never converts a pre-existing row.
		return fmt.Errorf("adopt %s: %w: the controller app is recorded pre-existing; the handoff converts bootstrap-owned rows only (ADR-0009)",
			app, ErrAdoptionRefused)
	}

	// 2. If no reconciliation handoff is wired (Options.FirstRun nil —
	// the composition root's wiring, T-406 slot), the phase DOES NOT
	// adopt: an info finding records the skip and the deployment stays
	// bootstrap-owned; the next wired install redoes the phase and
	// adopts then. Nothing fails: refusing the whole install for an
	// un-wired seam would punish environments that legitimately finished
	// install on an older build.
	if opts.FirstRun == nil {
		return adoptSkipped(res, extra, app,
			"no reconciler handoff wired (Options.FirstRun nil) — the bootstrap deployment stays bootstrap-owned; re-run `topctl install` once the composition wires gitsource+spec+reconcile")
	}

	rep, rerr := opts.FirstRun.FirstRun(ctx)
	if rerr != nil {
		return fmt.Errorf("first reconciliation: %w", rerr)
	}
	extra["firstRunOp"] = rep.RunID
	extra["firstRunRevision"] = rep.Revision
	extra["firstRunStatus"] = rep.Status
	res.Notes = append(res.Notes, rep.Notes...)

	if !rep.ControllerSpec {
		return adoptSkipped(res, extra, app,
			"the platform repo defines no core/top-controller spec — the bootstrap deployment stays bootstrap-owned; re-run `topctl install` after the spec lands and the handoff completes then")
	}
	if !rep.Succeeded {
		return fmt.Errorf("first reconciliation %s reported status %q, not green: the ledger handoff is NOT recorded (row %s stays bootstrap-owned); inspect `topctl history`, fix, re-run `topctl install`",
			rep.RunID, rep.Status, app)
	}

	// 3. The handoff. MarkManaged is the whole mutation: a state
	// conversion on the recorded row — no Dokku verb, no redeploy.
	if err := st.MarkManaged(ctx, inventory.KindApp, app); err != nil {
		return fmt.Errorf("adopt %s (bootstrap-owned → managed): %w", app, err)
	}
	extra["adopted"] = true
	extra["adoptedByOp"] = opID
	res.Adopted = append(res.Adopted,
		"ownership "+app+" bootstrap-owned → managed (adopted by op "+opID+", first run "+rep.RunID+" @ "+rep.Revision+")")
	res.Notes = append(res.Notes,
		"controller self-updates stay human-gated (ADR-0005): the reconciler reports core/top-controller image/revision drift but never applies it — upgrades go through `topctl deploy core/top-controller` (T-406)")
	return nil
}

// adoptSkipped records the adoption-declined outcome (info finding, op
// succeeds with details "adopted": false; the re-run redoes the phase —
// the scanPhases completion key is the adoption record itself).
func adoptSkipped(res *Result, extra phaseExtras, app, reason string) error {
	extra["adopted"] = false
	extra["adoptReason"] = reason
	res.Skipped = append(res.Skipped, "adoption: "+reason)
	res.Notes = append(res.Notes,
		"bootstrap deployment stays bootstrap-owned (first reconciliation recorded an adoption precondition miss); the next `topctl install` re-runs phase 5 and adopts then")
	return nil // info finding only: the phase op records success
}
