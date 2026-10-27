package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func capture(t *testing.T) (*Log, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	l := newLogForTest("info", buf)
	return l, buf
}

func decodeLast(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("no log records emitted; buffer empty")
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); err != nil {
		t.Fatalf("last record is not valid JSON: %v\n%s", err, buf.String())
	}
	return rec
}

func TestLogRecordShape(t *testing.T) {
	l, buf := capture(t)
	l.Logger().Info("hello", "k", "v")

	rec := decodeLast(t, buf)
	// Base fields the JSON handler always provides.
	for _, key := range []string{"time", "level", "msg"} {
		if _, ok := rec[key]; !ok {
			t.Errorf("missing default field %q; got %v", key, rec)
		}
	}
	if rec["level"] != "INFO" || rec["msg"] != "hello" {
		t.Errorf("unexpected level/msg: %v", rec)
	}
	if rec["k"] != "v" {
		t.Errorf("attr lost: %v", rec)
	}
}

// Correspondence test: a logger from Log.Ctx always emits the "op" group;
// the uncorrelated base logger never does.
func TestLogCorrelation(t *testing.T) {
	l, buf := capture(t)

	uncorrelated := l.Logger().With("phase", 1)
	uncorrelated.Info("uncorrelated")
	rec := decodeLast(t, buf)
	if _, ok := rec["op"]; ok {
		t.Errorf("base logger must not carry an op group: %v", rec)
	}

	ctx := l.Ctx(context.Background(), OpInfo{
		ID:      "op_01JTEST0000000000000000AB",
		Service: "core/top-controller",
		Actor:   "webhook",
	})
	if _, ok := OpFromContext(ctx); !ok {
		t.Fatal("op id lost from context")
	}
	AppCtxLogger(ctx).Warn("correlated") // delivery left empty → absent

	rec = decodeLast(t, buf)
	if rec["level"] != "WARN" {
		t.Errorf("level passthrough broken: %v", rec)
	}
	op, ok := rec["op"].(map[string]any)
	if !ok {
		t.Fatalf("no op group: %v", rec)
	}
	if op["id"] != "op_01JTEST0000000000000000AB" {
		t.Errorf("op.id: %v", op["id"])
	}
	if op["service"] != "core/top-controller" {
		t.Errorf("op.service: %v", op["service"])
	}
	if op["actor"] != "webhook" {
		t.Errorf("op.actor: %v", op["actor"])
	}
	if _, ok := op["delivery"]; ok {
		t.Errorf("empty op field must be skipped: %v", op)
	}

	// Extracting the identity directly mirrors what was logged.
	info, ok := OpFromContext(ctx)
	if !ok || info.Service != "core/top-controller" || info.DeliveryID != "" {
		t.Errorf("OpFromContext roundtrip: %+v ok=%v", info, ok)
	}
}

func TestAppCtxLoggerFallback(t *testing.T) {
	// Uncorrelated context falls back to slog.Default(); must not panic or nil.
	if got := AppCtxLogger(context.Background()); got == nil {
		t.Fatal("AppCtxLogger returned nil")
	}
	if got := AppCtxLogger(context.Background()); got != slog.Default() {
		t.Error("expected slog.Default() fallback")
	}
}

func TestLevelToggle(t *testing.T) {
	l, buf := capture(t)
	l.Logger().Debug("hidden")
	if buf.Len() != 0 {
		t.Errorf("debug must be filtered at info: %s", buf.String())
	}

	if err := l.SetLevel("DEBUG"); err != nil {
		t.Fatalf("toggle: %v", err)
	}
	l.Logger().Debug("visible")
	if !strings.Contains(buf.String(), `"visible"`) {
		t.Errorf("debug record missing after toggle: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"DEBUG"`) {
		t.Errorf("level not reported as DEBUG: %s", buf.String())
	}

	if err := l.SetLevel(""); err != nil {
		t.Errorf("empty level should reset to info: %v", err)
	}
	l.Logger().Debug("hidden again")
	if strings.Contains(buf.String(), `"hidden again"`) {
		t.Errorf("debug must be filtered after reset: %s", buf.String())
	}
}

func TestNewRejectsUnknownLevel(t *testing.T) {
	if _, err := New("loud"); err == nil {
		t.Fatal("expected error for unknown level")
	}
	if err := newLogForTest("info", &bytes.Buffer{}).SetLevel("loud"); err == nil {
		t.Error("SetLevel should reject unknown level")
	}
}

func TestParseLevelCaseInsensitiveTrimmed(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"":      slog.LevelInfo,
		"  ":    slog.LevelInfo,
		"Debug": slog.LevelDebug,
		"WARN":  slog.LevelWarn,
		"error": slog.LevelError,
	} {
		got, err := parseLevel(in)
		if err != nil || got != want {
			t.Errorf("parseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}
