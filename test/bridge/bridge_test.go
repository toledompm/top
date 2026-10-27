// SPDX-License-Identifier: MIT
// Differential cases: hand tables + a seeded randomized property pass.
package bridge

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/toledompm/top/internal/plan"
)

// runCase computes the Go plan and Bend answers for one reduced case and
// deep-compares the per-service surface.
func runCase(t *testing.T, binary string, name string, c redcase) {
	t.Helper()
	desired, observed, selfRefs, _ := c.toGo()
	opts := []plan.Option{plan.WithPruneAllowed(c.Prune)}
	for _, r := range selfRefs {
		opts = append(opts, plan.WithSelfService(r))
	}
	got := plan.ComputeAll(desired, observed, opts...)
	byRef := map[string]int{}
	for i, sp := range got.Services {
		byRef[sp.Ref.String()] = i
	}

	selfList := c.Self
	for _, s := range c.D {
		// Model probe input: synthesize observed unless the case says the
		// app is absent or shaped otherwise (converged modeling = case Q).
		var ob *redobs
		for _, q := range c.Q {
			if q.NS == s.NS && q.Name == s.Name {
				ob = q.Obs
			}
		}
		// ob == nil is legitimate: the model probe passes None{} (observed-
		// missing).
		want := bendAsk(t, lawDir(t), binary, s, ob, selfList)
		// Go side (same destructure shape).
		idx, ok := byRef[fmt.Sprintf("%s/%s", nsTag(s.NS), nameTag(s.Name))]
		if !ok {
			t.Fatalf("case %s: missing Go plan for %s/%s", name, nsTag(s.NS), nameTag(s.Name))
		}
		goSp, ok2 := goOne(t, got.Services[idx])
		if !ok2 {
			continue // kind outside the per-service surface (delete)
		}
		compare(t, name, goSp, want)
	}
}

// converged cases: synth+synthesized observed must plan noop on BOTH sides.
func TestBridgeConvergedNoop(t *testing.T) {
	binary := findBend(t)
	svcs := []redsvc{
		{NS: 1, Name: 1, Img: 3, Env: []pe{{1, 5}, {2, 7}}, Sek: []int{4, 9}},
		{NS: 0, Name: 2, Img: 7, Sek: []int{1}},
		{NS: 2, Name: 3, Img: -1, Env: []pe{{1, 1}}},
	}
	for i, s := range svcs {
		c := redcase{D: []redsvc{s}, Q: []redobs2{{NS: s.NS, Name: s.Name, Obs: modelObserved(s)}}}
		runCase(t, binary, fmt.Sprintf("converged-%d", i), c)
	}
}

func TestBridgeDivergenceTable(t *testing.T) {
	binary := findBend(t)
	cases := map[string]redcase{
		"rev-drift": {
			D:    []redsvc{{NS: 1, Name: 1, Img: 3}},
			Q:    []redobs2{{NS: 1, Name: 1, Obs: &redobs{Img: 8, EnvN: nil, Seu: nil}}},
			Self: nil, Prune: false,
		},
		"rev-plus": {
			D:    []redsvc{{NS: 1, Name: 2, Img: 1}},
			Q:    []redobs2{{NS: 1, Name: 2, Obs: &redobs{Img: -1}}},
			Self: nil, Prune: false,
		},
		"build-ref-create": {
			D:    []redsvc{{NS: 2, Name: 1, Img: -1}},
			Q:    []redobs2{{NS: 2, Name: 1, Obs: nil}},
			Self: nil, Prune: false,
		},
		"env-add": {
			D:    []redsvc{{NS: 1, Name: 1, Img: 3, Env: []pe{{1, 5}}}},
			Q:    []redobs2{{NS: 1, Name: 1, Obs: &redobs{Img: 3}}},
			Self: nil, Prune: false,
		},
		"secret-add": {
			D:    []redsvc{{NS: 1, Name: 1, Img: 3, Sek: []int{2}}},
			Q:    []redobs2{{NS: 1, Name: 1, Obs: &redobs{Img: 3}}},
			Self: nil, Prune: false,
		},
		"secret-unresolvable": {
			D:    []redsvc{{NS: 1, Name: 1, Img: 3, Sek: []int{2}}},
			Q:    []redobs2{{NS: 1, Name: 1, Obs: &redobs{Img: 3, EnvN: []int{2}}}},
			Self: nil, Prune: false,
		},
		"self-drift": {
			D:     []redsvc{{NS: 0, Name: 2, Img: 4}},
			Q:     []redobs2{{NS: 0, Name: 2, Obs: &redobs{Img: 9}}},
			Self:  []redref{{NS: 0, Name: 2}},
			Prune: false,
		},
	}
	for name, c := range cases {
		runCase(t, binary, name, c)
	}
}

// TestBridgeRandomizedDifferential: seeded random cases — the actual
// property pass. Divergence with the Bend model fails CI.
func TestBridgeRandomizedDifferential(t *testing.T) {
	binary := findBend(t)
	rng := rand.New(rand.NewSource(42)) //nolint:gosec // deterministic property seed
	for i := 0; i < 10; i++ {
		c := redcase{}
		n := 1 + rng.Intn(4)
		for k := 0; k < n; k++ {
			s := redsvc{
				NS:   1 + rng.Intn(3), // ns1..ns3 (0=core reserved — converged-core case covered above)
				Name: k + 1,
				Img:  rng.Intn(5),
			}
			if rng.Intn(4) == 0 {
				s.Img = -1
			}
			// Unique keys/secrets: domain.Env is a map (Go collapses
			// duplicates), so the model must see the same injective set.
			used := map[int]bool{}
			for j := 0; j < rng.Intn(3); j++ {
				k := rng.Intn(4)
				if used[k] {
					continue
				}
				used[k] = true
				s.Env = append(s.Env, pe{K: k, V: rng.Intn(6)})
			}
			usedS := map[int]bool{}
			for j := 0; j < rng.Intn(3); j++ {
				k := rng.Intn(5)
				if usedS[k] {
					continue
				}
				usedS[k] = true
				s.Sek = append(s.Sek, k)
			}
			c.D = append(c.D, s)
			q := redobs2{NS: s.NS, Name: s.Name}
			switch rng.Intn(3) {
			case 0:
				q.Obs = nil

			default:
				// drift the observed state: perturb exactly one dimension
				m := &redobs{Img: s.Img}
				if rng.Intn(2) == 0 {
					m.Img = rng.Intn(5)
				}
				m.EnvN = append(m.EnvN, keys(s)...)
				m.EnvN = uniqueInts(m.EnvN)
				if rng.Intn(2) == 0 && len(m.EnvN) > 0 {
					drop := rng.Intn(len(m.EnvN))
					m.EnvN = append(m.EnvN[:drop], m.EnvN[drop+1:]...)
				}
				for _, k := range m.EnvN {
					_ = k
				}
				if rng.Intn(3) == 0 {
					for _, sec := range sortedUnique(s.Sek) {
						if rng.Intn(2) == 0 {
							m.Seu = append(m.Seu, sec)
						}
					}
				}
				q.Obs = m
			}
			c.Q = append(c.Q, q)
		}
		if rng.Intn(2) == 0 {
			// an orphan not in Git
			c.Q = append(c.Q, redobs2{NS: 1 + rng.Intn(3), Name: 9, Obs: &redobs{Img: 2}})
		}
		runCase(t, binary, fmt.Sprintf("case-%02d", i), c)
	}
}

func keys(s redsvc) []int {
	out := []int{}
	for _, e := range s.Env {
		out = append(out, e.K)
	}
	out = append(out, sortedUnique(s.Sek)...)
	return out
}

func sortedUnique(xs []int) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Ints(out)
	return out
}

func uniqueInts(xs []int) []int { return sortedUnique(xs) }
