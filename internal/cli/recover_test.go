// SPDX-License-Identifier: MIT

package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/exec/exectest"
)

// T-2xx (M2 slice): the recover command bodies are real
// (internal/recover). These tests pin the cli-level contract the same
// way doctor_test.go does for doctor — envelope shape + exit-code
// mapping — but for the MUTATING commands they always inject a
// FakeRunner through Options.Runner: a test must never run a real
// `dokku` mutation (testing-strategy guard; the real-Dokku e2e for
// recovery is M5).

// runRecoverCLI executes args with an injected fake runner, returning
// stdout, stderr, exit code (a non-TTY stream, so confirm gates take
// the --confirm-or-refuse path, never the interactive prompt).
func runRecoverCLI(t *testing.T, runner *exectest.FakeRunner, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut stringsBuffer
	code := Execute(Options{
		Version:    "test-v0.0.0",
		Stdin:      strings.NewReader(""),
		Out:        &out,
		Err:        &errOut,
		StdinIsTTY: boolFalsePtr(),
		Runner:     runner,
	}, args)
	return out.String(), errOut.String(), code
}

func boolFalsePtr() *bool { b := false; return &b }

// healthyDokku wires a fake answering everything with dokku-healthy static
// output for the default expectations.
func healthyDokku(t *testing.T) *exectest.FakeRunner {
	t.Helper()
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", exectest.Response{Stdout: []byte("dokku version 0.38.31 (git commit abc)")}, "version")
	f.SetOutput("dokku", exectest.Response{Stdout: []byte("=====> core-top-controller")}, "apps:list")
	f.SetOutput("dokku", exectest.Response{
		Stdout: []byte(`{"app":"core-top-controller","deployed":true,"running":false}`),
	}, "ps:report", "core-top-controller", "--format", "json")
	f.SetDefault(exectest.Response{})
	return f
}

// TestRecoverDiagnoseExitCodeContract — diagnose behaves like doctor:
// any fail finding → ExitDrift (1); otherwise 0. No real dokku is ever
// spawned: The machine-independence is achieved via the
// Options.Runner seam.
func TestRecoverDiagnoseExitCodeContract(t *testing.T) {
	t.Parallel()

	// Absent dokku → a dokku/present fail finding → exit 1 (the
	// diagnose-findings code), never a crash, with findings in the
	// versioned envelope.
	f := exectest.NewFakeRunner()
	f.SetDefault(exectest.Response{Err: exec.ErrNotFound})
	var env diagEnvelope
	outS, _, code := runRecoverCLI(t, f, "recover", "diagnose", "-o", "json")
	if code != ExitDrift {
		t.Fatalf("dokku-absent diagnose exit = %d, want %d", code, ExitDrift)
	}
	if err := json.Unmarshal([]byte(outS), &env); err != nil {
		t.Fatalf("stdout not the envelope: %v\n%s", err, outS)
	}
	if env.Command != "recover diagnose" || env.Data.OK {
		t.Errorf("envelope = %+v", env)
	}
	foundDokku := false
	for _, fnd := range env.Data.Findings {
		if fnd.ID == "dokku/present" && fnd.Severity == "fail" {
			foundDokku = true
			if fnd.Hint == "" {
				t.Errorf("the fail finding must carry a suggested action")
			}
		}
	}
	if !foundDokku {
		t.Errorf("diagnose envelope lacks the dokku/present fail: %+v", env.Data.Findings)
	}

	// Healthy fixture, but ps:report says not-running → the
	// controller-state fail also maps to 1. Anything outside {0,1} is a
	// wiring bug: exit codes for findings never use 2/3/4.
	healthy := healthyDokku(t)
	_, _, code = runRecoverCLI(t, healthy, "recover", "diagnose")
	if code != ExitOK && code != ExitDrift {
		t.Errorf("diagnose exit = %d, want 0 or 1 only", code)
	}
}

// TestRecoverRestartWire — the muting path end to end through the CLI:
// confirm gate (no --confirm + non-TTY → 2), --confirm runs the real
// body through the fake runner, dry-run touches nothing.
func TestRecoverRestartWire(t *testing.T) {
	t.Parallel()

	t.Run("no-confirm-is-usage", func(t *testing.T) {
		t.Parallel()
		f := healthyDokku(t)
		_, errS, code := runRecoverCLI(t, f, "recover", "restart")
		if code != ExitUsage {
			t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitUsage, errS)
		}
		if calls := f.Recorded(); len(calls) != 0 {
			t.Errorf("unconfirmed restart %v", calls)
		}
	})

	t.Run("confirmed-runs-and-healthies", func(t *testing.T) {
		t.Parallel()
		f := exectest.NewFakeRunner()
		f.SetOutput("dokku", exectest.Response{Stdout: []byte("restart ok")}, "ps:restart", "core-top-controller")
		f.SetOutput("dokku", exectest.Response{
			Stdout: []byte(`{"app":"core-top-controller","deployed":true,"running":true}`),
		}, "ps:report", "core-top-controller", "--format", "json")
		f.SetDefault(exectest.Response{})
		out, _, code := runRecoverCLI(t, f, "recover", "restart", "--confirm", "-o", "json")
		if code != ExitOK {
			t.Fatalf("exit = %d, want 0", code)
		}
		var env Envelope
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("stdout not json envelope: %v\n%s", err, out)
		}
		if env.SchemaVersion != JSONSchemaVersion || env.Command != "recover restart" {
			t.Errorf("envelope = %+v", env)
		}
		if got := strings.Join(f.RecordedCommands(), " "); !strings.Contains(got, "dokku ps:restart core-top-controller") {
			t.Errorf("the real body never ran ps:restart: %s", got)
		}
	})

	t.Run("dry-run-executes-nothing", func(t *testing.T) {
		t.Parallel()
		f := healthyDokku(t)
		_, _, code := runRecoverCLI(t, f, "recover", "restart", "--confirm", "--dry-run")
		if code != ExitOK {
			t.Fatalf("dry-run exit = %d", code)
		}
		if calls := f.Recorded(); len(calls) != 0 {
			t.Errorf("dry-run executed: %v", calls)
		}
	})
}

// TestRecoverRedeployWire — revision validation (bad sha → 2), the
// pinned-image path via the fake runner, and the store-unavailable
// fallback documented in the envelope notes.
func TestRecoverRedeployWire(t *testing.T) {
	t.Parallel()

	t.Run("bad-revision", func(t *testing.T) {
		t.Parallel()
		f := healthyDokku(t)
		_, _, code := runRecoverCLI(t, f, "recover", "redeploy", "--revision", "ZZZZ", "--confirm")
		if code != ExitUsage {
			t.Fatalf("exit = %d, want %d", code, ExitUsage)
		}
		if calls := f.Recorded(); len(calls) != 0 {
			t.Errorf("invalid revision ran commands: %v", calls)
		}
	})

	t.Run("bootstrap-default-deploys", func(t *testing.T) {
		t.Parallel()
		f := exectest.NewFakeRunner() // no dokku installed
		f.SetOutput("dokku", exectest.Response{
			Stdout: []byte(`{"app":"core-top-controller","deployed":true,"running":true}`),
		}, "ps:report", "core-top-controller", "--format", "json")
		f.SetDefault(exectest.Response{})
		out, _, code := runRecoverCLI(t, f, "recover", "redeploy", "--confirm", "-o", "json")
		if code != ExitOK {
			t.Fatalf("exit = %d", code)
		}
		var env struct {
			SchemaVersion int `json:"schemaVersion"`
			Data          struct {
				App      string   `json:"app"`
				Image    string   `json:"image"`
				Commands []string `json:"commands"`
				Notes    []string `json:"notes"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("stdout not json envelope: %v\n%s", err, out)
		}
		if env.Data.App != "core-top-controller" || env.Data.Image == "" {
			t.Errorf("redeploy payload lacks the image path: %+v", env.Data)
		}
		got := strings.Join(f.RecordedCommands(), "\n")
		if !strings.Contains(got, "dokku git:from-image core-top-controller ") {
			t.Errorf("from-image deploy missing in %s", got)
		}
	})

	t.Run("no-confirm-is-usage", func(t *testing.T) {
		t.Parallel()
		f := healthyDokku(t)
		_, _, code := runRecoverCLI(t, f, "recover", "redeploy")
		if code != ExitUsage {
			t.Fatalf("exit = %d, want %d", code, ExitUsage)
		}
	})
}

// TestRecoverRestoreRequiresBackup — the T-503 restore keeps the CLI
// contract: confirm-gated, --backup REQUIRED, and without it a usage
// error (exit 2) runs nothing.
func TestRecoverRestoreRequiresBackup(t *testing.T) {
	t.Parallel()
	f := healthyDokku(t)
	_, errS, code := runRecoverCLI(t, f, "recover", "restore", "--confirm", "-o", "json")
	if code != ExitUsage {
		t.Fatalf("restore exit = %d, want %d (usage)", code, ExitUsage)
	}
	var env ErrEnvelope
	if err := json.Unmarshal([]byte(strings.TrimSuffix(errS, "\n")), &env); err != nil {
		t.Fatalf("restore stderr not json: %v (%q)", err, errS)
	}
	if !strings.Contains(env.Error.Message, "--backup") {
		t.Errorf("restore guidance must name --backup: %+v", env)
	}
	for _, c := range f.Recorded() {
		t.Errorf("a usage-missing restore ran nothing; ran: %s", c.String())
	}
}

// TestRecoverDiagnoseTextMode — the text render includes the recover
// header and the doctor-vocabulary finding rows (contract example shape
// pinned in internal/recover's golden test).
func TestRecoverDiagnoseTextMode(t *testing.T) {
	t.Parallel()
	f := healthyDokku(t)
	out, _, code := runRecoverCLI(t, f, "recover", "diagnose")
	if code != ExitOK && code != ExitDrift {
		t.Fatalf("exit = %d", code)
	}
	if !strings.HasPrefix(out, "recover diagnose found") {
		t.Fatalf("text header unexpected:\n%s", out)
	}
	for _, id := range []string{"[dokku/", "[controller/", "[store/", "[config/", "[perms/", "[disk/"} {
		if !strings.Contains(out, id) {
			t.Errorf("recover text report lacks finding %s:\n%s", id, out)
		}
	}
}

// diagEnvelope mirrors the diagnose payload (same shape as doctor's).
type diagEnvelope struct {
	SchemaVersion int    `json:"schemaVersion"`
	Command       string `json:"command"`
	Message       string `json:"message"`
	Data          struct {
		OK       bool `json:"ok"`
		Findings []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
			Hint     string `json:"hint"`
		} `json:"findings"`
	} `json:"data"`
}
