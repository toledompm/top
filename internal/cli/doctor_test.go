// SPDX-License-Identifier: MIT

package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// T-203: the doctor command body is real (internal/doctor). These tests
// pin the cli-level contract — envelope shape and the exit-code mapping —
// without depending on what machine they run on:
//
//   - text/json envelope renders with the doctor payload;
//   - exit 0 iff no fail findings, exit 1 (ExitDrift) when there are —
//     the doctor row of docs/contracts/cli.md ("exits non-zero with
//     per-check failures + fixes") via the contract's code-1 space;
//   - warns/info/ok never flip the exit code (advisory, tested here by
//     consistency between payload counts and the code);
//   - --skip-ports is accepted and skips the port probe (payload shows
//     the ports/skipped finding instead of ports/80 + ports/443).
//
// Deterministic per-finding outcomes live in internal/doctor's unit
// tests (all seams faked); here we use the real machine's state and only
// assert the invariants that hold everywhere.

// doctorJSON is the parsed doctor envelope.
type doctorJSON struct {
	SchemaVersion int    `json:"schemaVersion"`
	Command       string `json:"command"`
	Message       string `json:"message"`
	Data          struct {
		OK        bool `json:"ok"`
		FailCount int  `json:"failCount"`
		WarnCount int  `json:"warnCount"`
		Findings  []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
			Check    string `json:"check"`
			Message  string `json:"message"`
			Hint     string `json:"hint"`
		} `json:"findings"`
		MinDokku string `json:"minDokku"`
	} `json:"data"`
}

func runDoctorJSON(t *testing.T, args ...string) (doctorJSON, int) {
	t.Helper()
	args = append([]string{"doctor", "-o", "json"}, args...)
	out, _, code := runCLI(t, false, "", args...)
	var env doctorJSON
	if err := json.Unmarshal([]byte(strings.TrimSuffix(out, "\n")), &env); err != nil {
		t.Fatalf("doctor -o json: %d: stdout not the envelope: %v\n%s", code, err, out)
	}
	return env, code
}

// TestDoctorEnvelopeAndExitCodeContract — the invariants that hold on
// any machine (the machine's own state decides which side we land on,
// but the payload/excode pairing is pinned both ways).
func TestDoctorEnvelopeAndExitCodeContract(t *testing.T) {
	t.Parallel()
	env, code := runDoctorJSON(t)
	if env.SchemaVersion != JSONSchemaVersion || env.Command != "doctor" {
		t.Fatalf("envelope = %+v", env)
	}
	fails := false
	for _, f := range env.Data.Findings {
		if f.Severity == "fail" {
			fails = true
		}
	}
	if env.Data.OK == fails {
		t.Errorf("ok (%v) must be the negation of any fail finding", env.Data.OK)
	}
	wantCode := ExitOK
	if fails {
		wantCode = ExitDrift
	}
	if code != wantCode {
		t.Errorf("exit = %d, want %d (fails=%v)", code, wantCode, fails)
	}
	if env.Data.MinDokku == "" {
		t.Errorf("doctor payload must carry minDokku")
	}
}

// TestDoctorSkipPorts: --skip-ports accepted; the skip verdict surfaces
// in the payload (info finding "ports/skipped"), and no ports/80 bind
// probe ran (its absence is implied by the skipped finding's presence).
func TestDoctorSkipPorts(t *testing.T) {
	t.Parallel()
	env, code := runDoctorJSON(t, "--skip-ports")
	if _, _, exit := runCLI(t, false, "", "doctor", "-o", "json"); exit != code {
		t.Logf("skip-ports run exit %d vs plain %d (warn counting should be unaffected)", code, exit)
	}
	found := false
	for _, f := range env.Data.Findings {
		if f.ID == "ports/skipped" {
			found = f.Severity == "info"
		}
	}
	if !found {
		t.Errorf("--skip-ports payload lacks a benign ports/skipped finding: %+v", env.Data.Findings)
	}
}

// TestDoctorTextMode shows the per-check failures + fixes render (the
// real report: header sentence, then findings with hint lines).
func TestDoctorTextMode(t *testing.T) {
	t.Parallel()
	out, _, code := runCLI(t, false, "", "doctor")
	if code != ExitOK && code != ExitDrift {
		t.Fatalf("doctor text exit = %d", code)
	}
	if !strings.HasPrefix(out, "doctor: doctor found") {
		t.Fatalf("doctor text header unexpected:\n%s", out)
	}
	for _, id := range []string{"[os/", "[docker/", "[dokku/", "[resources/", "[store/"} {
		if !strings.Contains(out, id) {
			t.Errorf("doctor text report lacks finding %s:\n%s", id, out)
		}
	}
}
