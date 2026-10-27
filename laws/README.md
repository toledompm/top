# TOP's formal laws (Bend)

Machine-checked invariants over TOP's pure core, written in
[Bend](https://bend-lang.com) (v2.0.39) — `LAWS.bend` is the human-owned
claim sheet; `PROOF.bend` carries machine-checked proofs; `CODE.bend` is the
in-Bend reference model of the plan/domain slice; `reference.bend` renders
the JSON entrypoints the Go differential bridge drives. Scope: **the pure
invariant core only** — exec, store, and the Dokku adapter behave nothing
like these laws and stay in the Go test tiers (ADR-0012).

## The gate

```bash
make laws   # bend PROOF.bend  +  go test ./test/bridge/
```

`bend PROOF.bend` prints `SOME PROOFS FAIL` while any law is open or false
and `ALL PROOFS CHECK` once every law holds (`--verdict` rechecks with the
Lean kernel — needs Lean v4.34.0, optional locally). The differential bridge
property-tests Go's `internal/plan` against the reference model and fails
CI on divergence.

## Current laws

| Law | Claim |
| --- | --- |
| `plan_total` | Every reconciliation plan assigns exactly one service entry per desired service (structural induction). |
| `secrets_valueless` | Plan entries describing a secretRef never carry From/To values (four materialized dispatch cases). |
| `chg_sec_spec` | The membership-driven secret checker agrees with the documented spec table for every input. |
| `prune_protects_core` | A delete entry is emitted for an orphan iff pruning is allowed AND the ref is neither the controller-self ref nor in namespace `core`. |
| `op_transitions` | Operation lifecycle obeys the documented transition table exactly (25 materialized state pairs). |
| *(tested, not proved)* | Plan idempotency over the modeled **change list** is currently a property test (`internal/plan`, `conformance_check`) — the checker's `==` refinement stayed symbolic over the change-list fold; revisit when Bend grows reduced-form support. |

## The fidelity rule (honest part)

A proof over a reference model is only as strong as the model's fidelity to
the real implementation. That is what `test/bridge` enforces: random
desired/observed cases reduced to a shared tag schema, computed by BOTH
engines byte-for-byte, divergence = failure. Found this way during the
prototype: a verified-looking reference model that always returned FALSE
from membership (the cons case ANDed in the Nil base) would have silently
disabled orphan detection and prune gating — the checker certifies exactly
the *stated* laws, nothing else. Keep the bridge green whenever you touch
`laws/`, `internal/domain`, or `internal/plan`.
