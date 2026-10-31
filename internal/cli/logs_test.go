// SPDX-License-Identifier: MIT

package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/toledompm/top/internal/exec/exectest"
)

// T-308: the `topctl logs` command body is real. CLI-level wiring tests —
// envelope shape, exit mapping, and the readiness compose — all against
// exectest's FakeRunner and fictional fixtures (never the host; no
// t.Setenv, so the suite stays parallel-safe). Fine-grained argv/cap
// behavior lives in internal/backend/dokku's tests.

// seedLogsExists teaches the fake that the app exists and answers `logs`.
func seedLogsExists(f *exectest.FakeRunner, logOut string) {
	f.SetPrefix([]string{"dokku", "apps:exists"}, exectest.Response{Stdout: []byte("App exists")})
	f.SetPrefix([]string{"dokku", "logs"}, exectest.Response{Stdout: []byte(logOut)})
}

// seedReadinessNotReady seeds the inspect reports behind Readiness with a
// NOT-ready answer (not running + failed build), plus the failed-deploy
// log the compose then excerpts (T-308's broken-image scenario).
func seedReadinessNotReady(f *exectest.FakeRunner) {
	f.SetPrefix([]string{"dokku", "ps:report"}, exectest.Response{Stdout: []byte(`{"deployed":"true","running":"false"}`)})
	f.SetPrefix([]string{"dokku", "builds:report"}, exectest.Response{Stdout: []byte(`{"build-status":"failed"}`)})
	f.SetPrefix([]string{"dokku", "git:report"}, exectest.Response{Stdout: []byte(`{"source-image":"ghcr.io/example/whoami:broken-tag"}`)})
	f.SetPrefix([]string{"dokku", "apps:report"}, exectest.Response{Stdout: []byte(`{"deploy-source":"docker-image"}`)})
	f.SetPrefix([]string{"dokku", "network:report"}, exectest.Response{Stdout: []byte(`{}`)})
	f.SetPrefix([]string{"dokku", "resource:report"}, exectest.Response{Stdout: []byte(`{}`)})
	f.SetPrefix([]string{"dokku", "ports:report"}, exectest.Response{Stdout: []byte(`[]`)})
	f.SetPrefix([]string{"dokku", "storage:list"}, exectest.Response{Stdout: []byte(`[]`)})
	f.SetPrefix([]string{"dokku", "config:keys"}, exectest.Response{Stdout: []byte("")})
	f.SetPrefix([]string{"dokku", "logs:failed"}, exectest.Response{Stdout: []byte("error pulling image\nmanifest unknown\n")})
}

func runLogsJSON(t *testing.T, f *exectest.FakeRunner, args ...string) (Envelope, string, string, int) {
	t.Helper()
	outS, errS, code := runCLIOpts(t, Options{Runner: f}, append([]string{"logs", "-o", "json"}, args...)...)
	var env Envelope
	if code == 0 && json.Unmarshal([]byte(outS), &env) != nil {
		t.Fatalf("logs -o json: not one JSON object: %q", outS)
	}
	return env, outS, errS, code
}

// TestLogsHappyJSON: full retrieve → envelope carries ref/lines/hints +
// a ready readiness composition; exit 0.
func TestLogsHappyJSON(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	seedLogsExists(f, "2026-10-28 one\n2026-10-28 two\n")
	seedReadinessNotReady(f)
	// Override the not-ready reports with a ready composition: running+succeeded.
	f.SetPrefix([]string{"dokku", "ps:report"}, exectest.Response{Stdout: []byte(`{"deployed":"true","running":"true"}`)})
	f.SetPrefix([]string{"dokku", "builds:report"}, exectest.Response{Stdout: []byte(`{"build-status":"succeeded"}`)})
	f.SetPrefix([]string{"dokku", "checks:run"}, exectest.Response{ExitCode: 0})

	env, outS, errS, code := runLogsJSON(t, f, "web")
	if code != ExitOK {
		t.Fatalf("logs exit = %d, want 0 (out %q, err %q)", code, outS, errS)
	}
	if env.Command != "logs" || env.SchemaVersion != JSONSchemaVersion {
		t.Fatalf("envelope header = %+v", env)
	}
	if env.Data["ref"] != "default/web" {
		t.Fatalf("ref = %v", env.Data["ref"])
	}
	lines, _ := env.Data["lines"].([]any)
	if len(lines) != 2 {
		t.Fatalf("lines = %v, want the 2 seeded log lines", env.Data["lines"])
	}
	if isReady := mapReady(env.Data["readiness"]); isReady != "ready" {
		t.Fatalf("readiness = %v, want ready", env.Data["readiness"])
	}
}

// mapReady reduces the envelope's readiness object to ready/not-ready for
// assertions.
func mapReady(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return "missing"
	}
	deployed, _ := m["deployed"].(bool)
	running, _ := m["running"].(bool)
	checks, _ := m["checks"].(bool)
	build, _ := m["build"].(string)
	if deployed && running && checks && build == "succeeded" {
		return "ready"
	}
	return "not-ready"
}

// TestLogsBareName: bare "name" resolves to default/<name> (spec-default
// namespace, mirroring every other ref-taking command).
func TestLogsBareName(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	seedLogsExists(f, "line\n")
	env, _, _, code := runLogsJSON(t, f, "web")
	if code != ExitOK || env.Data["ref"] != "default/web" {
		t.Fatalf("bare-name resolution: (%v, code %d)", env.Data, code)
	}
}

// TestLogsFailedFlag: --failed switches to FailedLogs (logs:failed argv)
// and never tails runtime logs.
func TestLogsFailedFlag(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	seedLogsExists(f, "should NOT appear\n")
	f.SetPrefix([]string{"dokku", "logs:failed"}, exectest.Response{Stdout: []byte("build failed: missing image\n")})

	env, _, _, code := runLogsJSON(t, f, "--failed", "web")
	if code != ExitOK {
		t.Fatalf("logs --failed exit = %d", code)
	}
	if lines, _ := env.Data["lines"].([]any); len(lines) != 1 {
		t.Fatalf("failed log lines = %v, want 1", env.Data["lines"])
	}
	// --failed must never tail runtime logs.
	for _, c := range f.Recorded() {
		if c.String() == "dokku logs default-web" || (c.Name == "dokku" && len(c.Args) > 1 && c.Args[0] == "logs") {
			t.Fatalf("--failed must not tail runtime logs, saw call %v", c)
		}
	}
}

// TestLogsNotReadyComposesHints (T-308 AC): a broken-image scenario — not
// running, build failed — yields hints AND a failed-log excerpt in the
// envelope, while still delivering the requested log (exit 0).
func TestLogsNotReadyComposesHints(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	seedLogsExists(f, "old serving line\n")
	seedReadinessNotReady(f)

	env, _, _, code := runLogsJSON(t, f, "tops/web")
	if code != ExitOK {
		t.Fatalf("exit = %d (a not-ready answer is still a delivered log)", code)
	}
	hints, _ := env.Data["hints"].([]any)
	if len(hints) == 0 {
		t.Fatalf("hints = %v, want the failed-build hint", env.Data["hints"])
	}
	excerpt, _ := env.Data["failedLogs"].([]any)
	if len(excerpt) != 2 {
		t.Fatalf("failedLogs = %v, want the 2 failed-deploy lines", env.Data["failedLogs"])
	}
	if hint0, _ := hints[1].(string); !strings.Contains(hint0, "--failed") {
		t.Fatalf("build-failed hint %q lacks the actionable --failed pointer", hints)
	}
}

// TestLogsNotFound: missing app → error envelope code "not-found", exit 3
// (usage 2 would blame the ask; 1 stays reserved to findings).
func TestLogsNotFound(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	f.SetPrefix([]string{"dokku", "apps:exists"}, exectest.Response{ExitCode: 20, Stderr: []byte("!  App top-web does not exist")})

	_, _, errS, code := runLogsJSON(t, f, "tops/web")
	if code != ExitFailed {
		t.Fatalf("not-found exit = %d, want 3", code)
	}
	var env ErrEnvelope
	if err := json.Unmarshal([]byte(errS), &env); err != nil {
		t.Fatalf("stderr not an error envelope: %q", errS)
	}
	if env.Error.Code != "not-found" {
		t.Fatalf("error code = %q, want not-found", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "tops/web") {
		t.Fatalf("message lacks the ref: %q", env.Error.Message)
	}
}

// TestLogsBackendFailure: dokku answering with an error status → "failed",
// exit 3; the binary being unusable (runner-level error) →
// "backend-unavailable", exit 4.
func TestLogsBackendFailure(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	seedLogsExists(f, "")
	f.SetPrefix([]string{"dokku", "logs"}, exectest.Response{ExitCode: 9, Stderr: []byte("!  boom")})

	_, _, errS, code := runLogsJSON(t, f, "web")
	if code != ExitFailed {
		t.Fatalf("dokku-failure exit = %d, want 3", code)
	}
	var env ErrEnvelope
	if err := json.Unmarshal([]byte(errS), &env); err != nil {
		t.Fatalf("stderr not an error envelope: %q", errS)
	}
	if env.Error.Code != "failed" {
		t.Fatalf("error code = %q, want failed", env.Error.Code)
	}

	f2 := exectest.NewFakeRunner()
	f2.SetDefault(exectest.Response{Err: errDokkuAbsent})
	_, _, errS, code = runLogsJSON(t, f2, "web")
	if code != ExitUnreachable {
		t.Fatalf("dead-backend exit = %d, want 4", code)
	}
	if err := json.Unmarshal([]byte(errS), &env); err != nil {
		t.Fatalf("stderr not an error envelope: %q", errS)
	}
	if env.Error.Code != "backend-unavailable" {
		t.Fatalf("error code = %q, want backend-unavailable", env.Error.Code)
	}
}

// errDokkuAbsent simulates the runner failing to spawn the backend at all.
var errDokkuAbsent = errStub{"dokku: executable file not found"}

type errStub struct{ s string }

func (e errStub) Error() string { return e.s }

// TestLogsTextMode: text output prints the log lines verbatim (human-first
// pass-through) and the readiness cues as "hint:" lines; still exit 0.
func TestLogsTextMode(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	seedLogsExists(f, "plain line one\nplain line two\n")
	seedReadinessNotReady(f)

	outS, _, code := runCLIOpts(t, Options{Runner: f}, "logs", "web")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(outS, "plain line one\n") {
		t.Fatalf("log lines not verbatim in text mode:\n%s", outS)
	}
	if !strings.Contains(outS, "hint: ") {
		t.Fatalf("readiness hint missing in text mode:\n%s", outS)
	}
}

// TestLogsLinesValidation: --lines < 1 is a usage error (exit 2), and the
// valid flag bounds the returned lines (count-capped envelope).
func TestLogsLinesValidation(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	seedLogsExists(f, "a\nb\nc\n")

	_, _, errS, code := runLogsJSON(t, f, "web", "--lines", "0")
	if code != ExitUsage {
		t.Fatalf("--lines 0 exit = %d, want 2 (stderr %q)", code, errS)
	}

	f2 := exectest.NewFakeRunner()
	seedLogsExists(f2, "a\nb\nc\n")
	env, _, _, code := runLogsJSON(t, f2, "web", "--lines", "1")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if lines, _ := env.Data["lines"].([]any); len(lines) != 1 {
		t.Fatalf("--lines 1 returned %v, want exactly one line", env.Data["lines"])
	}
}
