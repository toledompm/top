// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/obs"
	"github.com/toledompm/top/internal/webhook"
)

const testSecret = "controller-test-hmac"

// signWebhook produces a valid GitHub-style HMAC header for body.
func signWebhook(t *testing.T, body string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// staticSecret satisfies webhook.SecretProvider (resolution happens per
// request; rotation needs no restart — the handler owns that behavior).
type staticSecret struct{ v string }

func (s staticSecret) Resolve(context.Context, string) (string, string, error) {
	return s.v, "test", nil
}

// newIntake builds a daemon whose intake is exercised through the REAL
// webhook handler via httptest (the handler→queue adapter is the seam the
// real server mounts).
func newIntakeServer(t *testing.T, src *fakeSource, st *fakeStore, m *obs.Metrics) (*Daemon, *httptest.Server) {
	t.Helper()
	be := newFakeBackend()
	d := newTestDaemon(t, src, st, be, Options{Metrics: m})
	d.cfg.Secret = staticSecret{testSecret}
	handler := webhook.NewHandler(webhook.Options{
		Secret:   d.cfg.Secret,
		Dedup:    st,
		Queue:    webhookEnqueuer{d: d},
		Recorder: m,
	})
	return d, httptest.NewServer(handler)
}

// postDelivery posts one signed push delivery.
func postDelivery(t *testing.T, ts *httptest.Server, deliveryID, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+webhookPath, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-Hub-Signature-256", signWebhook(t, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

const webhookPath = "/hooks/intake" // any path: the handler is path-agnostic (README)

// TestWebhookIntakeEnqueuesCoalescedTrigger is the webhook→enqueue
// integration: real handler + our queue adapter through httptest.
//   - first delivery: 202, one pending trigger.
//   - duplicate delivery id: handler dedups (200, NO second enqueue).
//   - a second, distinct delivery id: dedup passes, the queue coalesces
//     (same (webhook, main) identity); pending stays 1.
func TestWebhookIntakeEnqueuesCoalescedTrigger(t *testing.T) {
	src := &fakeSource{shas: []string{testSHA1}}
	st := newFakeStore()
	m, err := obs.NewMetrics("test", "nocommit")
	if err != nil {
		t.Fatal(err)
	}
	d, ts := newIntakeServer(t, src, st, m)
	defer ts.Close()

	body := `{"ref":"refs/heads/main","after":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	if got, want := postDelivery(t, ts, "d1", body), http.StatusAccepted; got != want {
		t.Fatalf("first delivery status got %d, want %d", got, want)
	}
	if got, want := d.queue.PendingCount(), 1; got != want {
		t.Fatalf("pending got %d, want 1", got)
	}

	// Same delivery id again: handler-level dedup — 200, no change.
	if got, want := postDelivery(t, ts, "d1", body), http.StatusOK; got != want {
		t.Fatalf("duplicate status got %d, want %d", got, want)
	}
	if got, want := d.queue.PendingCount(), 1; got != want {
		t.Fatalf("pending after duplicate got %d, want 1", got)
	}

	// Distinct delivery id (a real re-push): second-layer coalescing.
	if got, want := postDelivery(t, ts, "d2", body), http.StatusAccepted; got != want {
		t.Fatalf("second delivery status got %d, want %d", got, want)
	}
	if got, want := d.queue.PendingCount(), 1; got != want {
		t.Fatalf("pending after second distinct delivery got %d, want 1", got)
	}
	if got, want := d.queue.AbsorbedCount(), 1; got != want {
		t.Fatalf("absorbed got %d, want 1", got)
	}

	// Correlation id survives on the pending trigger (delivery d1 wins).
	got, ok := d.queue.take()
	if !ok || got.CorrelationID != "d1" {
		t.Fatalf("trigger got %+v, want correlation d1", got)
	}

	// ADR-0010 increments: both accepted deliveries are counted with the
	// bounded event label.
	if got := countMetric(t, m, "top_webhook_deliveries_total", "result", webhook.ResultAccepted); got != 2 {
		t.Fatalf("webhook_deliveries{accepted} got %v, want 2", got)
	}
}

// TestHealthzDrainSafe checks the readiness contract over the REAL mux:
// 200 while serving, 503 immediately when the daemon starts draining —
// before the intake socket itself goes away.
func TestHealthzDrainSafe(t *testing.T) {
	src := &fakeSource{shas: []string{testSHA1}}
	st := newFakeStore()
	be := newFakeBackend()
	d := newTestDaemon(t, src, st, be, Options{
		Addr:              "127.0.0.1:0",
		PollInterval:      time.Hour,
		ReconcileInterval: time.Hour,
	})
	// New pre-arms readiness when an Addr is configured (Run binds and
	// shutdown flips it off — drain-safe before the intake disappears).

	d.cfg.Secret = staticSecret{testSecret} // newMux mounts the real intake

	h := d.newMux()
	get := func() int {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if got := get(); got != http.StatusOK {
		t.Fatalf("ready healthz got %d, want %d", got, http.StatusOK)
	}
	d.ready.set(false)
	if d.Ready() {
		t.Fatal("after drain-flip, readiness should be false")
	}
	if got := get(); got != http.StatusServiceUnavailable {
		t.Fatalf("draining healthz got %d, want %d", got, http.StatusServiceUnavailable)
	}
}
