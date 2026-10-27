//go:build e2e

// SPDX-License-Identifier: MIT

// The tier-3 lifecycle tests, run over the real dokku/dokku container from
// harness_test.go (skipped unless TOP_E2E=1). Every app/network name is
// "top-" prefixed even on the dedicated container.
package e2etest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/backend/dokku"
	"github.com/toledompm/top/internal/domain"
)

// TestE2EHealthyBackend: dokku version + apps:list answer healthy.
func TestE2EHealthyBackend(t *testing.T) {
	env := skipGated(t)
	ok, err := env.backend.HealthyBackend(t.Context())
	if err != nil {
		t.Fatalf("HealthyBackend: %v", err)
	}
	if !ok {
		t.Fatal("backend reported unhealthy")
	}
}

// TestE2EEnsureNetwork: idempotent create (twice, same result).
func TestE2EEnsureNetwork(t *testing.T) {
	env := skipGated(t)
	ctx := t.Context()
	if err := env.backend.EnsureNetwork(ctx, networkName); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if err := env.backend.EnsureNetwork(ctx, networkName); err != nil {
		t.Fatalf("EnsureNetwork (idempotent re-run): %v", err)
	}
}

// TestE2EEnsureAndNeverDeployed: EnsureApp is idempotent (create → adopt)
// and an ensured-but-never-deployed app inspects as the "never deployed"
// ObservedState shape — the inversion mirror of deployed+running (contract
// AC: mirrors).
func TestE2EEnsureAndNeverDeployed(t *testing.T) {
	env := skipGated(t)
	ctx := t.Context()
	ref := appRef("e2eensure")
	defer debugLogs(t, env.runner)

	if err := ensureRetried(ctx, env, ref); err != nil {
		t.Fatalf("EnsureApp: %v", err)
	}
	if err := env.backend.EnsureApp(ctx, ref); err != nil {
		t.Fatalf("EnsureApp (adopt): %v", err)
	}

	obs, err := env.backend.Inspect(ctx, ref)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if obs.AppName != "top-e2eensure" {
		t.Fatalf("app name mapping: %q", obs.AppName)
	}
	if obs.Deployed || obs.Running {
		t.Fatalf("never-deployed app must not read deployed/running: %+v", obs)
	}
	if obs.Image != "" || obs.Revision != "" {
		t.Fatalf("never-deployed app must not report a revision/image: %+v", obs)
	}
	if obs.LastBuild != domain.BuildNone {
		t.Fatalf("never-deployed app must read BuildNone, got %v", obs.LastBuild)
	}
	if err := env.backend.Destroy(ctx, ref); err != nil {
		t.Fatalf("cleanup destroy: %v", err)
	}
}

// TestE2EAppLifecycle: the full CRUD/read story — deploy a tiny public
// image (git:from-image), Inspect shows deployed+running, revision
// plumbing, Stop idempotent, Destroy idempotent, Inspect → ErrNotFound.
func TestE2EAppLifecycle(t *testing.T) {
	env := skipGated(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	ref := appRef("e2elifecycle")
	defer debugLogs(t, env.runner)

	// Probe before the app exists → ErrNotFound.
	if _, err := env.backend.Inspect(ctx, ref); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("pre-existence Inspect: want ErrNotFound, got %v", err)
	}
	if err := ensureRetried(ctx, env, ref); err != nil {
		t.Fatalf("EnsureApp: %v", err)
	}

	obs, err := env.backend.Inspect(ctx, ref)
	if err != nil {
		t.Fatalf("Inspect (fresh): %v", err)
	}
	if obs.Deployed || obs.Running {
		t.Fatalf("fresh app must be not-deployed/not-running: %+v", obs)
	}

	// Deploy: skip zero-downtime checks (no HTTP traffic is sent in this
	// tier) then pull+deploy the tiny public image.
	if err := env.backend.EnsureNetwork(ctx, networkName); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if err := deployImage(ctx, env.runner, ref, imageDeployed); err != nil {
		t.Fatalf("git:from-image deploy: %v", err)
	}

	// Deploy-settling wait (harness-side only): git:from-image returns
	// while dokku finalizes the new release (container rename, listener
	// registration); poll Inspect until Running turns true (bounded) so
	// the test asserts steady state, not a mid-phase race.
	settleCtx, settleCancel := context.WithTimeout(ctx, time.Minute)
	defer settleCancel()
	var settled domain.ObservedState
	for {
		obs, err = env.backend.Inspect(settleCtx, ref)
		var be *dokku.BackendError
		if err != nil && (!errors.As(err, &be) || be.Unhealthy) {
			t.Fatalf("Inspect (deployed): %v", err)
		}
		// Per-call-timeout spikes during the dokku 0.38 fresh-boot window
		// are retried (idempotent read); failure out of the wait is fatal.
		if err == nil {
			if obs.Running {
				settled = obs
				break
			}
			if settleCtx.Err() != nil {
				t.Fatalf("deploy never reached running: %+v", obs)
			}
			time.Sleep(2 * time.Second)
			continue
		}
		if settleCtx.Err() != nil {
			t.Fatalf("Inspect (deployed): %v", err)
		}
	}
	sample := settled
	if !sample.Deployed {
		t.Fatalf("want deployed=true: %+v", sample)
	}
	if !sample.Running {
		t.Fatalf("want running=true: %+v", sample)
	}
	if !(strings.Contains(sample.Image, "httpd:2.4-alpine") || strings.Contains(sample.Image, "httpd")) {
		t.Fatalf("want image deploy recorded, got image=%q rev=%q", sample.Image, sample.Revision)
	}
	if sample.LastBuild == domain.BuildNone {
		t.Fatalf("a deploy counts as a build record; got none: %+v", sample)
	}

	rev, err := env.backend.LastGoodRevision(ctx, ref)
	if err != nil {
		t.Fatalf("LastGoodRevision: %v", err)
	}
	if rev == "" {
		t.Fatal("want a non-empty revision/image for a deployed app")
	}

	// Stop: observed running flips off; a second stop is a clean no-op.
	if err := env.backend.Stop(ctx, ref); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := env.backend.Stop(ctx, ref); err != nil {
		t.Fatalf("Stop (idempotent): %v", err)
	}
	obs, err = env.backend.Inspect(ctx, ref)
	if err != nil {
		t.Fatalf("Inspect (stopped): %v", err)
	}
	if obs.Running {
		t.Fatalf("want running=false after stop: %+v", obs)
	}
	if !obs.Deployed {
		t.Fatalf("stop must not clear deployment: %+v", obs)
	}

	// Destroy: the app vanishes (ErrNotFound afterwards); second destroy
	// is a clean no-op success.
	if err := env.backend.Destroy(ctx, ref); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := env.backend.Inspect(ctx, ref); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("post-destroy Inspect: want ErrNotFound, got %v", err)
	}
	if err := env.backend.Destroy(ctx, ref); err != nil {
		t.Fatalf("Destroy (idempotent): %v", err)
	}
}

// ensureRetried wraps EnsureApp with the documented reconciliation retry
// pattern: early in the dokku container's first-start bootstrap,
// `apps:create` can transiently exit 1 with "No web listeners specified"
// (verified flake window on dokku 0.38.32, one-shot reproducible only in
// the first seconds after boot). EnsureApp is idempotent, so a bounded
// retry is safe; production retries at the reconciler layer (M3).
func ensureRetried(ctx context.Context, env *e2eEnv, ref domain.ServiceRef) error {
	deadline := time.Now().Add(60 * time.Second)
	var err error
	for {
		if err = env.backend.EnsureApp(ctx, ref); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(2 * time.Second)
	}
}

// deployImage runs the verified deploy command path directly through the
// harness runner: `dokku checks:skip <app>` (documented, zero-downtime-deploys.md)
// then `dokku git:from-image <app> <image>`. (Apply is M3 scope; this tier
// deploys via the same verified mapping the contract lists, so Inspect has
// a meaningful deployment to observe.)
func deployImage(ctx context.Context, runner *e2eRunner, ref domain.ServiceRef, image string) error {
	app, err := domain.AppName(ref.Namespace, ref.Name)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if _, stderr, err := runner.Run(ctx, "dokku", "checks:skip", app); err != nil {
		return fmt.Errorf("checks:skip: exit info: %w (stderr: %q)", err, string(stderr))
	}
	if _, stderr, err := runner.Run(ctx, "dokku", "git:from-image", app, image); err != nil {
		return fmt.Errorf("git:from-image: %w (stderr: %q)", err, string(stderr))
	}
	return nil
}
