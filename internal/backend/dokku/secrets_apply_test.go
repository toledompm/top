// SPDX-License-Identifier: MIT

// Tier-1 tests for Apply's secret handling: the dash-kebab env key
// mapping (the raw secretRef NAME is invalid to dokku — live finding
// 2026-11-15) and just-in-time redaction seeding of resolved values
// (an earlier failure echoed the grafana secret value because the exec
// layer's seed never learned it).
package dokku

import (
	"strings"
	"testing"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec/exectest"
)

func TestApplySecretsDashKebabKeyAndRedactionSeed(t *testing.T) {
	f := exectest.NewFakeRunner()
	// config:set carries the secret value; then a failing command echoes
	// the value (the shape that leaked it before the seeding fix).
	f.SetPrefix([]string{"dokku", "config:set"}, exectest.Response{})
	f.SetPrefix([]string{"dokku", "config:unset"}, exectest.Response{})
	f.SetPrefix([]string{"dokku", "apps:exists"}, exectest.Response{})
	f.SetPrefix([]string{"dokku", "builds:report"}, exectest.Response{Stdout: []byte(`{"build-status":"succeeded"}`)})
	f.SetPrefix([]string{"dokku", "ps:report"}, exectest.Response{Stdout: []byte(`{"deployed":false,"running":false}`)})
	f.SetPrefix([]string{"dokku", "git:report"}, exectest.Response{Stdout: []byte(`{"sha":"HEAD","source-image":"ghcr.io/example/web:1.2.3"}`)})
	f.SetPrefix([]string{"dokku", "apps:report"}, exectest.Response{Stdout: []byte(`{"deploy-source":"docker-image","deploy-source-metadata":"ghcr.io/example/web:1.2.3"}`)})
	f.SetPrefix([]string{"dokku", "network:report"}, exectest.Response{Stdout: []byte(`{"computed-attach-post-deploy":""}`)})
	f.SetPrefix([]string{"dokku", "resource:report"}, exectest.Response{Stdout: []byte(`{}`)})
	f.SetPrefix([]string{"dokku", "ports:report"}, exectest.Response{Stdout: []byte(`[]`)})
	f.SetPrefix([]string{"dokku", "storage:list"}, exectest.Response{Stdout: []byte(`[]`)})
	f.SetPrefix([]string{"dokku", "config:keys"}, exectest.Response{Stdout: []byte("TZ\n")})
	f.SetDefault(exectest.Response{})

	spec := domain.Service{
		Ref:    domain.Ref("default", "web"),
		Source: domain.Source{Image: "ghcr.io/example/web", Tag: "1.2.3"},
		Env:    map[string]string{"TZ": "UTC"},
		SecretRefs: []string{
			"gf-security-admin-password", // dash-kebab: must map to GF_SECURITY_ADMIN_PASSWORD
		},
		Routing: domain.Routing{Mode: domain.RoutingProxy, Domains: []string{"web.example.com"}},
	}
	secrets := map[string]string{"gf-security-admin-password": "s3cr3t-value"}

	c := newC(f)
	if err := c.Apply(run(t), spec.Ref, spec, secrets, applyOpts()); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// The env set carried the MAPPED key (never the raw service-name).
	var envArgv string
	for _, call := range f.Recorded() {
		if call.Name == "dokku" && len(call.Args) > 1 && call.Args[0] == "config:set" {
			envArgv = strings.Join(call.Args, " ")
		}
	}
	if envArgv == "" {
		t.Fatalf("no config:set recorded: %v", f.Recorded())
	}
	if !strings.Contains(envArgv, "GF_SECURITY_ADMIN_PASSWORD=s3cr3t-value") {
		t.Errorf("config:set argv %q must carry the mapped dash-kebab key", envArgv)
	}
	if strings.Contains(envArgv, "gf-security-admin-password=s3cr3t-value") {
		t.Errorf("config:set argv %q must never carry the raw secretRef name as a key", envArgv)
	}

	// The resolved value was registered for redaction BEFORE the command
	// that could echo it.
	if seeds := f.SeedsOf(); len(seeds) == 0 || seeds[0] != "s3cr3t-value" {
		t.Errorf("redaction seeds = %v, want the resolved secret value registered", seeds)
	}
}
