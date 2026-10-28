// SPDX-License-Identifier: MIT

package gitsource

import (
	"context"
	"log/slog"
	"time"
)

// DefaultPollInterval is ADR-0003's 60 s canon (config
// controller.pollIntervalSeconds defaults to the same value).
const DefaultPollInterval = 60 * time.Second

// Poller is the trigger-half helper for the controller loop (T-302/T-310):
// it turns ADR-0003's 60 s canon into "what revision desired?" calls. It
// deliberately owns NO scheduling policy beyond the ticker — enqueueing,
// backoff of failed polls, overlap suppression are controller concerns.
type Poller struct {
	// Source polls this repo.
	Source *Source
	// Interval between resolution attempts; 0 means DefaultPollInterval.
	Interval time.Duration
	// OnError, when set, receives every failed poll instead of the loop
	// stopping (ADR-0003: failures retry forever, they do not kill sync).
	OnError func(err error)
}

// NewPoller builds a Poller with the default (config-loaded canonical)
// interval over s.
func (s *Source) NewPoller(interval time.Duration) *Poller {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	return &Poller{Source: s, Interval: interval}
}

// Poll resolves the branch and reports whether it changed relative to
// lastSHA (empty lastSHA means "first sight" — always changed). A vanished
// branch surfaces as ErrRevisionGone, NOT as "unchanged", so the caller
// alerts per the contract.
func (p *Poller) Poll(ctx context.Context, lastSHA string) (sha string, changed bool, err error) {
	sha, err = p.Source.ResolveLatest(ctx)
	if err != nil {
		return "", false, err
	}
	return sha, sha != lastSHA, nil
}

// Loop runs the poll cycle until ctx is done: resolve every Interval, and
// call onChange(currentSHA) whenever the rev differs from the last seen
// one. Poll failures never stop the loop (they are reported via OnError or
// slog); an onChange error is also non-fatal by design — the controller
// records it and the next poll decides again. Blocking onChange calls delay
// the next tick (goroutine-safe against repo thrash, ADR-0001 no-engaged
// overlap).
func (p *Poller) Loop(ctx context.Context, lastSHA string, onChange func(ctx context.Context, sha string) error) {
	if p.Interval <= 0 {
		p.Interval = DefaultPollInterval
	}
	t := time.NewTicker(p.Interval)
	defer t.Stop()

	saw := func() {
		sha, err := p.Source.ResolveLatest(ctx)
		if err != nil {
			if p.OnError != nil {
				p.OnError(err)
			} else {
				slog.Warn("gitsource: poll failed", "err", err)
			}
			return
		}
		if sha == lastSHA {
			return
		}
		lastSHA = sha // remember BEFORE invoking: a failing reconcile is
		// re-decided by the controller's own retry, not re-triggered here
		if err := onChange(ctx, sha); err != nil {
			slog.Warn("gitsource: poll change handler failed", "sha", sha, "err", err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			saw()
		}
	}
}
