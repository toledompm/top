package spec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/toledompm/top/internal/domain"
)

// writeSpec writes rel (e.g. "default/app.yaml") under dir, creating parents.
func writeSpec(t *testing.T, dir, rel, content string) string {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const digestFixture = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// baseDoc is a known-good single-file spec; table tests modify it. The
// source block is last so appended/edited lines stay in context.
const baseDoc = `apiVersion: top/v1alpha1
kind: Service
metadata:
  name: web
spec:
  ports:
    - { name: http, containerPort: 8080 }
  source:
    image: docker.io/example/web
    digest: ` + digestFixture + `
`

// expectLoad loads dir expecting exactly wantNum services.
func expectLoad(t *testing.T, dir string, wantNum int, opts ...Option) Result {
	t.Helper()
	res, err := LoadDir(dir, opts...)
	if err != nil {
		t.Fatalf("LoadDir: unexpected error: %v", err)
	}
	if len(res.Services) != wantNum {
		t.Fatalf("LoadDir: want %d services, got %d", wantNum, len(res.Services))
	}
	return res
}

func TestLoadValidTree(t *testing.T) {
	res, err := LoadDir(filepath.Join("testdata", "valid"), WithPlatformRepo(true))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	// Sorted: core tier first, then observability, then default.
	want := []string{"core/top-controller", "observability/prometheus", "default/grafana"}
	if got := fmt.Sprintf("%s|%s|%s", res.Services[0].Ref, res.Services[1].Ref, res.Services[2].Ref); got != strings.Join(want, "|") {
		t.Fatalf("order: got %q, want %v", got, want)
	}

	p := res.Services[1] // prometheus — the contract example
	if p.Source.Image != "docker.io/prom/prometheus" || p.Source.Digest != digestFixture ||
		p.Source.Tag != "" || p.Source.Build != nil {
		t.Errorf("prometheus source: %+v", p.Source)
	}
	if p.Source.Reference() != "docker.io/prom/prometheus@"+digestFixture {
		t.Errorf("prometheus reference: %q", p.Source.Reference())
	}
	if !p.Source.IsDigestPinned() {
		t.Error("prometheus source should be digest-pinned")
	}
	if len(p.Ports) != 1 || p.Ports[0].Name != "http" || p.Ports[0].ContainerPort != 9090 || p.Ports[0].HostPort != 0 {
		t.Errorf("prometheus ports: %+v", p.Ports)
	}
	if p.Routing.Mode != domain.RoutingInternal || len(p.Routing.Domains) != 0 {
		t.Errorf("prometheus routing: %+v", p.Routing)
	}
	if p.HealthCheck == nil || p.HealthCheck.HTTP == nil ||
		p.HealthCheck.HTTP.Path != "/-/healthy" || p.HealthCheck.HTTP.Port != "http" ||
		p.HealthCheck.IntervalSeconds != 30 {
		t.Errorf("prometheus healthcheck: %+v", p.HealthCheck)
	}
	if len(p.Storage) != 1 || p.Storage[0].Name != "data" || p.Storage[0].MountPath != "/prometheus" || p.Storage[0].Size != "2Gi" {
		t.Errorf("prometheus storage: %+v", p.Storage)
	}
	if p.Env["TZ"] != "UTC" || p.RestartPolicy != domain.RestartAlways {
		t.Errorf("prometheus env/restart: %v %q", p.Env, p.RestartPolicy)
	}
	if app, err := domain.AppName(p.Ref.Namespace, p.Ref.Name); err != nil || app != "observability-prometheus" {
		t.Errorf("app name: %q %v", app, err)
	}

	c := res.Services[0]
	if ann := c.Annotation("top/special"); ann != "bootstrap" {
		t.Errorf("core annotation: %q", ann)
	}
	if c.Rollout.MaxUnavailable != 0 {
		t.Errorf("core rollout: %+v", c.Rollout)
	}
	if c.Routing.Mode != domain.RoutingProxy || len(c.Routing.Domains) != 1 || c.Routing.Domains[0] != "top.example.com" {
		t.Errorf("core routing: %+v", c.Routing)
	}

	g := res.Services[2] // grafana: namespace defaults from dir, default applied
	if g.Ref.Namespace != "default" || string(g.RestartPolicy) != "always" {
		t.Errorf("grafana defaults: %+v", g)
	}
	if len(g.DependsOn) != 1 || !g.DependsOn[0].Equal(domain.Ref("default", "db")) {
		t.Errorf("grafana dependsOn: %+v", g.DependsOn)
	}
	found := false
	for _, svc := range res.Services {
		if svc.Ref.Equal(domain.Ref("default", "grafana")) {
			found = true
		}
	}
	if !found {
		t.Fatal("Refs() must include default/grafana")
	}
}

// TestUnknownFieldLine is the contract's "typos must fail loudly" example:
// `containerport` (wrong case) at a known line must surface as "<file>:<line>".
func TestUnknownFieldLine(t *testing.T) {
	dir := t.TempDir()
	doc := `apiVersion: top/v1alpha1
kind: Service
metadata:
  name: web
spec:
  source:
    image: docker.io/example/web
    digest: ` + digestFixture + `
  ports:
    - { name: http, containerport: 8080 }` // line 9 — a typo, strict parse must catch it
	writeSpec(t, dir, "default/web.yaml", doc)
	_, err := LoadDir(dir)
	if err == nil {
		t.Fatal("want error for unknown field 'containerport'")
	}
	msg := err.Error()
	for _, want := range []string{"field containerport not found", "web.yaml", ":10:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q: want substring %q", msg, want)
		}
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		wantSub string
	}{
		{
			name:    "unknown field at top",
			doc:     baseDoc + "  bogus: true\n",
			wantSub: "field bogus not found",
		},
		{
			name:    "unknown field in metadata",
			doc:     strings.Replace(baseDoc, "  name: web\n", "  name: web\n  bogus: 1\n", 1),
			wantSub: "field bogus not found",
		},
		{
			name:    "apiVersion wrong",
			doc:     strings.Replace(baseDoc, "apiVersion: top/v1alpha1", "apiVersion: v1", 1),
			wantSub: "apiVersion/kind must be top/v1alpha1/Service",
		},
		{
			name:    "kind wrong",
			doc:     strings.Replace(baseDoc, "kind: Service", "kind: Deployment", 1),
			wantSub: "apiVersion/kind must be top/v1alpha1/Service",
		},
		{
			name:    "missing apiVersion",
			doc:     strings.Replace(baseDoc, "apiVersion: top/v1alpha1\n", "", 1),
			wantSub: "<missing>",
		},
		{
			name:    "both image and build",
			doc:     baseDoc + "    build: { repo: github.com/example/web, ref: v1 }\n",
			wantSub: "exactly one of image or build",
		},
		{
			name:    "tag and digest both",
			doc:     strings.Replace(baseDoc, "    digest: "+digestFixture+"\n", "    digest: "+digestFixture+"\n    tag: v1\n", 1),
			wantSub: "exactly one of tag or digest",
		},
		{
			name:    "no source at all",
			doc:     strings.Replace(baseDoc, "    image: docker.io/example/web\n", "", 1),
			wantSub: "exactly one of image or build",
		},
		{
			name:    "build without ref",
			doc:     strings.Replace(baseDoc, "    image: docker.io/example/web\n", "", 1) + "    build: { repo: github.com/example/web }\n",
			wantSub: "source.build.ref: is required",
		},
		{
			name:    "build with tag/digest",
			doc:     strings.Replace(baseDoc, "    image: docker.io/example/web\n", "", 1) + "    build: { repo: github.com/example/web, ref: v1 }\n    tag: v2\n",
			wantSub: "only valid with an image source",
		},
		{
			name:    "bad digest format",
			doc:     strings.Replace(baseDoc, digestFixture, "sha256:abc", 1),
			wantSub: "source.digest: want",
		},
		{
			name:    "proxy without domains",
			doc:     baseDoc + "  routing:\n    mode: proxy\n",
			wantSub: "proxy: requires routing.domains",
		},
		{
			name:    "unknown routing mode",
			doc:     baseDoc + "  routing:\n    mode: kubernetes\n",
			wantSub: "want none, internal, or proxy",
		},
		{
			name:    "bad service name (uppercase)",
			doc:     strings.Replace(baseDoc, "name: web", "name: WebApp", 1),
			wantSub: "must match [a-z0-9]",
		},
		{
			name:    "bad service name (leading hyphen)",
			doc:     strings.Replace(baseDoc, "name: web", "name: -web", 1),
			wantSub: "must match [a-z0-9]",
		},
		{
			name:    "name exceeds 48-char app budget",
			doc:     strings.Replace(baseDoc, "name: web", "name: a-middle-paragraph-tuesday-extraordinary-longname", 1),
			wantSub: "is 49 chars, max 48",
		},
		{
			name:    "bad port range",
			doc:     strings.Replace(baseDoc, "containerPort: 8080", "containerPort: 99999", 1),
			wantSub: "out of range",
		},
		{
			name:    "bad port name",
			doc:     strings.Replace(baseDoc, "name: http, containerPort", "name: \"HTTP\", containerPort", 1),
			wantSub: "port name",
		},
		{
			name: "duplicate port names",
			doc: strings.Replace(baseDoc,
				"    - { name: http, containerPort: 8080 }",
				"    - { name: http, containerPort: 8080 }\n    - { name: http, containerPort: 8081 }", 1),
			wantSub: "duplicate port name",
		},
		{
			name:    "empty healthcheck",
			doc:     baseDoc + "  healthcheck: {}\n",
			wantSub: "healthcheck: empty; omit the key",
		},
		{
			name:    "healthcheck references undeclared port",
			doc:     baseDoc + "  healthcheck:\n    tcp: { port: 9999 }\n",
			wantSub: "does not match any declared spec.ports",
		},
		{
			name:    "healthcheck http missing path",
			doc:     baseDoc + "  healthcheck:\n    http: { port: 8080 }\n",
			wantSub: "http.path: is required",
		},
		{
			name:    "relative storage mountPath",
			doc:     baseDoc + "  storage:\n    - { name: data, mountPath: prometheus-data, size: 2Gi }\n",
			wantSub: "must be an absolute path",
		},
		{
			name:    "bad storage size",
			doc:     baseDoc + "  storage:\n    - { name: data, mountPath: /data, size: two }\n",
			wantSub: "size",
		},
		{
			name:    "bad env key",
			doc:     baseDoc + "  env: { \"bad-key\": v }\n",
			wantSub: "env key",
		},
		{
			name:    "bad secretRef name",
			doc:     baseDoc + "  secretRefs: [ \"UPPER\" ]\n",
			wantSub: "secretRefs entry \"UPPER\"",
		},
		{
			name:    "dependsOn with too many slashes",
			doc:     baseDoc + "  dependsOn: [ \"a/b/c\" ]\n",
			wantSub: "too many slashes",
		},
		{
			name:    "cross-namespace dependsOn rejected",
			doc:     baseDoc + "  dependsOn: [ \"other/api\" ]\n",
			wantSub: "cross-namespace",
		},
		{
			name:    "negative maxUnavailable",
			doc:     baseDoc + "  rollout: { maxUnavailable: -1 }\n",
			wantSub: ">= 0",
		},
		{
			name:    "unknown restart policy",
			doc:     baseDoc + "  restartPolicy: never\n",
			wantSub: "want always, on-failure, or no",
		},
		{
			name:    "bad cpu quantity",
			doc:     baseDoc + "  resources:\n    limits: { cpu: \"0.5\" }\n",
			wantSub: "resources.limits.cpu",
		},
		{
			name:    "bad memory quantity",
			doc:     baseDoc + "  resources:\n    limits: { memory: \"2GIB\" }\n",
			wantSub: "resources.limits.memory",
		},
		{
			name:    "bad namespace in metadata",
			doc:     strings.Replace(baseDoc, "name: web\n", "name: web\n  namespace: \"InvariantNamespace\"\n", 1),
			wantSub: "namespace",
		},
		{
			name:    "root-level file without namespace",
			doc:     baseDoc,
			wantSub: "root-level file requires metadata.namespace",
		},
		{
			name:    "nested file namespace mismatch",
			doc:     strings.Replace(baseDoc, "name: web\n", "name: web\n  namespace: other\n", 1),
			wantSub: "does not match file layout",
		},
		{
			name:    "reserved namespace without platform repo",
			doc:     strings.Replace(baseDoc, "name: web\n", "name: web\n  namespace: core\n", 1),
			wantSub: "reserved for the platform repo",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Reserved-namespace / namespace-location cases belong in their
			// matching directories so the only failing rule is the one under
			// test.
			rel := "default/web.yaml"
			switch tc.name {
			case "reserved namespace without platform repo":
				rel = "core/web.yaml"
			case "nested file namespace mismatch":
				rel = "tenant/web.yaml"
			case "root-level file without namespace":
				rel = "web.yaml"
			}
			writeSpec(t, dir, rel, tc.doc)
			_, err := LoadDir(dir)
			if err == nil {
				t.Fatalf("want error containing %q, got none", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q: want substring %q", err.Error(), tc.wantSub)
			}
			// Every failure must name its file.
			if !strings.Contains(err.Error(), "web.yaml") {
				t.Errorf("error %q: must carry the file path", err.Error())
			}
		})
	}
}

func TestErrorsAggregateAndUnwrapToInvalid(t *testing.T) {
	dir := t.TempDir()
	writeSpec(t, dir, "default/a.yaml", strings.Replace(baseDoc, "name: web", "name: A", 1))
	writeSpec(t, dir, "default/b.yaml", strings.Replace(baseDoc, "name: web", "name: B", 1))
	_, err := LoadDir(dir)
	if err == nil {
		t.Fatal("want errors")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("want *ParseError, got %T: %v", err, pe)
	}
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("error %v: should unwrap to domain.ErrInvalid", err)
	}
	// Names of both failing files appear.
	for _, want := range []string{"a.yaml", "b.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregated error must mention %q: %v", want, err)
		}
	}
}

func TestWarnings(t *testing.T) {
	dir := t.TempDir()
	// Unpinned tag: warning, not error; latest: distinct warning; implicit
	// latest (no tag/digest): warned too; branch build ref: warned.
	for _, c := range []struct{ file, name string }{
		{"unpinned", "unpinned"}, {"latest", "latest"}, {"implicit", "implicit"}, {"build", "builder"},
	} {
		doc := strings.Replace(baseDoc, "name: web", "name: "+c.name, 1)
		write := doc
		switch c.file {
		case "unpinned":
			write = strings.Replace(doc, "    digest: "+digestFixture+"\n", "    tag: v3.7.3\n", 1)
		case "latest":
			write = strings.Replace(doc, "    digest: "+digestFixture+"\n", "    tag: latest\n", 1)
		case "implicit":
			write = strings.Replace(doc, "    digest: "+digestFixture+"\n", "", 1)
		case "build":
			write = strings.Replace(doc,
				"    image: docker.io/example/web\n    digest: "+digestFixture+"\n",
				"    build: { repo: github.com/example/web, ref: main }\n", 1)
		}
		writeSpec(t, dir, fmt.Sprintf("default/%s.yaml", c.file), write)
	}

	res, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("mutable refs are warnings, not errors: %v", err)
	}
	if len(res.Services) != 4 {
		t.Fatalf("want 4 services, got %d", len(res.Services))
	}
	codes := map[string][]WarningCode{}
	for _, w := range res.Warnings {
		codes[w.Field] = append(codes[w.Field], w.Code)
	}
	pathCode := map[string]WarningCode{}
	for _, w := range res.Warnings {
		pathCode[filepath.Base(w.Path)] = w.Code
	}
	if pathCode["unpinned.yaml"] != WarnMutableTag {
		t.Errorf("unpinned: %v", pathCode)
	}
	if pathCode["latest.yaml"] != WarnLatestTag {
		t.Errorf("latest: %v", pathCode)
	}
	if pathCode["implicit.yaml"] != WarnMutableTag {
		t.Errorf("implicit-latest: %v", pathCode)
	}
	if pathCode["build.yaml"] != WarnMutableRef {
		t.Errorf("build branch ref: %v", pathCode)
	}
	// Every warning names its file and field.
	for _, w := range res.Warnings {
		if w.Path == "" || w.Field == "" || w.Code == "" {
			t.Errorf("incomplete warning: %+v", w)
		}
	}
	// digest-pinned base still parses with no warning (control).
	res2 := expectLoad(t, dirWithPinned(t), 1)
	if len(res2.Warnings) != 0 {
		t.Errorf("digest-pinned: unexpected warnings %v", res2.Warnings)
	}
}

func dirWithPinned(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeSpec(t, dir, "default/pinned.yaml", strings.Replace(baseDoc, "name: web", "name: pinned", 1))
	return dir
}

func TestReservedNamespacePlatformRepo(t *testing.T) {
	dir := t.TempDir()
	writeSpec(t, dir, "core/web.yaml", strings.Replace(baseDoc, "name: web", "name: top-controller", 1))
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("tenant repo claiming core: want reserved-namespace error, got %v", err)
	}
	expectLoad(t, dir, 1, WithPlatformRepo(true)) // platform repo is allowed

	writeSpec(t, dir, "observability/metrics-agent.yaml", strings.Replace(baseDoc, "name: web", "name: metrics-agent", 1))
	_, err = LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "observability") {
		t.Fatalf("observability is also reserved: %v", err)
	}
	expectLoad(t, dir, 2, WithPlatformRepo(true))
}

func TestDuplicateIdentity(t *testing.T) {
	dir := t.TempDir()
	writeSpec(t, dir, "default/web.yaml", baseDoc)
	writeSpec(t, dir, "default/other.yaml",
		strings.Replace(baseDoc, "name: web", "name: web", 1)) // same metadata.name, different file
	_, err := LoadDir(dir)
	if err == nil {
		t.Fatal("want duplicate-identity error")
	}
	if !strings.Contains(err.Error(), "duplicate service") {
		t.Errorf("error %q: want duplicate-identity wording", err.Error())
	}
}

func TestMultiDocumentRejected(t *testing.T) {
	dir := t.TempDir()
	second := strings.Replace(baseDoc, "name: web", "name: second", 1)
	writeSpec(t, dir, "default/web.yaml", baseDoc+"---\n"+second)
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "one spec per file") {
		t.Fatalf("multi-doc: want rejection, got %v", err)
	}
}

func TestEmptyAndIgnoredFiles(t *testing.T) {
	dir := t.TempDir()
	writeSpec(t, dir, "default/empty.yaml", "# just a comment\n")
	writeSpec(t, dir, "default/.hidden.yaml", baseDoc)
	writeSpec(t, dir, "notes.txt", baseDoc)
	res, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(res.Services) != 0 {
		t.Errorf("want no services (comments/hidden/non-yaml skipped), got %d", len(res.Services))
	}
}

func TestSymlinkedSpecSkipped(t *testing.T) {
	dir := t.TempDir()
	real := writeSpec(t, dir, "default/web.yaml", baseDoc)
	if err := os.Symlink(real, filepath.Join(dir, "default", "link.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	res, err := LoadDir(dir) // symlink: identity collision would error if followed
	if err != nil {
		t.Fatalf("symlink must be skipped, got: %v", err)
	}
	if len(res.Services) != 1 {
		t.Errorf("want 1 service (the real file), got %d", len(res.Services))
	}
}

func TestMissingDir(t *testing.T) {
	_, err := LoadDir(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("want error for missing dir")
	}
}

func TestDefaultsApplied(t *testing.T) {
	dir := t.TempDir()
	doc := `apiVersion: top/v1alpha1
kind: Service
metadata:
  name: minimal
spec:
  source:
    image: docker.io/example/minimal
    digest: ` + digestFixture + `
`
	writeSpec(t, dir, "nsx/minimal.yaml", doc)
	res := expectLoad(t, dir, 1)
	svc := res.Services[0]
	if svc.Routing.Mode != domain.RoutingNone {
		t.Errorf("routing default: %+v", svc.Routing)
	}
	if svc.RestartPolicy != domain.RestartAlways {
		t.Errorf("restartPolicy default: %+v", svc.RestartPolicy)
	}
	if svc.Rollout.MaxUnavailable != 0 {
		t.Errorf("rollout default: %+v", svc.Rollout)
	}
	if svc.Env == nil || len(svc.Env) != 0 {
		t.Errorf("env default: %v", svc.Env)
	}
	if svc.HealthCheck != nil {
		t.Errorf("healthcheck default: %+v", svc.HealthCheck)
	}
}

func TestRootLevelFileWithNamespace(t *testing.T) {
	dir := t.TempDir()
	writeSpec(t, dir, "web.yaml",
		strings.Replace(baseDoc, "name: web\n", "name: web\n  namespace: apps\n", 1))
	res := expectLoad(t, dir, 1)
	if got := res.Services[0].Ref.Namespace; got != "apps" {
		t.Errorf("root-level namespace: got %q", got)
	}
}

func TestBuildServiceResolution(t *testing.T) {
	dir := t.TempDir()
	writeSpec(t, dir, "apps/builder.yaml", strings.Replace(baseDoc,
		"    image: docker.io/example/web\n    digest: "+digestFixture+"\n",
		"    build: { repo: github.com/example/web, ref: 5f2c0b1, path: ./sub }\n", 1))
	res := expectLoad(t, dir, 1)
	s := res.Services[0]
	if s.Source.Image != "" || s.Source.Build == nil || s.Source.Build.Repo != "github.com/example/web" ||
		s.Source.Build.Ref != "5f2c0b1" || s.Source.Build.Path != "./sub" {
		t.Errorf("build source: %+v", s.Source)
	}
	if s.Source.Reference() != "" { // build sources have no image reference
		t.Errorf("build Reference: %q", s.Source.Reference())
	}
}
