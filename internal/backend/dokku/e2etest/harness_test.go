//go:build e2e

// SPDX-License-Identifier: MIT

// Tier-3 e2e harness (ADR-0012 tier 3): REAL Dokku, reachable only via
//
//	TOP_E2E=1 make test-e2e-backend
//
// The suite NEVER touches a host Dokku. It provisions the official
// dokku/dokku container (dedicated, disposable, via testcontainers-go)
// against the local Docker daemon's socket and drives the production
// adapter through a docker-exec Runner — so real argv, timing,
// exit-semantics and report shapes are exercised end-to-end (macOS-runnable
// via Docker Desktop).
//
// Cost: the first run pulls dokku/dokku (~1 GB) plus a tiny httpd deploy
// image; container teardown is automatic (testcontainers Ryuk), state is
// disposable. This tier never runs against a host Dokku — turning it on
// without local Docker fails the skip guard above.
package e2etest

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"github.com/toledompm/top/internal/backend/dokku"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
)

const (
	// envE2E gates the suite; honored by the Makefile target too.
	envE2E = "TOP_E2E"
	// envDokkuImage lets a run pin a different dokku/dokku tag.
	envDokkuImage = "TOP_E2E_DOKKU_IMAGE"
	// imageDokkuDefault: the official image. First pull is ~1 GB (README).
	imageDokkuDefault = "docker.io/dokku/dokku:latest"

	// networkName: "top-" prefixed per the adapter's shared-host safety
	// rule even though this tier only ever sees the dedicated container.
	networkName = "top-e2e-internal"

	// nsTop is the namespace that yields "top-<name>" app names
	// (domain.AppName = "<ns>-<name>"), honoring the "top-" prefix rule.
	nsTop = domain.Namespace("top")

	// imageDeployed is the tiny public deployment image proving the
	// deployed+running mapping (httpd: a container that stays alive with
	// EXPOSE 80; ~8 MB public pull; no real host identity involved).
	imageDeployed = "docker.io/library/httpd:2.4-alpine"
)

// e2eRunner adapts one local FastExec into "docker exec <ctr> ...": the
// adapter asks for ("dokku", args...); the harness runs
// ("docker", "exec", "-i", ctr, "dokku", args...). Everything else
// (e.g. "docker") passes through unchanged.
type e2eRunner struct {
	base      *exec.FastExec
	container string
}

func (w *e2eRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	if name != "dokku" {
		return w.base.Run(ctx, name, args...)
	}
	return w.base.Run(ctx, "docker", append([]string{"exec", "-i", w.container, "dokku"}, args...)...)
}

func (w *e2eRunner) RunStdin(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, []byte, error) {
	if name == "dokku" {
		return w.base.RunStdin(ctx, stdin, "docker", append([]string{"exec", "-i", w.container, "dokku"}, args...)...)
	}
	return w.base.RunStdin(ctx, stdin, name, args...)
}

func (w *e2eRunner) RunStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	if name != "dokku" {
		return w.base.RunStream(ctx, stdout, stderr, name, args...)
	}
	return w.base.RunStream(ctx, stdout, stderr, "docker", append([]string{"exec", "-i", w.container, "dokku"}, args...)...)
}

// e2eEnv is the wired per-suite fixture.
type e2eEnv struct {
	backend *dokku.Client
	runner  *e2eRunner
}

// start provisions the dokku container once per test-binary run; skipGated
// (below) turns the whole suite off without TOP_E2E=1 (tier-3 is opt-in,
// ADR-0012).
var shared *e2eEnv

func skipGated(t *testing.T) *e2eEnv {
	t.Helper()
	if env := environment(); env != "1" {
		t.Skipf("tier-3 backend e2e requires %s=1 (make test-e2e-backend); skipping", envE2E)
	}
	if shared != nil {
		return shared
	}
	shared = boot(t)
	return shared
}

func environment() string { return os.Getenv(envE2E) }

// boot builds the container + adapter client and registers cleanup.
func boot(t *testing.T) *e2eEnv {
	t.Helper()
	ctx := t.Context()
	ctr := startDokkuContainer(t, ctx)

	runner := &e2eRunner{base: &exec.FastExec{}, container: ctr}
	client := dokku.New(runner)

	cleanupStaleAppContainers(t, runner)

	if err := waitHealthy(t, ctx, client); err != nil {
		t.Fatalf("dokku container never became healthy: %v", err)
	}
	// Tier-3 readiness is two-stage now: healthy (commands answer) AND
	// deploy-warm (a canary converge outside the first-boot poison window;
	// convergeWarmCanary). A silent zero-process release inside the window
	// is sticky — the suite must never drill applies before warmth.
	if err := convergeWarmCanary(t, ctx, client, runner); err != nil {
		t.Fatalf("dokku container never deploy-warm: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &e2eEnv{backend: client, runner: runner}
}

// startDokkuContainer provisions the official dokku/dokku image in the
// official docker-installation shape: dokku drives the HOST docker daemon
// through the socket mount (never a nested daemon), no host ports are
// published (the harness execs in; nothing on the daemon is reused).
func startDokkuContainer(t *testing.T, ctx context.Context) string {
	t.Helper()
	image := imageDokkuDefault
	if v := os.Getenv(envDokkuImage); v != "" {
		image = v
	}
	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: image,
			Env: map[string]string{
				"DOKKU_HOSTNAME": "deploy.example.internal", // fictional, ADR-0014
			},
			Mounts: []testcontainers.ContainerMount{
				testcontainers.BindMount("/var/run/docker.sock",
					testcontainers.ContainerMountTarget("/var/run/docker.sock")),
			},
		},
		Started: true,
	}
	ctr, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		t.Fatalf("start dokku container %s: %v", image, err)
	}
	// No explicit Terminate registration: the container is shared across
	// the suite's tests; testcontainers' Ryuk reaps it at process end.
	return ctr.GetContainerID()
}

// cleanupStaleAppContainers removes leftover TOP app containers from the
// host daemon named "top-*.web.*". Testcontainers' Ryuk reaps only what
// the test process created — containers dokku spawned THROUGH the
// mounted socket outlive the suite, and a leftover container named like
// an app's .web.1 blocks the new run's container create (tier-3 verified
// 2026-10-28: dokku 0.38.32 silently deploys NOTHING — release committed,
// Processes 0, "no web listeners" — when the name is taken). Best-effort.
func cleanupStaleAppContainers(t *testing.T, runner *e2eRunner) {
	t.Helper()
	ctx := context.Background()
	out, _, err := runner.base.Run(ctx, "docker", "ps", "-a", "--format", "{{.Names}}")
	if err != nil {
		return
	}
	for _, name := range strings.Fields(string(out)) {
		if strings.Contains(name, ".web.") {
			_, _, _ = runner.base.Run(ctx, "docker", "rm", "-f", name)
		}
	}
}

// waitHealthy polls HealthyBackend plus a canary app-create until the
// dokku container finishes its first-start bootstrap. `dokku version`
// alone passes before the runit/nginx stack is fully up — apps:create can
// then still exit 1 ("No web listeners specified") — so the gate does an
// idempotent create+destroy canary ("top-" prefixed) and clears it before
// returning.
func waitHealthy(t *testing.T, ctx context.Context, client *dokku.Client) error {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	var lastErr error
	for {
		ok, err := client.HealthyBackend(ctx)
		if err == nil && ok {
			ref := domain.Ref(nsTop, "e2ewarmup")
			createErr := client.EnsureApp(ctx, ref)
			if createErr == nil {
				_ = client.Destroy(ctx, ref)
				return nil
			}
			lastErr = fmt.Errorf("canary create: %w", createErr)
		} else {
			lastErr = fmt.Errorf("health: %w", err)
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(2 * time.Second)
	}
}

// warmCanaryTar is the canary's synthesized deploy archive: identical
// shape to the adapter's (Dockerfile FROM + app.json HTTP startup check);
// a determinism-safe test-local twin (buildChecksArchive is unexported).
func warmCanaryTar(t *testing.T) []byte {
	t.Helper()
	checkJSON := `[{"type":"startup","path":"/","port":80,"wait":5}]` //nolint:dupl // literal twin of adapter synthesis
	dockerfile := "FROM docker.io/library/httpd:2.4-alpine\n"
	appJSON := fmt.Sprintf(`{"healthchecks":{"web":%s}}`, checkJSON)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{{"Dockerfile", dockerfile}, {"app.json", appJSON}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body))}); err != nil {
			t.Fatalf("canary tar header: %v", err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatalf("canary tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("canary tar close: %v", err)
	}
	return buf.Bytes()
}

// convergeWarmCanary proves the dokku container is WARM for the full
// apply range — not merely healthy. Tier-3 verified 2026-10-28 (dokku
// 0.38.32): a deploy in the first ~40s after boot silently commits the
// release with ZERO process containers (scale table empty, "no web
// listeners"), the state is sticky (redeploys see "No changes detected",
// ps:rebuild finds no commits), and the canary must converge (deployed +
// running + checks) before the suite may drill real applies. The probe is
// the full range's painful trio: network attach + byte-form resource
// limit + archive deploy with an HTTP check. Healed canary state is
// destroyed before returning.
func convergeWarmCanary(t *testing.T, ctx context.Context, client *dokku.Client, runner *e2eRunner) error {
	t.Helper()
	ref := domain.Ref(nsTop, "e2ewarm")
	if err := client.EnsureNetwork(ctx, networkName); err != nil {
		return fmt.Errorf("canary network: %w", err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	tarBytes := warmCanaryTar(t)
	for {
		err := canaryPass(ctx, client, runner, ref, tarBytes)
		if err == nil {
			_ = client.Destroy(ctx, ref)
			return nil
		}
		// poisoned or transient: destroy and probe over from scratch
		_ = client.Destroy(ctx, ref)
		cleanupStaleAppContainers(t, runner)
		if time.Now().After(deadline) {
			return fmt.Errorf("dokku never reached deploy-warm state (canary never converged): %v", err)
		}
		time.Sleep(3 * time.Second)
	}
}

// canaryPass runs ONE warm-canary probe: converge the special trio
// (network attach + byte-form memory limit + archive-with-HTTP-check
// deploy) and require Readiness(deployed+running+checks) within 30s.
func canaryPass(ctx context.Context, client *dokku.Client, runner *e2eRunner, ref domain.ServiceRef, tarBytes []byte) error {
	if err := client.EnsureApp(ctx, ref); err != nil {
		return fmt.Errorf("canary ensure app: %w", err)
	}
	for _, args := range [][]string{
		{"network:set", "top-e2ewarm", "attach-post-deploy", networkName},
		{"resource:limit", "--memory", "268435456", "--process-type", "web", "top-e2ewarm"},
		{"ps:set", "top-e2ewarm", "restart-policy", "always"},
	} {
		if _, _, err := runner.RunStdin(ctx, nil, "dokku", args...); err != nil {
			return fmt.Errorf("canary %v: %w", args, err)
		}
	}
	if _, _, err := runner.RunStdin(ctx, bytes.NewReader(tarBytes), "dokku", "git:from-archive", "top-e2ewarm", "--"); err != nil {
		return fmt.Errorf("canary deploy: %w", err)
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		r, rErr := client.Readiness(rctx, ref)
		if rErr != nil {
			return fmt.Errorf("canary readiness: %w", rErr)
		}
		if r.Deployed && r.Running && r.Checks {
			return nil
		}
		if err := rctx.Err(); err != nil {
			return fmt.Errorf("canary never ready in 30s (readiness %+v)", r)
		}
		time.Sleep(2 * time.Second)
	}
}

// debugLogs dumps the container's dokku event log tail when a test fails
// (the adapter errors carry only an excerpt; dokku's own event log has the
// full stderr of the failing command).
func debugLogs(t *testing.T, runner *e2eRunner) {
	t.Helper()
	var buf bytes.Buffer
	if err := runner.RunStream(t.Context(), &buf, &buf, "docker", "logs", "--tail", "60", runner.container); err != nil {
		t.Logf("debug: docker logs failed: %v", err)
		return
	}
	t.Logf("dokku container logs (tail 60):\n%s", buf.String())
}

// appRef derives a TOP app name for ns/name: "top-" prefixed.
func appRef(name domain.Name) domain.ServiceRef { return domain.Ref(nsTop, name) }
