// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/toledompm/top/internal/webhook"
)

// ready is the server's readiness bit: true while the daemon serves
// normally; flipped false at the START of shutdown-drain so /healthz
// fails with 503 under a load balancer BEFORE the intake itself goes
// away (drain-safe, per the T-310 brief). Atomic, read per request.
type ready struct{ v atomic.Bool }

// set toggles readiness.
func (r *ready) set(b bool) { r.v.Store(b) }

// is reports current readiness.
func (r *ready) is() bool { return r.v.Load() }

// httpServer wraps net/http with the drain behavior Run needs. The
// listener is bound in newServer (so a taken port fails Run fast, before
// any goroutine starts) and serve consumes it.
type httpServer struct {
	srv *http.Server
	ln  net.Listener
}

// serve runs until Shutdown or a non-shutdown serve error (run's srvErr
// channel distinguishes fatal from clean).
func (s *httpServer) serve() error {
	err := s.srv.Serve(s.ln)
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// close stops accepting, then waits bounded for in-flight requests (a
// mid-flight webhook POST either gets its response or dies with the
// timeout; the provider retries, the poll loop is the backstop).
func (s *httpServer) close(ctx context.Context, timeout time.Duration) {
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := s.srv.Shutdown(dctx); err != nil {
		_ = s.srv.Close() // force-close any stragglers
	}
}

// newMux builds the daemon's route table (kept separate so the drain-safe
// readiness contract is unit-testable without binding a port):
//
//	/webhooks/github — webhook.NewHandler (verified intake → queue)
//	/metrics         — obs.Metrics's handler over the private registry
//	/healthz         — "ok" while serving, 503 "draining" once readiness
//	                   was flipped off at shutdown start.
func (d *Daemon) newMux() http.Handler {
	// Recorder is the narrow slice of *obs.Metrics the handler wants;
	// nil-interface when unwired (assigning a nil *obs.Metrics into the
	// interface would make the handler's non-nil check lie).
	var rec webhook.Recorder
	if d.cfg.Metrics != nil {
		rec = d.cfg.Metrics
	}
	mux := http.NewServeMux()
	mux.Handle(HTTPPathWebhook, webhook.NewHandler(webhook.Options{
		Secret:       d.cfg.Secret,
		SecretName:   webhook.DefaultSecretName,
		Dedup:        d.cfg.Store,
		Queue:        webhookEnqueuer{d: d},
		Recorder:     rec,
		MaxBodyBytes: d.cfg.MaxBodyBytes,
		Log:          d.log,
	}))
	if d.cfg.Metrics != nil {
		mux.Handle("/metrics", d.cfg.Metrics.HTTPHandler())
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !d.ready.is() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("draining\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// newServer builds the server and binds the listener.
func (d *Daemon) newServer() (*httpServer, error) {
	if d.cfg.Secret == nil {
		return nil, fmt.Errorf("controller: server needs Options.Secret (the webhook-hmac SecretProvider, ADR-0008); set Addr=%q to run polls-only without HTTP", d.cfg.Addr)
	}

	mux := d.newMux()

	// Server hardening: header-read deadline first (slowloris), bounded
	// header size, finite read/write budgets. Body size itself is already
	// capped inside webhook.NewHandler (http.MaxBytesReader).
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	ln, err := net.Listen("tcp", d.cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", d.cfg.Addr, err)
	}
	return &httpServer{srv: srv, ln: ln}, nil
}
