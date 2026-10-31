// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"time"

	"github.com/toledompm/top/internal/obs"
)

// pollLoop is ADR-0003 trigger 2: `git ls-remote` every PollInterval (60 s
// default), compare against the store checkpoint's LastApplied sha, and
// enqueue ONE poll-trigger when the branch moved. This is the
// missed-webhook repair path and the mutable-move detector — the poll is
// the correctness backstop regardless of webhook health.
//
// Fresh-install semantics (decided here, documented in the README): the
// checkpoint is empty on a fresh store (LastApplied "") — see
// store.Checkpoint — so the FIRST successful resolution always enqueues
// the initial run. The poll NEVER outruns the queue: it just drops one
// trigger and lets coalescing absorb the rest.
func (d *Daemon) pollLoop(ctx context.Context) {
	t := time.NewTicker(d.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.pollOnce(ctx, time.Now())
		}
	}
}

// pollOnce does one resolve-compare-enqueue cycle. Every cycle is timed
// into git_poll_duration_seconds with its git_poll_total result: error
// (resolve failed), drift (branch moved), or ok (up to date). Failures
// never stop the loop (ADR-0003: sync survives an outage).
func (d *Daemon) pollOnce(ctx context.Context, start time.Time) {
	result := obs.ResultOK
	defer func() {
		if d.cfg.Metrics != nil {
			d.cfg.Metrics.RecordGitPoll(time.Since(start), result)
		}
	}()

	sha, err := d.cfg.Source.ResolveLatest(ctx)
	if err != nil {
		result = obs.ResultError
		d.log.Warn("controller: poll failed", "err", err)
		return
	}

	last := ""
	if cp, cpErr := d.cfg.Store.GetCheckpoint(ctx); cpErr == nil && cp != nil {
		last = cp.LastApplied
	}
	if sha == last {
		return
	}
	result = obs.ResultDrift
	corr := ""
	if sha != "" {
		corr = "poll:" + short(sha)
	}
	if err := d.queue.Enqueue(ctx, Trigger{
		Source:        SourcePoll,
		Branch:        d.cfg.Branch,
		CorrelationID: corr,
	}); err != nil {
		// Only possible after queue close; the producer is stopping
		// anyway. Note without this is the normal drain race.
		d.log.Debug("controller: poll trigger refused (draining)")
		return
	}
	d.log.Info("controller: poll saw branch movement", "sha", short(sha), "was", short(last))
}

// periodicLoop is ADR-0003 trigger 3: the drift sweep / out-of-band-host
// corrections at ReconcileInterval (1 h default). It enqueues regardless
// of sha state: the whole point is catching what neither webhook nor poll
// knows about (e.g. host-side `dokku` changes). A queued schedule trigger
// costs exactly one run whose diff is zero when everything is green.
func (d *Daemon) periodicLoop(ctx context.Context) {
	t := time.NewTicker(d.cfg.ReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case stamp := <-t.C:
			if err := d.queue.Enqueue(ctx, Trigger{
				Source:        SourceSchedule,
				Branch:        d.cfg.Branch,
				CorrelationID: "schedule:" + stamp.Format("20060102150405"),
			}); err != nil {
				d.log.Debug("controller: schedule trigger refused (draining)")
				return
			}
		}
	}
}
