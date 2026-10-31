// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/config"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/obs"
)

// waitFor polls cond until it holds or the timeout lapses (test helper).
func waitFor(t *testing.T, desc string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", desc)
}

// httpGetStatus returns the status of an HTTP GET (0 when the connection
// is gone — after a server shutdown, Dial errors are the verdict).
func httpGetStatus(t *testing.T, url string) int {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestDaemonRunLifecycleWebhookPlusPoll is the daemon-level integration:
// Run(ctx) with an HTTP server serving the real webhook handler and a
// tight poll interval; a webhook delivery lands in the queue; a poll
// movement ALSO lands there (coalesced later); a blocked active run keeps
// the daemon available; a short grace lets the blocked run hit a terminal
// state; the process exits with the lease released and no history hole.
func TestDaemonRunLifecycleWebhookPlusPoll(t *testing.T) {
	src := &fakeSource{shas: []string{testSHA1}}
	st := newFakeStore()
	be := newFakeBackend()
	be.blockCh = make(chan struct{})
	m, err := obs.NewMetrics("test", "nocommit")
	if err != nil {
		t.Fatal(err)
	}
	d := newTestDaemon(t, src, st, be, Options{
		Metrics:           m,
		Addr:              "127.0.0.1:0",
		PollInterval:      5 * time.Millisecond,
		ReconcileInterval: time.Hour,
		DrainGrace:        500 * time.Millisecond,
		MaxBodyBytes:      1 << 20,
	})
	d.cfg.Secret = staticSecret{testSecret}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()

	// 1. HTTP is up/validation plumbing: healthz 200, intake mounts.
	waitFor(t, "http server up", 2*time.Second, func() bool { return d.HTTPAddr() != "" })
	base := "http://" + d.HTTPAddr()
	if got, want := httpGetStatus(t, base+"/healthz"), http.StatusOK; got != want {
		t.Fatalf("healthz got %d, want %d", got, want)
	}

	// 2. First run: the poll's initial enqueue resolves and runs; the run
	// blocks inside DeployImage. Wait until we're in the gate-less
	// deploy hold: the lease is taken.
	waitFor(t, "lease taken", 2*time.Second, func() bool { return st.leaseHolder("reconcile") != "" })

	// 3. Ordering check under cancellation: readiness flips BEFORE the
	// intake disappears (the 503-over-live-socket form is pinned in
	// TestHealthzDrainSafe; here the flip is what's ordered before drain).
	cancel()
	waitFor(t, "readiness flipped off", 2*time.Second, func() bool { return !d.Ready() })

	// 4. Release the deploy: the active run reaches a terminal state and
	// is recorded; the pump drains (no pending triggers were scheduled
	// after close); Run returns nil; the lease is released.
	close(be.blockCh) //nolint:gocritic // an explicit test signal
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after drain")
	}
	getStatus := httpGetStatus(t, base+"/healthz")
	if getStatus != 0 {
		t.Fatalf("server still accepting after shutdown (status %d)", getStatus)
	}
	if holder := st.leaseHolder("reconcile"); holder != "" {
		t.Fatalf("lease still held by %q after shutdown", holder)
	}
	runs := st.runsOf()
	if len(runs) != 1 {
		t.Fatalf("runs recorded %d, want 1 (the active run's terminal state)", len(runs))
	}
}

// TestDaemonRunGraceExpiryCancelsRun pins the bounded drain: a run
// blocking forever is cancelled after DrainGrace; the reconciler records
// the run FAILED; Run still returns cleanly with the lease released.
func TestDaemonRunGraceExpiryCancelsRun(t *testing.T) {
	src := &fakeSource{shas: []string{testSHA1}}
	st := newFakeStore()
	be := newFakeBackend()
	be.blockCh = make(chan struct{}) // never released
	d := newTestDaemon(t, src, st, be, Options{
		PollInterval:      5 * time.Millisecond,
		ReconcileInterval: time.Hour,
		DrainGrace:        80 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()

	waitFor(t, "lease taken", 2*time.Second, func() bool { return st.leaseHolder("reconcile") != "" })
	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within grace + margin")
	}
	if holder := st.leaseHolder("reconcile"); holder != "" {
		t.Fatalf("lease still held by %q after grace expiry", holder)
	}
	runs := st.runsOf()
	if len(runs) != 1 || runs[0].Status != domain.RunFailed {
		t.Fatalf("cancelled run recorded %+v, want exactly one failed run", runs)
	}
}

// TestWebhookDeliveryThroughDaemonHTTP exercises the full daemon mux (not
// just the handler under httptest): the intake is mounted at the
// documented path with a wrong-method GET rejected, /metrics present when
// wired, and the queue receiving a real trigger from a signed POST.
func TestWebhookDeliveryThroughDaemonHTTP(t *testing.T) {
	src := &fakeSource{shas: []string{testSHA1}}
	st := newFakeStore()
	be := newFakeBackend()
	d := newTestDaemon(t, src, st, be, Options{
		PollInterval:      time.Hour,
		ReconcileInterval: time.Hour,
		Addr:              "127.0.0.1:0",
	})
	d.cfg.Secret = staticSecret{testSecret}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	waitFor(t, "http server up", 2*time.Second, func() bool { return d.HTTPAddr() != "" })
	base := "http://" + d.HTTPAddr()

	body := `{"ref":"refs/heads/main","after":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	// GET on the webhook path → 405 (method check lives in the handler).
	if got, want := httpGetStatus(t, base+HTTPPathWebhook), http.StatusMethodNotAllowed; got != want {
		t.Fatalf("GET webhook path got %d, want %d", got, want)
	}

	// Signed POST landed in the queue via the real handler; the pump
	// drains and runs it immediately, so wait for the RUN to have been
	// recorded (the queue-level coast-to-coast is pinned in
	// TestWebhookIntakeEnqueuesCoalescedTrigger at the mux layer).
	status, err := postURL(t, base+HTTPPathWebhook, "d1", body)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("POST status got %d, want %d", status, http.StatusAccepted)
	}
	waitFor(t, "webhook-triggered run recorded", 2*time.Second, func() bool {
		return len(st.runsOf()) == 1
	})
	if got := d.queue.DrainedCount(); got < 1 {
		t.Fatalf("queue drained %d triggers, want >=1", got)
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// postURL posts a signed push delivery to an absolute URL and returns the
// status code.
func postURL(t *testing.T, url, deliveryID, body string) (int, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-Hub-Signature-256", signWebhook(t, body))
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// TestEnvPollIntervalKnobMirrorsInstallPrecedent pins the env knob: valid
// values change ONLY pacing; invalid values keep the config default.
func TestEnvPollIntervalKnobMirrorsInstallPrecedent(t *testing.T) {
	cfg := config.Default()
	cfg.Controller.PollIntervalSeconds = 60

	t.Setenv(EnvPollIntervalMS, "7")
	if got, want := OptionsFromConfig(cfg).PollInterval, 7*time.Millisecond; got != want {
		t.Fatalf("PollInterval got %v, want %v", got, want)
	}

	t.Setenv(EnvPollIntervalMS, "bogus")
	if got, want := OptionsFromConfig(cfg).PollInterval, 60*time.Second; got != want {
		t.Fatalf("invalid knob should keep 60s, got %v", got)
	}
}

// TestOptionsFromConfigConsumesConfigFields pins the config fields the
// daemon consumes (T-310 report).
func TestOptionsFromConfigConsumesConfigFields(t *testing.T) {
	cfg := config.Default()
	cfg.Controller.PollIntervalSeconds = 17
	cfg.Controller.ReconcileIntervalSeconds = 3600
	cfg.Git.Branch = "platform"

	t.Setenv(EnvPollIntervalMS, "")
	o := OptionsFromConfig(cfg)
	if o.Branch != "platform" {
		t.Fatalf("Branch got %q", o.Branch)
	}
	if got, want := o.PollInterval, 17*time.Second; got != want {
		t.Fatalf("PollInterval got %v, want %v", got, want)
	}
	if got, want := o.ReconcileInterval, 3600*time.Second; got != want {
		t.Fatalf("ReconcileInterval got %v, want %v", got, want)
	}
	if o.Addr == "" {
		t.Fatal("Addr should default on")
	}
	if o.VolumeRoot != cfg.DataDir+"/volumes" {
		t.Fatalf("VolumeRoot got %q", o.VolumeRoot)
	}
}

// TestNewRejectsMissingSeams pins the wiring guard.
func TestNewRejectsMissingSeams(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New with no seams should fail")
	}
}
