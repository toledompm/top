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

// PlatformBackend is TOP's runtime abstraction over the Dokku host. v0
// covered the read slice (T-201) and app CRUD (T-202); the integrator
// deploy/teardown half (T-205..T-208) and the reconciler half
// (Apply/Readiness/FailedLogs, T-305..T-307) complete the contract by
// EXTENDING this interface — never forking it.
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

	// --- integrator surface (T-205..T-208, internal/install) ------------------
	//
	// These five methods back the installer's bootstrap deploy (ADR-0005)
	// and its teardown counterpart. They are on the INTERFACE, never
	// forked into a second one: the same extend-not-fork rule as the M3
	// reconciler methods.

	// DeployImage performs the one-time direct image deploy the bootstrap
	// uses: "git:from-image <app> <imageRef>". Long-running by nature —
	// the adapter applies the deploy timeout class to it (10 min, the
	// installer contract's bootstrap deploy budget). Unlike the other
	// mutating methods this is NOT no-op-safe: a failed deploy is a real
	// failure the caller records as the bootstrap-failed marker.
	DeployImage(ctx context.Context, ref domain.ServiceRef, imageRef string) error

	// EnsureStorageEntry idempotently registers a named Dokku storage
	// entry pointing at hostDir, without changing ownership of the host
	// directory ("storage:info <name>" probe → "storage:create <name>
	// <hostDir> --chown false": the host dir is TOP's own
	// (--purge-data-managed) directory, so Dokku must not chown it).
	EnsureStorageEntry(ctx context.Context, name, hostDir string) error

	// AttachStorage mounts the named storage entry into the app's
	// containers at containerPath ("storage:mount").
	AttachStorage(ctx context.Context, ref domain.ServiceRef, name, containerPath string) error

	// DetachStorage unmounts the named entry from the app's containers
	// ("storage:unmount"). Tearing down an app that does not exist is a
	// no-op success; an unmount that finds nothing is likewise a no-op
	// (idempotent teardown, T-208).
	DetachStorage(ctx context.Context, ref domain.ServiceRef, name, containerPath string) error

	// DestroyStorageEntry removes a named Dokku storage entry that is no
	// longer mounted anywhere ("storage:destroy <name> --force"). Caller
	// owns the ownership proof (two-key rule); --destroy-host-dir is
	// deliberately NOT passed — the host dir is removed by the caller's
	// own purge-data tier, under its own confirmation.
	DestroyStorageEntry(ctx context.Context, name string) error

	// --- reconciler surface (T-305..T-307, internal/reconcile) -----------------
	//
	// These three methods complete the platform-backend contract for the
	// M3 reconciler. They EXTEND this interface (extend-not-fork rule);
	// existing calls are untouched.

	// Apply converges ONE service to its resolved ServiceSpec (contract
	// `Apply(spec, secrets)`; the contract's names are illustrative — see
	// the deviations note in internal/backend/dokku/README.md). Semantics:
	// read-only delta against Inspect; mutate ONLY what differs (so an
	// idempotent re-apply against a converged host is zero-mutation); batch
	// env via one `config:set --no-restart` call (removed keys via one
	// `config:unset`); set ports/domains/storage/network/restart-policy/
	// resources; then deploy per source kind (image: `git:from-image
	// [--force]`, or the synthesized-archive path when the spec defines
	// healthchecks; build: `git:sync --build-if-changes <app> <repo>
	// <sha>`, skipped entirely when that sha is already the deployed
	// revision — Dokku's no-change path must never leave a build record).
	//
	// A failed deploy returns a *BackendError and leaves dokku's rollback
	// semantics (deploy branch + source-image restored) in effect — the
	// old release keeps serving.
	//
	// DEVIATIONS from the contract spelling (documented, not forked):
	//   - the service identity travels explicitly as ref, matching every
	//     other method (the reconciler may target adopted identities);
	//   - secrets is the resolved map of secret VALUES keyed exactly by
	//     spec.SecretRefs' names (ADR-0008: values merge with plain env in
	//     the same batched config:set; argv is redacted in the exec layer);
	//     nil is fine for services without SecretRefs;
	//   - ApplyOptions carries the M3 acceptance knobs (ForcePull today).
	Apply(ctx context.Context, ref domain.ServiceRef, spec domain.Service, secrets map[string]string, opts ApplyOptions) error

	// Readiness implements contract `Readiness(ns, name)`: is the applied
	// revision live and healthy? Deployed/Running/last-build come from the
	// documented reports; Checks is an ACTIVE `checks:run` probe. The app
	// not existing is domain.ErrNotFound.
	Readiness(ctx context.Context, ref domain.ServiceRef) (Readiness, error)

	// FailedLogs implements contract `FailedLogs(ns, name)`: the last
	// failed deploy's output, redacted (the exec layer's redaction), as
	// lines, most-recently-LAST, at most `lines` entries. An app with no
	// failed deploy returns an empty slice and nil error; a missing app is
	// domain.ErrNotFound.
	FailedLogs(ctx context.Context, ref domain.ServiceRef, lines int) ([]string, error)

	// ServiceLogs implements the CLI logs surface (T-308, addition
	// documented as a deviance in internal/backend/dokku/README.md): the
	// app's recent runtime logs (`logs`), redacted (exec-layer redaction),
	// as lines, oldest-LAST, at most `lines` entries (lines<=0 → default
	// cap). A missing app is domain.ErrNotFound.
	ServiceLogs(ctx context.Context, ref domain.ServiceRef, lines int) ([]string, error)

	// Close releases any adapter-held resources. The Dokku adapter holds
	// none (stateless CLI invocations), so this is a back-compat hook for
	// future backends that keep connections.
	Close() error
}

// Readiness is the answer to "is the applied revision live and healthy?"
// (contract `Readiness(ns, name)`, composed per the mapping table from the
// deployed/running flags, the last build status, and the active checks:run
// probe). Deployed and Running are the ps:report facts; Build is the last
// build's status; Checks reports the checks:run probe (true also when the
// backend has no running processes to probe — the probe is skipped and
// Deployed/Running carry the verdict). Revision/Image repeat
// LastGoodRevision so callers can gate "applied revision == live revision".
// Gate semantics are the reconciler's: a service is ready when Deployed &&
// Running && Build == BuildSucceeded && Checks.
type Readiness struct {
	Deployed bool
	Running  bool
	Build    domain.BuildStatus
	Checks   bool
	Revision string
	Image    string
}

// ApplyOptions carries per-apply knobs beyond the desired spec.
//
// ForcePull passes dokku `git:from-image --force`: the reconciler sets it
// when the desired source is a MUTABLE tag whose movement a poll detected
// (contract: "--force when the desired ref is a mutable tag and poll
// detected movement; digests never need it"). Digest-pinned and build
// sources never force.
type ApplyOptions struct {
	ForcePull bool
}
