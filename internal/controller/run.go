// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"time"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/obs"
	"github.com/toledompm/top/internal/reconcile"
	"github.com/toledompm/top/internal/spec"
	"github.com/toledompm/top/internal/webhook"
)

// webhookEnqueuer adapts the coalescing queue to internal/webhook's
// Enqueuer seam. The trigger identity of a delivery is always
// (webhook, configured-branch): payload refs are audit-only (webhook
// package contract), desired state is re-derived from the configured
// remote, and pinning the branch makes a push+poll storm coalesce into
// ONE pending trigger with the poll triggers instead of splitting.
type webhookEnqueuer struct {
	d *Daemon
}

// Enqueue implements webhook.Enqueuer: map the audited delivery onto a
// trigger. Queue decisions (coalesce / reject after close) are the
// queue's; the handler answers 503 on error so the provider retries
// while the poll loop remains the backstop.
func (w webhookEnqueuer) Enqueue(ctx context.Context, ev webhook.Event) error {
	return w.d.queue.Enqueue(ctx, Trigger{
		Source:        SourceWebhook,
		Branch:        w.d.cfg.Branch,
		CorrelationID: ev.DeliveryID,
	})
}

// actorFor maps the trigger source onto the operation record's actor
// (docs/contracts/README.md actor set, ADR-0003's history expectation:
// actor=webhook|poll|manual|schedule).
func actorFor(t Trigger) domain.Actor {
	switch t.Source {
	case SourceWebhook:
		return domain.ActorWebhook
	case SourceSchedule:
		return domain.ActorSchedule
	default:
		return domain.ActorController
	}
}

// runTrigger executes ONE queue drain end to end:
//
//	resolve (branch sha AT RUN START, ADR-0003) → fetch snapshot →
//	spec.LoadDir (platform repo: reserved namespaces allowed) →
//	reconcile.Run (whole pipeline: validate → lease → order → diff →
//	apply → gate → record) → metrics → (later).
//
// Error policy: run-level failures are recorded by the reconciler itself;
// here they only decide metric labels and a log line — a failed trigger
// NEVER kills the pump (ADR-0003; the next trigger or the periodic sweep
// retries). Record-freshness note: the reconciler advances the store
// checkpoint on every run it does not get lease-blocked in, so a failed
// run marks the state "seen" and the next poll re-triggers only on NEW
// branch movement, while the periodic sweep handles standing drift.
func (d *Daemon) runTrigger(ctx context.Context, t Trigger) {
	start := time.Now()
	names := "run"
	if t.CorrelationID != "" {
		names = t.Source + ":" + t.CorrelationID
	}

	sha, err := d.cfg.Source.ResolveLatest(ctx)
	if err != nil {
		d.log.Warn("controller: trigger resolve failed", "trigger", names, "err", err)
		return
	}

	snap, err := d.cfg.Source.FetchRevision(ctx, sha)
	if err != nil {
		// ADR-0003 git-outage behavior: desired state unresolved → run
		// aborted BEFORE any mutation; observed state untouched.
		d.log.Warn("controller: trigger fetch failed", "trigger", names, "sha", short(sha), "err", err)
		d.failMetrics(start)
		return
	}
	defer func() { _ = snap.Close() }()

	res, err := spec.LoadDir(snap.Dir, spec.WithPlatformRepo(true))
	if err != nil {
		// Validation of the repo bundle itself failed: contained, loud,
		// retried by the next trigger. Nothing was mutated; the reconciler
		// records its own history rows per run — a fetch load failure
		// means no RunInput existed, so there is nothing to record.
		d.log.Warn("controller: spec load failed", "trigger", names, "sha", short(sha), "err", err)
		d.failMetrics(start)
		return
	}
	for _, warn := range res.Warnings {
		d.log.Warn("spec warning", "file", warn.Path, "field", warn.Field, "code", warn.Code, "msg", warn.Message)
	}

	in := reconcile.RunInput{
		Specs:         res.Services,
		Revision:      sha,
		Actor:         actorFor(t),
		CorrelationID: t.CorrelationID,
		HolderID:      d.cfg.HolderID,
		VolumeRoot:    d.cfg.VolumeRoot,
		// T-403: the materialized revision's host path feeds the
		// provisioning-supply sync (internal/reconcile/provision.go) —
		// config/provisioning trees land in the VolumeRoot entry dirs
		// before any apply mounts them. Valid through defer snap.Close()
		// (the sync copies, never references).
		ProvisionSrc: snap.Dir,
	}
	out, err := d.cfg.Reconciler.Run(ctx, in)
	d.metricRun(t, start, out, err)

	// Lease-conflict canon (reconcile README open question, honored): a
	// run lease-blocked by another actor (e.g. `topctl reconcile --now`)
	// appends NO history row and mutates nothing — surface it as a plain
	// log line and let the winner's outcome stand.
	var lce reconcile.LeaseConflictError
	if errors.As(err, &lce) {
		d.log.Warn("controller: run skipped, reconcile lease held elsewhere",
			"trigger", names, "lease", lce.Lease, "holder", lce.Holder)
		return
	}
	if err != nil {
		d.log.Warn("controller: reconcile run failed", "trigger", names,
			"sha", short(sha), "operation", out.OperationID, "err", err)
		return
	}
	d.log.Info("controller: reconcile run finished", "trigger", names,
		"sha", short(sha), "operation", out.OperationID, "status", string(out.Status))
}

// short renders a sha for logs: first 12 hex chars ("unset" for none).
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "unset"
	}
	return sha
}

// metricRun records the ADR-0010 whole-run outcome. Label mapping
// (reconcile_total{result}): run-level failure or non-success status →
// error; a green run that changed something → drift; a green zero-change
// run → ok. The last-success gauge advances only on green.
func (d *Daemon) metricRun(t Trigger, start time.Time, out reconcile.RunResult, err error) {
	if d.cfg.Metrics == nil {
		return
	}
	dur := time.Since(start)
	result := obs.ResultOK
	switch {
	case err != nil:
		result = obs.ResultError
	case out.Status != domain.RunSucceeded:
		result = obs.ResultError
	case !out.Plan.Empty():
		result = obs.ResultDrift
	}
	d.cfg.Metrics.RecordReconcile(dur, result)
	if result == obs.ResultOK {
		d.cfg.Metrics.SetLastSuccessfulReconcileAt(time.Now())
	}
}

// failMetrics marks a trigger that died before a run existed (fetch or
// spec load failure) — it still counts as an errored reconcile attempt.
func (d *Daemon) failMetrics(start time.Time) {
	if d.cfg.Metrics == nil {
		return
	}
	d.cfg.Metrics.RecordReconcile(time.Since(start), obs.ResultError)
}
