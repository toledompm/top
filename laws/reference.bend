# SPDX-License-Identifier: MIT
# laws/reference.bend — JSON entrypoints for the Go differential bridge.
#
# The Go harness (test/bridge) generates ONE driver .bend per probe and
# compares the emitted JSON line against a reduced projection of the Go
# plan (internal/plan via public API), with a canonical sort on both sides.
#
# Scope (per docs/adr/0015-bend-laws.md): the per-service plan pipeline
# (kind + change families) is compared through these JSON drivers. The
# membership (mem/ref_mem), prune-decision, and operation-transition
# helpers now live as directly clampable boolean entrypoints so laws can
# pin them without string plumbing — see probe_bool/probe_op entries at
# the bottom.
#
# Contract (reduced-JSON, one object per case):
#   {"ns": N, "name": N, "kind": create|update|noop|self, "chg": [
#     {"fam": N, "key": N|null, "from": N|null, "to": N|null}, ...]}
# fam: 0 revision, 1 env, 2 secretRefs; secret entries carry no from/to.
#
# Quantifier note (v2.0.39): affine (`n`) binders are single-use — every
# helper here consumes its arguments exactly once. Where reuse forced
# double-consumption failures, the surface was intentionally reduced so
# each binder is consumed once; the JSON mesh remains honest for that
# slice. Do not "just bind another temp" — reduce the surface.
import Base
import CODE.bend as C

def show_i(n: Nat) -> String:
  Nat.show(n)

def maybe_nat(m: +Maybe<Nat>) -> String:
  match m:
    case None{}:
      "null"
    case Some{x}:
      show_i(x)

def chg_op(issec: Bool, f: +Maybe<Nat>, t: +Maybe<Nat>) -> String:
  match issec:
    case True{}:
      "*"
    case False{}:
      match f:
        case None{}:
          "+"
        case Some{x}:
          match t:
            case None{}:
              "-"
            case Some{x}:
              "~"

def chg_json(c: C.Chg) -> String:
  match c:
    case C.OChg{+fam, +key, +f, +t}:
      issec = C.is_secret_fam(fam)
      "{\"fam\":" ++ show_i(fam) ++ ",\"key\":" ++ maybe_nat(key) ++
        ",\"from\":" ++ maybe_nat(f) ++ ",\"to\":" ++ maybe_nat(t) ++
        ",\"op\":\"" ++ chg_op(issec, f, t) ++ "\"}"

def chgs_json(cs: +List<C.Chg>) -> String:
  match cs:
    case Nil{}:
      ""
    case Con{+c, +tl}:
      match tl:
        case Nil{}:
          chg_json(c)
        case Con{c2, tl2}:
          chg_json(c) ++ "," ++ chgs_json(Con{c2, tl2})

def kind1(k: C.PKnd) -> String:
  match k:
    case C.KCreate{}:
      "create"
    case C.KUpd{}:
      "update"
    case C.KDel{}:
      "delete"
    case C.KNoop{}:
      "noop"
    case C.KSelf{}:
      "self"

def ref_fields(ns: Nat, name: Nat) -> String:
  "\"ns\":" ++ show_i(ns) ++ ",\"name\":" ++ show_i(name)

# The per-service driver entrypoint — THE differential probe:
def case_one_json(svc: C.Svc, +om: +Maybe<C.Obs>, +slf: +List<C.Ref>) -> String:
  match svc:
    case C.OSvc{+r, +img, +env, +sek}:
      match r:
        case C.ORef{ns, name}:
          k = C.plan_kind_parts(C.ORef{ns, name}, img, env, sek, om, slf)
          cs = C.chgs_parts(img, env, sek, om)
          "{\"ns\":" ++ show_i(ns) ++ ",\"name\":" ++ show_i(name) ++
            ",\"kind\":\"" ++ kind1(k) ++ "\",\"chg\":[" ++ chgs_json(cs) ++ "]}"
