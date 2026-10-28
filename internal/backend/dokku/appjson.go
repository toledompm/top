// SPDX-License-Identifier: MIT

// Synthesization of the healthcheck archive for IMAGE deployments
// (ADR-0002 + platform-backend.md): an image deploy has no repository to
// carry app.json, so the adapter builds a tiny deterministic archive —
// a one-line Dockerfile (`FROM <imageRef>`) plus an app.json whose
// healthchecks.web entries derive from the ServiceSpec's HealthCheck —
// and streams it through `git:from-archive <app> --` (verified dokku
// 0.38: stdin form; identical archive content exits 0 early with no
// build record, which gives re-applies their idempotency).
//
// Design decision (archived vs spec-sourcing): the checks come from the
// SPEC, not from a reconciler-computed archive. Rationale: the spec IS
// the authoritative healthcheck intent (service-spec contract), the
// archive is fully derivable from (imageRef, spec) with no other input,
// and making the reconciler pre-compute a tar would leak Dokku-file
// format knowledge across the adapter boundary — the only package
// allowed to know Dokku internals (ADR-0001). The fallback contract
// clause ("fall back to Dokku default checks with a loud warning if the
// path misbehaves") is honored: a failed archive deploy surfaces as a
// *BackendError to the reconciler, and spec-absent checks never engage
// this path at all.
package dokku

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/toledompm/top/internal/domain"
)

// checkEntry is one app.json healthchecks.web item (file-formats/app-json
// + zero-downtime-deploys docs, verified 2026-10-27). Only intentionally
// set fields are marshaled.
type checkEntry struct {
	Type    string `json:"type,omitempty"`    // "startup" (deploy-gating)
	Name    string `json:"name,omitempty"`    // autogen when empty
	Path    string `json:"path,omitempty"`    // http checks
	Port    int    `json:"port,omitempty"`    // http/ whatever check port
	Command string `json:"command,omitempty"` // exec checks (container shell)
	Wait    int    `json:"wait,omitempty"`    // seconds between attempts (spec interval)
}

// buildChecksArchive renders the synthesized tar. Tar safety notes follow
// dokku's archive-safety rules: relative paths only, fixed sane modes.
func buildChecksArchive(imageRef string, spec domain.Service) ([]byte, error) {
	if imageRef == "" {
		return nil, fmt.Errorf("empty image ref")
	}
	hc := spec.HealthCheck
	if hc == nil || !probesPresent(hc) {
		return nil, fmt.Errorf("no healthcheck probes to synthesize")
	}
	var dockerfile strings.Builder
	dockerfile.WriteString("FROM " + imageRef + "\n")

	var checks []checkEntry
	base := checkEntry{Type: "startup"}
	if hc.IntervalSeconds > 0 {
		base.Wait = hc.IntervalSeconds
	}
	if hc.HTTP != nil {
		e := base
		e.Path = hc.HTTP.Path
		e.Port = resolveCheckPort(hc.HTTP.Port, spec.Ports)
		checks = append(checks, e)
	}
	if hc.TCP != nil {
		// app.json has no native TCP probe type (docker-container-
		// healthchecker schema: path/command). A TCP acceptance check is
		// expressed as an exec probe running netcat inside the container —
		// documented here, and only engaged when the spec asks for TCP.
		e := base
		e.Command = "nc -z localhost " + strconv.Itoa(resolveCheckPort(hc.TCP.Port, spec.Ports))
		checks = append(checks, e)
	}
	if hc.Exec != nil {
		e := base
		e.Command = shellJoin(hc.Exec.Command)
		checks = append(checks, e)
	}
	appjson, err := json.MarshalIndent(map[string]any{"healthchecks": map[string]any{"web": checks}}, "", "  ")
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	files := []struct {
		name string
		body []byte
	}{
		{"Dockerfile", []byte(dockerfile.String())},
		{"app.json", appjson},
	}
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// resolveCheckPort maps the spec's check port reference onto a container
// port number: numeric strings pass through; port NAMES resolve against
// the spec's declared ports; empty or named-but-unresolvable falls back
// to 80 (the conventional web port; resolved AGAINST SPEC declarations
// rather than guessed when names are used — the fallback is explicitly
// 80, never dokku's 5000 default, because TOP specs speak container
// ports).
func resolveCheckPort(ref string, ports []domain.Port) int {
	if n, err := strconv.Atoi(ref); err == nil {
		return n
	}
	for _, p := range ports {
		if p.Name == ref {
			return p.ContainerPort
		}
	}
	return 80
}

// shellJoin renders an argv slice as a container shell command line
// (app.json `command` is a single string executed in the container).
// Each argument is quoted, so argv decomposition survives the shell hop.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = strconv.Quote(a)
	}
	return strings.Join(parts, " ")
}
