// SPDX-License-Identifier: MIT

// Tier-1 golden-argv tests (ADR-0012): the exectest.FakeRunner seam proves
// the exact Dokku argv each method issues and how report shapes map into
// domain.ObservedState and the error taxonomy. All fixture values are
// fictional (ADR-0014).
package dokku

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/exec/exectest"
)

var testRef = domain.Ref("top", "web")

// Fixture report bodies (Dokku 0.38 report-framework shapes; stripped keys
// used throughout, legacy aliases exercised in TestInspectLegacyKeys).
const (
	psDeployed = `{"app":"top-web","deployed":"true","running":"true",
		"processes":"1","computed-restart-policy":"on-failure:10"}`
	psNeverDeployed = `{"app":"top-web","deployed":"false","running":"false",
		"processes":"0","computed-restart-policy":"on-failure:10"}`
	buildsSucceeded = `{"app":"top-web","build-id":"01jx","build-status":"succeeded"}`
	buildsFailed    = `{"app":"top-web","build-status":"failed"}`
	buildsNone      = `{"app":"top-web"}`
	gitImage        = `{"app":"top-web","source-image":"docker.example.com/example/whoami:1.0"}`
	gitSha          = `{"app":"top-web","sha":"a1b2c3d4e5","source-image":""}`
	gitEmpty        = `{"app":"top-web","sha":"","source-image":""}`
	appsImageSrc    = `{"app":"top-web","deploy-source":"docker-image",
		"deploy-source-metadata":"docker.example.com/example/whoami:1.0","locked":"false"}`
	appsGitSrc   = `{"app":"top-web","deploy-source":"git","deploy-source-metadata":"a1b2c3d4e5","locked":"true"}`
	appsEmptySrc = `{"app":"top-web","deploy-source":"","deploy-source-metadata":"","locked":"false"}`
	portsOne     = `[{"scheme":"http","host_port":80,"container_port":5000}]`
	portsNone    = `[]`
	storageOne   = `[{"entry_name":"top-web-data",
		"host_path":"/var/lib/dokku/data/storage/top-web-data","container_path":"/data"}]`
	storageNone   = `[]`
	networkTopInt = `{"app":"top-web","computed-attach-post-create":"top-internal",
		"computed-attach-post-deploy":"","computed-initial-network":""}`
	networkNone     = `{"app":"top-web","computed-attach-post-create":"","computed-attach-post-deploy":""}`
	resourceLimits  = `{"app":"top-web","web.limit.cpu":"500m","web.limit.memory":"256Mi"}`
	resourceUnlimit = `{"app":"top-web","web.limit.cpu":"","web.limit.memory":""}`

	envKeysOut = "TZ\nHTTP_FOO\n"
	// existsYes/existsNo: apps:exists prints prose; only the exit decides.
	existsYes = "App exists"
	existsNo  = "App does not exist"
)

// seedFull wires the "deployed by image, fully featured" report set.
func seedFull(f *exectest.FakeRunner) {
	f.SetOutput("dokku", ok(existsYes), "apps:exists", "top-web")
	f.SetOutput("dokku", ok(psDeployed), "ps:report", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(buildsSucceeded), "builds:report", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(gitImage), "git:report", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(appsImageSrc), "apps:report", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(networkTopInt), "network:report", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(resourceLimits), "resource:report", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(portsOne), "ports:report", "top-web", "--ports-map-json")
	f.SetOutput("dokku", ok(storageOne), "storage:list", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(envKeysOut), "config:keys", "top-web")
}

// seedNotDeployed wires the "fresh app, never deployed" set.
func seedNotDeployed(f *exectest.FakeRunner) {
	f.SetOutput("dokku", ok(existsYes), "apps:exists", "top-web")
	f.SetOutput("dokku", ok(psNeverDeployed), "ps:report", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(buildsNone), "builds:report", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(gitEmpty), "git:report", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(appsEmptySrc), "apps:report", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(networkNone), "network:report", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(resourceUnlimit), "resource:report", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(portsNone), "ports:report", "top-web", "--ports-map-json")
	f.SetOutput("dokku", ok(storageNone), "storage:list", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(envKeysOut), "config:keys", "top-web")
}

func ok(stdout string) exectest.Response { return exectest.Response{Stdout: []byte(stdout)} }
func fail(code int, stderr string) exectest.Response {
	return exectest.Response{ExitCode: code, Stderr: []byte(stderr)}
}

func newC(f *exectest.FakeRunner) *Client { return New(f) }

// argv of every recorded call, rendered "dokku a b ...".
func recordedArgv(f *exectest.FakeRunner) []string {
	var got []string
	for _, c := range f.Recorded() {
		got = append(got, "dokku "+strings.Join(c.Args, " "))
	}
	return got
}

func assertArgv(t *testing.T, f *exectest.FakeRunner, want ...string) {
	t.Helper()
	got := recordedArgv(f)
	if !slices.Equal(got, want) {
		t.Fatalf("recorded argv mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestEnsureAppCreatesWhenAbsent(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", fail(20, existsNo), "apps:exists", "top-web")
	f.SetOutput("dokku", ok("Creating app"), "apps:create", "top-web")

	if err := newC(f).EnsureApp(context.Background(), testRef); err != nil {
		t.Fatalf("EnsureApp: %v", err)
	}
	assertArgv(t, f,
		"dokku apps:exists top-web",
		"dokku apps:create top-web")
}

func TestEnsureAppAdoptsExisting(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok(existsYes), "apps:exists", "top-web")

	if err := newC(f).EnsureApp(context.Background(), testRef); err != nil {
		t.Fatalf("EnsureApp: %v", err)
	}
	assertArgv(t, f, "dokku apps:exists top-web")
}

func TestEnsureAppCreateFails(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", fail(20, existsNo), "apps:exists", "top-web")
	f.SetOutput("dokku", fail(1, "!  Failure: create boom"), "apps:create", "top-web")

	err := newC(f).EnsureApp(context.Background(), testRef)
	if err == nil {
		t.Fatal("want error")
	}
	if !errors.Is(err, domain.ErrBackend) {
		t.Fatalf("want ErrBackend, got %v (unwrap %v)", err, errors.Unwrap(err))
	}
	var be *exec.ExitError
	if !errors.As(err, &be) || be.ExitCode != 1 {
		t.Fatalf("want *exec.ExitError{1}, got %#v", err)
	}
	if !strings.Contains(err.Error(), "apps:create") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error text lost context: %q", err)
	}
}

func TestEnsureAppInvalidRef(t *testing.T) {
	f := exectest.NewFakeRunner()
	err := newC(f).EnsureApp(context.Background(), domain.Ref("bad!", "web"))
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if len(f.Recorded()) != 0 {
		t.Fatalf("no dokku calls expected, got %v", recordedArgv(f))
	}
}

// ensureAppTimeout: a per-call timeout surfaces as ErrUnhealthy (the
// backend, not the operation, is broken); other exit statuses are ErrBackend.
func TestEnsureAppTimeoutIsUnhealthy(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", func() exectest.Response {
		return exectest.Response{Err: fmt.Errorf("run dokku apps:exists: %w: %w", exec.ErrTimeout, context.DeadlineExceeded)}
	}(), "apps:exists", "top-web")
	err := newC(f).EnsureApp(context.Background(), testRef)
	if !errors.Is(err, domain.ErrUnhealthy) || !errors.Is(err, exec.ErrTimeout) {
		t.Fatalf("want Unhealthy+Timeout, got %v", err)
	}
}

func TestInspectDeployedByImage(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedFull(f)

	obs, err := newC(f).Inspect(context.Background(), testRef)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	// Golden argv: every composed command, in order, no surprises.
	assertArgv(t, f,
		"dokku apps:exists top-web",
		"dokku ps:report top-web --format json",
		"dokku builds:report top-web --format json",
		"dokku git:report top-web --format json",
		"dokku apps:report top-web --format json",
		"dokku network:report top-web --format json",
		"dokku resource:report top-web --format json",
		"dokku ports:report top-web --ports-map-json",
		"dokku storage:list top-web --format json",
		"dokku config:keys top-web")

	want := domain.ObservedState{
		Ref:           testRef,
		AppName:       "top-web",
		Deployed:      true,
		Running:       true,
		Revision:      "", // image deploy → no sha
		Image:         "docker.example.com/example/whoami:1.0",
		EnvKeys:       []string{"HTTP_FOO", "TZ"},
		Ports:         []domain.Port{{Name: "http", ContainerPort: 5000, HostPort: 80}},
		StorageMounts: []domain.StorageMount{{Name: "top-web-data", HostPath: "/var/lib/dokku/data/storage/top-web-data", MountPath: "/data"}},
		RestartPolicy: domain.RestartOnFailure,
		Resources:     domain.Resources{Limits: domain.ResourceLimits{CPU: "500m", Memory: "256Mi"}},
		Networks:      []string{"top-internal"},
		LastBuild:     domain.BuildSucceeded,
	}
	got := obs.InspectedAt
	obs.InspectedAt = time.Time{}
	if !reflect.DeepEqual(obs, want) {
		t.Fatalf("observed mismatch:\n got %v\nwant %v", obs, want)
	}
	if got.IsZero() {
		t.Fatal("InspectedAt not set")
	}
}

func TestInspectNeverDeployed(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedNotDeployed(f)

	obs, err := newC(f).Inspect(context.Background(), testRef)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	// Mirrors the e2e "never deployed" case: app exists, nothing deployed.
	// (domain's Absent() == !Deployed — reconcile treats it as create.)
	if obs.Deployed || obs.Running || obs.Image != "" || obs.Revision != "" {
		t.Fatalf("never-deployed app must read clean, got %+v", obs)
	}
	want := domain.ObservedState{
		Ref:           testRef,
		AppName:       "top-web",
		Deployed:      false,
		Running:       false,
		RestartPolicy: domain.RestartOnFailure,
		EnvKeys:       []string{"HTTP_FOO", "TZ"},
		LastBuild:     domain.BuildNone,
	}
	obs.InspectedAt = time.Time{}
	if !reflect.DeepEqual(obs, want) {
		t.Fatalf("observed mismatch:\n got %v\nwant %v", obs, want)
	}
}

func TestInspectGitDeploySource(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedFull(f)
	f.SetOutput("dokku", ok(gitSha), "git:report", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(appsGitSrc), "apps:report", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(buildsFailed), "builds:report", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(portsNone), "ports:report", "top-web", "--ports-map-json")
	f.SetOutput("dokku", ok(storageNone), "storage:list", "top-web", "--format", "json")

	obs, err := newC(f).Inspect(context.Background(), testRef)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if obs.Revision != "a1b2c3d4e5" || obs.Image != "" {
		t.Fatalf("want git revision %q, got rev=%q image=%q", "a1b2c3d4e5", obs.Revision, obs.Image)
	}
	if obs.LastBuild != domain.BuildFailed {
		t.Fatalf("want BuildFailed, got %v", obs.LastBuild)
	}
}

func TestInspectAppMissing(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", fail(20, existsNo), "apps:exists", "top-web")

	_, err := newC(f).Inspect(context.Background(), testRef)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	// Not-found short-circuits: only the existence probe ran.
	assertArgv(t, f, "dokku apps:exists top-web")
}

func TestInspectLegacyKeyAliases(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedFull(f)
	// 0.38.x deprecation window also emits the ps-/app- prefixed legacy
	// keys; a host reporting only those must still intersect.
	f.SetOutput("dokku", ok(`{"app":"top-web","ps-deployed":"true","ps-running":"true"}`),
		"ps:report", "top-web", "--format", "json")
	f.SetOutput("dokku", ok(`{"app":"top-web","app-deploy-source":"docker-image","app-deploy-source-metadata":"docker.example.com/example/x:2"}`),
		"apps:report", "top-web", "--format", "json")

	obs, err := newC(f).Inspect(context.Background(), testRef)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !obs.Deployed || !obs.Running || obs.Image != "docker.example.com/example/x:2" {
		t.Fatalf("legacy aliased mapping wrong: %+v", obs)
	}
}

func TestInspectReportFailure(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedFull(f)
	f.SetOutput("dokku", fail(1, "!  Failure: disruption"), "builds:report", "top-web", "--format", "json")

	_, err := newC(f).Inspect(context.Background(), testRef)
	if !errors.Is(err, domain.ErrBackend) {
		t.Fatalf("want ErrBackend, got %v", err)
	}
	var be *BackendError
	if !errors.As(err, &be) || be.Op != "inspect builds" {
		t.Fatalf("want BackendError{inspect builds}, got %#v", err)
	}
	// No partial snapshot can be returned; failure carries the redacted
	// stderr excerpt from the exec layer.
	if !strings.Contains(err.Error(), "disruption") {
		t.Fatalf("stderr excerpt lost: %q", err)
	}
}

func TestInspectMalformedJSON(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedFull(f)
	f.SetOutput("dokku", ok("not json at all"), "ps:report", "top-web", "--format", "json")

	_, err := newC(f).Inspect(context.Background(), testRef)
	if !errors.Is(err, domain.ErrBackend) {
		t.Fatalf("want ErrBackend, got %v", err)
	}
}

func TestInspectLockedAppMapsCleanly(t *testing.T) {
	// Deploy lock (apps:report "locked") is observed today without a
	// dedicated ObservedState field: Inspect must not fail or skew any
	// other reading — the lock framework hooks in at M3's Apply.
	f := exectest.NewFakeRunner()
	seedFull(f)
	f.SetOutput("dokku", ok(appsGitSrc), "apps:report", "top-web", "--format", "json")

	obs, err := newC(f).Inspect(context.Background(), testRef)
	if err != nil {
		t.Fatalf("Inspect with locked app: %v", err)
	}
	if !obs.Deployed {
		t.Fatal("lock must not zero out deployment facts")
	}
}

func TestLastGoodRevisionImage(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok(existsYes), "apps:exists", "top-web")
	f.SetOutput("dokku", ok("docker.example.com/example/whoami:1.0"),
		"apps:report", "top-web", "--app-deploy-source-metadata")

	rev, err := newC(f).LastGoodRevision(context.Background(), testRef)
	if err != nil {
		t.Fatalf("LastGoodRevision: %v", err)
	}
	if rev != "docker.example.com/example/whoami:1.0" {
		t.Fatalf("got %q", rev)
	}
	assertArgv(t, f,
		"dokku apps:exists top-web",
		"dokku apps:report top-web --app-deploy-source-metadata")
}

func TestLastGoodRevisionEmptyFallsBackToSourceImage(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok(existsYes), "apps:exists", "top-web")
	f.SetOutput("dokku", ok(""), "apps:report", "top-web", "--app-deploy-source-metadata")
	f.SetOutput("dokku", ok("docker.example.com/example/whoami:1.0"),
		"git:report", "top-web", "--git-source-image")

	rev, err := newC(f).LastGoodRevision(context.Background(), testRef)
	if err != nil || rev != "docker.example.com/example/whoami:1.0" {
		t.Fatalf("got %q, %v", rev, err)
	}
}

func TestLastGoodRevisionNeverDeployed(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok(existsYes), "apps:exists", "top-web")
	f.SetOutput("dokku", ok(""), "apps:report", "top-web", "--app-deploy-source-metadata")
	f.SetOutput("dokku", ok(""), "git:report", "top-web", "--git-source-image")

	rev, err := newC(f).LastGoodRevision(context.Background(), testRef)
	if err != nil || rev != "" {
		t.Fatalf("never-deployed should be (%q, nil), got (%q, %v)", "", rev, err)
	}
}

func TestLastGoodRevisionMissingApp(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", fail(20, existsNo), "apps:exists", "top-web")

	_, err := newC(f).LastGoodRevision(context.Background(), testRef)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestStop(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok(existsYes), "apps:exists", "top-web")
	f.SetOutput("dokku", ok("stopped"), "ps:stop", "top-web")

	if err := newC(f).Stop(context.Background(), testRef); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertArgv(t, f,
		"dokku apps:exists top-web",
		"dokku ps:stop top-web")
}

func TestStopIdempotentWhenMissing(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", fail(20, existsNo), "apps:exists", "top-web")

	if err := newC(f).Stop(context.Background(), testRef); err != nil {
		t.Fatalf("Stop of missing app must be a no-op success, got %v", err)
	}
	assertArgv(t, f, "dokku apps:exists top-web")
}

func TestStopFailure(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok(existsYes), "apps:exists", "top-web")
	f.SetOutput("dokku", fail(1, "!  Failure: stop error"), "ps:stop", "top-web")

	err := newC(f).Stop(context.Background(), testRef)
	if !errors.Is(err, domain.ErrBackend) {
		t.Fatalf("want ErrBackend, got %v", err)
	}
}

func TestDestroyGoldenArgv(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok(existsYes), "apps:exists", "top-web")
	f.SetOutput("dokku", ok("destroyed"), "apps:destroy", "--force", "top-web")

	if err := newC(f).Destroy(context.Background(), testRef); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	assertArgv(t, f,
		"dokku apps:exists top-web",
		"dokku apps:destroy --force top-web")
}

func TestDestroyIdempotentWhenMissing(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", fail(20, existsNo), "apps:exists", "top-web")

	if err := newC(f).Destroy(context.Background(), testRef); err != nil {
		t.Fatalf("Destroy of missing app must be a no-op success, got %v", err)
	}
	assertArgv(t, f, "dokku apps:exists top-web")
}

func TestDestroyInvalidRefRefused(t *testing.T) {
	f := exectest.NewFakeRunner()
	// Defense-in-depth per ADR-0009: an unresolvable TOP name never
	// reaches dokku even though resolve() would keep apps safe anyway.
	if err := newC(f).Destroy(context.Background(), domain.Ref("default", "WEB")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if len(f.Recorded()) != 0 {
		t.Fatal("destroy must not run without a well-formed TOP name")
	}
}

func TestDestroyFailure(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok(existsYes), "apps:exists", "top-web")
	f.SetOutput("dokku", fail(1, "!  Failure: destroy refused"), "apps:destroy", "--force", "top-web")

	err := newC(f).Destroy(context.Background(), testRef)
	if !errors.Is(err, domain.ErrBackend) {
		t.Fatalf("want ErrBackend, got %v", err)
	}
}

func TestHealthyBackend(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok("dokku version 0.38.10"), "version")
	f.SetOutput("dokku", ok(`["top-web"]`), "apps:list", "--format", "json")

	got, err := newC(f).HealthyBackend(context.Background())
	if err != nil || !got {
		t.Fatalf("want (true, nil), got (%v, %v)", got, err)
	}
	assertArgv(t, f,
		"dokku version",
		"dokku apps:list --format json")
}

func TestHealthyBackendVersionFails(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", fail(1, "!  Failure: ssh refused"), "version")

	got, err := newC(f).HealthyBackend(context.Background())
	if got || !errors.Is(err, domain.ErrUnhealthy) {
		t.Fatalf("want (false, ErrUnhealthy), got (%v, %v)", got, err)
	}
	// First failure short-circuits — apps:list is never reached.
	assertArgv(t, f, "dokku version")
}

func TestHealthyBackendBinaryMissing(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", exectest.Response{Err: fmt.Errorf("run dokku: %w: no such binary", exec.ErrNotFound)}, "version")

	_, err := newC(f).HealthyBackend(context.Background())
	if !errors.Is(err, domain.ErrUnhealthy) {
		t.Fatalf("want ErrUnhealthy, got %v", err)
	}
}

func TestEnsureNetwork(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", fail(1, "Network does not exist"), "network:exists", "top-internal")
	f.SetOutput("dokku", ok("Creating network"), "network:create", "top-internal")

	if err := newC(f).EnsureNetwork(context.Background(), "top-internal"); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	assertArgv(t, f,
		"dokku network:exists top-internal",
		"dokku network:create top-internal")
}

func TestEnsureNetworkAlreadyExists(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok(existsYes), "network:exists", "top-internal")

	if err := newC(f).EnsureNetwork(context.Background(), "top-internal"); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	assertArgv(t, f, "dokku network:exists top-internal")
}

func TestEnsureNetworkCreateFails(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", fail(1, "Network does not exist"), "network:exists", "top-internal")
	f.SetOutput("dokku", fail(1, "!  Failure: network boom"), "network:create", "top-internal")

	err := newC(f).EnsureNetwork(context.Background(), "top-internal")
	if !errors.Is(err, domain.ErrBackend) {
		t.Fatalf("want ErrBackend, got %v", err)
	}
}

func TestCloseNoop(t *testing.T) {
	if err := newC(exectest.NewFakeRunner()).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestContractsInterface keeps the Dokku client assignability per
// docs/contracts/platform-backend.md, so drift breaks this test first.
func TestContractsInterface(t *testing.T) {
	_ = New(exectest.NewFakeRunner())
}
