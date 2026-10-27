package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// runCLI builds a fresh root and executes argv with captured streams,
// returning (stdout, stderr, exit code). No real TTY anywhere: interactive
// confirm paths are exercised by forcing Options.StdinIsTTY (documented
// in confirm.go and the README — a captured io.Reader can't be a char
// device, so tests defeat detection instead of needing a PTY harness).
func runCLI(t *testing.T, stdinIsTTY bool, stdin string, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut stringsBuffer
	tty := stdinIsTTY
	code := Execute(Options{
		Version:    "test-v0.0.0",
		Stdin:      strings.NewReader(stdin),
		Out:        &out,
		Err:        &errOut,
		StdinIsTTY: &tty,
	}, args)
	return out.String(), errOut.String(), code
}

// stringsBuffer is a minimal in-memory writer (captured stdio).
type stringsBuffer struct{ strings.Builder }

// TestExitCodeContract pins the exit-code column of docs/contracts/cli.md
// against every command class while it is a stub: read-only → 0, mutating
// → 3 (the operation cannot succeed today), invalid usage/validation → 2.
func TestExitCodeContract(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want int
	}{
		// read-only stubs → 0 with the stub envelope
		{"status", []string{"status"}, ExitOK},
		{"diff", []string{"diff"}, ExitOK},
		{"history", []string{"history", "core/prometheus"}, ExitOK},
		{"logs", []string{"logs", "prometheus"}, ExitOK},
		{"doctor", []string{"doctor"}, ExitOK},
		{"backup-list", []string{"backup", "list"}, ExitOK},
		{"backup-verify", []string{"backup", "verify"}, ExitOK},
		{"recover-diagnose", []string{"recover", "diagnose"}, ExitOK},
		{"version", []string{"version"}, ExitOK},

		// mutating stubs → 3 (invocation is well formed; the gap is the tool)
		{"install-confirmed", []string{"install", "github.com/example/top", "--confirm"}, ExitFailed},
		{"uninstall-confirmed", []string{"uninstall", "--confirm"}, ExitFailed},
		{"reconcile", []string{"reconcile"}, ExitFailed},
		{"deploy", []string{"deploy", "core/prometheus"}, ExitFailed},
		{"backup-create-confirmed", []string{"backup", "create", "--confirm"}, ExitFailed},
		{"recover-restart-confirmed", []string{"recover", "restart", "--confirm"}, ExitFailed},
		{"recover-redeploy-confirmed", []string{"recover", "redeploy", "--confirm"}, ExitFailed},
		{"recover-restore-confirmed", []string{"recover", "restore", "--confirm"}, ExitFailed},
		{"controller-run", []string{"controller", "run"}, ExitFailed},

		// confirm gate: gated command without --confirm, non-TTY → 2
		{"install-no-confirm", []string{"install", "github.com/example/top"}, ExitUsage},
		{"uninstall-no-confirm", []string{"uninstall"}, ExitUsage},
		{"backup-create-no-confirm", []string{"backup", "create"}, ExitUsage},
		{"recover-restart-no-confirm", []string{"recover", "restart"}, ExitUsage},
		{"recover-restore-no-confirm", []string{"recover", "restore"}, ExitUsage},
		{"reconcile-prune-no-confirm", []string{"reconcile", "--prune"}, ExitUsage},
		{"reconcile-include-controller-no-confirm", []string{"reconcile", "--include-controller"}, ExitUsage},

		// validation errors → 2, even in stubs
		{"deploy-bad-ref", []string{"deploy", "NOT_VALID/ref-"}, ExitUsage},
		{"logs-too-many-slashes", []string{"logs", "ns/name/extra"}, ExitUsage},
		{"status-bad-namespace", []string{"status", "--namespace", "Core!"}, ExitUsage},
		{"diff-bad-revision", []string{"diff", "--revision", "ZZZ"}, ExitUsage},
		{"deploy-bad-revision", []string{"deploy", "core/prometheus", "--revision", "ABCDEF0"}, ExitUsage},
		{"history-zero-limit", []string{"history", "--limit", "0"}, ExitUsage},
		{"bad-output-format", []string{"status", "-o", "yaml"}, ExitUsage},
		{"bad-redact", []string{"status", "--redact", "off"}, ExitUsage},
		{"bad-schema-version", []string{"status", "--schema-version", "2"}, ExitUsage},
		{"backup-no-subcommand", []string{"backup"}, ExitUsage},
		{"recover-no-subcommand", []string{"recover"}, ExitUsage},
		{"unknown-flag", []string{"status", "--wat"}, ExitUsage},
		{"unknown-command", []string{"toplevel"}, ExitUsage},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, got := runCLI(t, false, "", tc.args...)
			if got != tc.want {
				t.Fatalf("%v: exit code = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

// TestStubEnvelopesAreVersioned: every successful stub prints the versioned
// envelope with the right command field; -o json output parses and carries
// schemaVersion=1 + "not implemented" message.
func TestStubEnvelopesAreVersioned(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{"status", "diff", "doctor", "backup list", "recover diagnose"} {
		args := strings.Fields(cmd)
		outS, _, code := runCLI(t, false, "", append(args, "-o", "json")...)
		if code != ExitOK {
			t.Fatalf("%s -o json: exit %d, want 0", cmd, code)
		}
		var env Envelope
		if err := json.Unmarshal([]byte(outS), &env); err != nil {
			t.Fatalf("%s -o json: not one JSON object: %q: %v", cmd, outS, err)
		}
		if env.SchemaVersion != JSONSchemaVersion {
			t.Errorf("%s: schemaVersion = %d, want %d", cmd, env.SchemaVersion, JSONSchemaVersion)
		}
		if env.Command != cmd {
			t.Errorf("%s: command = %q", cmd, env.Command)
		}
		if env.Message != notImplementedOp {
			t.Errorf("%s: message = %q, want %q", cmd, env.Message, notImplementedOp)
		}
	}
}

// TestVersionEnvelope: `topctl version` is real in M1 (build info + floors).
func TestVersionEnvelope(t *testing.T) {
	t.Parallel()
	outS, _, code := runCLI(t, false, "", "version", "-o", "json")
	if code != ExitOK {
		t.Fatalf("version exit = %d, want %d", code, ExitOK)
	}
	var env Envelope
	if err := json.Unmarshal([]byte(outS), &env); err != nil {
		t.Fatalf("version json: %v", err)
	}
	if env.SchemaVersion != JSONSchemaVersion || env.Command != "version" {
		t.Fatalf("version envelope = %+v", env)
	}
	for k, want := range map[string]any{"topctl": "test-v0.0.0", "minDokku": MinDokkuVersion, "schemaVersion": float64(JSONSchemaVersion)} {
		if got := env.Data[k]; got != want {
			t.Errorf("version data[%s] = %v, want %v", k, got, want)
		}
	}
}

// TestHelpMentionsTopctl everywhere (rename-safety, ADR-0011: the binary
// is topctl, never `top`; every usage/help surface must say so).
func TestHelpMentionsTopctl(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{},
		{"--help"},
		{"status", "--help"},
		{"backup", "--help"},
		{"backup", "create", "--help"},
		{"recover", "--help"},
		{"recover", "restart", "--help"},
		{"uninstall", "--help"},
		{"no-such-command"},
	} {
		outS, errS, code := runCLI(t, false, "", args...)
		if code != ExitOK && code != ExitUsage {
			t.Fatalf("%v: help exit = %d", args, code)
		}
		if !strings.Contains(outS+errS, "topctl") {
			t.Errorf("%v: usage output does not mention topctl:\n%s%s", args, outS, errS)
		}
	}
}

// TestConfirmPromptPaths: both branches of the confirm gate, driven with
// captured stdio and forced TTY (see runCLI note — no PTY harness needed).
func TestConfirmPromptPaths(t *testing.T) {
	// TTY forced → prompt offered; "y" consents; gated stub then exits 3.
	stdout, _, code := runCLI(t, true, "y\n", "install", "github.com/example/top")
	if code != ExitFailed {
		t.Fatalf("consented install: exit = %d, want %d", code, ExitFailed)
	}
	if !strings.Contains(stdout, "Proceed? [y/N]") {
		t.Fatalf("consented install: prompt missing, stdout:\n%s", stdout)
	}

	// Prompt declined → failed authorization (usage exit), never success.
	_, _, code = runCLI(t, true, "n\n", "uninstall")
	if code != ExitUsage {
		t.Fatalf("declined uninstall: exit = %d, want %d", code, ExitUsage)
	}

	// TTY but EOF (empty stdin) → refused authorization.
	_, _, code = runCLI(t, true, "", "uninstall")
	if code != ExitUsage {
		t.Fatalf("EOF uninstall: exit = %d, want %d", code, ExitUsage)
	}

	// --confirm short-circuits the prompt; nothing asks on a TTY either.
	stdout, _, code = runCLI(t, true, "", "install", "github.com/example/top", "--confirm")
	if code != ExitFailed {
		t.Fatalf("--confirm install: exit = %d, want %d", code, ExitFailed)
	}
	if strings.Contains(stdout, "Proceed?") {
		t.Fatalf("--confirm must not prompt; stdout:\n%s", stdout)
	}
}

// --config: strict load through internal/config; an unreadable or invalid
// file is a usage error (exit 2), a good one changes nothing for stubs.
func TestConfigFlag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := dir + "/top-config.yaml"
	goodYaml := "apiVersion: top/v1alpha1\nlogLevel: debug\n"
	if err := os.WriteFile(good, []byte(goodYaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runCLI(t, false, "", "status", "--config", good); code != ExitOK {
		t.Fatalf("--config good exit = %d, want %d", code, ExitOK)
	}
	bad := dir + "/bad.yaml"
	if err := os.WriteFile(bad, []byte("apiVersion: wrong/false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errS, code := runCLI(t, false, "", "status", "--config", bad)
	if code != ExitUsage {
		t.Fatalf("--config bad exit = %d, want %d (stderr %q)", code, ExitUsage, errS)
	}
	if !strings.Contains(errS, "loading config") {
		t.Errorf("bad config stderr lacks diagnosis: %q", errS)
	}
}

// Text render of the stub notice (themed renderer, golden inline).
func TestTextRenderStub(t *testing.T) {
	t.Parallel()
	outS, _, code := runCLI(t, false, "", "status")
	if code != ExitOK {
		t.Fatalf("status exit = %d", code)
	}
	want := "status\n  message: not implemented: planned M2/M3\n"
	if outS != want {
		t.Fatalf("text mode:\n%q\nwant:\n%q", outS, want)
	}
}
