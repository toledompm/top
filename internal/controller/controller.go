// SPDX-License-Identifier: MIT

// Package controller implements TOP's controller daemon (T-302/T-310,
// docs/adr/0003-git-sync-webhooks-polling.md): the coalescing trigger
// queue ("three triggers, one queue"), the poll + periodic full-reconcile
// loops, the HTTP server (webhook intake + /healthz + /metrics), and the
// graceful start/stop orchestration around internal/reconcile's Reconciler.
//
// Three triggers, ONE queue (ADR-0003): webhook deliveries (verified +
// deduped by internal/webhook), poll-sha movement, and the periodic
// sweep all Enqueue Triggers keyed (source, branch); the pump dequeues
// one, resolves the branch's sha AT RUN START, and runs the reconciler's
// full pipeline once. Trigger storms while a run is queued/executing
// coalesce into a single pending entry.
//
// What is NOT here: the fresh-install handoff / first-reconcile adoption
// stays M4 (T-172) — the daemon simply runs the reconciler over whatever
// the configured branch resolves to. The CLI entrypoint (`topctl
// controller run`) is Wave C glue: it builds Options (see
// OptionsFromConfig), constructs the Reconciler/Store/Source seams, and
// either calls Serve itself or wraps Serve with signal handling.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/toledompm/top/internal/config"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/gitsource"
	"github.com/toledompm/top/internal/obs"
	"github.com/toledompm/top/internal/reconcile"
	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/webhook"
)

// GitSource is the minimal surface of *gitsource.Source the controller
// consumes — stated structurally, matching exactly that type's method
// shapes, so tests fake it without touching internal/gitsource. *Source
// satisfies it.
type GitSource interface {
	// ResolveLatest reports the configured branch's current head sha
	// (`git ls-remote`).
	ResolveLatest(ctx context.Context) (string, error)
	// FetchRevision materializes one revision's tree into a private
	// snapshot directory (consume then Close).
	FetchRevision(ctx context.Context, sha string) (*gitsource.Snapshot, error)
}

var (
	_ GitSource        = (*gitsource.Source)(nil)
	_ webhook.Enqueuer = webhookEnqueuer{}
)

// EnvPollIntervalMS is the daemon-local pacing knob, mirroring
// internal/install's TOP_INSTALL_POLL_INTERVAL_MS precedent: it changes
// ONLY the poll cadence (hermetic tests, slow CI) — never correctness,
// and never a TOP-config schema field. Invalid values keep the config
// default (a typo must not stall convergence).
const EnvPollIntervalMS = "TOP_CONTROLLER_POLL_INTERVAL_MS"

// Internal/config has no controller HTTP address field yet — the CLI owns
// the override; the defaults live here once.
const (
	// DefaultHTTPAddr for the controller server (webhook + healthz +
	// metrics). Wave C wires a topctl flag/env over it.
	DefaultHTTPAddr = ":8080"
	// HTTPPathWebhook is the intake mount point (webhook package README
	// + ADR-0003).
	HTTPPathWebhook = "/webhooks/github"
	// DefaultDrainGrace bounds the shutdown wait for the ACTIVE run.
	DefaultDrainGrace = 30 * time.Second
	// HTTPDrainTimeout bounds the server's in-flight-request drain.
	HTTPDrainTimeout = 10 * time.Second
)

// DefaultReconcileInterval is ADR-0003's 1 h periodic full reconcile.
const DefaultReconcileInterval = 3600 * time.Second

// Options configures the daemon. Reconciler, Store and Source are
// required (nil → New fails with a clear wiring error — that is a config
// bug, not a runtime condition). Everything else defaults.
type Options struct {
	// Reconciler executes runs (CLI wiring builds it with reconcile.New:
	// Backend, Clock, Secrets, namespaces, VolumeRoot).
	Reconciler *reconcile.Reconciler
	// Store is the StateStore: webhook dedup, the poll checkpoint, and —
	// through the reconciler — run persistence and the reconcile lease.
	Store store.Store
	// Source is the platform-repo Git source (gitsource.New(cfg)).
	Source GitSource
	// Metrics is optional (nil = every metric call is skipped; tests).
	Metrics *obs.Metrics
	// Secret is the webhook-HMAC SecretProvider (ADR-0008 name
	// webhook-hmac; *secrets.HostFile satisfies the seam). Required only
	// when Addr is set. Per-request resolution means rotation needs no
	// restart.
	Secret webhook.SecretProvider
	// Log receives lifecycle and per-run lines. Never secret values,
	// never the remote URL (may embed a token — ADR-0008).
	Log *slog.Logger

	// Branch is the trigger identity's second axis (the configured
	// platform-repo branch); default "main".
	Branch string
	// PollInterval is ADR-0003's cadence; default 60 s, or
	// EnvPollIntervalMS when set (pacing-only knob).
	PollInterval time.Duration
	// ReconcileInterval is the periodic full-reconcile sweep; default 1 h.
	ReconcileInterval time.Duration
	// Addr is the HTTP listen address ("/webhooks/github", /healthz,
	// /metrics). "" disables the server — polls-only mode (ADR-0003:
	// poll-only deployments are fully supported).
	Addr string
	// HolderID names this daemon in the store's reconcile lease
	// (state-store contract); default "controller".
	HolderID string
	// VolumeRoot goes into every RunInput; empty → storage-bearing
	// services fail contained (TOP never mounts un-owned host dirs,
	// ADR-0009). Same value the CLI passes to reconcile.New for pruning.
	VolumeRoot string
	// MaxBodyBytes hands webhook.NewHandler its body cap; 0 → handler
	// default (64 KiB).
	MaxBodyBytes int64
	// DrainGrace bounds the shutdown wait for the ACTIVE run before it
	// is cancelled; default DefaultDrainGrace (bounded per ADR-0005's
	// never-hang-forever stance).
	DrainGrace time.Duration
}

// OptionsFromConfig returns daemon Options with every config-derivable
// knob defaulted: controller.pollIntervalSeconds /
// controller.reconcileIntervalSeconds, git.branch, and dataDir (volume
// root; the webhook secret dir and git source are the CLI wiring's
// construction — see d's README). Wave-C CLI builds on this: load config
// → construct Reconciler/Store/Source/Secret → fill seam fields → Serve.
// Addr defaults on; topctl flags override or clear it before Serve.
func OptionsFromConfig(cfg config.Config) Options {
	o := Options{
		Branch:            cfg.Git.Branch,
		PollInterval:      time.Duration(cfg.Controller.PollIntervalSeconds) * time.Second,
		ReconcileInterval: time.Duration(cfg.Controller.ReconcileIntervalSeconds) * time.Second,
		Addr:              DefaultHTTPAddr,
		VolumeRoot:        cfg.DataDir + "/volumes",
	}
	if v := os.Getenv(EnvPollIntervalMS); v != "" {
		if n, err := parseMS(v); err == nil {
			o.PollInterval = n
		}
	}
	return o
}

// parseMS is EnvPollIntervalMS's strict shape: a positive integer.
func parseMS(v string) (time.Duration, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("controller: %s=%q: want a positive integer", EnvPollIntervalMS, v)
	}
	return time.Duration(n) * time.Millisecond, nil
}

// pollIntervalOrDefault applies the pacing knob on top of a defaulted
// interval (kept in one place for Options as well as OptionsFromConfig).
func pollIntervalOrDefault(d time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	if v := os.Getenv(EnvPollIntervalMS); v != "" {
		if ms, err := parseMS(v); err == nil {
			return ms
		}
	}
	return gitsource.DefaultPollInterval // config canon, 60 s (ADR-0003)
}

// New builds a daemon, applying defaults and validating the seams.
func New(o Options) (*Daemon, error) {
	if o.Reconciler == nil {
		return nil, fmt.Errorf("%w: controller: Options.Reconciler is required", domain.ErrInvalid)
	}
	if o.Store == nil {
		return nil, fmt.Errorf("%w: controller: Options.Store is required", domain.ErrInvalid)
	}
	if o.Source == nil {
		return nil, fmt.Errorf("%w: controller: Options.Source is required", domain.ErrInvalid)
	}
	if o.Branch == "" {
		o.Branch = "main"
	}
	o.PollInterval = pollIntervalOrDefault(o.PollInterval)
	if o.ReconcileInterval <= 0 {
		o.ReconcileInterval = DefaultReconcileInterval
	}
	if o.HolderID == "" {
		o.HolderID = "controller"
	}
	if o.DrainGrace <= 0 {
		o.DrainGrace = DefaultDrainGrace
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	ready := newReady()
	if o.Addr != "" {
		// The daemon will serve (Run re-binds later); readiness is a
		// server property, so it starts TRUE when an Addr is configured
		// and stays false in polls-only mode (no server, no /healthz).
		ready.set(true)
	}
	return &Daemon{
		cfg:   o,
		queue: NewQueue(),
		ready: ready,
		log:   log,
	}, nil
}

// newReady builds the daemon's readiness bit (true flips on when the
// HTTP server binds; never when Addr is empty — polls-only mode reports
// no readiness).
func newReady() *ready { return new(ready) }

// Daemon owns the three triggers, the coalescing queue, the single-run
// pump, and the HTTP server. Build with New; Run is the lifecycle;
// Serve is the production entrypoint (Wave C CLI glue calls it).
type Daemon struct {
	cfg   Options
	queue *Queue
	ready *ready
	log   *slog.Logger

	addrMu   sync.Mutex
	httpAddr string
}

// HTTPAddr reports the bound listener address ("" when no server or not
// started yet); tests and logs use it.
func (d *Daemon) HTTPAddr() string {
	d.addrMu.Lock()
	defer d.addrMu.Unlock()
	return d.httpAddr
}

// Enqueuer exposes the queue to any caller in this process that wants to
// hand the daemon a trigger (tests, future CLI hooks). The webhook
// handler uses the internal webhookEnqueuer adapter instead.
func (d *Daemon) Enqueuer() *Queue { return d.queue }

// Ready reports whether the server currently reports healthy (tests).
func (d *Daemon) Ready() bool { return d.ready.is() }

// Run executes the daemon until ctx is done or the HTTP server fails
// fatally, then drains. Lifecycle:
//
//  1. bind + start the HTTP server (if Addr set) and mark ready.
//  2. start the producers (poll loop, periodic loop).
//  3. start the pump (one reconcile at a time out of the queue).
//  4. block on ctx.Done() / server error.
//  5. run the ordered shutdown drain (below); return nil on signal, the
//     server error on a fatal bind/serve failure.
//
// The reconcile-run context is deliberately NOT a child of ctx: shutdown
// gives the ACTIVE run a bounded grace (DrainGrace) before cancelling it
// — a Ctrl-C mid-deploy does not orphan a half-applied plan; the run
// records its own outcome first, and the reconciler releases the lease
// with context.WithoutCancel either way.
func (d *Daemon) Run(ctx context.Context) error {
	runCtx, runCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer runCancel()

	var srv *httpServer
	srvErr := make(chan error, 1)
	if d.cfg.Addr != "" {
		s, err := d.newServer()
		if err != nil {
			return fmt.Errorf("controller: server: %w", err)
		}
		srv = s
		d.ready.set(true)
		d.addrMu.Lock()
		d.httpAddr = s.ln.Addr().String()
		d.addrMu.Unlock()
		go func() { srvErr <- srv.serve() }()
		d.log.Info("controller: http server listening", "addr", d.cfg.Addr,
			"webhook", HTTPPathWebhook)
	}

	// Producers stop BEFORE the queue closes so no trigger is enqueued
	// after Close's pending-discard (a trigger lost in that window is
	// harmless — poll re-arms in 60 s — but the ordering makes the race
	// impossible rather than merely benign).
	var prodWG sync.WaitGroup
	prodCtx, prodCancel := context.WithCancel(ctx)
	prodWG.Add(2)
	go func() { defer prodWG.Done(); d.pollLoop(prodCtx) }()
	go func() { defer prodWG.Done(); d.periodicLoop(prodCtx) }()

	// Pump: dequeue → a single reconcile Run → record → next.
	pumpDone := d.queue.Pump(func(t Trigger) { d.runTrigger(runCtx, t) })

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-srvErr:
		// Fatal server error (only reachable for a broken listener;
		// shutdown itself is clean). Drain the rest, then report.
	}

	d.shutdown(srv, prodCancel, &prodWG, runCancel, pumpDone)
	return runErr
}

// shutdown is the ordered drain (asserted in shutdown_test):
//
//  1. readiness OFF first — /healthz fails immediately under load
//     balancers while removal still works,
//  2. HTTP drain (bounded by HTTPDrainTimeout),
//  3. producers stop,
//  4. queue closed: no NEW run scheduled; pending triggers discarded,
//  5. ACTIVE run drained to a terminal state bounded by DrainGrace,
//     then cancelled; the reconciler's record + lease release follow
//     (release uses context.WithoutCancel), then full exit.
func (d *Daemon) shutdown(srv *httpServer, prodCancel context.CancelFunc, prodWG *sync.WaitGroup, runCancel context.CancelFunc, pumpDone chan struct{}) {
	d.ready.set(false)
	if srv != nil {
		srv.close(context.Background(), HTTPDrainTimeout)
	}
	prodCancel()
	prodWG.Wait()
	d.queue.Close()
	grace := time.NewTimer(d.cfg.DrainGrace)
	defer grace.Stop()
	select {
	case <-pumpDone:
	case <-grace.C:
		d.log.Warn("controller: drain grace expired; cancelling the active reconcile",
			"grace", d.cfg.DrainGrace)
		runCancel()
		<-pumpDone
	}
	d.log.Info("controller: drained and stopped")
}

// Serve is the production entrypoint for Wave C (`topctl controller
// run`): signal-aware wrapper around Run. Build Options via
// OptionsFromConfig, fill Reconciler/Store/Source/Secret (and Log), call
// Serve; it returns nil on SIGINT/SIGTERM (or the fatal server error).
func Serve(o Options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d, err := New(o)
	if err != nil {
		return err
	}
	return d.Run(ctx)
}
