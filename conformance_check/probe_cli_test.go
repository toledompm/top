//go:build conformance

package conformance_check

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/toledompm/top/internal/cli"
)

// runTopctl invokes cli.Execute the way the cli.md contract's consumers (a
// shell or an agent) see it: captured stdio, non-TTY stdin. Mirrors
// internal/cli's own harness (its runCLI is not exported).
func runTopctl(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut strings.Builder
	stdinIsTTY := false
	code = cli.Execute(cli.Options{
		Version:    "test-v0.0.0",
		Stdin:      strings.NewReader(""),
		Out:        &out,
		Err:        &errOut,
		StdinIsTTY: &stdinIsTTY,
	}, args)
	return out.String(), errOut.String(), code
}

// TestCLIExitCodeContractSpotChecks pins the exit-code column of
// docs/contracts/cli.md ("0 ok; ... 2 usage; 3 operation failed") against
// `topctl` stubs — a spot-check of the contract table's observable rows:
//
//	version → 0 (read-only)
//	status  → 0 (read-only)
//	install without --confirm, non-TTY → 2 (usage: it needs its gate)
//	uninstall --confirm → 3 (operation "failed": stub cannot mutate — the
//	  invocation was well formed, so never 2)
//	unknown command → 2 (usage)
func TestCLIExitCodeContractSpotChecks(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"version", []string{"version"}, cli.ExitOK},
		{"status", []string{"status"}, cli.ExitOK},
		{"install-unconfirmed", []string{"install", "github.com/example/top"}, cli.ExitUsage},
		{"uninstall-confirmed", []string{"uninstall", "--confirm"}, cli.ExitFailed},
		{"unknown-command", []string{"frobnicate"}, cli.ExitUsage},
	}
	for _, tc := range cases {
		stdout, _, code := runTopctl(t, tc.args...)
		if code != tc.want {
			t.Fatalf("%s: exit %d, want %d (stdout: %s)", tc.name, code, tc.want, stdout)
		}
	}
}

// TestCLIEnvelopeJSONIsVersioned pins cli.md's "JSON outputs are versioned
// objects ("schemaVersion": 1)" — success envelopes on stdout, error
// envelopes on stderr, both parse as one JSON object carrying schemaVersion.
func TestCLIEnvelopeJSONIsVersioned(t *testing.T) {
	cases := []struct {
		args     []string
		wantCode int
	}{
		{[]string{"version", "-o", "json"}, cli.ExitOK},
		{[]string{"status", "-o", "json"}, cli.ExitOK},
		{[]string{"install", "github.com/example/top", "-o", "json"}, cli.ExitUsage},
	}
	for _, tc := range cases {
		stdout, stderr, code := runTopctl(t, tc.args...)
		if code != tc.wantCode {
			t.Fatalf("%v: exit %d, want %d", tc.args, code, tc.wantCode)
		}
		blob := stdout
		if code != cli.ExitOK {
			blob = stderr
		} else if strings.TrimSpace(stderr) != "" {
			t.Fatalf("%v: anything on stderr: %q", tc.args, stderr)
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(blob)), &env); err != nil {
			t.Fatalf("%v: not one JSON object: %q: %v", tc.args, blob, err)
		}
		if sv, ok := env["schemaVersion"].(float64); !ok || sv != float64(cli.JSONSchemaVersion) {
			t.Fatalf("%v: schemaVersion = %v, want %d", tc.args, env["schemaVersion"], cli.JSONSchemaVersion)
		}
	}
}
