// SPDX-License-Identifier: MIT

package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Handler serves POST <mount> webhook deliveries. Mount it on the
// controller server's mux (next wave) with
//
//	mux.Handle("/webhooks/github", webhook.NewHandler(...))
//
// per ADR-0003's endpoint name. The handler is path-agnostic, so a later
// Gitea/GitLab variant can mount additive paths (ADR-0003 provider scope).
type Handler struct {
	secret   SecretProvider
	secretNm string
	dedup    Deduper
	queue    Enqueuer
	obs      Recorder
	events   map[string]bool
	maxBody  int64
	log      *slog.Logger
}

// Options configures the handler. SecretProvider, Deduper and Enqueuer are
// required (nil → NewHandler panics: wiring bugs, not runtime conditions);
// Recorder and Log are optional.
type Options struct {
	// SecretProvider resolves the HMAC secret (ADR-0008). The secret name
	// resolved is SecretName (default webhook-hmac).
	Secret SecretProvider
	// SecretName overrides DefaultSecretName.
	SecretName string
	// Dedup records delivery ids in the StateStore (store.Store).
	Dedup Deduper
	// Queue receives verified, deduped deliveries for reconciliation.
	Queue Enqueuer
	// Obs receives webhook_deliveries_total increments; optional ( nil =
	// no metrics). Use *obs.Metrics; Recorder is the narrow slice this
	// package actually needs.
	Recorder Recorder
	// AllowedEvents is the allowlist of provider event names; default and
	// empty → push only (ADR-0003 v0).
	AllowedEvents []string
	// MaxBodyBytes caps the raw body; zero → DefaultMaxBodyBytes.
	MaxBodyBytes int64
	// Log receives the audit line (delivery id, event, ref — never secret
	// or payload content).
	Log *slog.Logger
}

// Recorder is the slice of obs.Metrics this package uses: one truthful
// increment per processed delivery, event already mapped onto a bounded
// label value.
type Recorder interface {
	RecordWebhookDelivery(event, result string)
}

// NewHandler builds the intake handler.
func NewHandler(o Options) *Handler {
	if o.Secret == nil || o.Dedup == nil || o.Queue == nil {
		panic("webhook.NewHandler: Secret, Dedup and Queue are required")
	}
	name := o.SecretName
	if name == "" {
		name = DefaultSecretName
	}
	events := map[string]bool{"push": true}
	if len(o.AllowedEvents) > 0 {
		events = make(map[string]bool, len(o.AllowedEvents))
		for _, e := range o.AllowedEvents {
			events[e] = true
		}
	}
	max := o.MaxBodyBytes
	if max <= 0 {
		max = DefaultMaxBodyBytes
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		secret: o.Secret, secretNm: name, dedup: o.Dedup, queue: o.Queue,
		obs: o.Recorder, events: events, maxBody: max, log: log,
	}
}

// Provider header names (GitHub convention; ADR-0003 names the signature
// format "sha256=" and the event allowlist header X-GitHub-Event).
const (
	headerSignature = "X-Hub-Signature-256"
	headerEvent     = "X-GitHub-Event"
	headerDelivery  = "X-GitHub-Delivery"
	// sigPrefix is the expected signature scheme tag (ADR-0003).
	sigPrefix = "sha256="
)

// errBody is the OK/no-op response envelope; shapes fixed in the README.
type respBody struct {
	Accepted bool `json:"accepted"`
	Deduped  bool `json:"deduped"`
}

func (h *Handler) record(event, result string) {
	if h.obs != nil {
		h.obs.RecordWebhookDelivery(event, result)
	}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Envelope/dedup responses are decided here; status first, then JSON.
	write := func(status int, accepted, deduped bool) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status != http.StatusNoContent && status != http.StatusNotModified {
			_ = json.NewEncoder(w).Encode(respBody{Accepted: accepted, Deduped: deduped})
		}
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		h.record(resultEventInvalid, ResultRejected)
		write(http.StatusMethodNotAllowed, false, false)
		return
	}

	// Secret resolution FIRST: a config problem (missing webhook-hmac
	// secret, unsafe perms) is a 500, never a silently-unverifiable request.
	secret, _, serr := h.secret.Resolve(r.Context(), h.secretNm)
	if serr != nil || secret == "" {
		// Never log the value or the error detail (may embed paths).
		h.log.Error("webhook: hmac secret unavailable")
		h.record(resultEventInvalid, ResultError)
		write(http.StatusInternalServerError, false, false)
		return
	}

	// Metric event labels use a bounded mapping (ADR-0010) — allowlisted
	// event header names, "other" for non-allowlisted values, "invalid"
	// when absent. Delivery id check follows (it needs this label).
	event := r.Header.Get(headerEvent)
	metricEvent := event
	switch {
	case event == "":
		metricEvent = resultEventInvalid
	case !h.events[event]:
		metricEvent = resultEventOther
	}

	// Delivery id: required BEFORE dedup; without it we cannot record, so
	// refuse (github always sends it; a spoofed client omitting it gets 400
	// without any store write).
	deliveryID := r.Header.Get(headerDelivery)
	if deliveryID == "" || len(deliveryID) > 128 {
		h.record(metricEvent, ResultRejected)
		write(http.StatusBadRequest, false, false)
		return
	}

	// Body cap (ADR-0003 http.MaxBytesReader).
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		h.record(metricEvent, ResultRejected) // 413 or truncated body
		write(http.StatusRequestEntityTooLarge, false, false)
		return
	}

	// Constant-time HMAC-SHA256 over the raw body (ADR-0003: hmac.Equal,
	// no early-exit compare).
	if !verifySignature([]byte(secret), body, r.Header.Get(headerSignature)) {
		h.log.Warn("webhook: signature verification failed", "delivery", deliveryID)
		h.record(metricEvent, ResultRejected)
		write(http.StatusUnauthorized, false, false)
		return
	}

	// Allowlisted events parse the audit ref first — a verified but
	// malformed push is 400 with NO store write (dedup only successful
	// parses). Non-allowlisted events are never parsed.

	var ref string
	if h.events[event] {
		// Parse the payload for the audit ref ONLY. Nothing parsed here is
		// ever executed (git-source contract: payload audit only).
		var perr error
		ref, perr = parsePushRef(body)
		if perr != nil {
			h.log.Warn("webhook: verified delivery with unparseable payload", "delivery", deliveryID)
			h.record(metricEvent, ResultRejected)
			write(http.StatusBadRequest, false, false)
			return
		}
	}

	// Dedup via the StateStore (INSERT OR IGNORE semantics). Duplicate →
	// 200 Accepted-with-deduped and NO second enqueue (ADR-0003 + state
	// store contract line: `return 200, "duplicate"`).
	first, derr := h.dedup.RecordDelivery(r.Context(), deliveryID, time.Now())
	if derr != nil {
		h.log.Error("webhook: dedup store failure", "err", derr)
		h.record(metricEvent, ResultError)
		write(http.StatusInternalServerError, false, false)
		return
	}
	if !first {
		h.record(metricEvent, ResultDuplicate)
		write(http.StatusOK, false, true)
		return
	}

	// Unknown (non-allowlisted) events → 202 no-op (ADR-0003 open-question
	// proposal: audit completeness beats intake strictness). They were
	// deduped above, so a retried unknown event never double-counts.
	if !h.events[event] {
		h.log.Info("webhook: event not allowlisted, ignored", "delivery", deliveryID, "event", event)
		h.record(metricEvent, ResultIgnored)
		write(http.StatusAccepted, true, false)
		return
	}

	ev := Event{DeliveryID: deliveryID, Event: event, Ref: ref}
	if qerr := h.queue.Enqueue(r.Context(), ev); qerr != nil {
		h.log.Error("webhook: enqueue rejected", "err", qerr)
		h.record(metricEvent, ResultError)
		write(http.StatusServiceUnavailable, false, false)
		return
	}

	h.log.Info("webhook: delivery accepted", "delivery", deliveryID, "event", event, "ref", ref)
	h.record(metricEvent, ResultAccepted)
	write(http.StatusAccepted, true, false)
}

// verifySignature computes HMAC-SHA256(secret, body), hex-encodes it, and
// compares against the given "sha256=<hex>" header in constant time via
// hmac.Equal. Any malformed header (missing, wrong scheme, bad hex) fails.
func verifySignature(secret []byte, body []byte, header string) bool {
	if header == "" {
		return false
	}
	trimmed, ok := strings.CutPrefix(header, sigPrefix)
	if !ok {
		return false // wrong scheme; never fall back to other variants
	}
	given, err := hex.DecodeString(trimmed)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), given)
}

// parsePushRef extracts the single audit field we allow ourselves: the git
// ref of a push payload ("ref": "refs/heads/main"). It fails on anything
// it does not recognize, and its output never reaches exec (the
// reconciler re-derives refs from the configured remote).
func parsePushRef(body []byte) (string, error) {
	var payload struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("webhook: payload: %w", err)
	}
	if payload.Ref == "" {
		return "", errors.New("webhook: payload: empty ref")
	}
	return payload.Ref, nil
}
