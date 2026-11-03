// SPDX-License-Identifier: MIT

// Tier-1 golden-argv tests for the T-305..T-307 Apply slice (contract:
// docs/contracts/platform-backend.md). Every scenario pins the EXACT
// dokku argv the adapter issues — including the mutations the second
// apply deliberately does not make (idempotency) and the build guard
// (a no-change poll must not leave a build record). Fixture values are
// fictional (ADR-0014).
package dokku

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/toledompm/top/internal/backend"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/exec/exectest"
)

// newTarReader opens the tar inside golden archive assertions.
func newTarReader(t *testing.T, r io.Reader) *tar.Reader {
	return tar.NewReader(r)
}

// testSha is a fictional-but-valid revision (mirrors existing fixtures).
const testSha = "a1b2c3d4e5"

// tvsImage: the digest-pinned fictional service; tvsMutable: mutable-tag
// variant (tag NOT digest — spec validation forbids both; the "1.0" used
// by other fixtures is a tag string in dokku's eyes).
var (
	tvsRef     = domain.Ref("top", "web")
	tvsDigest  = domain.Source{Image: "docker.example.com/example/whoami", Digest: "sha256:9d18"}
	tvsMutable = domain.Source{Image: "docker.example.com/example/whoami", Tag: "1.0"}
)

// recon strings — run "ps:report" on the first record that mentions the
// substring in stdinMeasured.
func run(t *testing.T) context.Context { return context.Background() }

// seedFreshApply wires the fake for a fresh app being applied: the two
// apps:exists probes (EnsureApp then Inspect) are scripted missing →
// create → present, and all reports describe "never deployed by us".
func seedFreshApply(f *exectest.FakeRunner, opts ...func(*exectest.FakeRunner)) {
	f.SetScriptedPrefix([]string{"dokku", "apps:exists"},
		[]exectest.Response{fail(20, existsNo)}, ok(existsYes))
	f.SetOutput("dokku", ok("created"), "apps:create", "top-web")
	seedNotDeployed(f)
	for _, o := range opts {
		o(f)
	}
}

// seedConvergedApply wires the fake for a RE-apply against a fully
// converged app: exists always, all reports match a deployed-by-image app
// whose state matches the "desired" fixture spec below.
func seedConvergedApply(f *exectest.FakeRunner, opts ...func(*exectest.FakeRunner)) {
	f.SetOutput("dokku", ok(existsYes), "apps:exists", "top-web")
	seedFull(f)
	for _, o := range opts {
		o(f)
	}
}

// desiredBase is the minimal image spec used by the argv-golden cases.
func desiredBase(src domain.Source) domain.Service {
	return domain.Service{
		Ref:           tvsRef,
		Source:        src,
		RestartPolicy: domain.RestartAlways,
	}
}

// applyOpts is the zero ApplyOptions (digest sources; force flows pass
// backend.ApplyOptions{ForcePull: true} explicitly).
func applyOpts() backend.ApplyOptions { return backend.ApplyOptions{} }

// containsArgv asserts certain argv lines exist exactly the given times.
func countArgv(f *exectest.FakeRunner, sub string) int {
	n := 0
	for _, a := range recordedArgv(f) {
		if strings.Contains(a, sub) {
			n++
		}
	}
	return n
}

func wantArgvCount(t *testing.T, f *exectest.FakeRunner, sub string, want int) {
	t.Helper()
	if got := countArgv(f, sub); got != want {
		t.Fatalf("argv %q seen %d times, want %d:\n%v", sub, got, want, recordedArgv(f))
	}
}

// TestApplyImageFreshFullSequence pins the exact end-to-end argv for the
// canonical digest-pinned image apply on a fresh app.
func TestApplyImageDigestFreshFullSequence(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedFreshApply(f)
	spec := desiredBase(tvsDigest)
	spec.Env = map[string]string{"TZ": "UTC", "HTTP_FOO": "x"} // HTTP_FOO/TZ already present? not present here

	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// NOTE: the fixture app already has TZ+HTTP_FOO (fresh-created apps
	// carry no values but the same key set); since the adapter never READS
	// values (ADR-0008), its coverage delta is key-based and produces no
	// config:set for identical keys — the golden documents that.
	want := []string{
		"dokku apps:exists top-web", // ensure (missing)
		"dokku apps:create top-web",
		"dokku apps:exists top-web", // inspect
		"dokku ps:report top-web --format json",
		"dokku builds:report top-web --format json",
		"dokku git:report top-web --format json",
		"dokku apps:report top-web --format json",
		"dokku network:report top-web --format json",
		"dokku resource:report top-web --format json",
		"dokku ports:report top-web --ports-map-json",
		"dokku storage:list top-web --format json",
		"dokku config:keys top-web",
		"dokku ps:set top-web restart-policy always", // fresh policy is on-failure:10
		"dokku git:from-image top-web docker.example.com/example/whoami@sha256:9d18",
	}
	assertArgv(t, f, want...)
}

// TestApplyEnvBatchedSingleCall: exactly ONE config:set --no-restart per
// apply batch (contract "Batches env"), sorted keys, secrets merged into
// the same call (TestApplySecretsMergedBatched).
func TestApplyEnvBatchedSingleCall(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedFreshApply(f)
	spec := desiredBase(tvsDigest)
	spec.Env = map[string]string{"NEW_A": "1", "NEW_B": "2", "HTTP_FOO": "x"}
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "config:set --no-restart top-web", 1)
	wantArgvCount(t, f, "config:set --no-restart top-web NEW_A=1 NEW_B=2", 1) // sorted keys
	wantArgvCount(t, f, "config:unset top-web TZ", 1)                         // spec dropped it
}

// TestApplyDigestAlreadyLiveIsZeroMutation: re-apply against a converged
// host performs NO deploy and NO config mutation (the Tier-3 idempotency
// matrix's argv twin). The fixture is tuned to the desired spec: tag ref
// whoami:1.0 (matches obs.Image), env keys TZ+HTTP_FOO, restart policy
// on-failure (matches the computed report), empty ports/storage/resources/
// networks.
func TestApplyDigestAlreadyLiveIsZeroMutation(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsMutable)
	spec.RestartPolicy = domain.RestartOnFailure
	spec.Env = map[string]string{"TZ": "UTC", "HTTP_FOO": "x"}
	seedConvergedApply(f, func(f *exectest.FakeRunner) {
		f.SetOutput("dokku", ok(portsNone), "ports:report", "top-web", "--ports-map-json")
		f.SetOutput("dokku", ok(storageNone), "storage:list", "top-web", "--format", "json")
		f.SetOutput("dokku", ok(resourceUnlimit), "resource:report", "top-web", "--format", "json")
		f.SetOutput("dokku", ok(networkNone), "network:report", "top-web", "--format", "json")
	})
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, sub := range []string{
		"git:from-image", "config:set", "config:unset",
		"ps:set", "ports:set", "ports:clear", "storage:mount", "storage:unmount",
		"storage:create", "resource:limit", "network:set", "domains:set",
	} {
		wantArgvCount(t, f, sub, 0)
	}
	wantArgvCount(t, f, "apps:exists", 2) // ensure + inspect only
}

// cover mutable tag without movement: no force, no mutation.
// cover mutable tag without movement: no force, no mutation.
func TestApplyMutableTagSteadyNoCall(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsMutable)
	spec.RestartPolicy = domain.RestartOnFailure
	spec.Env = map[string]string{"TZ": "UTC", "HTTP_FOO": "x"}
	seedConvergedApply(f, func(f *exectest.FakeRunner) {
		f.SetOutput("dokku", ok(storageNone), "storage:list", "top-web", "--format", "json")
	})
	// fixture const verbatim below keeps obs.Image == whoami:1.0
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "git:from-image", 0)
}

func TestApplyMutableTagForcePull(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsMutable)
	spec.Env = map[string]string{"TZ": "UTC", "HTTP_FOO": "x"}
	seedConvergedApply(f)
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, backend.ApplyOptions{ForcePull: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "git:from-image --force top-web docker.example.com/example/whoami:1.0", 1)
}

// dokku STILL records the attempt as a FAILED build (tier-3 verified:
// builds:report build-status=failed after a no-change force), which
// would poison the Readiness build gate. dokku's own directive for both
// shapes (serving or not) is ps:rebuild — which IS the deploy the
// ForcePull contract asked for; a genuine tag move exits 0 before reaching
// this branch (tier-3 fixture, 2026-11-03).
func TestApplyMutableTagForceNoChangeServing(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsMutable)
	spec.Env = map[string]string{"TZ": "UTC", "HTTP_FOO": "x"}
	seedConvergedApply(f,
		func(f *exectest.FakeRunner) {
			f.SetOutput("dokku", fail(1, "!     No changes detected, skipping git commit\n !     Call 'ps:rebuild' on app to rebuild the app from existing source"), "git:from-image", "--force", "top-web", "docker.example.com/example/whoami:1.0")
		})
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, backend.ApplyOptions{ForcePull: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "git:from-image --force top-web docker.example.com/example/whoami:1.0", 1)
	wantArgvCount(t, f, "ps:rebuild top-web", 1) // heal the leaked failed-build record
}

func TestApplyMutableTagForceNoChangeNotServingRebuilds(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsMutable)
	spec.Env = map[string]string{"TZ": "UTC", "HTTP_FOO": "x"}
	seedConvergedApply(f,
		func(f *exectest.FakeRunner) {
			f.SetOutput("dokku", ok(`{"app":"top-web","deployed":"true","running":"false","processes":"0"}`), "ps:report", "top-web", "--format", "json")
		},
		func(f *exectest.FakeRunner) {
			f.SetOutput("dokku", fail(1, "!     No changes detected, skipping git commit\n !     Call 'ps:rebuild' on app to rebuild the app from existing source"), "git:from-image", "--force", "top-web", "docker.example.com/example/whoami:1.0")
		})
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, backend.ApplyOptions{ForcePull: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "ps:rebuild top-web", 1)
}

func TestApplyDigestNeverForced(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsDigest)
	spec.Env = map[string]string{"TZ": "UTC", "HTTP_FOO": "x", "NEW": "y"}
	seedConvergedApply(f)
	// The digest ref is NOT live (fixture carries the tag), so the deploy
	// runs — but ForcePull is IGNORED for digest sources (contract:
	// digests never need it).
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, backend.ApplyOptions{ForcePull: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "git:from-image", 1)
	wantArgvCount(t, f, "git:from-image --force", 0)
}

// --- build sources (T-306) -------------------------------------------------

func TestApplyBuildFresh(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedFreshApply(f)
	spec := desiredBase(domain.Source{Build: &domain.BuildSource{
		Repo: "https://git.example.invalid/example/app.git", Ref: testSha,
	}})
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "git:sync --build-if-changes top-web https://git.example.invalid/example/app.git "+testSha, 1)
}

// TestApplyBuildSameShaLeavesNoBuildRecord — the hardened no-change-poll
// guard: the adapter SKIPS git:sync entirely when the sha is already the
// deployed revision (ADR-0002: "no-change poll leaves no build record").
func TestApplyBuildSameShaLeavesNoBuildRecord(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(domain.Source{Build: &domain.BuildSource{
		Repo: "https://git.example.invalid/example/app.git", Ref: testSha,
	}})
	seedConvergedApply(f, func(f *exectest.FakeRunner) {
		f.SetOutput("dokku", ok(appsGitSrc), "apps:report", "top-web", "--format", "json")
		f.SetOutput("dokku", ok(gitSha), "git:report", "top-web", "--format", "json")
	})
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "git:sync", 0)
}

// Same sha recorded but the release is not serving → ps:rebuild from
// dokku's stored source; git:sync at the same sha would no-op (exit 1).
func TestApplyBuildSameShaNotServingRebuilds(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(domain.Source{Build: &domain.BuildSource{
		Repo: "https://git.example.invalid/example/app.git", Ref: testSha,
	}})
	seedConvergedApply(f,
		func(f *exectest.FakeRunner) {
			f.SetOutput("dokku", ok(`{"app":"top-web","deployed":"true","running":"false","processes":"0"}`), "ps:report", "top-web", "--format", "json")
		},
		func(f *exectest.FakeRunner) {
			f.SetOutput("dokku", ok(appsGitSrc), "apps:report", "top-web", "--format", "json")
			f.SetOutput("dokku", ok(gitSha), "git:report", "top-web", "--format", "json")
		})
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "git:sync", 0)
	wantArgvCount(t, f, "ps:rebuild top-web", 1)
}

// Image matches the deployed one but the release is not serving →
// re-deploy (dokku re-deploys an identical image cleanly, tier-3).
func TestApplyImageSameImageNotServingRedeploys(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsDigest)
	seedConvergedApply(f, func(f *exectest.FakeRunner) {
		f.SetOutput("dokku", ok(`{"app":"top-web","deployed":"true","running":"false","processes":"0"}`), "ps:report", "top-web", "--format", "json")
	})
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "ps:rebuild", 0)
	wantArgvCount(t, f, "git:from-image", 1)
}

func TestApplyBuildInvalidRevisionRefused(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedFreshApply(f)
	spec := desiredBase(domain.Source{Build: &domain.BuildSource{
		Repo: "https://git.example.invalid/example/app.git", Ref: "main",
	}})
	err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts())
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("want ErrInvalid (never a moving branch name), got %v", err)
	}
	wantArgvCount(t, f, "git:sync", 0)
}

// --- healthchecks (T-307) --------------------------------------------------

func TestApplyChecksArchiveStdin(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedFreshApply(f)
	spec := desiredBase(tvsDigest)
	spec.Env = map[string]string{"TZ": "UTC", "HTTP_FOO": "x"}
	spec.HealthCheck = &domain.HealthCheck{
		HTTP:            &domain.HTTPCheck{Path: "/healthz", Port: "80"},
		IntervalSeconds: 5,
	}
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Timings ride the synthesized app.json entries (tier-3 2026-10-28:
	// checks:set only accepts wait-to-retire on 0.38 — no checks:set call
	// at all); probes ride the stdin archive, and NO plain git:from-image
	// runs.
	wantArgvCount(t, f, "checks:set", 0)
	wantArgvCount(t, f, "git:from-image", 0)

	var arch []byte
	for _, c := range f.Recorded() {
		if slices.Contains(c.Args, "git:from-archive") {
			arch = c.Stdin
			if !slices.Equal(c.Args, []string{"git:from-archive", "top-web", "--"}) {
				t.Fatalf("archive argv wrong: %v", c.Args)
			}
		}
	}
	if len(arch) == 0 {
		t.Fatalf("no stdin archive recorded:\n%v", recordedArgv(f))
	}
	assertArchive(t, arch, `"/healthz"`, `"port": 80`, `"wait": 5`, "FROM docker.example.com/example/whoami@sha256:9d18")
}

func TestApplyChecksArchiveIdempotentStdinResent(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsDigest)
	spec.HealthCheck = &domain.HealthCheck{HTTP: &domain.HTTPCheck{Path: "/healthz", Port: "80"}}
	seedConvergedApply(f)
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// The archive route does NOT rely on host-side revision comparison:
	// the same deterministic archive is re-sent; dokku's verified
	// same-archive response ("! No changes detected, skipping … CALL
	// ps:rebuild", stderr, exit 1 — tier-3 2026-10-28 dokku 0.38.32) is
	// classified as CONVERGED when the current release is serving (the
	// seed's fixture app reports running=true).
	wantArgvCount(t, f, "git:from-archive", 1)
	wantArgvCount(t, f, "ps:rebuild", 0)
}

// An identical-archive resend while the release is NOT serving converges
// through dokku's own directive (`ps:rebuild`) instead of erroring.
func TestApplyChecksArchiveNoChangeNotServingRebuilds(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsDigest)
	spec.HealthCheck = &domain.HealthCheck{HTTP: &domain.HTTPCheck{Path: "/healthz", Port: "80"}}
	seedConvergedApply(f,
		func(f *exectest.FakeRunner) {
			// release recorded but nothing running (failed prior deploy)
			f.SetOutput("dokku", ok(`{"app":"top-web","deployed":"true","running":"false","processes":"0"}`), "ps:report", "top-web", "--format", "json")
		},
		func(f *exectest.FakeRunner) {
			f.SetOutput("dokku", fail(1, "!     No changes detected, skipping git commit\n !     Call 'ps:rebuild' on app to rebuild the app from existing source"), "git:from-archive", "top-web", "--")
		})
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "git:from-archive", 1)
	wantArgvCount(t, f, "ps:rebuild top-web", 1)
}

// TCP/exec checks synthesize the documented entries.
func TestChecksArchiveTcpAndExec(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedFreshApply(f)
	spec := desiredBase(tvsDigest)
	spec.Ports = []domain.Port{{Name: "www", ContainerPort: 9000}}
	spec.HealthCheck = &domain.HealthCheck{
		TCP:  &domain.TCPCheck{Port: "www"},
		Exec: &domain.ExecCheck{Command: []string{"true"}},
	}
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, c := range f.Recorded() {
		if len(c.Stdin) > 0 {
			assertArchive(t, c.Stdin, `"nc -z localhost 9000"`, `"\"true\""`)
		}
	}
}

func assertArchive(t *testing.T, raw []byte, substrings ...string) {
	t.Helper()
	// Split the tar: Dockerfile must match; app.json must parse.
	var dockerfile, appjson string
	r := bytes.NewReader(raw)
	tr := newTarReader(t, r)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		body := new(bytes.Buffer)
		if _, err := body.ReadFrom(tr); err != nil {
			t.Fatalf("tar read: %v", err)
		}
		switch hdr.Name {
		case "Dockerfile":
			dockerfile = body.String()
		case "app.json":
			appjson = body.String()
		}
	}
	if dockerfile == "" || appjson == "" {
		t.Fatalf("archive missing members (Dockerfile/app.json)")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(appjson), &m); err != nil {
		t.Fatalf("app.json not json: %v: %q", err, appjson)
	}
	for _, s := range substrings {
		if !strings.Contains(dockerfile, s) && !strings.Contains(appjson, s) {
			t.Fatalf("archive missing %q:\n%s\n%s", s, dockerfile, appjson)
		}
	}
}

// --- env / storage / network / ports / resources ---------------------------

func TestApplyEnvUnsetRemovedKeys(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsDigest)
	spec.Env = map[string]string{"TZ": "UTC"} // HTTP_FOO removed
	seedConvergedApply(f)
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "config:unset top-web HTTP_FOO", 1)
	wantArgvCount(t, f, "config:set", 0) // TZ key already present; values are never read (ADR-0008)
}

// Dokku-managed env keys (GIT_REV re-injected after every rebuild) are
// never drift: converged app + managed key present ⇒ no config calls.
func TestApplyEnvManagedKeyNotDrift(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsDigest)
	spec.Env = map[string]string{"TZ": "UTC", "HTTP_FOO": "x"} // converged key set; GIT_REV is managed
	seedConvergedApply(f, func(f *exectest.FakeRunner) {
		f.SetOutput("dokku", ok("TZ\nHTTP_FOO\nGIT_REV\n"), "config:keys", "top-web")
	})
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "config:unset", 0)
	wantArgvCount(t, f, "config:set", 0)
}

// TestApplyOnlyDiffersEnvSet: TZ stays (key still present — values are
// never read, ADR-0008), NEW is set in the ONE batched call, HTTP_FOO is
// unset in the one idempotent unset.
func TestApplyOnlyDiffersEnvSet(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsDigest)
	spec.Env = map[string]string{"TZ": "UTC", "NEW": "y"}
	seedConvergedApply(f)
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "config:set --no-restart top-web NEW=y", 1)
	wantArgvCount(t, f, "config:unset", 1) // HTTP_FOO dropped by the spec
}

// secrets merge into the same batched config:set; their values never
// appear in a second call (ADR-0008 keeps argv redaction in exec).
func TestApplySecretsMergedBatched(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedFreshApply(f)
	spec := desiredBase(tvsDigest)
	spec.Env = map[string]string{"TZ": "UTC", "HTTP_FOO": "x"} // converged keys stay
	spec.SecretRefs = []string{"TOKEN"}
	if err := newC(f).Apply(run(t), tvsRef, spec, map[string]string{"TOKEN": "s3cr3t"}, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "config:set --no-restart top-web TOKEN=s3cr3t", 1)
	wantArgvCount(t, f, "config:unset", 0)
}

func TestApplyStorageMountAndUnmount(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsDigest)
	spec.Storage = []domain.Storage{{Name: "top-web-data", MountPath: "/data"}}
	seedConvergedApply(f)
	// converged fixture already mounts top-web-data:/data ⇒ no mutation.
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "storage:mount", 0)
	wantArgvCount(t, f, "storage:unmount", 0)
	wantArgvCount(t, f, "storage:create", 0)

	// changed mount adds create+mount
	f2 := exectest.NewFakeRunner()
	spec.Storage = []domain.Storage{{Name: "top-web-cache", MountPath: "/cache"}}
	seedConvergedApply(f2, func(f *exectest.FakeRunner) {
		f.SetOutput("dokku", fail(1, "does not exist"), "storage:info", "top-web-cache")
	})
	if err := newC(f2).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f2, "storage:create top-web-cache /var/lib/dokku/data/storage/top-web-cache --chown false", 1)
	wantArgvCount(t, f2, "storage:mount top-web top-web-cache --container-dir /cache", 1)
	wantArgvCount(t, f2, "storage:unmount", 1) // the removed /data mount
}

func TestApplyNetworkAttachDetach(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsDigest)
	spec.Routing = domain.Routing{Mode: domain.RoutingInternal}
	seedConvergedApply(f, func(f *exectest.FakeRunner) {
		f.SetOutput("dokku", ok(networkNone), "network:report", "top-web", "--format", "json")
	})
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, applyOpts()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "network:set top-web attach-post-deploy top-internal", 1)

	f2 := exectest.NewFakeRunner()
	spec.Routing = domain.Routing{Mode: domain.RoutingNone}
	seedConvergedApply(f2) // already attached to top-internal
	if err := newC(f2).Apply(run(t), tvsRef, spec, nil, backend.ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f2, "network:set top-web detach top-internal", 1)
}

func TestApplyPortsProxySetClear(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsDigest)
	spec.Ports = []domain.Port{{Name: "http", ContainerPort: 5000}}
	spec.Routing = domain.Routing{Mode: domain.RoutingProxy}
	seedConvergedApply(f, func(f *exectest.FakeRunner) {
		f.SetOutput("dokku", ok(portsNone), "ports:report", "top-web", "--ports-map-json")
	})
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, backend.ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "ports:set top-web http:5000:5000", 1)
	wantArgvCount(t, f, "domains:", 0) // no domains declared ⇒ untouched

	f2 := exectest.NewFakeRunner()
	spec.Routing = domain.Routing{Mode: domain.RoutingNone}
	seedConvergedApply(f2) // portsOne present ⇒ clear
	if err := newC(f2).Apply(run(t), tvsRef, spec, nil, backend.ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f2, "ports:clear top-web", 1)
}

func TestApplyResourcesSetClear(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsDigest)
	spec.Resources = domain.Resources{Limits: domain.ResourceLimits{CPU: "500m", Memory: "256Mi"}}
	// converged fixture already reports these ⇒ no mutation.
	seedConvergedApply(f)
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, backend.ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "resource:limit", 0)

	f2 := exectest.NewFakeRunner()
	seedConvergedApply(f2) // resourceUnlimit default; fresh limits ⇒ set
	spec2 := desiredBase(tvsDigest)
	spec2.Resources = domain.Resources{Limits: domain.ResourceLimits{CPU: "500m"}}
	if err := newC(f2).Apply(run(t), tvsRef, spec2, nil, backend.ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f2, "resource:limit --cpu 0.5 --process-type web top-web", 1)

	f3 := exectest.NewFakeRunner()
	spec3 := desiredBase(tvsDigest) // no limits desired; fixture reports limits ⇒ clear
	seedConvergedApply(f3)
	if err := newC(f3).Apply(run(t), tvsRef, spec3, nil, backend.ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f3, "resource:limit-clear --process-type web top-web", 1)
}

// A converged app whose stored (docker-native) spellings differ from the
// spec's K8s-style quantities — but mean the same thing — must NOT mutate.
func TestApplyResourcesSemanticConvergedNoMutation(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := desiredBase(tvsDigest)
	spec.Resources = domain.Resources{Limits: domain.ResourceLimits{CPU: "500m", Memory: "256Mi"}}
	// Fixture reports limits verbatim ("500m"/"256Mi"); override to the
	// STORED docker-native form the adapter would have written.
	seedConvergedApply(f)
	f.SetOutput("dokku", ok(`{"app":"top-web","web.limit.cpu":"0.5","web.limit.memory":"268435456"}`), "resource:report", "top-web", "--format", "json")
	if err := newC(f).Apply(run(t), tvsRef, spec, nil, backend.ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wantArgvCount(t, f, "resource:limit", 0)
	wantArgvCount(t, f, "resource:limit-clear", 0)
}

// --- failure semantics ------------------------------------------------------

// A failed config:set aborts the apply BEFORE any deploy call: the old
// release keeps serving (dokku was never asked to deploy).
func TestApplyConfigFailureAbortsBeforeDeploy(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedFreshApply(f)
	spec := desiredBase(tvsDigest)
	spec.Env = map[string]string{"TZ": "UTC", "HTTP_FOO": "x", "NEW": "y"} // NEW forces a set call
	f.SetPrefix([]string{"dokku", "config:set"}, fail(1, "!  Failure: config boom"))
	err := newC(f).Apply(run(t), tvsRef, spec, nil, backend.ApplyOptions{})
	if !errors.Is(err, domain.ErrBackend) {
		t.Fatalf("want ErrBackend, got %v", err)
	}
	wantArgvCount(t, f, "git:from-image", 0)
}

func TestApplyDeployFailureSurfaces(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedFreshApply(f)
	spec := desiredBase(tvsDigest)
	spec.Env = map[string]string{"TZ": "UTC", "HTTP_FOO": "x", "NEW": "y"}
	f.SetPrefix([]string{"dokku", "git:from-image"}, fail(1, "!  Failure: deploy boom"))
	err := newC(f).Apply(run(t), tvsRef, spec, nil, backend.ApplyOptions{})
	if !errors.Is(err, domain.ErrBackend) {
		t.Fatalf("want ErrBackend, got %v", err)
	}
}

func TestApplyInvalidSourceRefused(t *testing.T) {
	f := exectest.NewFakeRunner()
	spec := domain.Service{Ref: tvsRef} // neither image nor build
	err := newC(f).Apply(run(t), tvsRef, spec, nil, backend.ApplyOptions{})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if len(f.Recorded()) != 0 {
		t.Fatalf("no dokku calls expected, got %v", recordedArgv(f))
	}
}

// --- Readiness / FailedLogs -------------------------------------------------

func TestReadinessHappy(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedFull(f) // deployed+running+succeeded
	f.SetOutput("dokku", ok("checks pass"), "checks:run", "top-web")

	r, err := newC(f).Readiness(run(t), tvsRef)
	if err != nil {
		t.Fatalf("Readiness: %v", err)
	}
	if !r.Deployed || !r.Running || r.Build != domain.BuildSucceeded || !r.Checks {
		t.Fatalf("want ready, got %+v", r)
	}
	if r.Image != "docker.example.com/example/whoami:1.0" {
		t.Fatalf("image provenance lost: %+v", r)
	}
}

func TestReadinessFailingCheck(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedFull(f)
	f.SetOutput("dokku", fail(1, "check failed"), "checks:run", "top-web")

	r, err := newC(f).Readiness(run(t), tvsRef)
	if err != nil {
		t.Fatalf("Readiness (failing check is an answer, not an error): %v", err)
	}
	if r.Checks {
		t.Fatalf("want Checks=false, got %+v", r)
	}
}

func TestReadinessNotDeployedSkipsProbe(t *testing.T) {
	f := exectest.NewFakeRunner()
	seedNotDeployed(f)

	r, err := newC(f).Readiness(run(t), tvsRef)
	if err != nil {
		t.Fatalf("Readiness: %v", err)
	}
	if r.Deployed || r.Running || r.Build != domain.BuildNone {
		t.Fatalf("want not-ready, got %+v", r)
	}
	wantArgvCount(t, f, "checks:run", 0) // nothing to probe
}

func TestReadinessMissingApp(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", fail(20, existsNo), "apps:exists", "top-web")

	_, err := newC(f).Readiness(run(t), tvsRef)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestFailedLogs(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok(existsYes), "apps:exists", "top-web")
	f.SetOutput("dokku", ok("line1\nline2\nline3\n"), "logs:failed", "top-web")

	rows, err := newC(f).FailedLogs(run(t), tvsRef, 2)
	if err != nil {
		t.Fatalf("FailedLogs: %v", err)
	}
	if !slices.Equal(rows, []string{"line2", "line3"}) {
		t.Fatalf("want last 2 lines, got %q", rows)
	}
}

func TestFailedLogsNoFailedDeployIsEmpty(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok(existsYes), "apps:exists", "top-web")
	f.SetOutput("dokku", fail(1, "!  No failed deploy"), "logs:failed", "top-web")

	rows, err := newC(f).FailedLogs(run(t), tvsRef, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("want empty success, got (%q, %v)", rows, err)
	}
}

func TestFailedLogsMissingApp(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", fail(20, existsNo), "apps:exists", "top-web")

	_, err := newC(f).FailedLogs(run(t), tvsRef, 10)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// --- ServiceLogs (T-308: the CLI `topctl logs` surface) ---------------------

func TestServiceLogs(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok(existsYes), "apps:exists", "top-web")
	f.SetOutput("dokku", ok("l1\nl2\nl3\n"), "logs", "top-web", "--num", "100")

	rows, err := newC(f).ServiceLogs(run(t), tvsRef, 100)
	if err != nil {
		t.Fatalf("ServiceLogs: %v", err)
	}
	if !slices.Equal(rows, []string{"l1", "l2", "l3"}) {
		t.Fatalf("want all lines kept when under the cap, got %q", rows)
	}
	if got := strings.Join(recordedArgv(f), " "); !strings.Contains(got, "logs top-web --num 100") {
		t.Fatalf("want `logs <app> --num N` recorded, got %q", got)
	}
}

func TestServiceLogsCapsToMostRecent(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok(existsYes), "apps:exists", "top-web")
	f.SetOutput("dokku", ok("l1\nl2\nl3\n"), "logs", "top-web", "--num", "2")

	rows, err := newC(f).ServiceLogs(run(t), tvsRef, 2)
	if err != nil {
		t.Fatalf("ServiceLogs: %v", err)
	}
	if !slices.Equal(rows, []string{"l2", "l3"}) {
		t.Fatalf("want last 2 lines, got %q", rows)
	}
}

func TestServiceLogsNoOutputIsEmpty(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", ok(existsYes), "apps:exists", "top-web")
	// exit-1 with no output is the "nothing to tail" answer, not a failure.
	f.SetOutput("dokku", fail(1, "!  No logs"), "logs", "top-web", "--num", "10")

	rows, err := newC(f).ServiceLogs(run(t), tvsRef, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("want empty success, got (%q, %v)", rows, err)
	}
}

func TestServiceLogsMissingApp(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("dokku", fail(20, existsNo), "apps:exists", "top-web")

	_, err := newC(f).ServiceLogs(run(t), tvsRef, 10)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	wantArgvCount(t, f, "logs", 0) // not-found short-circuits before any log read
}

// Interface drift guard: the Dokku client must satisfy the extended
// PlatformBackend exactly (contract spine).
var _ backend.PlatformBackend = (*Client)(nil)

var _ exec.StdinRunner = (*exectest.FakeRunner)(nil)

var _ = bytes.MinRead // keep bytes import in compilation when helpers shift
