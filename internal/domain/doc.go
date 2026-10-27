// Package domain is TOP's shared vocabulary: the concrete types and enums
// that every other package (spec parser, reconciler, backend, store, CLI)
// speaks.
//
// Scope and boundaries:
//
//   - Pure vocabulary. No I/O of any kind: no filesystem, network, exec, or
//     database imports. This package never performs a host mutation; it only
//     names and describes them.
//   - Concrete, not abstract. This is the one package deliberately without
//     interfaces; ITS (the spec parser, the reconciler, ...) live elsewhere.
//   - Secret-safe by construction. Secrets are never values — they are
//     references (names) resolved by the SecretProvider (ADR-0008). Types
//     here must never carry or render a secret value; see plan.FieldChange
//     and the secret-safe rendering tests.
//   - It is the semantic model, not the syntax. The ServiceSpec YAML format
//     lives in Git; internal/spec (task T-103) parses it into these types.
//     Domain types therefore avoid framing anything as "the YAML field X":
//     they model the resolved, defaulted meaning.
//
// Key types at a glance (all detail lives in the respective files):
//
//	Namespace, Name, ServiceRef — identity; AppName() names the Dokku app.
//	DesiredState, Service       — the resolved desired state at a revision.
//	ObservedState               — what the PlatformBackend reports.
//	Plan, ServicePlan, FieldChange — diff between the two; secret-safe.
//	Operation                   — the tracked unit of mutating work.
//	ReconciliationRun, Checkpoint — run summaries and resume points.
//	ErrNotFound/ErrConflict/... — the shared error taxonomy.
//
// Terminology: a *revision* is a resolved Git commit sha of the platform
// repo (never a branch name, ADR-0002); a *reference* is a mutable image tag
// or branch, which the reconciler never considers authoritative.
package domain
