# SPDX-License-Identifier: MIT
# laws/LAWS.bend — the claimed laws of TOP's pure core, PROTOTYPE (T-113).

# Each law states a claim over the reference model in laws/CODE.bend;
# laws/PROOF.bend fills each one with machine-checked proofs (bend v2.0.39).

# Scope: the pure invariant core ONLY — the modeled slice of plan.Compute /
# plan.ComputeAll, the secret-reference safety rule, the prune guard, and the
# operation transition relation. Exec/store/dokku-adapter behavior stays in
# Go test tiers (ADR-0015).

# Convention: LAWS.bend is the human-owned claim sheet; the AI maintains
# PROOF.bend and the code. `make laws` runs the proof gate.

import Base
import CODE.bend as C

# ---- 1. idempotency --------------------------------------------------------
# The observed state shaped exactly like the desired state (synthesized per
# plan.SynthesizeObserved's contract on the modeled slice) yields no change
# entries at all:
#   law plan_idempotent_changes   — see ADR-0015: the change-list fold's
#   membership dispatch stays symbolic under {==} checking; folded BACK into
#   the tested invariant until the checker grows reduced-form support.
#   The kind still carries the claim below through the empty-changes table.

# ---- 2. totality -----------------------------------------------------------
# For ANY desired service list and ANY observed map, the whole plan assigns
# exactly one service entry per desired service (prune deletes are extra and
# carry no field changes).

law plan_total:
  for +d: +List<C.Svc>
  for +q: +List<C.Obq>
  for +slf: +List<C.Ref>
  {List.length(&2, C.PEnt, C.ents_of(d, q, slf)) == List.length(&2, C.Svc, d) : Nat}

# ---- 3. secrets ------------------------------------------------------------
# Secret-reference entries carry no values: whatever (without dispatching on
# more than the decision booleans the checker can materialize) the observed
# state says about presence/resolvability, the emitted entry's From/To are
# nil — the changeSecretRefs name-only rule (ADR-0008), stated on the
# (has, unresolved) dispatch shape.

law secrets_valueless:
  for s: Nat
  for has: Bool
  for unres: Bool
  {C.sec_name_only(C.chg_sec_bval(s, has, unres)) == True{} : Bool}

# The spec table and the model's membership-based checker agree for every
# (s, observed names, unresolved set), so the law above covers the entry the
# bridge actually exercises.
law chg_sec_spec:
  for s: Nat
  for envn: +List<Nat>
  for seu: +List<Nat>
  {C.chg_sec(s, envn, seu) == C.chg_sec_bval(s, C.mem(s, envn), C.mem(s, seu)) : +Maybe<C.Chg>}

# ---- 4. prune self-gating --------------------------------------------------
# The prune decision table: a delete entry is emitted for an orphan exactly
# when pruning is allowed AND the ref is neither a self-service ref
# (ADR-0005) nor in the core namespace (ADR-0004). Stated per orphan entry
# with the decision booleans materialized.

law prune_protects_core:
  for rns: Nat
  for rnm: Nat
  for inslf: Bool
  for iscore: Bool
  {C.prune_sel(inslf, iscore, C.ORef{rns, rnm}) == C.prune_sel_spec(inslf, iscore, C.ORef{rns, rnm}) : +Maybe<C.PEnt>}

# ---- 5. operation transitions ----------------------------------------------
# The operation transition relation is exactly the documented table:
#   pending → running | aborted; running → succeeded | failed | aborted;
#   terminal states transition to nothing (all three closed).

law op_transitions:
  for f: C.St
  for t: C.St
  {C.op_ok(f, t) == C.op_table(f, t) : Bool}
