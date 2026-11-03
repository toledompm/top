// SPDX-License-Identifier: MIT

// Tier-1 (hermetic) tests for the tier-4 VM script helpers in lib.sh
// (docs/architecture/testing-strategy.md: every tier-4 script's pure
// helpers are unit-tested portable). The shell functions are exercised by
// ACTUALLY running them under sh — the runtime contract, not a re-parse.
//
// Missing-bin cases: the hmac test requires openssl (it skips if absent);
// everything else needs only a POSIX sh.

package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runLib executes a snippet under sh with lib.sh sourced; returns output.
func runLib(t *testing.T, snippet string) (string, string, error) {
	t.Helper()
	script := ". " + shellQuote(filepath.Join(".", "lib.sh")) + "\n" + snippet
	cmd := exec.Command("sh", "-c", script)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func runLibFail(t *testing.T, snippet string) {
	t.Helper()
	_, errOut, err := runLib(t, snippet)
	if err == nil {
		t.Fatalf("expected failure, got success (stderr %q)", errOut)
	}
	if !bytes.Contains([]byte(errOut), []byte("FAIL")) {
		t.Fatalf("failure must print a FAIL line, got stderr %q", errOut)
	}
}

func shellQuote(s string) string { return "'" + s + "'" }

const probeFixture = `apiVersion: top/v1alpha1
kind: Service
metadata:
  name: grafana
  namespace: observability
spec:
  env:
    TZ: UTC
    GF_SECURITY_ADMIN_USER: admin
`

func TestInjectEnvProbe(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "grafana.yaml")
	if err := os.WriteFile(spec, []byte(probeFixture), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	// inject inserts the probe line right after the env: header
	if _, errOut, err := runLib(t, "inject_env_probe "+shellQuote(spec)+" E2E_PROBE_T404 '\"probe-webhook-1\"'"); err != nil {
		t.Fatalf("inject failed: %v (stderr %s)", err, errOut)
	}
	got, err := os.ReadFile(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := `apiVersion: top/v1alpha1
kind: Service
metadata:
  name: grafana
  namespace: observability
spec:
  env:
    E2E_PROBE_T404: "probe-webhook-1"
    TZ: UTC
    GF_SECURITY_ADMIN_USER: admin
`
	if string(got) != want {
		t.Fatalf("injected spec:\n%s\nwant:\n%s", got, want)
	}

	// inject over an existing key updates the value in place (one key, one line)
	if _, _, err := runLib(t, "inject_env_probe "+shellQuote(spec)+" E2E_PROBE_T404 '\"probe-2\"'"); err != nil {
		t.Fatalf("idempotent inject failed: %v", err)
	}
	got2, _ := os.ReadFile(spec)
	if !bytes.Contains(got2, []byte("  E2E_PROBE_T404: \"probe-2\"\n")) {
		t.Fatalf("idempotent inject did not rewrite the value: %s", got2)
	}
	if bytes.Count(got2, []byte("E2E_PROBE_T404")) != 1 {
		t.Fatalf("idempotent inject duplicated the key: %s", got2)
	}

	// inject into a spec WITHOUT an env block fails loudly (exit 3)
	noenv := filepath.Join(dir, "noenv.yaml")
	_ = os.WriteFile(noenv, []byte("apiVersion: top/v1alpha1\nkind: Service\n"), 0o644)
	if _, _, err := runLib(t, "inject_env_probe "+shellQuote(noenv)+" K '\"v\"'"); err == nil {
		t.Fatal("inject must fail (exit 3) when the spec has no env block")
	}
}

func TestSetEnvProbe(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "g.yaml")
	if err := os.WriteFile(spec, []byte(probeFixture), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if _, _, err := runLib(t, "set_env_probe "+shellQuote(spec)+" TZ '\"UTC-x\"'"); err != nil {
		t.Fatalf("set_env_probe failed: %v", err)
	}
	got, _ := os.ReadFile(spec)
	if !bytes.Contains(got, []byte("    TZ: \"UTC-x\"\n")) {
		t.Fatalf("set_env_probe did not rewrite the value:\n%s", got)
	}
	// ambiguous (duplicated) keys refuse to rewrite: exit 3
	dup := filepath.Join(dir, "dup.yaml")
	_ = os.WriteFile(dup, []byte("env:\n  K: a\n  other: 1\n  K: b\n"), 0o644)
	if _, _, err := runLib(t, "set_env_probe "+shellQuote(dup)+" K '\"v\"'"); err == nil {
		t.Fatal("set_env_probe must refuse ambiguous keys (exit 3)")
	}
}

func TestAssertHelpers(t *testing.T) {
	if _, _, err := runLib(t, "assert_eq name 1 1"); err != nil {
		t.Fatalf("assert_eq success path failed: %v", err)
	}
	runLibFail(t, "assert_eq name 1 2")
	if _, _, err := runLib(t, "assert_contains name \"value includes needle here\" needle"); err != nil {
		t.Fatalf("assert_contains hit failed: %v", err)
	}
	runLibFail(t, "assert_contains name \"no such\" needle")
	if _, _, err := runLib(t, "assert_not_contains name \"clean\" needle"); err != nil {
		t.Fatalf("assert_not_contains clean failed: %v", err)
	}
	runLibFail(t, "assert_not_contains name \"needle here\" needle")
}

func TestWaitUntil(t *testing.T) {
	// success on an instant probe
	if _, _, err := runLib(t, "wait_until $(($(date +%s)+5)) immediate sh -c 'exit 0'"); err != nil {
		t.Fatalf("wait_until success: %v", err)
	}
	// an already-elapsed deadline fails immediately (bounds the runtime)
	runLibFail(t, "wait_until $(($(date +%s)+0)) never sh -c 'exit 1'")
}

func TestHistoryHasSplitsCorrelation(t *testing.T) {
	// A structural check of the jq predicate WITHOUT a TOP install: the
	// real history surface is pinned on the VM, but the doc-dependent
	// column names are contract-fixed (internal/cli/reconcmd.go), so a
	// canned history payload exercises the full predicate.
	fixture := filepath.Join(t.TempDir(), "history.json")
	payload := `{"operations":[
      {"operationId":"op_a","kind":"reconcile","state":"succeeded","actor":"webhook","correlationId":"t404-webhook-1-","revision":"aaa","startedAt":"2026-11-03T10:00:00Z"}
    ]}`
	if err := os.WriteFile(fixture, []byte(payload), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	// history_has is wired to `topctl`; here we run the same jq predicate
	// through lib's documented jq invocation by shelling the helper's core
	// query directly (the topctl-bound wrapper stays VM-covered).
	_, _, err := runLib(
		t, "jq -e --arg state succeeded --arg actor webhook --arg rev aaa --arg corr t404-webhook-1- "+
			"'.operations[] | select(.state == $state and .actor == $actor and .revision == $rev) | if $corr == \"\" then . else select(.correlationId == $corr) end' "+
			shellQuote(fixture)+">/dev/null")
	if err != nil {
		t.Fatalf("history predicate does not match the contract shape: %v", err)
	}
	if _, errOut, err := runLib(
		t, "jq -e --arg state succeeded --arg actor webhook --arg rev zz9 --arg corr \"\" "+
			"'.operations[] | select(.state == $state and .actor == $actor and .revision == $rev) | if $corr == \"\" then . else select(.correlationId == $corr) end' "+
			shellQuote(fixture)+">/dev/null"); err == nil {
		t.Fatalf("history predicate matched a non-matching revision (stderr %s)", errOut)
	}
}

func TestHmacSha256HexMatchesGo(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not available; the tier-4 VM is the real rung")
	}
	secret := "t404-fake-operator-secret"
	body := `{"ref":"refs/heads/main","after":"broken\" payload qu”ote"}b` // deliberately odd bytes
	bodyFile := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	out, _, err := runLib(t, "hmac_sha256_hex "+shellQuote(secret)+" "+shellQuote(bodyFile))
	if err != nil {
		t.Fatalf("hmac helper failed: %v", err)
	}
	got := strings.TrimSpace(out) // $(..) keeps the trailing newline
	if len(got) != hashHexLen || !isLowerHex(got) {
		t.Fatalf("hmac output not 64 lowercase hex chars: %q", got)
	}
	// byte-for-byte against the Go computation internal/webhook verifies with
	want := goHmacSha256Hex(t, secret, body)
	if got != want {
		t.Fatalf("shell HMAC %q != Go HMAC %q (webhook.verify would reject the script's signature)", got, want)
	}
}

const hashHexLen = sha256.Size * 2

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func goHmacSha256Hex(t *testing.T, secret, body string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestDockerRunningName(t *testing.T) {
	bin := t.TempDir()
	dockerStub := filepath.Join(bin, "docker")
	stubBody := `#!/bin/sh
# stub honoring ONLY the exact invocation lib.sh makes; anything else is
# drift the test must catch (exit 9).
if [ "$1" = "ps" ] && [ "$2" = "--filter" ] && [ "$3" = "status=running" ] && [ "$4" = "--filter" ]; then
  printf 'app-one.web.1\napp-two.web.2\n'
  exit 0
fi
exit 9
`
	if err := os.WriteFile(dockerStub, []byte(stubBody), 0o755); err != nil { //nolint:gosec // test stub
		t.Fatal(err)
	}
	prepend := "export PATH=" + shellQuote(bin+":"+os.Getenv("PATH")) + "; export DOCKER_BIN=docker; "

	// first match wins (sed -n 1p). The stub prints both names regardless
	// of the filter arg — the substring filtering is docker's job, which
	// this contract only measures indirectly on the VM.
	out, _, err := runLib(t, prepend+"docker_running_name 'anything' | tr -d '\\n'")
	if err != nil {
		t.Fatal(err)
	}
	if out != "app-one.web.1" {
		t.Fatalf("docker_running_name got %q, want first match app-one.web.1", out)
	}

	// no match: honest empty output (the scripts' preconditions key off it)
	if err := os.WriteFile(dockerStub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // test stub
		t.Fatal(err)
	}
	out2, _, _ := runLib(t, prepend+"docker_running_name 'none'; echo END")
	if out2 != "END\n" {
		t.Fatalf("docker_running_name with no match must print nothing, got %q", out2)
	}
}
