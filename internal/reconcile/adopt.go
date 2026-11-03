// SPDX-License-Identifier: MIT

package reconcile

import (
	"context"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/inventory"
	"github.com/toledompm/top/internal/store"
)

// T-402 — adoption awareness for the controller-self special case.
//
// ADR-0005's bootstrap boundary works like this: the installer deploys
// core/top-controller directly (ledger row `bootstrap-owned`) and, at
// contract phase 5, hands management of that SAME deployment to the
// reconciler by flipping the ledger row to `managed` (store.MarkManaged).
// Nothing is redeployed: the adoption is a state-conversion record.
//
// The special case in apply.go therefore has THREE shapes now, keyed on
// the ledger:
//
//   - row absent or `bootstrap-owned` → pre-adoption: drift on self is
//     report-only (outcome skipped / self-drift-reported), exactly as
//     before T-402;
//   - row `managed` and the drift carries a revision/image change → the
//     ADR-0005 human gate: an unattended run (poll/webhook/loop) NEVER
//     applies a new image to its own process (a wedging self-deploy
//     would kill every future reconciliation, including the fix). The
//     drift is reported and the outcome points at
//     `topctl deploy core/top-controller` (T-406) — whose run itself
//     carries SelfDeployApproved and is allowed to apply, and echoes
//     the old-release preservation + rollback hint on the way out.
//   - row `managed` and the drift is field-only (env, ports, domains,
//     storage, restart policy, networks) → self converges like any other
//     service. EnsureApp is adopt-if-exists, none of these verbs carries
//     an image, so no redeploy can sneak in through this path either.
//
// A noop self plan is untouched in every case: the anti-redeploy
// invariant's strongest form — a spec identical to the bootstrap
// deployment produces zero apply verbs against the controller app.

// SelfAppName derives the controller self app's Dokku name through the
// one naming rule (domain.AppName on the core namespace). SelfRef is
// hard-validated vocabulary, so the error path is unreachable; "" (with
// "not adopted" semantics downstream) keeps the helper panic-free.
func SelfAppName() string {
	app, err := domain.AppName(SelfRef.Namespace, SelfRef.Name)
	if err != nil {
		return "" // unreachable: SelfRef is constant, pre-validated
	}
	return app
}

// adoptedSelf reads the controller app's ownership row: true only when a
// row exists AND its state is managed — i.e. the install handoff
// (docs/contracts/installer.md phase 5) recorded the ADR-0005 adoption.
// A missing row is "not adopted", not an error: the reconciler may run
// against a repo whose bootstrap row was never converted and must keep
// the report-only posture.
func adoptedSelf(ctx context.Context, st StateStore) (bool, error) {
	app := SelfAppName()
	if app == "" {
		return false, nil
	}
	entries, err := st.ListLedger(ctx, inventory.KindApp)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Identifier == app {
			return e.State == store.ResourceManaged, nil
		}
	}
	return false, nil
}

// selfNeedsHumanGate reports whether the controller's drift bundle
// contains a revision/image change — the ADR-0005 human-gated half
// (`topctl deploy core/top-controller`). Field-only drift is not gated.
func selfNeedsHumanGate(sp domain.ServicePlan) bool {
	for _, c := range sp.Changes {
		if c.Path == "revision" {
			return true
		}
	}
	return false
}
