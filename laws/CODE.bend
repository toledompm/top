# SPDX-License-Identifier: MIT
# laws/CODE.bend — Bend reference model of TOP's pure core (v0 prototype).

# A deliberately SMALL model of the invariant-relevant slice of
# internal/plan + internal/domain: identity, the diff over the modeled
# families (revision / plain env / secret refs), synthesis of a converged
# observed state, the per-service plan kind, whole-plan assembly (one entry
# per desired service, orphaning + optional prune) and the operation
# transition relation.

# It is NOT a reimplementation of TOP: ports, routing domains, networks,
# storage, restart policy and resources are outside the model — the bridge
# fidelity boundary is documented in docs/adr/0015-bend-laws.md.

# Encoding: modeled identities are Nat tags. ns tags: 0n = "core" (reserved
# namespace), 1n+ = fictional namespaces. The Go bridge in test/bridge maps
# tags to fictional names and drives the JSON entrypoints in
# laws/reference.bend.

# Model contract: plan inputs must be pre-sorted (env pairs by key tag,
# secret names ascending, desired services by ref, observed records by ref).
# Sorting is a presentation concern; plan.Compute sorts internally and its
# own Go tests cover determinism, so the model always receives sorted input
# here and emits entries in input order.

# Bend v2 style: a match only scrutinizes parameters and
# pattern-destructured binders, so computed-Bool dispatch goes through
# Bool.and / Bool.or / Bool.pick (as the Base library itself does). Defs
# appear in bottom-up order; self-recursion is structural.

import Base

# ---- identity (domain.ServiceRef, reduced) --------------------------------

# ns 0n = "core" (reserved: never pruned, ADR-0004); 1n+ = user namespaces.
# name is the service-name tag. ns/name are fiction-mapped by the bridge.
type Ref is Data:
  ORef{ns: Nat, name: Nat}

# One desired plain env entry: key and value tags. (ObservedState never
# carries values, so values appear only on the desired side.)
type PE is Data:
  OPE{k: Nat, v: Nat}

# Desired service, reduced slice of domain.Service:
#   img — pinned revision tag (image source or full-sha build); None models
#         "not comparable here" (branch-pinned build), as desiredRevision
#         does in Compute.
#   env — plain env entries, sorted by key tag
#   sek — secretRef name tags, sorted ascending
type Svc is Data:
  OSvc{ref: Ref, img: +Maybe<Nat>, env: +List<PE>, sek: +List<Nat>}

# Observed state, reduced slice of domain.ObservedState:
#   img  — revision tag reported by the backend (None = nothing recorded)
#   envn — env key NAMES present (values never observed; ADR-0008)
#   seu  — secret-ref names present but unresolvable (empty checksum)
# An absent app is modeled by Maybe<Obs> = None (ObservedState.Absent()).
type Obs is Data:
  OObs{img: +Maybe<Nat>, envn: +List<Nat>, seu: +List<Nat>}

# One (ref, observed?) record of the bulk-inspection map.
type Obq is Data:
  OObq{ref: Ref, obs: +Maybe<Obs>}

# plan kinds (domain.ChangeKind): create/update/delete/noop/drifted(self).
type PKnd is Data:
  KCreate{}
  KUpd{}
  KDel{}
  KNoop{}
  KSelf{}

# Change entry, reduced FieldChange: fam 0n = revision, 1n = env, 2n =
# secretRefs. key is the entry's parameter tag (env key / secret ref name);
# from/to are the value tags (None = the pointer is nil). Secret entries are
# name-only by construction, so from/to are None in every case (the
# secrets_valueless law).
type Chg is Data:
  OChg{fam: Nat, key: +Maybe<Nat>, from: +Maybe<Nat>, to: +Maybe<Nat>}

# Per-service plan entry + whole-plan model. The model plan keeps `ents`
# (one per desired service, in desired order) and `dels` (prune-permitted
# orphans) separate; `orph` is the always-reported orphan list. ComputeAll's
# sortPlans is the presentation merge documented under Model contract; the
# bridge normalizes order on both sides before comparing.
type PEnt is Data:
  OEnt{ref: Ref, kind: PKnd}

type Plan is Data:
  OPlan{ents: +List<PEnt>, dels: +List<PEnt>, orph: +List<Ref>}

# Operation lifecycle (domain.OperationState): pending/running are live,
# succeeded/failed/aborted terminal (state-store contract).
type St is Data:
  OPending{}
  ORunning{}
  OSucceeded{}
  OFailed{}
  OAborted{}

# ---- membership ------------------------------------------------------------

def mem(+x: Nat, xs: +List<Nat>) -> Bool:
  match xs:
    case Nil{}:
      False{}
    case Con{h, +t}:
      Bool.pick(Bool, Nat.is_eq(h, x),
                True{},
                mem(x, t))

def ref_mem2(+xns: Nat, +xnm: Nat, xs: +List<Ref>) -> Bool:
  match xs:
    case Nil{}:
      False{}
    case Con{h, +t}:
      match h:
        case ORef{hns, hnm}:
          Bool.pick(Bool,
                    Bool.and(Nat.is_eq(hns, xns), Nat.is_eq(hnm, xnm)),
                    True{},
                    ref_mem2(xns, xnm, t))

def ref_mem(+x: Ref, xs: +List<Ref>) -> Bool:
  match x:
    case ORef{xns, xnm}:
      ref_mem2(xns, xnm, xs)

# ---- synthesis (mirror of plan.SynthesizeObserved on the modeled slice) ----

# env names of desired: plain env pair names ++ secret ref names.
def env_names(env: +List<PE>, sek: +List<Nat>) -> +List<Nat>:
  match env:
    case Nil{}:
      sek
    case Con{e, rest}:
      match e:
        case OPE{k, v}:
          Con{k, env_names(rest, sek)}

# synthesize: what a correct backend Inspect reports for an app converged to
# the desired Service: image equal, every plain env key present, every
# secretRef present with a checksum (never in seu). Values ship nowhere.
def synthesize(d: Svc) -> +Maybe<Obs>:
  match d:
    case OSvc{ref, img, env, sek}:
      Some{OObs{img, env_names(env, sek), Nil{}}}

# ---- per-family change entries (mirror of Compute's comparers) -------------

# Mirror of changeRevision: want None is not comparable; equal computed tags
# converge; changed tags are a modify with both values; an unreported observed
# revision is a modify with an absent From.
def chg_rev(dimg: +Maybe<Nat>, oimg: +Maybe<Nat>) -> +Maybe<Chg>:
  match dimg oimg:
    case None{} _:
      None{}
    case Some{+di} None{}:
      Some{OChg{0n, None{}, None{}, Some{di}}}
    case Some{+di} Some{+oi}:
      Bool.pick(+Maybe<Chg>, Nat.is_eq(oi, di),
                None{},
                Some{OChg{0n, None{}, Some{oi}, Some{di}}})

# Mirror of changeEnv for one plain key: absent from observed key names →
# add entry (To = the desired value tag); present → converged (env value
# drift is not detectable in ObservedState v0 — a documented gap, not
# modeled here).
def chg_env(+k: Nat, v: Nat, envn: +List<Nat>) -> +Maybe<Chg>:
  Bool.pick(+Maybe<Chg>, mem(k, envn),
            None{},
            Some{OChg{1n, Some{k}, None{}, Some{v}}})

# Mirror of changeSecretRefs for one name: not an observed env key at all →
# add (marker "+"); present but unresolvable → modify (marker "", forced
# re-set); present with a checksum → converged. Entries are name-only:
# from/to are None in every case (the secrets_valueless law).
def chg_sec2(t: Bool, +s: Nat) -> +Maybe<Chg>:
  Bool.pick(+Maybe<Chg>, t,
            Some{OChg{2n, Some{s}, None{}, None{}}},
            None{})

def chg_sec(+s: Nat, envn: +List<Nat>, seu: +List<Nat>) -> +Maybe<Chg>:
  Bool.pick(+Maybe<Chg>, mem(s, envn),
            chg_sec2(mem(s, seu), s),
            Some{OChg{2n, Some{s}, None{}, None{}}})

# maybe-cons of a per-entry match: nil drops, some conses onto an already
# built tail (this keeps change lists a simple structural fold).
def chg_cons(c: +Maybe<Chg>, tl: +List<Chg>) -> +List<Chg>:
  match c:
    case None{}:
      tl
    case Some{cc}:
      Con{cc, tl}

# Change list per family, in the pre-sorted input order (Model contract).
def chgs_env(env: +List<PE>, +envn: +List<Nat>) -> +List<Chg>:
  match env:
    case Nil{}:
      Nil{}
    case Con{e, rest}:
      match e:
        case OPE{k, v}:
          chg_cons(chg_env(k, v, envn), chgs_env(rest, envn))

def chgs_sec(sek: +List<Nat>, +envn: +List<Nat>, +seu: +List<Nat>) -> +List<Chg>:
  match sek:
    case Nil{}:
      Nil{}
    case Con{s, rest}:
      chg_cons(chg_sec(s, envn, seu), chgs_sec(rest, envn, seu))

# maybe1: a Maybe<Chg> as a 0/1-element list.
def maybe1(m: +Maybe<Chg>) -> +List<Chg>:
  match m:
    case None{}:
      Nil{}
    case Some{c}:
      Con{c, Nil{}}

def app(xs: +List<Chg>, ys: +List<Chg>) -> +List<Chg>:
  match xs:
    case Nil{}:
      ys
    case Con{h, +t}:
      Con{h, app(t, ys)}

# The full single-service change list: revision + env + secretRef families.
# (Compute sorts the final list by path — presentation; the model emits the
# family-grouped order and the bridge's normalizer reorders both sides.)
def chgs_one(d: Svc, om: +Maybe<Obs>) -> +List<Chg>:
  match d:
    case OSvc{ref, img, env, sek}:
      match om:
        case None{}:
          app(app(maybe1(chg_rev(img, None{})), chgs_env(env, Nil{})),
              chgs_sec(sek, Nil{}, Nil{}))
        case Some{o}:
          match o:
            case OObs{oimg, +envn, +seu}:
              app(app(maybe1(chg_rev(img, oimg)), chgs_env(env, envn)),
                  chgs_sec(sek, envn, seu))

# ---- per-service plan kind (mirror of Compute) ----------------------------

def chgs_parts(img: +Maybe<Nat>, env: +List<PE>, sek: +List<Nat>,
               om: +Maybe<Obs>) -> +List<Chg>:
  match om:
    case None{}:
      app(app(maybe1(chg_rev(img, None{})), chgs_env(env, Nil{})),
          chgs_sec(sek, Nil{}, Nil{}))
    case Some{o}:
      match o:
        case OObs{oimg, +envn, +seu}:
          app(app(maybe1(chg_rev(img, oimg)), chgs_env(env, envn)),
              chgs_sec(sek, envn, seu))

# Mirror of Compute's kind selection. Absent observed → create regardless of
# self (Compute's create path short-circuits the self check); otherwise
# changes decide, with WithSelfService upgrading drift to drifted(self).
def plan_kind_parts(+ref: Ref, +img: +Maybe<Nat>, env: +List<PE>, sek: +List<Nat>,
                    om: +Maybe<Obs>, +slf: +List<Ref>) -> PKnd:
  match om:
    case None{}:
      KCreate{}
    case Some{x}:
      cs = chgs_parts(img, env, sek, Some{x})
      empty = List.is_empty(&2, Chg, cs)
      bs = ref_mem(ref, slf)
      Bool.pick(PKnd, empty,
                KNoop{},
                Bool.pick(PKnd, bs, KSelf{}, KUpd{}))

def plan_kind(+d: Svc, om: +Maybe<Obs>, +slf: +List<Ref>) -> PKnd:
  match d:
    case OSvc{ref, img, env, sek}:
      plan_kind_parts(ref, img, env, sek, om, slf)

# The single-service change list from a service's parts, so plan_kind
# need not pass a consumed service value onward.
# ---- whole-plan assembly (mirror of ComputeAll) ----------------------------

# join: the bulk-inspection map's record for one ref (Compute's map lookup).
def join(+r: Ref, q: +List<Obq>) -> +Maybe<Obs>:
  match q:
    case Nil{}:
      None{}
    case Con{h, +t}:
      match h:
        case OObq{href, hobs}:
          Bool.pick(+Maybe<Obs>, ref_mem(href, Con{r, Nil{}}), hobs,
                    join(r, t))

# desired refs as a plain list (the membership set for orphaning).
def drefs(d: +List<Svc>) -> +List<Ref>:
  match d:
    case Nil{}:
      Nil{}
    case Con{s, rest}:
      match s:
        case OSvc{+ref, img, env, sek}:
          Con{ref, drefs(rest)}

# Observed records whose ref is absent from Git, in input (sorted) order —
# ComputeAll sorts observed keys first; the model receives them sorted.
def orphan_of(q: +List<Obq>, +drefs: +List<Ref>) -> +List<Ref>:
  match q:
    case Nil{}:
      Nil{}
    case Con{h, +t}:
      match h:
        case OObq{+href, hobs}:
          Bool.pick(+List<Ref>, ref_mem(href, drefs),
                    orphan_of(t, drefs),
                    Con{href, orphan_of(t, drefs)})

# Mirror of the prune branch (ComputeAll's Services loop): with
# WithPruneAllowed, every non-core orphan not marked self gets a delete
# entry; core is `ns == 0n` (domain.CoreNamespace). Non-permitted orphans are
# reported only. Note the Go code order: the self check runs first, so a
# self ref skips the core check too — the model mirrors it.
def prune_dels(orph: +List<Ref>, +slf: +List<Ref>) -> +List<PEnt>:
  match orph:
    case Nil{}:
      Nil{}
    case Con{r, +t}:
      match r:
        case ORef{+rns, +rnm}:
          Bool.pick(+List<PEnt>, ref_mem(ORef{rns, rnm}, slf),
                    prune_dels(t, slf),
                    Bool.pick(+List<PEnt>, mem(rns, Con{0n, Nil{}}),
                              prune_dels(t, slf),
                              Con{OEnt{ORef{rns, rnm}, KDel{}}, prune_dels(t, slf)}))

# One per-service entry (ComputeAll's Services for desired services).
def ents_of(d: +List<Svc>, +q: +List<Obq>, +slf: +List<Ref>) -> +List<PEnt>:
  match d:
    case Nil{}:
      Nil{}
    case Con{s, rest}:
      match s:
        case OSvc{+ref, +img, +env, +sek}:
          Con{OEnt{ref, plan_kind_parts(ref, img, env, sek, join(ref, q), slf)},
              ents_of(rest, q, slf)}

# The whole-plan model, mirroring ComputeAll on the modeled slice.
def plan_model(+d: +List<Svc>, +q: +List<Obq>, +slf: +List<Ref>, prune: Bool) -> Plan:
  +orph = orphan_of(q, drefs(d))
  dels = prune_dels(orph, slf)
  OPlan{ents_of(d, q, slf), dels, orph}

# ---- operation transition relation (mirror of domain.CanTransition) --------

# The documented table (domain.OperationState doc comment, contracts):
#   pending  → running | aborted
#   running  → succeeded | failed | aborted
#   terminal states transition to nothing.
# The law-side checks this against a LIST OF PAIRS table (laws/PROOF.bend
# uses it as the spec shape; laws/LAWS.bend states the equality law).
def op_pend(t: St) -> Bool:
  match t:
    case ORunning{}:
      True{}
    case OAborted{}:
      True{}
    case _:
      False{}


def op_run(t: St) -> Bool:
  match t:
    case OSucceeded{}:
      True{}
    case OFailed{}:
      True{}
    case OAborted{}:
      True{}
    case _:
      False{}

def op_ok(f: St, t: St) -> Bool:
  match f:
    case OPending{}:
      op_pend(t)
    case ORunning{}:
      op_run(t)
    case _:
      False{}


# ---- law-side spec functions (the invariant tables the LAWS check) ---------

# The secret-entry SPEC as defined values: (has, unresolved) dispatch — the
# exact ternary decision changeSecretRefs implements; entries are name-only
# (from/to None) in every branch.
def chg_sec_bval(+s: Nat, has: Bool, unres: Bool) -> +Maybe<Chg>:
  Bool.pick(+Maybe<Chg>, has,
            chg_sec2(unres, s),
            Some{OChg{2n, Some{s}, None{}, None{}}})

# sec_name_only: the name-only safety property — no entry carries a from/to
# value in any case; None vacuously holds.
def maybe_eq_none(m: +Maybe<Nat>) -> Bool:
  match m:
    case None{}:
      True{}
    case Some{x}:
      False{}

def sec_name_only(m: +Maybe<Chg>) -> Bool:
  match m:
    case None{}:
      True{}
    case Some{c}:
      match c:
        case OChg{fam, key, cfrom, cto}:
          Bool.and(maybe_eq_none(cfrom), maybe_eq_none(cto))

# The operation transition table as a LIST of (from, to) entries (the spec
# shape the op_transitions law verifies against): pending->running/aborted;
# running->succeeded/failed/aborted; terminal states closed.
type TE is Data:
  OTE{f: St, t: St}

def op_st_eq(a: St, b: St) -> Bool:
  match a b:
    case OPending{} OPending{}:
      True{}
    case ORunning{} ORunning{}:
      True{}
    case OSucceeded{} OSucceeded{}:
      True{}
    case OFailed{} OFailed{}:
      True{}
    case OAborted{} OAborted{}:
      True{}
    case _ _:
      False{}

def op_tab_app() -> +List<TE>:
  Con{OTE{OPending{}, ORunning{}},
      Con{OTE{OPending{}, OAborted{}},
          Con{OTE{ORunning{}, OSucceeded{}},
              Con{OTE{ORunning{}, OFailed{}},
                  Con{OTE{ORunning{}, OAborted{}},
                      Nil{}}}}}}

def op_tab() -> +List<TE>:
  op_tab_app()

# tab_has: is the pair (from, to) recorded in the table?
def tab_has(+f: St, +t: St, xs: +List<TE>) -> Bool:
  match xs:
    case Nil{}:
      False{}
    case Con{e, rest}:
      match e:
        case OTE{ef, et}:
          Bool.or(Bool.and(op_st_eq(f, ef), op_st_eq(t, et)),
                  tab_has(f, t, rest))

def op_table(f: St, t: St) -> Bool:
  tab_has(f, t, op_tab())

def is_secret_fam(fam: Nat) -> Bool:
  Nat.is_eq(fam, 2n)

# The prune decision for one orphan entry: with WithPruneAllowed, an orphan
# r gets a delete entry exactly when it is neither a self-service ref
# (ADR-0005) nor in the core namespace (ADR-0004, ns tag 0n). inslf and
# iscore are the two decision booleans the whole-plan evaluator computes;
# the model keeps them explicit so the law can case-split materialized
# verdicts instead of symbolic list searches.
def prune_sel(inslf: Bool, iscore: Bool, r: Ref) -> +Maybe<PEnt>:
  Bool.pick(+Maybe<PEnt>, inslf,
            None{},
            Bool.pick(+Maybe<PEnt>, iscore,
                      None{},
                      Some{OEnt{r, KDel{}}}))

# prune_sel_spec: the documented decision table, computed conventionally.
def prune_sel_spec_ins(inslf: Bool, iscore: Bool, r: Ref) -> +Maybe<PEnt>:
  Bool.pick(+Maybe<PEnt>, iscore,
            None{},
            Some{OEnt{r, KDel{}}})

def prune_sel_spec(+inslf: Bool, iscore: Bool, r: Ref) -> +Maybe<PEnt>:
  Bool.pick(+Maybe<PEnt>, inslf,
            None{},
            prune_sel_spec_ins(inslf, iscore, r))
