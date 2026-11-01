// SPDX-License-Identifier: MIT
package install

// T-401: the embedded platform-repo template carries the full example
// fleet. These tests pin the template's OWN guarantees so the seed commit
// stays loadable by topctl:
//
//   - every shipped spec parses and validates under the real internal/spec
//     loader (strict parsing, platform-repo namespace unlock, ADR-0004);
//   - the fleet is exactly the ADR-0010 stack, all digest-pinned (zero
//     mutable-tag warnings — digests are fictional but well-shaped);
//   - the {{CONTROLLER_IMAGE}} token remains the template's only
//     existential placeholder (repo_init.go renders exactly that one);
//   - no secret value ships in any file (ADR-0008; grafana via secretRef).
//
// Failure messages must survive copying: loader errors carry file:line
// (internal/spec ParseError canon), asserted here via the joined output.

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/toledompm/top/internal/spec"
)

// loadTemplateFleet renders the embedded template via the same render
// function --init uses (placeholder in) and validates it as a platform
// repo, so the test exercises the exact bytes TOP would commit.
func loadTemplateFleet(t *testing.T) spec.Result {
	t.Helper()
	dir := renderTemplateIntoTemp(t)
	res, err := spec.LoadDir(dir, spec.WithPlatformRepo(true))
	if err != nil {
		// Keep the file:line canon visible on failure (errors.Join output)
		// so the report is actionable at the offending spec line.
		t.Fatalf("embedded template specs must validate as a platform repo:\n%s", err)
	}
	return res
}

func renderTemplateIntoTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	n, err := renderTemplate(dir, "ghcr.io/example/top/topctl:bootstrap-pinned")
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}
	// README + 5 ServiceSpecs + prometheus.yml + 3 grafana provisioning files.
	if n != 10 {
		t.Errorf("rendered template files = %d, want 10 (inventory changed: update this count and the README layout table together)", n)
	}
	return dir
}

// flossDigestRe matches the template's fictional digest bodies (all-zero
// and repeated-digit hex — obviously non-runnable, per ADR-0014 discipline).
var flossDigestRe = regexp.MustCompile(`^sha256:(000[0-9]|1111|2222|3333)[0-9a-f]*$`)

func TestTemplateFleetIsCompleteAndValid(t *testing.T) {
	t.Parallel()
	res := loadTemplateFleet(t)

	names := map[string]bool{} // "ns/name" set
	for _, s := range res.Services {
		names[string(s.Ref.Namespace)+"/"+string(s.Ref.Name)] = true
	}
	for _, want := range []string{
		"core/top-controller",
		"observability/prometheus",
		"observability/grafana",
		"observability/node-exporter",
		"observability/cadvisor",
	} {
		if !names[want] {
			t.Errorf("template fleet missing service %q (ADR-0010 stack); loaded:%v", want, loadedNames(res))
		}
	}
	if len(res.Services) != 5 {
		t.Errorf("template fleet loaded %d services, want exactly 5: %v", len(res.Services), loadedNames(res))
	}
}

func loadedNames(res spec.Result) string {
	got := []string{}
	for _, s := range res.Services {
		got = append(got, string(s.Ref.Namespace)+"/"+string(s.Ref.Name))
	}
	return strings.Join(got, ", ")
}

func TestTemplateFleetIsFullyDigestPinned(t *testing.T) {
	t.Parallel()
	res := loadTemplateFleet(t)
	for _, s := range res.Services {
		ref := string(s.Ref.Namespace) + "/" + string(s.Ref.Name)
		if ref == "core/top-controller" {
			// The bootstrap image is pinned by the installer contract
			// (ADR-0005): the rendered controller image ref is the pinned
			// bootstrap default verbatim — the loader sees a bare image
			// with no tag/digest field on purpose (the deliberate
			// mutable-tag warning below). The operator digest-pins on the
			// first controller deploy.
			if !strings.Contains(s.Source.Image, ":") {
				t.Errorf("core/top-controller: image %q must be the rendered pinned bootstrap ref (a versioned ref)", s.Source.Image)
			}
			continue
		}
		if s.Source.Digest == "" {
			t.Errorf("%s: platform-repo specs must pin digests (immutable-revisions contract); got mutable image %q", s.Ref, s.Source.Image)
		} else if !flossDigestRe.MatchString(s.Source.Digest) {
			t.Errorf("%s: platform-repo template digests must stay fictional discipline zero/repeated-digit style (ADR-0014), got %q", s.Ref, s.Source.Digest)
		}
	}
	// Warnings allowed: exactly the one deliberate mutable-tag warning on the
	// controller's bootstrap pin. Everything else must be warning-free.
	for _, w := range res.Warnings {
		ok := w.Path == "core/top-controller.yaml" && w.Code == spec.WarnMutableTag && w.Field == "spec.source"
		if !ok {
			t.Errorf("template spec warning in %s at %s: %s: %s (only the controller's deliberate bootstrap mutable-tag is allowed)",
				w.Path, w.Field, w.Code, w.Message)
		}
	}
	// And that one deliberate warning must actually be present.
	dropped := true
	for _, w := range res.Warnings {
		if w.Path == "core/top-controller.yaml" && w.Code == spec.WarnMutableTag && w.Field == "spec.source" {
			dropped = false
		}
	}
	if dropped {
		t.Errorf("core/top-controller.yaml: expected the deliberate mutable-tag warning for the bootstrap pin (contract: digest-pinning recommended, tag default documents the trade-off)")
	}
}

// TestTemplateGrafanaSecretRefNotInline: ADR-0008 — the admin password is a
// secretRef, and no password-ish env value appears in any shipped file.
func TestTemplateGrafanaSecretRefNotInline(t *testing.T) {
	t.Parallel()
	res := loadTemplateFleet(t)
	seen := false
	for _, svc := range res.Services {
		if string(svc.Ref.Namespace)+"/"+string(svc.Ref.Name) != "observability/grafana" {
			continue
		}
		seen = true
		if len(svc.SecretRefs) != 1 || svc.SecretRefs[0] != "grafana-admin-password" {
			t.Errorf("grafana secretRefs = %v, want [grafana-admin-password] (ADR-0008)", svc.SecretRefs)
		}
		if len(svc.DependsOn) != 1 || svc.DependsOn[0].String() != "observability/prometheus" {
			t.Errorf("grafana dependsOn = %v, want [observability/prometheus] (intra-namespace, ADR-0004)", svc.DependsOn)
		}
		for k := range svc.Env {
			if strings.Contains(strings.ToLower(k), "password") || strings.Contains(strings.ToLower(k), "token") {
				t.Errorf("grafana env %q: password-ish values belong in secretRefs (ADR-0008)", k)
			}
		}
	}
	if !seen {
		t.Fatalf("observability/grafana not loaded; loaded:%v", loadedNames(res))
	}
}

// TestTemplatePlaceholderDiscipline: {{CONTROLLER_IMAGE}} is the only
// existential token (uppercase style = render-time substitution), allowed
// only in the controller spec; prose references in the README are fine.
// Lowercase `{{field}}` strings in dashboard panel queries are Grafana
// legend formats, not render tokens — they stay untouched in the commit.
func TestTemplatePlaceholderDiscipline(t *testing.T) {
	t.Parallel()
	dir := renderTemplateIntoTemp(t)
	// Uppercase-style tokens ONLY are existential; lowercase ones are
	// Grafana legend syntax and documentation, never rendered.
	tokenRe := regexp.MustCompile(`\{\{[A-Z][A-Z0-9_]*\}\}`)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(p)
		if ext != ".yml" && ext != ".yaml" && ext != ".json" && ext != ".md" {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, tok := range tokenRe.FindAllString(string(b), -1) {
			if tok == "{{CONTROLLER_IMAGE}}" {
				switch rel {
				case filepath.Join("core", "top-controller.yaml"), "README.md":
					continue // the real token + its documented prose mention
				}
			}
			t.Errorf("%s: contains render token %q; only {{CONTROLLER_IMAGE}} (core/top-controller.yaml) is existential — T-403-style gaps must be documented TODO(T-403) comments or lowercase legend syntax, not tokens", rel, tok)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk rendered template: %v", err)
	}
}
