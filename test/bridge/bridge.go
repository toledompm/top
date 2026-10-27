// SPDX-License-Identifier: MIT
// Package bridge — differential harness: TOP's Go pure core (internal/plan)
// against the Bend reference model in laws/.
//
// Flow (per case):
//  1. A reduced (fictional-tag) case maps to loyally fictional domain
//     objects via injective tag↔name tables.
//  2. plan.ComputeAll produces the Go plan; each per-service ServicePlan is
//     reduced to the tag schema the Bend model emits (reference.bend:
//     case_one_json). Secret entries drop `op` in the comparison: the model
//     renders "*" for every secret variant, Go distinguishes
//     "+"(add)/""(unresolvable) — the name-only invariant (fam/key/from/to)
//     is identical on both sides, exactly what the proven
//     secrets_valueless law pins.
//  3. One driver .bend file is generated per service with inline
//     constructor literals (no parser involvement); `bend` executes it and
//     the JSON line is captured. Whole-repo rules (orphans, prune gating,
//     totality) are pinned by the proven LAWS (plan_total,
//     prune_protects_core, chg_sec_spec, op_transitions) and Go-side tests;
//     the differential surface is the per-service pipeline.
//  4. Both sides are sorted canonically; comparison is reflect.DeepEqual.
//
// HERMETIC: with no bend binary on PATH or at $HOME/.bend/bin/bend, every
// test skips — default `make test` is unaffected (ADR-0012 interplay;
// `make laws` includes this suite when bend is installed).
package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/toledompm/top/internal/domain"
)

// ---- fictional tag↔name bijection (deterministic, injective) ---------------

func nsTag(m int) string {
	if m == 0 {
		return "core" // domain.CoreNamespace (prune-exempt, ADR-0004)
	}
	return fmt.Sprintf("ns%d", m)
}
func nameTag(n int) string { return fmt.Sprintf("svc%d", n) }
func imgTag(m int) string  { return fmt.Sprintf("sha256:%064x", m) }
func keyTag(k int) string  { return fmt.Sprintf("key%d", k) }
func valTag(v int) string  { return fmt.Sprintf("val%d", v) }
func secTag(s int) string  { return fmt.Sprintf("SEC%d", s) }

func unTag(prefix, kind string, s string) int {
	var n int
	if _, err := fmt.Sscanf(strings.TrimPrefix(s, prefix), kind, &n); err != nil {
		panic("bridge: unknown tag " + s)
	}
	return n
}

// sekShift moves secretRef names into a private Nat subrange of the
// model's shared name list (the model has one flat name space; plain env
// keys must not collide with secret names).
const sekShift = 1000

func nsUnTag(s string) int {
	if s == "core" {
		return 0
	}
	return unTag("ns", "%d", s)
}

// ---- reduced case shape (what both sides speak) ----------------------------

type redsvc struct {
	NS, Name int
	Img      int // >=0 pinned digest tag; <0 branch-pinned build (not comparable)
	Env      []pe
	Sek      []int
}
type pe struct{ K, V int }

type redobs struct {
	Img   int // >=0 image tag reported; <0 nothing recorded
	EnvN  []int
	SekOK []int // secret names present WITH checksums (converged)
	Seu   []int // secret names present but unresolvable (checksum "")
}

type redref struct{ NS, Name int }

type redcase struct {
	D     []redsvc
	Q     []redobs2 // every observed ref (desired ones included, some absent)
	Self  []redref
	Prune bool
}

type redobs2 struct {
	NS, Name int
	Obs      *redobs // nil = observed-missing / never deployed
}

// modelObserved synthesizes the observed state a correct backend Inspect
// would report for a desired service (mirror of plan.SynthesizeObserved on
// the modeled slice) — converged cases are built from it.
func modelObserved(s redsvc) *redobs {
	w := &redobs{}
	t := s.Img
	if s.Img >= 0 {
		w.Img = t
	} else {
		w.Img = -1
	}
	for _, e := range s.Env {
		w.EnvN = append(w.EnvN, e.K)
	}
	w.SekOK = append(w.SekOK, s.Sek...)
	sort.Ints(w.EnvN)
	return w
}

// ---- reduced case → loyal Go domain objects --------------------------------

func goRef(m, n int) domain.ServiceRef {
	r, err := domain.ParseServiceRef(nsTag(m) + "/" + nameTag(n))
	if err != nil {
		panic(fmt.Sprintf("bridge: tag mapping bug: %v", err))
	}
	return r
}

func goService(s redsvc) domain.Service {
	out := domain.Service{Ref: goRef(s.NS, s.Name)}
	if s.Img >= 0 {
		out.Source = domain.Source{Image: "fictional.registry.example/app", Digest: imgTag(s.Img)}
	} else {
		out.Source = domain.Source{Build: &domain.BuildSource{Repo: "https://example.com/example/repo", Ref: "not-a-sha-fixture"}}
	}
	out.Env = map[string]string{}
	for _, e := range s.Env {
		out.Env[keyTag(e.K)] = valTag(e.V)
	}
	out.SecretRefs = []string{}
	for _, t := range s.Sek {
		out.SecretRefs = append(out.SecretRefs, secTag(t))
	}
	sort.Strings(out.SecretRefs)
	return out
}

func goObserved2(o redobs2) domain.ObservedState {
	w := domain.ObservedState{Ref: goRef(o.NS, o.Name)}
	if o.Obs == nil {
		return w // Absent() = true
	}
	w.Deployed = true
	w.Running = true
	if o.Obs.Img >= 0 {
		w.Image = "fictional.registry.example/app@" + imgTag(o.Obs.Img)
	}
	w.EnvKeys = []string{}
	w.SecretChecksums = map[string]string{}
	for _, k := range o.Obs.EnvN {
		w.EnvKeys = append(w.EnvKeys, keyTag(k))
	}
	for _, k := range o.Obs.SekOK {
		w.EnvKeys = append(w.EnvKeys, secTag(k))
		w.SecretChecksums[secTag(k)] = "synthetic-checksum"
	}
	for _, s := range o.Obs.Seu {
		w.SecretChecksums[secTag(s)] = "" // present but unresolvable
	}
	return w
}

// toGo builds ComputeAll's mirrored Go inputs.
func (c redcase) toGo() ([]domain.Service, map[domain.ServiceRef]domain.ObservedState, []domain.ServiceRef, bool) {
	desired := []domain.Service{}
	for _, s := range c.D {
		desired = append(desired, goService(s))
	}
	observed := map[domain.ServiceRef]domain.ObservedState{}
	for _, o := range c.Q {
		observed[goRef(o.NS, o.Name)] = goObserved2(o)
	}
	self := []domain.ServiceRef{}
	for _, r := range c.Self {
		self = append(self, goRef(r.NS, r.Name))
	}
	return desired, observed, self, c.Prune
}

// ---- both sides → canonical comparable structs -----------------------------

type jchg struct {
	Fam  int
	Key  *int
	From *int
	To   *int
}

type jent struct {
	NS   int
	Name int
	Kind string
	Chg  []jchg
}

func (a jchg) less(b jchg) bool {
	if a.Fam != b.Fam {
		return a.Fam < b.Fam
	}
	if a.Key == nil || b.Key == nil {
		if (a.Key == nil) != (b.Key == nil) {
			return a.Key == nil
		}
	} else if *a.Key != *b.Key {
		return *a.Key < *b.Key
	}
	if (a.From == nil) != (b.From == nil) {
		return a.From == nil
	}
	if a.From != nil && b.From != nil && *a.From != *b.From {
		return *a.From < *b.From
	}
	if (a.To == nil) != (b.To == nil) {
		return a.To == nil
	}
	if a.To != nil && b.To != nil && *a.To != *b.To {
		return *a.To < *b.To
	}
	return false
}
func (a jent) less(b jent) bool {
	if a.NS != b.NS {
		return a.NS < b.NS
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.Kind < b.Kind
}

// kindOf maps a Go ChangeKind to the model's emitted string.
func kindOf(k domain.ChangeKind) (string, error) {
	switch k {
	case domain.PlanCreate:
		return "create", nil
	case domain.PlanUpdate:
		return "update", nil
	case domain.PlanNoop:
		return "noop", nil
	case domain.PlanDriftedSelf:
		return "self", nil
	default:
		return "", fmt.Errorf("bridge: kind %q outside the per-service surface", string(k))
	}
}

// decodeVal maps an emitted plain value (digest / env value) back into its
// fictional tag.
func decodeVal(fam int, v string, t *testing.T) int {
	t.Helper()
	switch fam {
	case 0:
		return imgUnTag(v)
	case 1:
		return valUnTag(v)
	default:
		t.Fatalf("decodeVal: family %d carries no decodable values", fam)
		return 0
	}
}

// goOne reduces one Go ServicePlan to the canonical jent. Returns ok=false
// for kinds the per-service surface deliberately excludes (PlanDelete,
// whose gating is law-pinned) and for changes outside the modeled slice.
func goOne(t *testing.T, sp domain.ServicePlan) (jent, bool) {
	t.Helper()
	kind, err := kindOf(sp.Kind)
	if err != nil {
		return jent{}, false
	}
	e := jent{NS: nsUnTag(sp.Ref.Namespace.String()), Name: nameUnTag(sp.Ref.Name.String()), Kind: kind}
	for _, fc := range sp.Changes {
		var c jchg
		var fam int
		switch {
		case fc.Path == "revision":
			fam = 0
		case strings.HasPrefix(fc.Path, "env."):
			fam = 1
			k := keyUnTag(strings.TrimPrefix(fc.Path, "env."))
			c.Key = &k
		case strings.HasPrefix(fc.Path, "secretRefs."):
			fam = 2
			k := secUnTag(strings.TrimPrefix(fc.Path, "secretRefs.")) + sekShift // model name space (see redobs doc)
			c.Key = &k
		default:
			continue // documented v0 model gap
		}
		c.Fam = fam
		if fc.From != nil {
			v := decodeVal(fam, *fc.From, t)
			c.From = &v
		}
		if fc.To != nil {
			v := decodeVal(fam, *fc.To, t)
			c.To = &v
		}
		e.Chg = append(e.Chg, c)
	}
	sort.Slice(e.Chg, func(i, j int) bool { return e.Chg[i].less(e.Chg[j]) })
	return e, true
}

// ---- bend side ---------------------------------------------------------

// lawDir resolves the repo's laws/ adjacent to this package via the source
// path (PACKAGE-LOCATION-BASED, immune to test CWD).
func lawDir(t *testing.T) string {
	t.Helper()
	_, this, _, ok := runtime.Caller(1)
	if !ok {
		t.Fatal("bridge: runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(this), "../../laws")
}

func findBend(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("bend"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".bend", "bin", "bend")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("bend not installed (PATH / $HOME/.bend/bin) — differential bridge skipped")
	return ""
}

// bend renders the fictional constructor literal tree (inline, no parser).
func (s redsvc) bend() string {
	img := "None{}"
	if s.Img >= 0 {
		img = fmt.Sprintf("Some{%dn}", s.Img)
	}
	env := "Nil{}"
	for i := len(s.Env) - 1; i >= 0; i-- {
		prev := env
		env = fmt.Sprintf("Con{C.OPE{%dn, %dn}, %s}", s.Env[i].K, s.Env[i].V, prev)
	}
	sek := "Nil{}"
	for i := len(s.Sek) - 1; i >= 0; i-- {
		prev := sek
		sek = fmt.Sprintf("Con{%dn, %s}", sekShift+s.Sek[i], prev)
	}
	return fmt.Sprintf("C.OSvc{C.ORef{%dn, %dn}, %s, %s, %s}", s.NS, s.Name, img, env, sek)
}

func (o *redobs) bend() string {
	if o == nil {
		return "None{}"
	}
	img := "None{}"
	if o.Img >= 0 {
		img = fmt.Sprintf("Some{%dn}", o.Img)
	}
	envn := "Nil{}"
	for i := len(o.EnvN) - 1; i >= 0; i-- {
		prev := envn
		envn = fmt.Sprintf("Con{%dn, %s}", o.EnvN[i], prev)
	}
	for i := len(o.SekOK) - 1; i >= 0; i-- {
		prev := envn
		envn = fmt.Sprintf("Con{%dn, %s}", sekShift+o.SekOK[i], prev)
	}
	for i := len(o.Seu) - 1; i >= 0; i-- {
		prev := envn
		envn = fmt.Sprintf("Con{%dn, %s}", sekShift+o.Seu[i], prev)
	}
	seu := "Nil{}"
	for i := len(o.Seu) - 1; i >= 0; i-- {
		prev := seu
		seu = fmt.Sprintf("Con{%dn, %s}", sekShift+o.Seu[i], prev)
	}
	return fmt.Sprintf("Some{C.OObs{%s, %s, %s}}", img, envn, seu)
}

func selfBend(refs []redref) string {
	xs := "Nil{}"
	for i := len(refs) - 1; i >= 0; i-- {
		prev := xs
		xs = fmt.Sprintf("Con{C.ORef{%dn, %dn}, %s}", refs[i].NS, refs[i].Name, prev)
	}
	return xs
}

const driverTemplate = `import Base
import CODE.bend as C
import reference.bend as R

def main() -> IO(Unit):
  do IO<Unit>:
    IO.print(R.case_one_json(%s, %s, %s))
`

// bendAsk writes one driver file, runs bend, and parses the emitted line.
func bendAsk(t *testing.T, lawDir, binary string, s redsvc, ob *redobs, self []redref) jent {
	t.Helper()
	body := fmt.Sprintf(driverTemplate, s.bend(), ob.bend(), selfBend(self))
	p := filepath.Join(lawDir, "bridge_driver.bend")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("driver write: %v", err)
	}
	cmd := exec.Command(binary, "bridge_driver.bend")
	cmd.Dir = lawDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bend run: %v\n%s", err, out)
	}
	line := strings.TrimSpace(string(out))
	if !strings.HasPrefix(line, "{") {
		t.Fatalf("bend emitted non-JSON: %q", line)
	}
	e := jent{}
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatalf("bend JSON parse: %v: %q\nDRIVER:\n%s", err, line, body)
	}
	sort.Slice(e.Chg, func(i, j int) bool { return e.Chg[i].less(e.Chg[j]) })
	return e
}

// compare asserts canonical deep-equality with a readable divergence.
func compare(t *testing.T, name string, want, got jent) {
	t.Helper()
	normalize(&want)
	normalize(&got)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("case %s divergence:\nGo:   %+v\nBend: %+v", name, want, got)
	}
}

// normalize folds nil slices to empty so nil-vs-empty does not mask real
// divergence (the Bend side parses "chg":[] into a non-nil empty slice;
// a Go reduction with no changes starts nil).
func normalize(e *jent) {
	if e.Chg == nil {
		e.Chg = []jchg{}
	}
}

func imgUnTag(s string) int {
	// domain revisions are "registry/repo@sha256:<64hex>"; decode the part.
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	var m int
	if _, err := fmt.Sscanf(s, "sha256:%x", &m); err != nil {
		panic("bridge: unknown revision tag " + s)
	}
	return m
}

func valUnTag(s string) int {
	var n int
	if _, err := fmt.Sscanf(s, "val%d", &n); err != nil {
		panic("bridge: unknown value tag " + s)
	}
	return n
}

func keyUnTag(s string) int {
	var n int
	if _, err := fmt.Sscanf(s, "key%d", &n); err != nil {
		panic("bridge: unknown key tag " + s)
	}
	return n
}

func secUnTag(s string) int {
	var n int
	if _, err := fmt.Sscanf(s, "SEC%d", &n); err != nil {
		panic("bridge: unknown secret tag " + s)
	}
	return n
}

func nameUnTag(s string) int {
	var n int
	if _, err := fmt.Sscanf(s, "svc%d", &n); err != nil {
		panic("bridge: unknown name tag " + s)
	}
	return n
}
