# SPDX-License-Identifier: MIT
# laws/PROOF.bend — machine-checked proofs for laws/LAWS.bend (bend v2.0.39).

# `bend PROOF.bend` prints SOME PROOFS FAIL while any law is open or false
# and ALL PROOFS CHECK once every law holds; `--verdict` rechecks with the
# BendTT Lean kernel (needs Lean v4.34.0; see Makefile note).
import Base
import CODE.bend as C
import LAWS.bend as L

# ---- plan_total -------------------------------------------------------------
# Induction over the desired list: the model's ents_of conses exactly one
# entry per service (its kind may stay symbolically computed — the count is
# structural), so length(map) == length(list).

def L.plan_total(d, q, slf):
  match d:
    case Nil{}:
      {==}
    case Con{s, rest}:
      match s:
        case C.OSvc{ref, img, env, sek}:
          %L.plan_total(rest, q, slf) : {(1n + List.length(&2, C.PEnt, C.ents_of(rest, q, slf)) : Nat) == (1n+_) : Nat}
          {==}

# ---- secrets_valueless ------------------------------------------------------
# Four materialized dispatch cases; in each, the emitted entry (if any) is
# name-only, so sec_name_only computes to True.

def L.secrets_valueless(s, has, unres):
  match has unres:
    case False{} False{}:
      {==}
    case False{} True{}:
      {==}
    case True{} False{}:
      {==}
    case True{} True{}:
      {==}

# ---- chg_sec_spec -----------------------------------------------------------
# The membership-based checker and the decision-boolean spec table compute
# the identical dispatch.

def L.chg_sec_spec(s, envn, seu):
  {==}

# ---- prune_protects_core ----------------------------------------------------
# Four materialized decision combinations — the delete entry appears exactly
# in (not self, not core).

def L.prune_protects_core(rns, rnm, inslf, iscore):
  match inslf iscore:
    case False{} False{}:
      {==}
    case False{} True{}:
      {==}
    case True{} False{}:
      {==}
    case True{} True{}:
      {==}

# ---- op_transitions ---------------------------------------------------------
# 25 materialized state pairs: the five table entries compute True on both
# sides; every other pair computes False on both; terminals are closed.

def L.op_transitions(f, t):
  match f t:
    case C.OPending{} C.OPending{}:
      {==}
    case C.OPending{} C.ORunning{}:
      {==}
    case C.OPending{} C.OSucceeded{}:
      {==}
    case C.OPending{} C.OFailed{}:
      {==}
    case C.OPending{} C.OAborted{}:
      {==}
    case C.ORunning{} C.OPending{}:
      {==}
    case C.ORunning{} C.ORunning{}:
      {==}
    case C.ORunning{} C.OSucceeded{}:
      {==}
    case C.ORunning{} C.OFailed{}:
      {==}
    case C.ORunning{} C.OAborted{}:
      {==}
    case C.OSucceeded{} C.OPending{}:
      {==}
    case C.OSucceeded{} C.ORunning{}:
      {==}
    case C.OSucceeded{} C.OSucceeded{}:
      {==}
    case C.OSucceeded{} C.OFailed{}:
      {==}
    case C.OSucceeded{} C.OAborted{}:
      {==}
    case C.OFailed{} C.OPending{}:
      {==}
    case C.OFailed{} C.ORunning{}:
      {==}
    case C.OFailed{} C.OSucceeded{}:
      {==}
    case C.OFailed{} C.OFailed{}:
      {==}
    case C.OFailed{} C.OAborted{}:
      {==}
    case C.OAborted{} C.OPending{}:
      {==}
    case C.OAborted{} C.ORunning{}:
      {==}
    case C.OAborted{} C.OSucceeded{}:
      {==}
    case C.OAborted{} C.OFailed{}:
      {==}
    case C.OAborted{} C.OAborted{}:
      {==}
