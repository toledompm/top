//go:build conformance

package conformance_check

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/toledompm/top/internal/cli"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/plan"
	"github.com/toledompm/top/internal/secrets"
)

// TestSecretsNeverInJSON is the observer-contract probe: docs/contracts/
// README.md's shared semantics ("no contract ever carries a secret value;
// only secret references") and ADR-0008 as an end-to-end property over
// everything JSON-shaped: secret manifests, renderered plans through the
// CLI's own envelope renderer, and exec-captured child output.
func TestSecretsNeverInJSON(t *testing.T) {
	const secretVal = "PA$$WORD-t111-poison"
	ctx := context.Background()

	// 1) Secret provider: resolves the value in-process, but the manifest
	//    it publishes is names+checksums only.
	prov := secrets.NewHostFile(secretsDir(t))
	if err := prov.Write("smtp-password", secretVal, false); err != nil {
		t.Fatalf("write fixture secret: %v", err)
	}
	if value, _, err := prov.Resolve(ctx, "smtp-password"); err != nil || value != secretVal {
		t.Fatalf("resolve fixture: %q err=%v", value, err)
	}
	manifest, err := prov.List()
	if err != nil || len(manifest) != 1 {
		t.Fatalf("List: %+v err=%v", manifest, err)
	}
	manifestJSON, merr := json.Marshal(manifest)
	if merr != nil {
		t.Fatalf("marshal manifest: %v", merr)
	}
	if strings.Contains(string(manifestJSON), secretVal) {
		t.Fatalf("manifest JSON leaked the value: %s", manifestJSON)
	}

	// 2) Drift on a secretRef renders through the plan as name-level
	//    markers only — rotate the host content (checksum changes) by using
	//    an unresolvable reference (checksum ""), diff, and push the plan
	//    through the same renderer `topctl diff -o json` uses. No value may
	//    survive any of it.
	desired := domain_serviceWithSecret()
	observed := plan.SynthesizeObserved(desired)
	observed.SecretChecksums = map[string]string{"smtp-password": ""} // unresolvable on host
	sp := plan.Compute(desired, observed)
	if !strings.Contains(repr(sp.Changes), "smtp-password") {
		t.Fatalf("expected a secretRefs change, got %+v", sp.Changes)
	}
	spj, err := json.Marshal(sp)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if strings.Contains(string(spj), secretVal) {
		t.Fatalf("plan JSON leaked the value: %s", spj)
	}

	var out bytes.Buffer
	if err := cli.RenderEnvelope(&out, cli.FormatJSON, cli.Envelope{
		SchemaVersion: cli.JSONSchemaVersion,
		Command:       "diff",
		Message:       "drift",
		Data:          map[string]any{"service": json.RawMessage(spj)},
	}); err != nil {
		t.Fatalf("RenderEnvelope: %v", err)
	}
	if strings.Contains(out.String(), secretVal) {
		t.Fatalf("rendered CLI JSON leaked the value: %s", out.String())
	}
	if !strings.Contains(out.String(), `"schemaVersion":1`) {
		t.Fatalf("rendered envelope missing schemaVersion: %s", out.String())
	}

	// 3) Exec-layer redaction: even when a secret value reaches a child
	//    process pipe, a seeded runner reports only "***".
	r := &exec.FastExec{RedactSeed: []string{secretVal}}
	stdout, stderr, rerr := r.Run(ctx, helperEcho, "-err", secretVal)
	if rerr != nil {
		t.Fatalf("helper run: %v", rerr)
	}
	if strings.Contains(string(stdout), secretVal) || strings.Contains(string(stderr), secretVal) {
		t.Fatalf("exec buffers leaked the value: out=%q err=%q", stdout, stderr)
	}
	if !strings.Contains(string(stderr), "***") {
		t.Fatalf("exec redaction missing: %q", stderr)
	}
}

// TestRedactionExportedHelpers pins exec.Redact/RedactString for callers
// applying the same seeds to other sinks (exec README).
func TestRedactionExportedHelpers(t *testing.T) {
	const seed = "s3cret"
	if got := exec.RedactString("tok "+seed, []string{seed}); got != "tok ***" {
		t.Fatalf("RedactString = %q", got)
	}
	if got := string(exec.Redact([]byte(seed), []string{seed})); got != "***" {
		t.Fatalf("Redact = %q", got)
	}
}

// domain_serviceWithSecret is a desired service with one secretRef (the
// contract world where the value lives ONLY in the host store).
func domain_serviceWithSecret() domain.Service {
	return domain.Service{
		Ref: domain.Ref("observability", "grafana"),
		Source: domain.Source{
			Image:  "docker.io/grafana/grafana",
			Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		SecretRefs:    []string{"smtp-password"},
		RestartPolicy: domain.RestartAlways,
	}
}

// repr is a tiny stringify for diagnostics.
func repr(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmtErr(err)
	}
	return string(b)
}

func fmtErr(err error) string { return "<unserializable:" + err.Error() + ">" }
