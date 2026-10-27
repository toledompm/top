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
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
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

	if err := waitHealthy(t, ctx, client); err != nil {
		t.Fatalf("dokku container never became healthy: %v", err)
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

// debugLogs dumps the container's dokku event log tail when a test fails
// (the adapter errors carry only an excerpt; dokku's own event log has the
// full stderr of the failing command).
func debugLogs(t *testing.T, runner *e2eRunner) {
	t.Helper()
	if !t.Failed() {
		return
	}
	var buf bytes.Buffer
	if err := runner.RunStream(t.Context(), &buf, &buf, "docker", "logs", "--tail", "60", runner.container); err != nil {
		t.Logf("debug: docker logs failed: %v", err)
		return
	}
	t.Logf("dokku container logs (tail 60):\n%s", buf.String())
}

// appRef derives a TOP app name for ns/name: "top-" prefixed.
func appRef(name domain.Name) domain.ServiceRef { return domain.Ref(nsTop, name) }
