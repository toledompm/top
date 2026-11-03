// Package plan is TOP's diff/plan computation: the pure function that turns
// (desired, observed) into a domain.Plan without touching the host, disk, or
// network. It is the implementation of the reconciler contract's Diff step
// and of `topctl diff` — which is, by ADR-0002's parity guarantee, exactly
// the plan reconcilers Apply iterates over, with zero side effects.
//
// Drift scope (ADR-0002): app existence, revision/image, env keys (names +
// plain values; secrets by reference presence), ports, routing domains,
// storage mounts, restart policy, resource limits, network attachments.
// Anything domain.ObservedState does not carry in v0 (healthcheck, rollout
// shape, mount sizes, routing.mode itself) is simply not compared — see the
// coverage matrix in the package README.
//
// The package is pure: stdlib + internal/domain imports only, no I/O of any
// kind. Every output ordering is deterministic (field changes sorted by
// path, service plans sorted by ServiceRef), so the same inputs always
// produce byte-identical plans — the tested idempotency invariant's sibling.
package plan

import (
	"sort"
	"strconv"
	"strings"

	"github.com/toledompm/top/internal/domain"
)

// internalNetwork is the shared docker network ADR-0002 has TOP manage for
// `routing.mode: internal` services. v0 hardcodes the name here until the
// backend (M3) exposes the configured value; the backend owns the real
// `network:set` invocation and must agree.
const internalNetwork = "top-internal"

// options is the resolved value of Compute/ComputeAll's Option arguments.
type options struct {
	// pruneAllowed: `topctl reconcile --prune` was passed. Prune is never
	// implicit (contracts/reconciler.md): without this option, orphaned
	// services surface as Plan.Orphaned reports only.
	pruneAllowed bool
	// self names the controller-self-service refs (ADR-0005): drift on
	// them yields kind drifted(self) — reported, never auto-applied.
	self map[domain.ServiceRef]bool
}

// Option tunes Compute / ComputeAll.
type Option func(*options)

// WithPruneAllowed allows PlanDelete service plans for orphaned apps.
// Default false: pruning is never implicit (ADR-0002, ADR-0004).
func WithPruneAllowed(v bool) Option { return func(o *options) { o.pruneAllowed = v } }

// WithSelfService marks ref as TOP's own controller service (ADR-0005):
// any drift on it produces a drifted(self) plan that a human must act on;
// the plan carries the same field-level changes as a report but no caller
// honoring the contract derives any apply (least of all a destructive
// one) from it.
func WithSelfService(ref domain.ServiceRef) Option {
	return func(o *options) {
		if !ref.IsZero() {
			o.self[ref] = true
		}
	}
}

// Compute diffs one resolved desired Service against the observed
// ObservedState the PlatformBackend.Inspect contract returned for it.
//
//   - observed.Absent() (app does not exist / nothing deployed) ⇒
//     PlanCreate with every comparable field as an add entry;
//   - otherwise an empty field diff ⇒ PlanNoop (the idempotency
//     invariant: an observed state shaped exactly like the desired state
//     yields an empty plan — see the invariant test);
//   - otherwise PlanUpdate, or PlanDriftedSelf when WithSelfService
//     marked the service as TOP's own controller. drifted(self) plans
//     keep their change entries (they are the report) but are consumed
//     only by humans.
//
// Compute never errors: both inputs are already validated upstream (spec
// loading, backend contract). Unrepresentable comparisons (invalid source
// shapes, unreported observed fields) are skipped rather than guessed —
// the README's coverage matrix is the contract for what is compared.
func Compute(desired domain.Service, observed domain.ObservedState, opts ...Option) domain.ServicePlan {
	o := resolveOptions(opts)

	if observed.Absent() {
		// The app is not there: converge-by-create. The same field diff
		// against an empty observed state yields pure add entries.
		return domain.ServicePlan{
			Ref:     desired.Ref,
			Kind:    domain.PlanCreate,
			Changes: diffFields(desired, domain.ObservedState{}),
		}
	}

	changes := diffFields(desired, observed)
	kind := domain.PlanNoop
	switch {
	case len(changes) == 0:
		// noop; the plan below carries no changes.
	case o.self[desired.Ref]:
		kind = domain.PlanDriftedSelf // reported, human-gated (ADR-0005)
	default:
		kind = domain.PlanUpdate
	}
	return domain.ServicePlan{Ref: desired.Ref, Kind: kind, Changes: changes}
}

// ComputeAll plans a whole reconciliation: one service plan per desired
// service (sorted by ServiceRef) plus orphaned-reports / optional deletes
// for observed apps absent from Git.
//
// A missing entry in observed is treated as a never-deployed app ⇒ create
// plan. Services absent from Git are recorded in Plan.Orphaned always; only
// when WithPruneAllowed was passed does each non-core orphaned service also
// get a PlanDelete service plan (core is never pruned, ADR-0004, and a
// self-service ref is never either). Delete plans carry no field changes:
// they remove the whole app or nothing.
//
// Plan.Revision is left to the caller (only the reconciler knows the
// resolved sha at that point); duplicate refs in desired are not detected
// here — DesiredState.Validate owns that.
func ComputeAll(desired []domain.Service, observed map[domain.ServiceRef]domain.ObservedState, opts ...Option) domain.Plan {
	o := resolveOptions(opts)

	p := domain.Plan{}

	desiredSet := make(map[domain.ServiceRef]bool, len(desired))
	plans := make([]domain.ServicePlan, 0, len(desired))
	for _, svc := range desired {
		desiredSet[svc.Ref] = true
		obs, ok := observed[svc.Ref]
		if !ok {
			obs = domain.ObservedState{Ref: svc.Ref} // missing ⇒ never deployed
		}
		plans = append(plans, Compute(svc, obs, opts...))
	}
	p.Services = plans

	// Orphans: observed apps absent from Git, reported always; pruned (as
	// delete plans) only on request and only on non-core namespaces.
	refs := make([]domain.ServiceRef, 0, len(observed))
	for ref := range observed {
		if !desiredSet[ref] && ref.Validate() == nil {
			refs = append(refs, ref)
		}
	}
	sortRefs(refs)
	p.Orphaned = refs
	if o.pruneAllowed {
		for _, ref := range refs {
			if ref.Namespace == domain.CoreNamespace || o.self[ref] {
				continue // never pruned (ADR-0004/0005); report only
			}
			p.Services = append(p.Services, domain.ServicePlan{Ref: ref, Kind: domain.PlanDelete})
		}
	}
	sortPlans(p.Services)
	return p
}

func resolveOptions(opts []Option) options {
	o := options{self: map[domain.ServiceRef]bool{}}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// diffFields builds every field-level change between desired and observed,
// sorted by path for determinism. Each comparer below owns exactly one
// field family; final sorting makes section order irrelevant.
func diffFields(d domain.Service, o domain.ObservedState) []domain.FieldChange {
	var cs []domain.FieldChange
	cs = append(cs, changeRevision(d, o)...)
	cs = append(cs, changeEnv(d, o)...)
	cs = append(cs, changeSecretRefs(d, o)...)
	cs = append(cs, changePorts(d, o)...)
	cs = append(cs, changeDomains(d, o)...)
	cs = append(cs, changeStorage(d, o)...)
	cs = append(cs, changeRestartPolicy(d, o)...)
	cs = append(cs, changeResources(d, o)...)
	cs = append(cs, changeNetworks(d, o)...)
	sortChanges(cs)
	return cs
}

// ---- revision / image -------------------------------------------------

// desiredRevision renders what Compute expects to find deployed: the pinned
// image reference for image sources, or the build ref for source builds.
// A branch-pinned build ref (not a full commit sha) returns "" — a moving
// ref cannot be compared against the recorded deploy sha without resolving
// it first; the reconciler's resolve step does that, not the planner
// (ADR-0002).
func desiredRevision(d domain.Service) string {
	switch {
	case d.Source.Image != "":
		return d.Source.Reference()
	case d.Source.Build != nil:
		if isFullSHA(d.Source.Build.Ref) {
			return d.Source.Build.Ref
		}
		return ""
	default:
		return "" // invalid source shape; spec validation owns it
	}
}

// isFullSHA reports whether s is a full-length (40 or 64 hex char) commit
// sha — long enough that comparing it against a recorded deploy sha is
// meaningful. Shorter forms are display vocabulary, not pinning vocabulary.
func isFullSHA(s string) bool {
	return (len(s) == 40 || len(s) == 64) && domain.ValidRevision(s) == nil
}

// observedRevision is the best source of "what is actually deployed": the
// source-image reference for image deploys, else the git sha.
func observedRevision(o domain.ObservedState) string {
	if o.Image != "" {
		return o.Image
	}
	return o.Revision
}

// changeRevision emits the `revision` change when the deployed revision
// differs from the desired one — covering sha drift, digest/tag change,
// and source-type switches (git build ↔ image).
func changeRevision(d domain.Service, o domain.ObservedState) []domain.FieldChange {
	want := desiredRevision(d)
	if want == "" {
		return nil // branch-pinned or invalid source: not comparable yet
	}
	have := observedRevision(o)
	if have == want {
		return nil
	}
	return []domain.FieldChange{fieldChange("revision", have, want)}
}

// ---- env --------------------------------------------------------------

// envValueChange builds the add/modify entry for one desired plain env key.
// To always carries a pointer (env values may legitimately be the empty
// string, which is still a value, not an absence).
func envValueChange(key, to string) domain.FieldChange {
	c, err := domain.NewFieldChange("env."+key, nil, &to)
	if err != nil {
		panic("plan: " + err.Error())
	}
	return c
}

// changeEnv compares desired plain env keys against ObservedState.EnvKeys.
//
// ObservedState v0 carries env *names only* (redaction, ADR-0008; "drift
// detection compares names/checksums, not values") — so:
//
//   - a desired key missing on the host ⇒ add entry, To = the plain
//     desired value (plain values live in Git; they are not secret);
//   - a desired key present on the host ⇒ assumed converged. Value drift
//     is not detectable without a value- or checksum-bearing observed
//     report — a documented v0 gap (README), closed by a backend env
//     checksum in M3;
//   - extra observed keys ⇒ left alone: Dokku env is a sink (ADR-0008).
//     TOP cannot tell a manually-set key from the residue of a secret ref
//     an operator just dropped from the spec, and never silently purges
//     env it did not set this run. Removal detection relative to the
//     recorded last-applied state belongs to the store-backed reconciler
//     (M3), not to (desired, observed)-only Compute.
func changeEnv(d domain.Service, o domain.ObservedState) []domain.FieldChange {
	var cs []domain.FieldChange
	for _, k := range sortedKeys(d.Env) {
		if envHasKey(o, k) {
			continue
		}
		cs = append(cs, envValueChange(k, d.Env[k]))
	}
	return cs
}

// envHasKey reports whether the observed config carries a key name.
func envHasKey(o domain.ObservedState, key string) bool {
	for _, k := range o.EnvKeys {
		if k == key {
			return true
		}
	}
	return false
}

// ---- secret refs -------------------------------------------------------

// changeSecretRefs compares desired secretRef *names* (ADR-0008: secrets
// are referenced, never valued) against the host:
//
//   - ref name absent from the observed config ⇒ add change (name-only);
//   - ref present but ObservedState marks its checksum "" (the backend
//     explicitly reports the reference as unresolvable on the host) ⇒
//     modify change so the apply re-sets and re-injects it;
//   - ref present with a checksum ⇒ converged. Detecting *rotation* (the
//     secret's value changed on the host) requires comparing against the
//     store's recorded checksum — upstream reconciler state that pure
//     Compute does not see (README gap, M3).
//
// Every emitted change goes through domain.NewSecretFieldChange: name +
// marker only, From/To structurally nil, so no value can ever leak — the
// domain-layer invariant this package stands behind.
func changeSecretRefs(d domain.Service, o domain.ObservedState) []domain.FieldChange {
	var cs []domain.FieldChange
	names := append([]string(nil), d.SecretRefs...)
	sort.Strings(names)
	for _, ref := range names {
		path := "secretRefs." + ref
		// The OBSERVED config carries the mapped env var key (the
		// T-403 dash-kebab rule the apply runs by: internal/reconcile's
		// SecretEnvKey — dashes → underscores, uppercased), never the
		// ref name (tier-4 verified 2026-11-03: grafana's applied
		// GF_SECURITY_ADMIN_PASSWORD read as an eternal "+secret-rel
		// (secret ref)" drift row while the plan keyed on the raw ref).
		envKey := strings.ToUpper(strings.ReplaceAll(ref, "-", "_"))
		switch {
		case !envHasKey(o, envKey):
			cs = append(cs, secretChange(path, "+"))
		case o.SecretChecksums[ref] == "":
			cs = append(cs, secretChange(path, "")) // unresolvable: force re-set
		}
	}
	return cs
}

// secretChange wraps NewSecretFieldChange; the name always derives from a
// non-empty desired ref, so the constructor's errors are unreachable — an
// unreachable error from a documented constructor is fine to drop.
func secretChange(path, op string) domain.FieldChange {
	sc, err := domain.NewSecretFieldChange(path, op)
	if err != nil {
		panic("plan: " + err.Error())
	}
	return sc
}

// ---- ports -------------------------------------------------------------

// portValue renders a port identity for a change entry: the container port,
// plus the host port when one is pinned (host ports are discouraged in the
// PlatformBackend contract).
func portValue(p domain.Port) string {
	v := strconv.Itoa(p.ContainerPort)
	if p.HostPort != 0 {
		v += ":" + strconv.Itoa(p.HostPort)
	}
	return v
}

// portPath is the change path for one port: "ports.<name>" for named
// ports, "ports.<number>" for unnamed ones.
func portPath(p domain.Port) string {
	if p.Name != "" {
		return "ports." + p.Name
	}
	return "ports." + strconv.Itoa(p.ContainerPort)
}

// changePorts compares desired ports against observed ports as a set:
//
//   - a desired port missing on the host ⇒ add (From absent, To = value);
//   - a desired port present with different numbers ⇒ modify entry;
//   - an observed port matching no desired port ⇒ remove (From = value).
//
// Matching is primarily by port name (specs name their ports); the backend
// may also report unnamed ports, which are matched by containerPort number
// against not-yet-matched desired ports so the plan proposes the *real*
// delta instead of add+remove noise.
func changePorts(d domain.Service, o domain.ObservedState) []domain.FieldChange {
	var cs []domain.FieldChange

	obs := make([]obsPort, 0, len(o.Ports))
	for _, op := range o.Ports {
		obs = append(obs, obsPort{p: op})
	}
	byName := map[string]*obsPort{}
	for i := range obs {
		if obs[i].p.Name != "" {
			byName[obs[i].p.Name] = &obs[i]
		}
	}

	for _, dp := range d.Ports {
		path, dv := portPath(dp), portValue(dp)
		// Match by port name first; fall back to matching an unnamed
		// observed port by containerPort so the plan shows the real
		// delta instead of add+remove noise when the backend reports
		// nameless ports.
		var op *obsPort
		if dp.Name != "" {
			op = byName[dp.Name]
		}
		if op == nil {
			for i := range obs {
				if !obs[i].taken && obs[i].p.Name == "" && obs[i].p.ContainerPort == dp.ContainerPort {
					op = &obs[i]
					break
				}
			}
		}
		switch {
		case op == nil:
			cs = append(cs, fieldChange(path, "", dv))
		case op.p.ContainerPort != dp.ContainerPort || op.p.HostPort != dp.HostPort:
			cs = append(cs, fieldChange(path, portValue(op.p), dv))
			op.taken = true
		default:
			op.taken = true
		}
	}
	// Leftover observed ports (no desired counterpart) are removals.
	for _, ob := range obs {
		if !ob.taken {
			cs = append(cs, fieldChange(portPath(ob.p), portValue(ob.p), ""))
		}
	}
	return cs
}

// obsPort pairs an observed port with bookkeeping: was it already matched
// to a desired port during diffing? Local helper type; domain.Port itself
// is never modified.
type obsPort struct {
	p     domain.Port
	taken bool
}

// ---- routing domains ----------------------------------------------------

// changeDomains compares the desired domain set against the proxy's
// attached domains as a symmetric set: missing ⇒ add, extra ⇒ remove
// (shaving the domain set is exactly what re-running `domains:set` does).
// This is also how a “proxy → internal/none“ switch is represented — the
// domain set shrinking to nothing — because ObservedState v0 does not
// carry the routing mode itself (README gap).
func changeDomains(d domain.Service, o domain.ObservedState) []domain.FieldChange {
	want := append([]string(nil), d.Routing.Domains...)
	sort.Strings(want)
	return setChanges("domains", want, o.Domains)
}

// changeNetworks compares desired attachments (a pure derivation from the
// routing mode: internal ⇒ the shared top-internal network, else none)
// against the observed attachment list, as a set.
func changeNetworks(d domain.Service, o domain.ObservedState) []domain.FieldChange {
	want := []string{}
	if d.Routing.Mode == domain.RoutingInternal {
		want = []string{internalNetwork}
	}
	return setChanges("networks", want, o.Networks)
}

// setChanges is the symmetric string-set differ shared by domains and
// networks: entries are added when missing, removed when extra, with paths
// "<prefix>.<value>". `want` must already be sorted.
func setChanges(prefix string, want, have []string) []domain.FieldChange {
	wantSet, haveSet := map[string]bool{}, map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	for _, h := range have {
		haveSet[h] = true
	}
	var cs []domain.FieldChange
	for _, w := range want {
		if !haveSet[w] {
			cs = append(cs, fieldChange(prefix+"."+w, "", w))
		}
	}
	extra := make([]string, 0, len(have))
	for _, h := range have {
		if !wantSet[h] {
			extra = append(extra, h)
		}
	}
	sort.Strings(extra)
	for _, h := range extra {
		cs = append(cs, fieldChange(prefix+"."+h, h, ""))
	}
	return cs
}

// ---- storage ------------------------------------------------------------

// changeStorage compares desired mounts against the observed mounts.
// Identity is the MountPath; the name is compared after tolerantly
// stripping the "<app>-" episode prefix ADR-0004 prescribes for entry
// names ("observability-prometheus-data" ⇔ desired name "data"). Size is
// advisory in the spec and not reported by the backend — not comparable
// (README).
func changeStorage(d domain.Service, o domain.ObservedState) []domain.FieldChange {
	want := map[string]domain.Storage{} // by mount path
	for _, s := range d.Storage {
		want[s.MountPath] = s
	}
	obs := map[string]domain.StorageMount{} // by mount path
	for _, m := range o.StorageMounts {
		obs[m.MountPath] = m
	}

	appPrefix := ""
	if app, err := domain.AppName(d.Ref.Namespace, d.Ref.Name); err == nil {
		appPrefix = app + "-"
	}

	var cs []domain.FieldChange
	for _, s := range d.Storage {
		path := "storage." + s.String() // e.g. "storage.data:/prometheus"
		m, ok := obs[s.MountPath]
		if !ok {
			cs = append(cs, fieldChange(path, "", s.String()))
			continue
		}
		if obsName := strings.TrimPrefix(m.Name, appPrefix); obsName != s.Name {
			cs = append(cs, fieldChange(path, m.String(), s.String()))
		}
	}
	for _, m := range o.StorageMounts {
		if _, ok := want[m.MountPath]; !ok {
			cs = append(cs, fieldChange("storage."+m.String(), m.String(), ""))
		}
	}
	return cs
}

// ---- restart policy / resources ------------------------------------------

// changeRestartPolicy compares the desired policy (defaulted to `always` by
// spec loading) against the reported one. An unreported observed policy
// (empty) is not comparable and is skipped — the report shape does not
// guarantee the policy is echoed back.
func changeRestartPolicy(d domain.Service, o domain.ObservedState) []domain.FieldChange {
	if d.RestartPolicy == "" || o.RestartPolicy == "" {
		return nil
	}
	if d.RestartPolicy == o.RestartPolicy {
		return nil
	}
	return []domain.FieldChange{fieldChange("restart-policy", string(o.RestartPolicy), string(d.RestartPolicy))}
}

// changeResources compares advisory resource limits, per quantity:
// unset/unset equal, added when it was absent, removed when the desired
// drops it, modified on value change. A fully-unreported observed
// Resources (zero value) is skipped — the backend did not answer, so
// comparing would invent drift.
func changeResources(d domain.Service, o domain.ObservedState) []domain.FieldChange {
	if o.Resources == (domain.Resources{}) {
		return nil
	}
	var cs []domain.FieldChange
	cs = append(cs, limitChange("resources.cpu", d.Resources.Limits.CPU, o.Resources.Limits.CPU)...)
	cs = append(cs, limitChange("resources.memory", d.Resources.Limits.Memory, o.Resources.Limits.Memory)...)
	return cs
}

func limitChange(path, want, have string) []domain.FieldChange {
	if want == have {
		return nil
	}
	return []domain.FieldChange{fieldChange(path, have, want)}
}

// ---- helpers -----------------------------------------------------------

// fieldChange builds a From/To change from plain values, mapping "" to the
// absent pointer side. Callers always pass a non-empty To for adds, a
// non-empty From for removals, and both for modifications, so the domain
// constructor's both-absent guarantee holds by construction; if a caller
// bug breaks it, the empty FieldChange produced by NewFieldChange's
// contract is loud in tests rather than at apply time.
func fieldChange(path, from, to string) domain.FieldChange {
	c, err := domain.NewFieldChange(path, ptrOrNil(from), ptrOrNil(to))
	if err != nil {
		panic("plan: " + err.Error())
	}
	return c
}

func ptrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// sortedKeys returns a map's keys sorted (map-order determinism without
// reflection).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortChanges orders field changes by path (stable, so equal paths keep
// relative order — each compare emits at most one change per path anyway).
func sortChanges(cs []domain.FieldChange) {
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Path < cs[j].Path })
}

// sortRefs orders service refs by namespace, then name.
func sortRefs(refs []domain.ServiceRef) {
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].Namespace != refs[j].Namespace {
			return refs[i].Namespace < refs[j].Namespace
		}
		return refs[i].Name < refs[j].Name
	})
}

// sortPlans orders service plans by their refs (same order as sortRefs).
func sortPlans(ps []domain.ServicePlan) {
	sort.SliceStable(ps, func(i, j int) bool {
		if ps[i].Ref.Namespace != ps[j].Ref.Namespace {
			return ps[i].Ref.Namespace < ps[j].Ref.Namespace
		}
		return ps[i].Ref.Name < ps[j].Ref.Name
	})
}
