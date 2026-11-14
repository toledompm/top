// SPDX-License-Identifier: MIT

package reconcile

import (
	"strings"
	"testing"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
)

// T-406 guard tests for RunInput.SelfDeployApproved: the explicit-human
// self-deploy gate (`topctl deploy core/top-controller`, ADR-0005). The
// reconciler still refuses self-apply on every run it initiates; ONLY a
// run that saw the adopted (managed) ledger row AND carries a
// `user`-actor approval may apply a revision/image change to the
// controller's own deployment. Fixture values are fictional.

// approvedSelfRun runs the harness with the given actor + approval over a
// controller spec whose image drifts from the bootstrap deployment.
func approvedSelfRun(t *testing.T, ledger store.ResourceState, actor domain.Actor, approved bool) (RunResult, *fakeBackend, error) {
	t.Helper()
	fs := newFakeStore()
	fb := newFakeBackend()
	if ledger != "" {
		seedSelfLedger(t, fs, ledger)
	}
	fb.seed(SelfRef, func(a *fakeApp) {
		a.deployed, a.running = true, true
		a.image = "ghcr.io/toledompm/top/topctl:v0.1.0"
		a.env["TZ"] = "UTC"
	})
	svc := selfSpec("", nil)
	svc.Source.Tag = "v2" // the new controller revision in git

	cc := newControlledClock()
	r, err := New(Options{Backend: fb, Store: fs, Clock: cc})
	if err != nil {
		t.Fatal(err)
	}
	in := RunInput{
		Specs:              []domain.Service{svc},
		Revision:           "ac00000000000000000000000000000000000000",
		Actor:              actor,
		SelfDeployApproved: approved,
	}
	res, err := r.Run(t.Context(), in)
	return res, fb, err
}

// TestSelfDeployApprovedHumanApplies: the deploy command's run — ledger
// managed, actor user, approval set — APPLIES the new image (DeployImage
// fires; the readiness pass succeeds against a seeded deployed-and-new
// controller) instead of the report-only skip.
func TestSelfDeployApprovedHumanApplies(t *testing.T) {
	t.Parallel()

	res, fb, err := approvedSelfRun(t, store.ResourceManaged, domain.ActorUser, true)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	out := selfOutcome(t, res, domain.OutcomeSucceeded, "")
	if len(out.Changes) == 0 || !strings.Contains(strings.Join(out.Changes, "; "), "~revision: ghcr.io/toledompm/top/topctl:v0.1.0 → ghcr.io/toledompm/top/topctl:v2") {
		t.Errorf("changes = %v, want the revision entry the gate applies", out.Changes)
	}
	if !strings.Contains(strings.Join(out.Changes, "; "), "+ports.http") {
		t.Errorf("the whole drift bundle applies (field sets included): %v", out.Changes)
	}
	seq := fb.sequenceOf()
	if len(seq) == 0 {
		t.Fatalf("approved human self-deploy must apply; zero verbs")
	}
	if !strings.HasPrefix(seq[0], "ensure:") {
		t.Errorf("first verb = %s, want ensure (adopt-if-exists) before the deploy", seq[0])
	}
	found := false
	for _, s := range seq {
		if strings.HasPrefix(s, "deploy:") {
			found = true
		}
	}
	if !found {
		t.Errorf("approved human self-deploy must fire DeployImage: %v", seq)
	}
}

// TestSelfDeployWithoutApprovalStaysReportOnly: the reconciler's own runs
// (no approval) keep the human-gate skip with the `topctl deploy`
// hint — pinned here so the T-406 field can only ever loosen the gate on
// a run that DECLARES it.
func TestSelfDeployWithoutApprovalStaysReportOnly(t *testing.T) {
	t.Parallel()

	res, fb, err := approvedSelfRun(t, store.ResourceManaged, domain.ActorUser, false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	out := selfOutcome(t, res, domain.OutcomeSkipped, domain.SkipSelfDrift)
	if !strings.Contains(out.Hint, "topctl deploy core/top-controller") {
		t.Errorf("hint = %q, want the deploy hint", out.Hint)
	}
	if seq := fb.sequenceOf(); len(seq) != 0 {
		t.Errorf("unapproved run must not touch the controller: %v", seq)
	}
}

// TestSelfDeployApprovalNeedsManagedRow: an approval cannot bypass the
// pre-adoption report-only posture — with the ledger still
// bootstrap-owned (or absent) the drift is skipped even with
// SelfDeployApproved (the human path deploys the ADOPTED deployment).
func TestSelfDeployApprovalNeedsManagedRow(t *testing.T) {
	t.Parallel()

	for _, ledger := range []store.ResourceState{store.ResourceBootstrapOwned, store.ResourceState("none")} {
		t.Run(string(ledger), func(t *testing.T) {
			res, fb, err := approvedSelfRun(t, ledger, domain.ActorUser, true)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			out := selfOutcome(t, res, domain.OutcomeSkipped, domain.SkipSelfDrift)
			if !strings.Contains(out.Hint, "report-only until the bootstrap handoff") {
				t.Errorf("hint = %q, want the pre-adoption posture", out.Hint)
			}
			if seq := fb.sequenceOf(); len(seq) != 0 {
				t.Errorf("pre-adoption approval must not apply: %v", seq)
			}
		})
	}
}

// TestSelfDeployRejectedForNonUserActor: the poll/webhook guard — an
// approval attached to a controller or webhook run (the loops must never
// carry one) fails validation as a run-level record and mutates nothing.
func TestSelfDeployRejectedForNonUserActor(t *testing.T) {
	t.Parallel()

	for _, actor := range []domain.Actor{domain.ActorController, domain.ActorWebhook} {
		t.Run(string(actor), func(t *testing.T) {
			res, fb, err := approvedSelfRun(t, store.ResourceManaged, actor, true)
			if err == nil || !strings.Contains(err.Error(), "self-deploy approval") {
				t.Fatalf("err = %v, want the self-deploy validation refusal", err)
			}
			if res.ValidationErr == "" {
				t.Errorf("validation error must be recorded on the run")
			}
			if seq := fb.sequenceOf(); len(seq) != 0 {
				t.Errorf("a refused run must not touch the controller: %v", seq)
			}
		})
	}
}
