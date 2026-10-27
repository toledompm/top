// SPDX-License-Identifier: MIT

// Package backend defines PlatformBackend, TOP's runtime abstraction over the
// workload engine (docs/contracts/platform-backend.md). One implementation in
// v0: internal/backend/dokku — the only package in the tree allowed to know
// Dokku command syntax (ADR-0001).
//
// The interface is the reconciler's seam (M3): every method takes a context,
// respects cancellation, and maps host reality into internal/domain types.
package backend

import (
	"context"

	"github.com/toledompm/top/internal/domain"
)

// PlatformBackend is the thin CRUD/read surface over the Dokku host. It is
// deliberately smaller than the full contract for this wave: v0 covers the
// read slice (T-201) and app CRUD (T-202); the Apply/Readiness/FailedLogs
// lines land with the reconciler (M3, T-305..T-308) and extend this
// interface, never fork it.
//
// Refs are qualified identities (namespace/name); the Dokku app name is
// derived via domain.AppName ("<ns>-<name>"). All command argv knowledge
// lives in internal/backend/dokku; this interface knows nothing of Dokku.
type PlatformBackend interface {
	// EnsureApp creates the app when absent ("apps:exists" exit code →
	// "apps:create") and adopts it when it already exists. Adoption is by
	// name-prefix match (ADR-0009 naming layer); ledger-based ownership
	// proof is layered on top by the reconciler/installer (T-204,
	// T-402), not by the backend.
	EnsureApp(ctx context.Context, ref domain.ServiceRef) error

	// Inspect returns a structured snapshot of the host's actual state for
	// the mapped app, composed from the documented per-plugin reports
	// (ps/builds/git/apps/ports/storage/network/resource/config). The app
	// not existing is domain.ErrNotFound.
	Inspect(ctx context.Context, ref domain.ServiceRef) (domain.ObservedState, error)

	// LastGoodRevision returns the deploy-source metadata of the last
	// successful deploy: a commit sha for git/source builds, the deployed
	// image reference for image deploys, or "" when never deployed.
	LastGoodRevision(ctx context.Context, ref domain.ServiceRef) (string, error)

	// Stop halts the app's processes ("ps:stop"). Stopping an app that
	// does not exist is a no-op success (idempotency per T-202).
	Stop(ctx context.Context, ref domain.ServiceRef) error

	// Destroy removes the app ("apps:destroy --force"). Ownership proof is
	// the CALLER's job per ADR-0009 (ledger + label/naming two-key rule in
	// the uninstall/prune flows); the adapter only asserts defense-in-depth
	// that the derived app name is a well-formed TOP name. Destroying an
	// app that does not exist is a no-op success.
	Destroy(ctx context.Context, ref domain.ServiceRef) error

	// HealthyBackend reports whether Dokku itself is usable ("dokku
	// version" + a cheap "apps:list"). Any failure returns (false, err)
	// wrapping domain.ErrUnhealthy.
	HealthyBackend(ctx context.Context) (bool, error)

	// EnsureNetwork idempotently creates the shared internal Docker network
	// ("network:exists" → "network:create") for routing.mode: internal.
	EnsureNetwork(ctx context.Context, name string) error

	// Close releases any adapter-held resources. The Dokku adapter holds
	// none (stateless CLI invocations), so this is a back-compat hook for
	// future backends that keep connections.
	Close() error
}
