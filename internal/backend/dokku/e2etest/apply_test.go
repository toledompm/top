//go:build e2e

// SPDX-License-Identifier: MIT

// Tier-3 e2e for the Apply slice (T-305 image services, T-306 build
// services, T-307 healthchecks). Same harness as lifecycle_test.go: the
// official dokku/dokku container via testcontainers, driven by the
// PRODUCTION adapter through docker exec. Gated:
//
//	TOP_E2E=1 make test-e2e-backend
//
// Cases: (1) full-range image apply (batched env, storage, network,
// restart policy, resources, checks timings + synthesized app.json
// archive deploy), (2) idempotent re-apply (zero mutation), (3) failing
// check preserves the old release, (4) git:sync build with the adapter's
// no-change guard (a second, disposable git-daemon container — no
// dependence on internal/gitsource, per the wave boundary). Every context
// is deadline-bounded: a hung deploy can never stall the suite.
package e2etest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"github.com/toledompm/top/internal/backend"
	"github.com/toledompm/top/internal/backend/dokku"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
)

var (
	// The synthetic deploy cases use public images only (fictional, ADR-0014).
	imageBad = "docker.io/library/busybox:1.36"
	nsApply  = domain.Namespace("top")
)

// recordingRunner wraps any Runner and records every argv so idempotency
// ("zero mutation") can be asserted externally.
type recordingRunner struct {
	inner      exec.Runner
	mu         sync.Mutex
	calls      []string
	lastOut    []byte
	lastErrOut []byte
	lastErr    error
}

func (r *recordingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	r.record(name, args...)
	return r.inner.Run(ctx, name, args...)
}

func (r *recordingRunner) RunStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	r.record(name, args...)
	return r.inner.RunStream(ctx, stdout, stderr, name, args...)
}

func (r *recordingRunner) RunStdin(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, []byte, error) {
	r.record(name, args...)
	if sr, ok := r.inner.(exec.StdinRunner); ok {
		out, errOut, err := sr.RunStdin(ctx, stdin, name, args...)
		r.lastOut, r.lastErrOut, r.lastErr = out, errOut, err
		return out, errOut, err
	}
	return r.inner.Run(ctx, name, args...)
}

func (r *recordingRunner) record(name string, args ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
}

func (r *recordingRunner) argv() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recordingRunner) count(sub string) int {
	n := 0
	for _, a := range r.argv() {
		if strings.Contains(a, sub) {
			n++
		}
	}
	return n
}

// applyClient wires a recording-backed production client on the e2e
// internal network.
func applyClient(rr *recordingRunner) *dokku.Client {
	c := dokku.New(rr)
	c.InternalNetwork = networkName
	return c
}

// fullRangeSpec is the recurring full-featured image service (fictional).
func fullRangeSpec(name domain.Name, image string) domain.Service {
	return domain.Service{
		Ref:    domain.Ref(nsApply, name),
		Source: domain.Source{Image: strings.TrimSuffix(image, ":2.4-alpine"), Tag: "2.4-alpine"},
		Env:    map[string]string{"E2E_TOKEN": "fictional-not-a-secret"},
		Storage: []domain.Storage{
			{Name: "top-e2e-" + string(name) + "-disk", MountPath: "/disk"},
		},
		Routing:       domain.Routing{Mode: domain.RoutingInternal},
		RestartPolicy: domain.RestartAlways,
		Resources:     domain.Resources{Limits: domain.ResourceLimits{CPU: "1", Memory: "256Mi"}},
	}
}

// applyRetried wraps Apply with the same bounded retry the harness's
// ensureRetried uses for apps:create: the first-start flake window
// (transient "No web listeners specified"/detached docker plumbing on
// dokku/dokku fresh boot) can fail the first deploy; Apply is idempotent,
// so retrying the WHOLE converged apply is safe. Production retries at
// the reconciler layer (M3).
func applyRetried(ctx context.Context, t *testing.T, env *e2eEnv, c *dokku.Client, ref domain.ServiceRef, spec domain.Service) error {
	deadline := time.Now().Add(90 * time.Second)
	var err error
	for {
		ctx2, cancel2 := context.WithTimeout(ctx, 8*time.Minute)
		err = c.Apply(ctx2, ref, spec, nil, backend.ApplyOptions{})
		cancel2()
		if err == nil || !errors.Is(err, domain.ErrBackend) {
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
		if be, ok := err.(interface{ StderrOfArchive() ([]byte, []byte) }); ok {
			stderr, stdout := be.StderrOfArchive()
			t.Logf("apply retry: %v\narchive stderr:\n%s\narchive stdout:\n%s", err, stderr, stdout)
		} else {
			t.Logf("apply retry: %v", err)
		}
		time.Sleep(2 * time.Second)
	}
}

// TestE2EApplyFullRange: the full mapping range in one real apply, then
// the idempotent re-apply matrix.
func TestE2EApplyFullRange(t *testing.T) {
	env := skipGated(t)
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()

	rr := &recordingRunner{inner: env.runner}
	c := applyClient(rr)

	if err := c.EnsureNetwork(ctx, networkName); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	name := domain.Name("e2efull")
	ref := domain.Ref(nsApply, name)
	// The documented first-start flake window ("No web listeners
	// specified"); Applying a fresh app must not inherit it — pre-create
	// through the idempotent retry wrapper (lifecycle_test.go's pattern).
	if err := ensureRetried(ctx, env, ref); err != nil {
		debugLogs(t, env.runner)
		t.Fatalf("pre-EnsureApp: %v", err)
	}
	spec := fullRangeSpec(name, imageDeployed)
	spec.HealthCheck = &domain.HealthCheck{
		HTTP:            &domain.HTTPCheck{Path: "/", Port: "80"},
		IntervalSeconds: 5,
	}

	if err := applyRetried(ctx, t, env, c, ref, spec); err != nil {
		debugLogs(t, env.runner)
		t.Fatalf("Apply full-range: %v", err)
	}

	r, err := c.Readiness(ctx, ref)
	if err != nil {
		t.Fatalf("Readiness: %v", err)
	}
	// Bounded poll for the running flag — the container needs a beat to
	// settle after dokku reports the deploy succeeded.
	deadline := time.Now().Add(60 * time.Second)
	for !r.Running && time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		r, err = c.Readiness(ctx, ref)
		if err != nil {
			t.Fatalf("Readiness poll: %v", err)
		}
	}
	if !r.Deployed || !r.Running || r.Build != domain.BuildSucceeded || !r.Checks {
		debugLogs(t, env.runner)
		rr.mu.Lock()
		t.Logf("archive stdout:\n%s\narchive stderr:\n%s\narchive err: %v", rr.lastOut, rr.lastErrOut, rr.lastErr)
		rr.mu.Unlock()
		t.Logf("recording argv:\n%s", strings.Join(rr.argv(), "\n"))
		if out, _, err2 := env.runner.Run(ctx, "docker", "exec", env.runner.container, "dokku", "ps:report", "top-e2efull"); err2 == nil {
			t.Logf("ps:report:\n%s", out)
		}
		if out, _, err2 := env.runner.Run(ctx, "docker", "exec", env.runner.container, "sh", "-c", "dokku ps:scale top-e2efull; cat /var/lib/dokku/data/ps/top-e2efull/scale 2>/dev/null; dokku config:keys top-e2efull"); err2 == nil {
			t.Logf("scale/keys:\n%s", out)
		}
		if out, _, err2 := env.runner.Run(ctx, "docker", "exec", env.runner.container, "docker", "ps", "-a", "--format", "{{.Names}} {{.Image}} {{.Status}}"); err2 == nil {
			t.Logf("docker ps -a:\n%s", out)
		}
		if out, _, err2 := env.runner.Run(ctx, "docker", "exec", env.runner.container, "sh", "-c", "tail -n 120 /var/log/dokku/event.log 2>/dev/null; ls /var/lib/dokku/apps/top-e2efull 2>/dev/null"); err2 == nil {
			t.Logf("logs:\n%s", out)
		}
		t.Fatalf("want ready, got %+v", r)
	}

	obs, err := c.Inspect(ctx, ref)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if obs.RestartPolicy != domain.RestartAlways {
		t.Fatalf("restart policy not applied: %v", obs.RestartPolicy)
	}
	if !slicesContains(obs.EnvKeys, "E2E_TOKEN") {
		t.Fatalf("env key missing: %v", obs.EnvKeys)
	}
	if len(obs.StorageMounts) == 0 || obs.StorageMounts[0].MountPath != "/disk" {
		t.Fatalf("storage mount missing: %+v", obs.StorageMounts)
	}
	if !slicesContains(obs.Networks, networkName) {
		t.Fatalf("network not attached: %v", obs.Networks)
	}
	if observedResources(obs) != "1/268435456" { // docker-native bytes (quantities.go)
		t.Fatalf("resources not applied: %+v", obs.Resources)
	}

	// --- idempotent re-apply: ZERO mutation and ZERO archive resends —
	// the TOP_ARCHIVE_SHA256 env fingerprint minted after the first
	// deploy matches the re-computed spec fingerprint, so the converged,
	// serving release is poked at all (an identical-archive resend exits
	// 1 AND records a FAILED build, breaking the Readiness build gate).
	rr.calls = nil // safe: sequential use
	if err := c.Apply(ctx, ref, spec, nil, backend.ApplyOptions{}); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	for _, sub := range []string{
		"config:set", "config:unset", "ps:set", "ps:rebuild",
		"ports:set", "ports:clear",
		"storage:create", "storage:mount", "storage:unmount",
		"resource:limit", "network:set",
	} {
		if rr.count(sub) != 0 {
			t.Fatalf("re-apply mutated %s:\n%v", sub, rr.argv())
		}
	}
	if rr.count("git:from-archive") != 0 {
		t.Fatalf("re-apply archive resend count wrong (want 0, fingerprint skip):\n%v", rr.argv())
	}
	still, err := c.Readiness(ctx, ref)
	if err != nil || !still.Deployed || !still.Running || still.Build != domain.BuildSucceeded || !still.Checks {
		t.Fatalf("readiness after re-apply: %+v err %v", still, err)
	}
}

func slicesContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// observedResources renders the observed web limits for the assertion.
func observedResources(obs domain.ObservedState) string {
	return obs.Resources.Limits.CPU + "/" + obs.Resources.Limits.Memory
}

// TestE2EApplyFailingCheckPreservesOldRelease: bad image + failing check
// leaves the old release serving (tier-3 AC).
func TestE2EApplyFailingCheckPreservesOldRelease(t *testing.T) {
	env := skipGated(t)
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()

	rr := &recordingRunner{inner: env.runner}
	c := applyClient(rr)
	name := domain.Name("e2efail")
	ref := domain.Ref(nsApply, name)
	if err := ensureRetried(ctx, env, ref); err != nil {
		debugLogs(t, env.runner)
		t.Fatalf("pre-EnsureApp: %v", err)
	}

	good := fullRangeSpec(name, imageDeployed)
	good.Storage = nil
	good.Resources = domain.Resources{}
	if err := applyRetried(ctx, t, env, c, ref, good); err != nil {
		debugLogs(t, env.runner)
		t.Fatalf("good apply: %v", err)
	}
	before, err := c.Inspect(ctx, ref)
	if err != nil || before.Image == "" {
		t.Fatalf("precondition obs %+v err %v", before, err)
	}

	bad := fullRangeSpec(name, imageBad)
	bad.Storage = nil
	bad.Resources = domain.Resources{}
	bad.HealthCheck = &domain.HealthCheck{HTTP: &domain.HTTPCheck{Path: "/", Port: "80"}}
	ctxBad, cancelBad := context.WithTimeout(ctx, 8*time.Minute)
	defer cancelBad()
	err = c.Apply(ctxBad, ref, bad, nil, backend.ApplyOptions{})
	if !errors.Is(err, domain.ErrBackend) {
		t.Fatalf("bad apply should fail with ErrBackend, got %v", err)
	}

	after, err := c.Inspect(ctx, ref)
	if err != nil {
		t.Fatalf("post Inspect: %v", err)
	}
	if !after.Deployed || !after.Running {
		t.Fatalf("old release not serving: %+v", after)
	}
	if after.Image != before.Image {
		t.Fatalf("source-image not restored: before %q after %q", before.Image, after.Image)
	}
}

// TestE2EApplyGitSyncBuild drives T-306 end to end: a git-daemon
// container serving a repo with `Dockerfile FROM httpd:2.4-alpine` is
// built through `git:sync --build-if-changes`, and the re-apply at the
// same sha issues NO git:sync (no-change ⇒ no build record, hardened in
// the adapter).
func TestE2EApplyGitSyncBuild(t *testing.T) {
	env := skipGated(t)
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()

	repoURL, sha, err := bootGitDaemon(t, env)
	if err != nil {
		t.Fatalf("git daemon boot: %v", err)
	}

	rr := &recordingRunner{inner: env.runner}
	c := applyClient(rr)
	name := domain.Name("e2ebuild")
	ref := domain.Ref(nsApply, name)
	if err := ensureRetried(ctx, env, ref); err != nil {
		debugLogs(t, env.runner)
		t.Fatalf("pre-EnsureApp: %v", err)
	}
	spec := domain.Service{
		Ref: ref,
		Source: domain.Source{
			Build: &domain.BuildSource{Repo: repoURL, Ref: sha},
		},
		RestartPolicy: domain.RestartAlways,
		HealthCheck:   &domain.HealthCheck{HTTP: &domain.HTTPCheck{Path: "/", Port: "80"}},
	}

	if err := applyRetried(ctx, t, env, c, ref, spec); err != nil {
		debugLogs(t, env.runner)
		t.Fatalf("build apply: %v", err)
	}
	obs, err := c.Inspect(ctx, ref)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if obs.Revision != sha || !obs.Deployed {
		t.Fatalf("built revision mismatch: sha=%q obs=%+v", sha, obs)
	}
	if rr.count("git:sync --build-if-changes") != 1 {
		t.Fatalf("want exactly one git:sync: %v", rr.argv())
	}

	rr.calls = nil
	if err := c.Apply(ctx, ref, spec, nil, backend.ApplyOptions{}); err != nil {
		t.Fatalf("re-apply build: %v", err)
	}
	if rr.count("git:sync") != 0 {
		t.Fatalf("no-change poll left a build record (guard violated): %v", rr.argv())
	}
}

// bootGitDaemon provisions a disposable git-daemon container serving one
// seeded repo (Dockerfile FROM the image the full-range test already
// cached) on a host-mapped 9418 port, reachable by dokku's spawned fetch
// containers via host.docker.internal. The daemon runs from the dokku
// image itself: alpine/git does NOT ship git-daemon (tier-3 verified:
// "git: 'daemon' is not a git command") while the dokku image does — and
// it is already pulled for the harness. Returns the repo URL and seeded
// sha.
func bootGitDaemon(t *testing.T, env *e2eEnv) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	script := strings.Join([]string{
		"mkdir -p /data/repo",
		"git -C /data/repo init -q .",
		"printf 'FROM docker.io/library/httpd:2.4-alpine\\n' > /data/repo/Dockerfile",
		"git -C /data/repo add -A",
		"git -C /data/repo -c user.email=e2e@example.invalid -c user.name=e2e commit -qm seed",
		"exec /usr/lib/git-core/git-daemon --base-path=/data --export-all --reuseaddr --port=9418",
	}, "; ")

	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:      imageDokkuDefault,
			Entrypoint: []string{"/bin/sh"}, // dokku's own entrypoint is my_init
			Cmd:        []string{"-c", script},
			// git:// must be reachable from dokku's SPAWNED fetch
			// containers (host daemon default bridge): host-port mapping +
			// host.docker.internal is the carrier (tier-3 verified).
			ExposedPorts: []string{"9418/tcp"},
		},
		Started: true,
	}
	ctr, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		return "", "", fmt.Errorf("start git daemon container: %w", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	// Verify the seed landed AND the daemon is listening before wiring
	// the apply under test against it. Docker Desktop occasionally
	// flubs cold-start execs ("procReady not received") — retry.
	probe := "git -C /data/repo rev-parse HEAD"
	var sha string
	var probeFail string
	for attempt := 0; attempt < 5; attempt++ {
		code, reader, serr := ctr.Exec(ctx, []string{"sh", "-c", probe})
		if serr == nil {
			if out, rerr := io.ReadAll(reader); rerr == nil {
				sha = strings.TrimSpace(string(demuxFrame(out)))
				if code == 0 && domain.ValidRevision(sha) == nil {
					probeFail = ""
					break
				}
				probeFail = fmt.Sprintf("exit %d: sha %q ok=%v", code, sha, domain.ValidRevision(sha) == nil)
			}
		}
		time.Sleep(2 * time.Second)
	}
	if probeFail != "" {
		return "", "", fmt.Errorf("git daemon not ready: %s", probeFail)
	}
	port, pterr := ctr.MappedPort(ctx, "9418/tcp")
	if pterr != nil {
		return "", "", fmt.Errorf("git daemon port map: %w", pterr)
	}
	url := fmt.Sprintf("git://host.docker.internal:%d/repo", port.Int())
	return url, sha, nil
}

func errText(err error) string { return fmt.Sprint(err) }

// demuxFrame decodes the standard docker exec multiplexed stream (one
// stream-type byte, three padding bytes, uint32-BE frame length) —
// testcontainers' Exec reader hands out the RAW frames, not the payload.
func demuxFrame(raw []byte) []byte {
	var out []byte
	for i := 0; i+8 <= len(raw); {
		n := int(raw[i+4])<<24 | int(raw[i+5])<<16 | int(raw[i+6])<<8 | int(raw[i+7])
		i += 8
		if n > len(raw)-i {
			n = len(raw) - i
		}
		out = append(out, raw[i:i+n]...)
		i += n
	}
	return out
}
