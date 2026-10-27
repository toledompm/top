package plan

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/spec"
)

// mustLoadFixtures loads internal/spec's valid testdata as resolved domain
// services: the same fixtures spec (T-103) ships, confirming the plan layer
// consumes the parser's output verbatim (T-110's killer invariant runs over
// every one of them).
func mustLoadFixtures(t *testing.T) []domain.Service {
	t.Helper()
	res, err := spec.LoadDir("../spec/testdata/valid", spec.WithPlatformRepo(true))
	if err != nil {
		t.Fatalf("LoadDir fixtures: %v", err)
	}
	if len(res.Services) == 0 {
		t.Fatal("fixture dir produced no services")
	}
	return res.Services
}

// fullSHA is a syntactically valid 64-hex commit sha for build-source tests.
func fullSHA() string {
	return "222233334444555566667777888899990000aaaabbbbccccddddeeeeffff1111"
}

// baseService is the drift-table fixture: an image service with one port,
// proxy routing, storage, env, secretRef, restart policy, resources, and
// internal-network-neutral routing.
func baseService() domain.Service {
	return domain.Service{
		Ref: domain.Ref("observability", "prometheus"),
		Source: domain.Source{
			Image:  "docker.io/prom/prometheus",
			Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		Ports:         []domain.Port{{Name: "http", ContainerPort: 9090}},
		Routing:       domain.Routing{Mode: domain.RoutingProxy, Domains: []string{"metrics.example.com"}},
		Storage:       []domain.Storage{{Name: "data", MountPath: "/prometheus", Size: "2Gi"}},
		Env:           map[string]string{"TZ": "UTC", "LOG_LEVEL": "info"},
		SecretRefs:    []string{"SMTP_PASSWORD"},
		RestartPolicy: domain.RestartAlways,
		Resources:     domain.Resources{Limits: domain.ResourceLimits{CPU: "500m", Memory: "512Mi"}},
	}
}

func obsFor(base domain.Service) domain.ObservedState {
	return SynthesizeObserved(base)
}

func assertPaths(t *testing.T, got []domain.FieldChange, want []string) {
	t.Helper()
	paths := make([]string, 0, len(got))
	for _, c := range got {
		paths = append(paths, c.Path)
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("change paths:\n got %q\nwant %q", paths, want)
	}
}

// ---- the killer invariant -----------------------------------------------

// TestComputeIdempotencyInvariant is THE invariant (contracts/reconciler.md:
// "running the same revision twice yields empty plans"): for every valid
// fixture service, an ObservedState of exactly the desired shape must plan
// as noop, with zero field changes.
func TestComputeIdempotencyInvariant(t *testing.T) {
	for _, d := range mustLoadFixtures(t) {
		d := d
		t.Run(d.Ref.String(), func(t *testing.T) {
			p := Compute(d, SynthesizeObserved(d))
			if !p.Empty() {
				t.Fatalf("expected noop plan, got:\n%s", p)
			}
		})
	}
}

// TestComputeAllIdempotencyInvariant extends the same invariant to the
// whole-fleet plan (no orphan reports when every observed service is in Git).
func TestComputeAllIdempotencyInvariant(t *testing.T) {
	svcs := mustLoadFixtures(t)
	obs := map[domain.ServiceRef]domain.ObservedState{}
	for _, d := range svcs {
		obs[d.Ref] = SynthesizeObserved(d)
	}
	p := ComputeAll(svcs, obs)
	if !p.Empty() {
		t.Fatalf("expected fully empty fleet plan, got:\n%s", p)
	}
	if len(p.Orphaned) != 0 {
		t.Fatalf("unexpected orphans: %v", p.Orphaned)
	}
}

// ---- create -------------------------------------------------------------

func TestComputeAbsentIsCreate(t *testing.T) {
	d := baseService()
	p := Compute(d, domain.ObservedState{Ref: d.Ref})
	if p.Kind != domain.PlanCreate {
		t.Fatalf("kind = %q, want create", p.Kind)
	}
	paths := map[string]bool{}
	for _, c := range p.Changes {
		paths[c.Path] = true
	}
	for _, want := range []string{
		"revision",
		"env.TZ", "env.LOG_LEVEL",
		"secretRefs.SMTP_PASSWORD",
		"ports.http",
		"domains.metrics.example.com",
		"storage.data:/prometheus",
	} {
		if !paths[want] {
			t.Fatalf("create plan missing add entry %q; got %v", want, paths)
		}
	}
	// Create adds carry no From value.
	for _, c := range p.Changes {
		if c.From != nil {
			t.Fatalf("create entry %s has From %q; adds must be From-absent", c.Path, *c.From)
		}
	}
}

func TestComputeAllMissingObservedIsCreate(t *testing.T) {
	d := baseService()
	p := ComputeAll([]domain.Service{d}, nil)
	if len(p.Services) != 1 || p.Services[0].Kind != domain.PlanCreate {
		t.Fatalf("missing observed must plan create, got %s", p)
	}
}

// ---- drift table --------------------------------------------------------

// TestComputeDriftTable: each case mutates the base fixture one way,
// synthesizes the rest of the observed state correct, and asserts exactly
// the expected field-change paths in order (sorted by path).
func TestComputeDriftTable(t *testing.T) {
	tests := []struct {
		name  string
		mut   func(*domain.Service)
		obs   func(*domain.ObservedState)
		paths []string
	}{
		{
			name: "revision sha drift",
			mut: func(d *domain.Service) {
				d.Source.Digest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
			},
			paths: []string{"revision"},
		},
		{
			name: "source-type switch image→build",
			mut: func(d *domain.Service) {
				d.Source = domain.Source{Build: &domain.BuildSource{
					Repo: "github.com/example/top", Ref: fullSHA(), Path: ".",
				}}
			},
			obs:   func(o *domain.ObservedState) { o.Revision = ""; o.Image = "docker.io/prom/prometheus@sha256:0123" },
			paths: []string{"revision"},
		},
		{
			name: "env key added to spec",
			mut:  func(d *domain.Service) { d.Env["NEW_KEY"] = "value" },
			paths: []string{
				"env.NEW_KEY",
			},
		},
		{
			name:  "env plain value change is undetectable in v0",
			mut:   func(d *domain.Service) { d.Env["TZ"] = "Europe/Brussels" },
			obs:   func(o *domain.ObservedState) { o.EnvKeys = []string{"LOG_LEVEL", "SMTP_PASSWORD", "TZ"} },
			paths: []string{
				// none: observed carries names only (ADR-0008);
				// documented v0 gap, not a false-positive add/remove.
			},
		},
		{
			name:  "env removal from spec is a sink; host key stays",
			mut:   func(d *domain.Service) { delete(d.Env, "TZ") },
			obs:   func(o *domain.ObservedState) { o.EnvKeys = []string{"LOG_LEVEL", "SMTP_PASSWORD", "TZ"} },
			paths: []string{}, // sink semantics: never purge keys we didn't set
		},
		{
			name: "secret ref added",
			mut:  func(d *domain.Service) { d.SecretRefs = append(d.SecretRefs, "API_TOKEN") },
			obs:  func(o *domain.ObservedState) { o.EnvKeys = []string{"API_TOKEN_X", "LOG_LEVEL", "SMTP_PASSWORD", "TZ"} },
			paths: []string{
				"secretRefs.API_TOKEN",
			},
		},
		{
			name: "secret ref unresolvable on host",
			obs:  func(o *domain.ObservedState) { o.SecretChecksums["SMTP_PASSWORD"] = "" },
			paths: []string{
				"secretRefs.SMTP_PASSWORD",
			},
		},
		{
			name: "port containerPort changed",
			mut:  func(d *domain.Service) { d.Ports[0].ContainerPort = 9091 },
			paths: []string{
				"ports.http",
			},
		},
		{
			name:  "port removed from spec",
			mut:   func(d *domain.Service) { d.Ports = nil },
			paths: []string{"ports.http"},
		},
		{
			name: "port added to spec",
			mut: func(d *domain.Service) {
				d.Ports = []domain.Port{{Name: "http", ContainerPort: 9090}, {Name: "http2", ContainerPort: 8080}}
			},
			paths: []string{
				"ports.http2",
			},
		},
		{
			name:  "domain removed from spec",
			mut:   func(d *domain.Service) { d.Routing.Domains = nil },
			paths: []string{"domains.metrics.example.com"},
		},
		{
			name:  "extra domain on host",
			mut:   func(d *domain.Service) {},
			obs:   func(o *domain.ObservedState) { o.Domains = append(o.Domains, "legacy.example.com") },
			paths: []string{"domains.legacy.example.com"},
		},
		{
			name:  "storage mount added to spec",
			mut:   func(d *domain.Service) { d.Storage = append(d.Storage, domain.Storage{Name: "wal", MountPath: "/wal"}) },
			paths: []string{"storage.wal:/wal"},
		},
		{
			name: "extra observed storage mount",
			mut:  func(d *domain.Service) {},
			obs: func(o *domain.ObservedState) {
				o.StorageMounts = append(o.StorageMounts, domain.StorageMount{Name: mustAppName(baseService().Ref) + "-junk", MountPath: "/junk"})
			},
			paths: []string{"storage." + mustAppName(baseService().Ref) + "-junk:/junk"},
		},
		{
			name:  "storage name mismatch on same mount path",
			mut:   func(d *domain.Service) { d.Storage[0].Name = "data2" },
			paths: []string{"storage.data2:/prometheus"},
		},
		{
			name:  "restart policy drifted",
			mut:   func(d *domain.Service) { d.RestartPolicy = domain.RestartNever },
			paths: []string{"restart-policy"},
		},
		{
			name:  "resource limit added",
			mut:   func(d *domain.Service) { d.Resources.Limits.Memory = "1Gi" },
			paths: []string{"resources.memory"},
		},
		{
			name:  "resource limit removed",
			mut:   func(d *domain.Service) { d.Resources.Limits.Memory = "" },
			paths: []string{"resources.memory"},
		},
		{
			name:  "internal routing wants network attachment",
			mut:   func(d *domain.Service) { d.Routing = domain.Routing{Mode: domain.RoutingInternal} },
			paths: []string{"domains.metrics.example.com", "networks." + internalNetwork},
		},
		{
			name:  "extra observed network",
			mut:   func(d *domain.Service) {},
			obs:   func(o *domain.ObservedState) { o.Networks = []string{"junk-net"} },
			paths: []string{"networks.junk-net"},
		},
		{
			name: "combined drift sorts by path",
			mut: func(d *domain.Service) {
				d.Env["TZ"] = "UTC"
				d.Env["NEW"] = "x"
				d.Resources.Limits.CPU = "1"
				d.RestartPolicy = domain.RestartOnFailure
			},
			obs: func(o *domain.ObservedState) { o.RestartPolicy = domain.RestartAlways },
			paths: []string{
				"env.NEW",        // add (env sorted; TZ value drift undetectable v0)
				"resources.cpu",  // ~
				"restart-policy", // ~
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := baseService()
			d := baseService()
			if tt.mut != nil {
				tt.mut(&d)
			}
			// Observed is faithful to the *base* desired state (what the
			// backend would report pre-drift); cases override it further.
			o := obsFor(base)
			if tt.obs != nil {
				tt.obs(&o)
			}
			sp := Compute(d, o)
			wantKind := domain.PlanUpdate
			if len(tt.paths) == 0 {
				wantKind = domain.PlanNoop // every remaining delta is undetectable by design
			}
			if sp.Kind != wantKind {
				t.Fatalf("kind = %s, want %s (changes %v)", sp.Kind, wantKind, sp.Changes)
			}
			assertPaths(t, sp.Changes, tt.paths)
			// Domain invariant: every entry has one side present.
			for _, c := range sp.Changes {
				if c.From == nil && c.To == nil && !c.SecretRef {
					t.Fatalf("change %s is fully absent in both directions", c.Path)
				}
			}
		})
	}
}

// ---- deterministic ordering ----------------------------------------------

func TestChangesAreSortedAndStable(t *testing.T) {
	d := baseService()
	d.Env["Z_KEY"], d.Env["A_KEY"] = "z", "a"
	o := obsFor(d)
	p1 := Compute(d, o)
	p2 := Compute(d, o)
	for i := 1; i < len(p1.Changes); i++ {
		if p1.Changes[i-1].Path > p1.Changes[i].Path {
			t.Fatalf("changes not sorted: %v", p1.Changes)
		}
	}
	if !reflect.DeepEqual(p1, p2) {
		t.Fatal("Compute is not deterministic across calls")
	}
}

func TestComputeAllSortedByRef(t *testing.T) {
	svcs := mustLoadFixtures(t) // tier-ordered input (core, observability, default)
	obs := map[domain.ServiceRef]domain.ObservedState{}
	for _, d := range svcs {
		obs[d.Ref] = SynthesizeObserved(d)
	}
	p := ComputeAll(svcs, obs)
	for i := 1; i < len(p.Services); i++ {
		a, b := p.Services[i-1].Ref, p.Services[i].Ref
		if a.Namespace > b.Namespace || (a.Namespace == b.Namespace && a.Name > b.Name) {
			t.Fatalf("ComputeAll output not sorted by ref: %v before %v", a, b)
		}
	}
}

// ---- self service (ADR-0005) ----------------------------------------------

func TestSelfServiceDriftIsReportOnly(t *testing.T) {
	d := baseService()
	d.Ref = domain.Ref(domain.CoreNamespace, "top-controller")
	d.Annotations = map[string]string{"top/special": "bootstrap"}

	drifted := obsFor(d)
	drifted.Image = "ghcr.io/example/other@sha256:2222"

	sp := Compute(d, drifted, WithSelfService(d.Ref))
	if sp.Kind != domain.PlanDriftedSelf {
		t.Fatalf("kind = %s, want drifted(self)", sp.Kind)
	}
	if len(sp.Changes) == 0 {
		t.Fatal("drifted(self) must still report its field changes")
	}
	if !sp.Kind.IsDestructive() {
		t.Fatal("drifted(self) must surface in Plan.Destructive so callers gate it")
	}

	// And when converged, the self service is an ordinary noop.
	if got := Compute(d, obsFor(d), WithSelfService(d.Ref)); got.Kind != domain.PlanNoop {
		t.Fatalf("converged self service = %s, want noop", got.Kind)
	}
}

// ---- orphan / prune rules ---------------------------------------------------

func TestOrphanedReportingAndPrune(t *testing.T) {
	d := baseService()
	orphan := domain.ObservedState{Ref: domain.Ref("default", "legacy"), Deployed: true, Running: true}
	coreOrphan := domain.ObservedState{Ref: domain.Ref(domain.CoreNamespace, "oneoff"), Deployed: true}

	t.Run("without prune: report only", func(t *testing.T) {
		p := ComputeAll([]domain.Service{d}, map[domain.ServiceRef]domain.ObservedState{
			d.Ref: obsFor(d), orphan.Ref: orphan, coreOrphan.Ref: coreOrphan,
		})
		if got := fmt.Sprint(p.Orphaned); got != "[core/oneoff default/legacy]" {
			t.Fatalf("orphans = %s", got)
		}
		for _, sp := range p.Services {
			if sp.Kind == domain.PlanDelete {
				t.Fatal("no delete plans without WithPruneAllowed")
			}
		}
		if p.Empty() {
			t.Fatal("orphaned reports make the plan non-empty for prune purposes... but reporting is the point: want false")
		}
	})

	t.Run("with prune: delete plans, core excluded", func(t *testing.T) {
		p := ComputeAll([]domain.Service{d}, map[domain.ServiceRef]domain.ObservedState{
			d.Ref: obsFor(d), orphan.Ref: orphan, coreOrphan.Ref: coreOrphan,
		}, WithPruneAllowed(true))
		deletes := map[domain.ServiceRef]bool{}
		for _, sp := range p.Services {
			if sp.Kind == domain.PlanDelete {
				deletes[sp.Ref] = true
				if len(sp.Changes) != 0 {
					t.Fatal("delete plans carry no field changes")
				}
			}
		}
		if deletes[orphan.Ref] == false || deletes[coreOrphan.Ref] {
			t.Fatalf("want delete for %v only, got %v", orphan.Ref, deletes)
		}
	})

	t.Run("self service orphan is never pruned", func(t *testing.T) {
		self := domain.Ref(domain.ObservabilityNamespace, "top-mirror")
		p := ComputeAll([]domain.Service{d}, map[domain.ServiceRef]domain.ObservedState{
			d.Ref: obsFor(d), self: {Ref: self, Deployed: true},
		}, WithPruneAllowed(true), WithSelfService(self))
		for _, sp := range p.Services {
			if sp.Kind == domain.PlanDelete {
				t.Fatal("self-service orphan must not get a delete plan")
			}
		}
	})
}

// ---- secret safety --------------------------------------------------------

func TestSecretEntriesNeverCarryValues(t *testing.T) {
	d := baseService()
	o := obsFor(d)
	o.SecretChecksums["SMTP_PASSWORD"] = "" // force a modify (name-only) entry
	sp := Compute(d, o)
	for _, c := range sp.Changes {
		if c.SecretRef && (c.From != nil || c.To != nil) {
			t.Fatalf("secret entry %s carries a value field", c)
		}
	}
	if sp.Changes == nil {
		t.Fatal("expected at least the unresolvable secret entry")
	}
}

// TestBranchPinnedBuildRevisionNotComparable documents the resolve-step
// boundary: a source-build ref that is a branch (not a sha) yields no
// revision entry — resolving branch→sha is the reconciler's job.
func TestBranchPinnedBuildRevisionNotComparable(t *testing.T) {
	d := domain.Service{
		Ref:    domain.Ref("default", "app"),
		Source: domain.Source{Image: "", Build: &domain.BuildSource{Repo: "example", Ref: "main"}},
	}
	o := domain.ObservedState{Ref: d.Ref, Deployed: true, Revision: "1111111111111111111111111111111111111111", Image: ""}
	sp := Compute(d, o)
	for _, c := range sp.Changes {
		if c.Path == "revision" {
			t.Fatal("branch-pinned build ref must not produce a revision change")
		}
	}
}

func TestFullSHABuildRevisionComparable(t *testing.T) {
	sha := "222233334444555566667777888899990000aaaabbbbccccddddeeeeffff1111"
	d := domain.Service{
		Ref:    domain.Ref("observability", "builder"),
		Source: domain.Source{Build: &domain.BuildSource{Repo: "example/platform", Ref: sha, Path: "."}},
	}
	o := domain.ObservedState{Ref: d.Ref, Deployed: true, Revision: "1111111111111111111111111111111111111111"}
	sp := Compute(d, o)
	if sp.Kind != domain.PlanUpdate || len(sp.Changes) != 1 || sp.Changes[0].Path != "revision" {
		t.Fatalf("want one revision change, got %s", sp)
	}
}

// ---- sanity: plan renders, Apply-able --------------------------------------

func TestPlanRenders(t *testing.T) {
	d := baseService()
	o := obsFor(d)
	d.Source.Digest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	sp := Compute(d, o)
	full := ComputeAll([]domain.Service{d}, map[domain.ServiceRef]domain.ObservedState{d.Ref: o}, WithSelfService(d.Ref))
	full.Revision = "9f8e7d6"
	if s := full.String(); len(s) == 0 {
		t.Fatal("rendered plan empty")
	}
	_ = sp
}
