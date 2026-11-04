# 0015 — Bend laws: formal invariants for the pure core (PROTOTYPE)

- Status: **accepted** (2026-10-27, owner approval; open questions resolved)
- Date: 2026-10-27; amended 2026-11-04 (owner verdict: laws CI job REQUIRED)
- Deciders: owner + orchestrator
- Toolchain: bend v2.0.39 (installed locally via the official installer)

## Context

The owner asked whether [Bend](https://bend-lang.com) — as of v2, a proof-
carrying language (Lean-style `LAWS.bend`/`PROOF.bend`, BendTT affine
dependent type theory, ~seconds checking) — could carry TOP's testing
review workflow, noting our ADRs are already structured like laws. In v0,
TOP's pure invariant layer (domain vocabulary, plan engine, operation state
machine) is exactly law-shaped: total functions over Data with classical
invariants (idempotency, secrecy, totality).

## Decision (prototype scope, bounded)

1. **`laws/` owns the claims.** `LAWS.bend` = human-written claims; the AI
   maintains `PROOF.bend` + the reference model + the Go-side implementation
   it mirrors; `make laws` is the gate (proof gate + differential bridge).
2. **Five proven laws** cover: plan totality, secret-ref valuelessness,
   secret-checker/spec equivalence, prune protection for `core`/self, and
   the operation transition table. Plan-idempotency over the change *list*
   is a property test for now (the checker's equality refinement stays
   symbolic on that fold — recorded in `laws/README.md`).
3. **The differential bridge is the fidelity mechanism.** A proof over a
   reference model procures nothing by itself: `test/bridge` reduces random
   cases to a shared tag schema and deep-compares both engines. It caught
   REAL model bugs during the prototype (membership always-False via a
   base-case AND; inverted env branch; JSON malformations) — precisely the
   class the checker alone cannot see, because it only certifies stated
   claims.
4. **AGENTS.md carries the instructions + scope guard**: laws cover the
   pure core only; exec/store/adapter behavior remains in Go test tiers; a
   reference-model change must keep `test/bridge` green.

## Alternatives considered

- **Status quo (property tests only, T-110/T-111)** — worked, but invariants
  stay sampled, not stated; the owner wants law-grade precision for
  review.
- **OPA/Rego or CUE policy engines** — good for *checking artifacts*
  (configs), wrong for *proving program-level invariants* over recursive
  domain functions; no equivalent of `{==}`-style computational equalities
  over our own domain types.
- **Lean/Rocq properly** — true gold standard; minutes-scale checking and a
  heavy ecosystem — disproportional for one appliance project.
- **All code in Bend** — explicitly rejected by the scope guard: Bend v2 is
  young, its I/O story is early, and the platform's real complexity is in
  the operating envelope (Dokku hosts, failures), not the pure core.

## Rationale and trade-offs

The pure core is small, total, and worth theorem-grade review; everything
else is better served by integration tests where they live. Bend's ~second
proof checking fits the agent loop the product is designed around. Costs
accepted deliberately: one more language/toolchain (version-pinned), young
tooling risk (owner-directed: escalate upstream issues), supply chain (the
installer is `curl | sh` — CI uses the pinned release; a digest-pinned
vendored install is the documented hardening option), and the +1000-shift /
reduced-surface quantifier workarounds are honest patches recorded in
`laws/README.md`, to be revisited when the checker matures.

## Consequences

- `make laws` + CI: bend in a separate CI job (graceful skip when absent
  locally; go test remains hermetic); CI supply-chain note above.
- ADR-0014's hygiene sweep now applies to `laws/` too (machine-audited for
  private paths — the prototype caught and fixed an absolute-path import).
- Future law additions follow the review doc: state each claim in
  `LAWS.bend`, prove it OR fold it back into a property test (recorded in
  the law table); never leave a law open in a merged commit.

## Resolved owner decisions (2026-10-27)

- The differential bridge ships as a separate optional CI job now
  (installs the pinned bend release; graceful skip when absent locally);
  it becomes *release-gating* after Milestone 3, when ObservedState
  extends and the bridge covers more of Compute.
- GPU/CUDA CI: never required for v0 laws (CPU mode suffices).

## Amendment 2026-11-04 (owner verdict — laws job REQUIRED)

The M3-carried "make laws CI REQUIRED-vs-optional" question is resolved:
the `laws` CI job (`.github/workflows/ci.yml`) is **REQUIRED** — a
non-optional PR/push gate, not a noted-only job. ADR-0015 makes no
proof-vs-bridge distinction inside CI: `make laws` is one gate
(proof gate + differential bridge) and is blessed as a whole, so the
bridge runs in CI too — the 2026-10-27 "release-gating after Milestone 3"
clause is satisfied early by this single required job. The gate runs on
every push/PR (not paths-filtered): it takes seconds, and its scope guard
(`laws/`, `internal/domain`, `internal/plan`) exceeds a fixed path list.
Two intended contrasts stand unchanged: the graceful skip remains a
**local** convenience (Makefile), never a CI behavior; and if the
`curl | sh` bend install breaks upstream, the documented hardening option
(digest-pinned vendored install) is the fix — an unavailable toolchain
fails the job rather than silently voiding the gate.
