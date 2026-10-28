// SPDX-License-Identifier: MIT

package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// --- fakes -----------------------------------------------------------------

type fakeSecret struct{ val string }

func (f fakeSecret) Resolve(context.Context, string) (string, string, error) {
	return f.val, "v1", nil
}

type fakeSecretErr struct{}

func (fakeSecretErr) Resolve(context.Context, string) (string, string, error) {
	return "", "", errors.New("secret: not found")
}

type countingDedup struct {
	mu    sync.Mutex
	seen  map[string]int
	at    time.Time
	fail  bool
	calls int
}

func newCountingDedup() *countingDedup { return &countingDedup{seen: map[string]int{}, at: time.Now()} }

func (c *countingDedup) RecordDelivery(_ context.Context, id string, at time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.at = at
	if c.fail {
		return false, errors.New("store: disk error")
	}
	c.seen[id]++
	return c.seen[id] == 1, nil
}

type recordingQueue struct {
	mu     sync.Mutex
	events []Event
	fail   bool
}

func (q *recordingQueue) Enqueue(_ context.Context, ev Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.fail {
		return errors.New("queue: full")
	}
	q.events = append(q.events, ev)
	return nil
}

type fakeMetrics struct {
	mu      sync.Mutex
	tallied [][2]string
}

func (f *fakeMetrics) RecordWebhookDelivery(event, result string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tallied = append(f.tallied, [2]string{event, result})
}

func (f *fakeMetrics) tally() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]string(nil), f.tallied...)
}

// --- helpers ----------------------------------------------------------------

const testSecret = "top-test-hmac-secret"

func sign(t *testing.T, key, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

const pushBody = `{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"example/x"}}`

type harness struct {
	handler http.Handler
	dedup   *countingDedup
	queue   *recordingQueue
	metrics *fakeMetrics
}

func newHarness(t *testing.T, mutate func(*Options)) *harness { //nolint:unparam
	t.Helper()
	h := &harness{dedup: newCountingDedup(), queue: &recordingQueue{}, metrics: &fakeMetrics{}}
	o := Options{
		Secret:   fakeSecret{testSecret},
		Dedup:    h.dedup,
		Queue:    h.queue,
		Recorder: h.metrics,
		Log:      slog.New(slog.DiscardHandler),
	}
	if mutate != nil {
		mutate(&o)
	}
	h.handler = NewHandler(o)
	return h
}

func do(h *harness, method string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/webhooks/github", bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func signedHeaders(t *testing.T, body []byte, extras map[string]string) map[string]string {
	t.Helper()
	hdrs := map[string]string{
		headerSignature: sign(t, []byte(testSecret), body),
		headerEvent:     "push",
		headerDelivery:  "delivery-1",
	}
	for k, v := range extras {
		hdrs[k] = v
	}
	return hdrs
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) respBody {
	t.Helper()
	var got respBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response body %q is not the JSON envelope: %v", rec.Body.String(), err)
	}
	return got
}

// --- table: intake outcomes ---------------------------------------------------

func TestServeHTTPOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		body       string
		headers    func(t *testing.T) map[string]string
		wantStatus int
		wantBody   respBody
		wantQueue  int // enqueued deliveries
		wantDedup  int // dedup store calls
		wantMetric [2]string
	}{
		{
			name:       "valid push enqueued",
			method:     http.MethodPost,
			body:       pushBody,
			headers:    func(t *testing.T) map[string]string { return signedHeaders(t, []byte(pushBody), nil) },
			wantStatus: http.StatusAccepted,
			wantBody:   respBody{Accepted: true, Deduped: false},
			wantQueue:  1, wantDedup: 1,
			wantMetric: [2]string{"push", "accepted"},
		},
		{
			name:   "forged signature: 401, no store write, no enqueue",
			method: http.MethodPost,
			body:   pushBody,
			headers: func(t *testing.T) map[string]string {
				h := signedHeaders(t, []byte(pushBody), nil)
				h[headerSignature] = "sha256=" + hex.EncodeToString(make([]byte, sha256.Size))
				return h
			},
			wantStatus: http.StatusUnauthorized,
			wantBody:   respBody{Accepted: false, Deduped: false},
			wantQueue:  0, wantDedup: 0,
			wantMetric: [2]string{"push", "rejected"},
		},
		{
			name:   "missing signature: 401",
			method: http.MethodPost,
			body:   pushBody,
			headers: func(t *testing.T) map[string]string {
				return signedHeaders(t, []byte(pushBody), map[string]string{headerSignature: ""})
			},
			wantStatus: http.StatusUnauthorized,
			wantQueue:  0, wantDedup: 0,
			wantMetric: [2]string{"push", "rejected"},
		},
		{
			name:   "wrong scheme: 401 (no fallback)",
			method: http.MethodPost,
			body:   pushBody,
			headers: func(t *testing.T) map[string]string {
				return signedHeaders(t, []byte(pushBody), map[string]string{headerSignature: "sha1=abcdef"})
			},
			wantStatus: http.StatusUnauthorized,
			wantQueue:  0, wantDedup: 0,
			wantMetric: [2]string{"push", "rejected"},
		},
		{
			name:   "unknown event: 202 accepted-noop, dedup recorded, no enqueue",
			method: http.MethodPost,
			body:   pushBody,
			headers: func(t *testing.T) map[string]string {
				return signedHeaders(t, []byte(pushBody), map[string]string{headerEvent: "ping"})
			},
			wantStatus: http.StatusAccepted,
			wantBody:   respBody{Accepted: true, Deduped: false},
			wantQueue:  0, wantDedup: 1,
			wantMetric: [2]string{"other", "ignored"},
		},
		{
			name:       "body too large: 413, no store write",
			method:     http.MethodPost,
			body:       pushBody,
			headers:    func(t *testing.T) map[string]string { return signedHeaders(t, []byte(pushBody), nil) },
			wantStatus: http.StatusRequestEntityTooLarge,
			wantQueue:  0, wantDedup: 0,
			wantMetric: [2]string{"push", "rejected"},
		},
		{
			name:       "malformed payload after HMAC pass: 400, no dedup, no enqueue",
			method:     http.MethodPost,
			body:       "this is not json",
			headers:    func(t *testing.T) map[string]string { return signedHeaders(t, []byte("this is not json"), nil) },
			wantStatus: http.StatusBadRequest,
			wantQueue:  0, wantDedup: 0,
			wantMetric: [2]string{"push", "rejected"},
		},
		{
			name:   "missing delivery id: 400, no store write",
			method: http.MethodPost,
			body:   pushBody,
			headers: func(t *testing.T) map[string]string {
				return signedHeaders(t, []byte(pushBody), map[string]string{headerDelivery: ""})
			},
			wantStatus: http.StatusBadRequest,
			wantQueue:  0, wantDedup: 0,
			wantMetric: [2]string{"push", "rejected"},
		},
		{
			name:       "GET: 405",
			method:     http.MethodGet,
			headers:    func(t *testing.T) map[string]string { return map[string]string{} },
			wantStatus: http.StatusMethodNotAllowed,
			wantQueue:  0, wantDedup: 0,
			wantMetric: [2]string{"invalid", "rejected"},
		},
		{
			name:       "secret unavailable: 500, no store write",
			method:     http.MethodPost,
			body:       pushBody,
			headers:    func(t *testing.T) map[string]string { return signedHeaders(t, []byte(pushBody), nil) },
			wantStatus: http.StatusInternalServerError,
			wantQueue:  0, wantDedup: 0,
			wantMetric: [2]string{"invalid", "error"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var h *harness
			switch tt.name {
			case "body too large: 413, no store write":
				h = newHarness(t, func(o *Options) { o.MaxBodyBytes = 8 })
			case "secret unavailable: 500, no store write":
				h = newHarness(t, func(o *Options) { o.Secret = fakeSecretErr{} })
			default:
				h = newHarness(t, nil)
			}
			rec := do(h, tt.method, []byte(tt.body), tt.headers(t))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), tt.wantStatus)
			}
			if tt.wantStatus >= 200 && tt.wantStatus < 500 || tt.wantStatus == http.StatusMethodNotAllowed {
				if got := decodeBody(t, rec); got != (respBody{Accepted: tt.wantBody.Accepted, Deduped: tt.wantBody.Deduped}) {
					t.Errorf("body shape = %+v, want %+v", got, tt.wantBody)
				}
			}
			if got := len(h.queue.events); got != tt.wantQueue {
				t.Errorf("enqueued = %d, want %d", got, tt.wantQueue)
			}
			if got := h.dedup.calls; got != tt.wantDedup {
				t.Errorf("dedup calls = %d, want %d", got, tt.wantDedup)
			}
			if tt.wantMetric != [2]string{} {
				got := h.metrics.tally()
				if len(got) != 1 || got[0] != tt.wantMetric {
					t.Errorf("metrics = %v, want [%v]", got, tt.wantMetric)
				}
			}
		})
	}
}

// --- dedup: duplicate delivery → 200, single run, real store path ----------

func TestDuplicateDelivery(t *testing.T) {
	h := newHarness(t, nil)
	headers := signedHeaders(t, []byte(pushBody), nil)

	first := do(h, http.MethodPost, []byte(pushBody), headers)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", first.Code)
	}
	if got := decodeBody(t, first); got != (respBody{true, false}) {
		t.Errorf("first body = %+v, want {true,false}", got)
	}

	second := do(h, http.MethodPost, []byte(pushBody), headers)
	if second.Code != http.StatusOK { // ADR-0003: duplicate → 200, not a second 202 enqueue
		t.Fatalf("duplicate status = %d, want 200", second.Code)
	}
	if got := decodeBody(t, second); got != (respBody{false, true}) {
		t.Errorf("duplicate body = %+v, want {false,true}", got)
	}
	if got := len(h.queue.events); got != 1 {
		t.Errorf("enqueue count = %d, want exactly 1 across duplicate", got)
	}
}

// --- enqueue seam error → 503, delivery stays first-seen in store ----------

func TestEnqueueFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.queue.fail = true
	rec := do(h, http.MethodPost, []byte(pushBody), signedHeaders(t, []byte(pushBody), nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := h.metrics.tally(); len(got) != 1 || got[0] != [2]string{"push", "error"} {
		t.Errorf("metrics = %v, want push/error", got)
	}
}

// --- dedup store failure → 500 ------------------------------------------------

func TestDedupStoreFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.dedup.fail = true
	rec := do(h, http.MethodPost, []byte(pushBody), signedHeaders(t, []byte(pushBody), nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// --- store-backed dedup against the REAL internal/store/sqlite --------

func TestDedupAgainstRealStore(t *testing.T) {
	sqlstore := newTestStore(t)
	q := &recordingQueue{}
	h := NewHandler(Options{
		Secret: fakeSecret{testSecret},
		Dedup:  sqlstore,
		Queue:  q,
		Log:    slog.New(slog.DiscardHandler),
	})
	body := []byte(pushBody)
	hdr := signedHeaders(t, body, nil)

	for i, want := range []struct {
		status  int
		enqueue int
	}{
		{http.StatusAccepted, 1},
		{http.StatusOK, 1}, // duplicate: still one enqueue total
	} {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want.status {
			t.Fatalf("round %d: status = %d, want %d", i, rec.Code, want.status)
		}
		if got := len(q.events); got != want.enqueue {
			t.Errorf("round %d: enqueue count = %d, want %d", i, got, want.enqueue)
		}
	}
}

// --- stock config: missing required deps is a wiring panic ---------------

func TestNilOptionsPanic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewHandler with nil Secret/Dedup/Queue must panic (wiring bug, not runtime condition)")
		}
	}()
	NewHandler(Options{}) //nolint:staticcheck // deliberate
}

// --- constant-time signature comparison evidence ---------------------------

// hmac.Equal is used (no bytes.Compare early exit); this test pins the
// acceptance property itself: exact-match only, any single-bit difference
// in the presented signature must fail.
func TestSignatureComparisonStrictness(t *testing.T) {
	body := []byte(pushBody)
	correct := sign(t, []byte(testSecret), body)
	if !verifySignature([]byte(testSecret), body, correct) {
		t.Fatal("correct signature must verify")
	}
	raw, _ := hex.DecodeString(correct[len(sigPrefix):])
	for i := range raw {
		flipped := append([]byte(nil), raw...)
		flipped[i] ^= 0x01
		forged := sigPrefix + hex.EncodeToString(flipped)
		if verifySignature([]byte(testSecret), body, forged) {
			t.Fatalf("flipped byte %d must not verify (early-exit / non-constant-time suspicion)", i)
		}
	}
	// Wrong key: same length, must still fail.
	if verifySignature([]byte("another-secret-entirely"), body, correct) {
		t.Fatal("signature under wrong key must not verify")
	}
	// Wrong body: must fail.
	if verifySignature([]byte(testSecret), []byte(`{"ref":"refs/heads/main"}`), correct) {
		t.Fatal("signature over different body must not verify")
	}
}

// --- Event surface: payload never carries executable content ----------------

func TestEventCarriesOnlyAuditFields(t *testing.T) {
	h := newHarness(t, nil)
	do(h, http.MethodPost, []byte(pushBody), signedHeaders(t, []byte(pushBody), nil))
	if len(h.queue.events) != 1 {
		t.Fatalf("enqueue count = %d, want 1", len(h.queue.events))
	}
	ev := h.queue.events[0]
	if ev.DeliveryID != "delivery-1" || ev.Event != "push" {
		t.Errorf("event = %+v, want delivery-1/push", ev)
	}
	if ev.Ref != "refs/heads/main" {
		t.Errorf("Ref = %q, want the parsed audit ref", ev.Ref)
	}
	// Structurally: Event has no body/sha fields at all — enforced by the
	// type shape in webhook.go; here we pin the audit contract.
	if fmt.Sprintf("%+v", ev) != `{DeliveryID:delivery-1 Event:push Ref:refs/heads/main}` {
		t.Errorf("Event grew fields: %+v — payload content must never join the Event", ev)
	}
}
