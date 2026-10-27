// Package conformance_check is TOP's contract conformance harness — the
// executable fence between the docs/contracts/*.md documents and the Go
// implementation built to satisfy them (T-111). It has two layers:
//
//   - compliances.go (this file, compiled always): `var _ X = Y` assertions
//     proving the declared Go types still have the shape the contracts
//     derive their interfaces from. Zero test cost, catches drift at build
//     time in every default `go build ./...`.
//   - behaviour probes (*_test.go behind `//go:build conformance`): small
//     cross-package integration tests pinning runtime semantics to contract
//     clauses. Run with:
//     `make test-conformance` (go test -tags conformance ./conformance_check/... -race).
//
// It is a meta-package: plain `package conformance_check`, normal imports
// of internal/* allowed, no code of its own beyond these assertions.
//
// Location note: the backlog leaves T-111's path unspecified; the package
// sits top-level (sibling of internal/) rather than under internal/ or
// test/, because it deliberately imports only exported API — the same
// boundary as an external consumer. Documented as a pragmatic deviation in
// conformance_check/README.md and docs/tasks/backlog.md stays the tracker.
package conformance_check

import (
	"github.com/toledompm/top/internal/cli"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/plan"
	"github.com/toledompm/top/internal/secrets"
	"github.com/toledompm/top/internal/spec"
	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/store/sqlite"
)

// --- Compile-time conformance assertions ---------------------------------
//
// Each block names the contract it defends. If one of these lines stops
// compiling, a contract-visible interface changed shape: fix the code, the
// contract, or — with an ADR — amend both.

// service-spec.md (strict parser) + its "Spawn in domain vocabulary" rule:
// LoadDir consumes spec-YAML and produces resolved domain services.
var _ func(string, ...spec.Option) (spec.Result, error) = spec.LoadDir

// resultShape must keep scroll-tight to spec.Result: the resolved output is
// domain vocabulary plus spec warnings, nothing else. The unnamed-struct
// assignment exploits Go's assignability rule (a defined type may be
// assigned to an *unnamed* struct with the same fields) so this fails to
// compile the moment spec.Result gains another field.
var _ struct {
	Services []domain.Service
	Warnings []spec.Warning
} = spec.Result{}

// state-store.md "Responsibilities": sqlite is the one v0 implementation.
var _ store.Store = (*sqlite.Store)(nil)

// platform-backend.md "Operations — final Go signatures are Runner-shaped":
// FastExec is production, NewFastExec returns it, and all exist on Runner.
var (
	_ exec.Runner = (*exec.FastExec)(nil)
	_ exec.Runner = exec.NewFastExec()
)

// secrets.go's provider seam (ADR-0008, T-108/T-602 pure-addition rule).
var (
	_ secrets.Provider = (*secrets.HostFile)(nil)
	_ secrets.Provider = secrets.VaultProvider{}
)

// reconciler.go's pipeline heart: Compute/ComputeAll signatures exactly.
var (
	_ func(domain.Service, domain.ObservedState, ...plan.Option) domain.ServicePlan                  = plan.Compute
	_ func([]domain.Service, map[domain.ServiceRef]domain.ObservedState, ...plan.Option) domain.Plan = plan.ComputeAll
)

// cli.md: the versioned JSON envelope is schemaVersion 1. Compile-time
// assertion via unsigned-underflow: 1 - JSONSchemaVersion must be zero, and
// a nonzero result is not representable as uint — the file stops compiling
// if the envelope ever changes version without an ADR.
const _ = uint(1 - cli.JSONSchemaVersion)
