// SPDX-License-Identifier: MIT

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/inventory"
)

// applyAndGate walks the ordered services and, per the contract's
// per-service loop — Inspect → Diff → if no-op: record → Apply → Readiness
// gate → record — produces one ServiceOutcome each. Failures are
// contained: a failing service records failed (its last known-good keeps
// serving), dependents skip with dependency-failed, independent services
// continue.
func (r *Reconciler) applyAndGate(ctx context.Context, in RunInput, ordered []domain.ServiceRef, pl domain.Plan, selfAdopted bool, selfAdoptErr error, res *RunResult) {
	planByRef := make(map[domain.ServiceRef]domain.ServicePlan, len(pl.Services))
	for _, sp := range pl.Services {
		planByRef[sp.Ref] = sp
	}
	failed := map[domain.ServiceRef]bool{} // skip source for dependents
	svcByRef := make(map[domain.ServiceRef]domain.Service, len(in.Specs))
	for _, svc := range in.Specs {
		svcByRef[svc.Ref] = svc
	}

	for _, ref := range ordered {
		if ctx.Err() != nil {
			// Cancelled mid-run: services not yet attempted simply stop.
			// They are not skips in the contract's sense (dependency
			// failure/self drift) — the run's own status carries the stop
			// (Run checks ctx after the loop and fails the run).
			return
		}
		// Controller-self special case (ADR-0005), adoption-aware
		// (T-402; internal/reconcile/adopt.go documents the three shapes).
		if ref.Equal(SelfRef) {
			sp, ok := planByRef[ref]
			if !ok || sp.Empty() || sp.Kind == domain.PlanNoop {
				// Noop (the anti-redeploy proof: a spec matching the
				// bootstrap deployment issues ZERO verbs against the app).
				res.Outcomes = append(res.Outcomes, domain.ServiceOutcome{
					Ref: ref, Result: domain.OutcomeNoop,
				})
				continue
			}
			if selfAdoptErr != nil {
				// The adoption check itself could not read the store:
				// contained — the ledger decides this run's self posture,
				// so an unreadable one is a real defect and records as
				// the outcome's failure, never a silent guess.
				res.Outcomes = append(res.Outcomes, domain.ServiceOutcome{
					Ref:  ref, Result: domain.OutcomeFailed,
					Err:  "adoption check: " + selfAdoptErr.Error(),
					Hint: "the ownership ledger decides the controller-self posture; " + hintLogs,
				})
				continue
			}
			if !selfAdopted {
				res.Outcomes = append(res.Outcomes, domain.ServiceOutcome{
					Ref: ref, Result: domain.OutcomeSkipped, Skip: domain.SkipSelfDrift,
					Changes: renderChanges(sp.Changes),
					Hint:    "controller drift is report-only until the bootstrap handoff (install's first-reconcile adoption, contract phase 5)",
				})
				continue
			}
			if selfNeedsHumanGate(sp) {
				res.Outcomes = append(res.Outcomes, domain.ServiceOutcome{
					Ref: ref, Result: domain.OutcomeSkipped, Skip: domain.SkipSelfDrift,
					Changes: renderChanges(sp.Changes),
					Hint:    "controller self-update is human-gated (ADR-0005): run `topctl deploy core/top-controller`; the reconciler reports image/revision drift on itself but never applies it",
				})
				continue
			}
			// Adopted, field-only drift: fall through to the normal
			// per-service path (EnsureApp is adopt-if-exists; the drift
			// carries no revision entry, so no deploy verb fires).
		}
		// A failed dependency skips this service (contract rule).
		if blockedByFailed(ref, svcByRef, failed) {
			res.Outcomes = append(res.Outcomes, domain.ServiceOutcome{
				Ref: ref, Result: domain.OutcomeSkipped, Skip: domain.SkipDependencyFailed,
				Hint: "a dependsOn prerequisite failed this run",
			})
			continue
		}
		sp, ok := planByRef[ref]
		if !ok || sp.Empty() {
			// No plan entry (orphan-only ref) or a genuine noop: record.
			res.Outcomes = append(res.Outcomes, domain.ServiceOutcome{
				Ref: ref, Result: domain.OutcomeNoop, Changes: planChanges(planByRef[ref]),
			})
			continue
		}
		if sp.Kind == domain.PlanDriftedSelf && !ref.Equal(SelfRef) {
			res.Outcomes = append(res.Outcomes, domain.ServiceOutcome{
				Ref: ref, Result: domain.OutcomeSkipped, Skip: domain.SkipSelfDrift,
				Changes: renderChanges(sp.Changes),
				Hint:    "self-service drift is reported, never auto-applied (ADR-0005)",
			})
			continue
		}
		if sp.Kind == domain.PlanDelete {
			// Delete decisions execute in the prune step, never here.
			continue
		}
		out := r.applyService(ctx, in, svcByRef[ref], sp)
		if out.Result == domain.OutcomeFailed {
			failed[ref] = true
		}
		res.Outcomes = append(res.Outcomes, out)
	}
}

// planChanges renders the change strings of a plan entry ("" for absent).
func planChanges(sp domain.ServicePlan) []string {
	if sp.Kind == "" {
		return nil
	}
	if sp.Kind == domain.PlanDelete {
		return nil // deletions carry no field changes
	}
	return renderChanges(sp.Changes)
}

// renderChanges stringifies field changes (secret values can never leak —
// the domain renderer refuses From/To on SecretRef entries).
func renderChanges(cs []domain.FieldChange) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.String())
	}
	return out
}

// blockedByFailed reports whether any of the service's dependsOn targets
// failed this run. Transitive failure propagates: A fails, B depends on A
// skips, C depends on B sees B skipped — B's result is skipped, not
// failed, so C's skip traces to the first real failure through B's own
// dependsOn (A, failed).
func blockedByFailed(ref domain.ServiceRef, byRef map[domain.ServiceRef]domain.Service, failed map[domain.ServiceRef]bool) bool {
	svc, ok := byRef[ref]
	if !ok || len(svc.DependsOn) == 0 {
		return false
	}
	for _, dep := range svc.DependsOn {
		if failed[dep] {
			return true
		}
	}
	return false
}

// applyService converges ONE service: create/update per the plan's field
// changes, then the readiness gate. Every failure is the outcome's typed
// error text; nothing here aborts the run.
func (r *Reconciler) applyService(ctx context.Context, in RunInput, svc domain.Service, sp domain.ServicePlan) domain.ServiceOutcome {
	out := domain.ServiceOutcome{Ref: svc.Ref}
	changes := renderChanges(sp.Changes)
	start := r.clock.Now()
	defer func() { out.DurationMs = r.clock.Now().Sub(start).Milliseconds() }()
	if len(changes) > 0 {
		out.Changes = changes
	}

	fail := func(err error) domain.ServiceOutcome {
		out.Result = domain.OutcomeFailed
		out.Err = err.Error()
		out.Hint = hintLogs
		return out
	}

	// 1. Ensure the app exists (adopt-if-exists is the backend's rule).
	if err := r.backend.EnsureApp(ctx, svc.Ref); err != nil {
		app, _ := domain.AppName(svc.Ref.Namespace, svc.Ref.Name)
		return fail(fmt.Errorf("ensure app %s: %w", app, err))
	}
	if err := r.ledgerApp(ctx, svc.Ref); err != nil {
		return fail(fmt.Errorf("ledger app %s: %w", svc.Ref, err))
	}

	// 2. Apply the plan's field changes (apply anchors itself on what the
	// plan observed; the same objects apply executed).
	needed, err := r.applyChanges(ctx, in, svc, sp)
	if err != nil {
		return fail(err)
	}

	// 3. New deployment when the revision changed (DeployImage is
	// deliberately NOT no-op-safe — only call it when the diff says so).
	if needed.deploy {
		switch {
		case svc.Source.Image != "":
			if err := r.backend.DeployImage(ctx, svc.Ref, svc.Source.Reference()); err != nil {
				return fail(fmt.Errorf("deploy %s: %w", svc.Source.Reference(), err))
			}
		default:
			// Source builds reach this seam only through git:sync, which
			// the gitsource wave owns (T-303). A revision change the
			// reconciler cannot apply is a contained failure — the old
			// release keeps serving behind the readiness gate.
			return fail(errors.New(
				"apply: source-build revision changes require the git:sync seam (T-303); reported, last known-good keeps serving"))
		}
	}
	// 4. Readiness gate (deployed + running under the bounded deadline;
	// build sources additionally refuse a last-failed build).
	if err := r.awaitReadiness(ctx, svc, needed.deploy); err != nil {
		return fail(err)
	}
	out.Result = domain.OutcomeSucceeded
	return out
}

// applyNeeded carries which apply effects the run must execute for one
// service: a new deployment, and the family flags that fire the
// full-replace convergence calls.
type applyNeeded struct {
	deploy                                bool
	env, ports, domains, restart, storage bool
}

// applyChanges executes the plan's field-level entries for ONE service.
// Full-replace families (ports, domains) collapse their +/~/- entries into
// a single set call with the desired state — that is what `ports:set` /
// `domains:set` do, and it makes removal idempotent.
func (r *Reconciler) applyChanges(ctx context.Context, in RunInput, svc domain.Service, sp domain.ServicePlan) (applyNeeded, error) {
	var need applyNeeded

	for _, c := range sp.Changes {
		switch {
		case c.Path == "revision":
			need.deploy = true

		case strings.HasPrefix(c.Path, "env."):
			need.env = true

		case c.SecretRef && strings.HasPrefix(c.Path, "secretRefs."):
			name := strings.TrimPrefix(c.Path, "secretRefs.")
			// Never execute a secret change without a resolver: values
			// must come from the resolution seam, never be guessed.
			if r.secrets == nil {
				return need, fmt.Errorf("%w: secret ref %s: no secret provider configured (ADR-0008)",
					domain.ErrInvalid, name)
			}
			value, _, err := r.secrets.Resolve(ctx, name)
			if err != nil {
				return need, fmt.Errorf("resolve secret %s: %w", name, err)
			}
			// The value goes straight into the backend call; it is never
			// logged or recorded (the outcome's error text above and the
			// plan carry only the NAME).
			if err := r.backend.SetSecretEnv(ctx, svc.Ref, name, value); err != nil {
				return need, fmt.Errorf("set secret %s: %w", name, err)
			}
		case strings.HasPrefix(c.Path, "ports."):
			need.ports = true

		case strings.HasPrefix(c.Path, "domains."):
			need.domains = true

		case strings.HasPrefix(c.Path, "networks."):
			switch {
			case c.To != nil:
				if err := r.attachNetwork(ctx, svc, *c.To); err != nil {
					return need, err
				}
			default:
				// Network removals (operator-attached extras) are NOT ours
				// to slide: like Dokku env, the attach list is a sink for
				// things TOP did not set this run.
			}

		case strings.HasPrefix(c.Path, "storage."):
			// Storage adds/renames ensure+attach; removals detach (the
			// entry itself stays — data lifecycle is never an implicit
			// decision, ADR-0009).
			name, mount, ok := splitStoragePath(c.Path)
			if !ok {
				return need, fmt.Errorf("%w: storage change %q: unnamable entry", domain.ErrInvalid, c.Path)
			}
			hostDir := storageHostDir(in.VolumeRoot, svc.Ref, name)
			if hostDir == "" {
				return need, errors.New(
					"apply: storage change but no VolumeRoot configured; refusing to mount un-owned host dirs (ADR-0009)")
			}
			app, _ := domain.AppName(svc.Ref.Namespace, svc.Ref.Name)
			if err := r.backend.EnsureStorageEntry(ctx, app+"-"+name, hostDir); err != nil {
				return need, fmt.Errorf("ensure storage entry %s: %w", app+"-"+name, err)
			}
			if c.To != nil {
				if err := r.backend.AttachStorage(ctx, svc.Ref, app+"-"+name, mount); err != nil {
					return need, fmt.Errorf("attach storage %s:%s: %w", name, mount, err)
				}
			} else if c.From != nil {
				if err := r.backend.DetachStorage(ctx, svc.Ref, app+"-"+name, mount); err != nil {
					return need, fmt.Errorf("detach storage %s:%s: %w", name, mount, err)
				}
			}
			need.storage = true

		case c.Path == "restart-policy":
			if err := r.backend.SetRestartPolicy(ctx, svc.Ref, svc.RestartPolicy); err != nil {
				return need, fmt.Errorf("set restart policy: %w", err)
			}
			need.restart = true

		default:
			// Unapplied-by-this-seam change families are deliberate
			// report-only gaps (plan README's coverage matrix), not
			// failures to force-reapply every run.
		}
	}

	// Full-replace families fire once per service (even when several field
	// entries belong to the same set call).
	if need.env {
		if err := r.backend.SetConfigEnv(ctx, svc.Ref, svc.Env); err != nil {
			return need, fmt.Errorf("set config env: %w", err)
		}
	}
	if need.ports {
		if err := r.backend.SetPorts(ctx, svc.Ref, svc.Ports); err != nil {
			return need, fmt.Errorf("set ports: %w", err)
		}
	}
	if need.domains {
		if err := r.backend.SetDomains(ctx, svc.Ref, svc.Routing.Domains); err != nil {
			return need, fmt.Errorf("set domains: %w", err)
		}
	}
	return need, nil
}

// attachNetwork attaches the shared internal network when the routing mode
// is internal (the network itself is ensured before the first such attach).
func (r *Reconciler) attachNetwork(ctx context.Context, svc domain.Service, network string) error {
	if err := r.backend.EnsureNetwork(ctx, network); err != nil {
		return fmt.Errorf("ensure network %s: %w", network, err)
	}
	if err := r.backend.AttachNetwork(ctx, svc.Ref, network); err != nil {
		return fmt.Errorf("attach network %s: %w", network, err)
	}
	return nil
}

// splitStoragePath pulls ("<entry-name>", "<mountpath>") out of the
// plan's "storage.<name>:<mount>" change path.
func splitStoragePath(path string) (name, mount string, ok bool) {
	if !strings.HasPrefix(path, "storage.") {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, "storage.")
	name, mount, ok = strings.Cut(rest, ":")
	if !ok || name == "" || mount == "" {
		return "", "", false
	}
	return name, mount, true
}

// storageHostDir renders the TOP-managed host directory for one entry:
// <VolumeRoot>/<app>-<name>. Empty VolumeRoot returns "" (refusal).
func storageHostDir(root string, ref domain.ServiceRef, name string) string {
	if root == "" {
		return ""
	}
	app, _ := domain.AppName(ref.Namespace, ref.Name)
	return filepath.Join(root, app+"-"+name)
}

// ledgerApp records the app row in the ownership ledger when absent
// (ADR-0009: everything TOP creates is ledgered, attributed to the run's
// operation). Idempotent — an existing row is left alone.
func (r *Reconciler) ledgerApp(ctx context.Context, ref domain.ServiceRef) error {
	app, err := domain.AppName(ref.Namespace, ref.Name)
	if err != nil {
		return fmt.Errorf("%w: ledger app name for %s: %v", domain.ErrInvalid, ref, err)
	}
	entries, err := r.store.ListLedger(ctx, inventory.KindApp)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Identifier == app {
			return nil
		}
	}
	labels := map[string]string{
		inventory.LabelManaged:   inventory.ManagedTrue,
		inventory.LabelNamespace: string(ref.Namespace),
		inventory.LabelService:   string(ref.Name),
	}
	return inventory.MarkCreated(ctx, r.store, inventory.KindApp, app,
		opIDFromContext(ctx), labels, r.clock.Now())
}

// opContextKey threads the run's operation id into the apply path for
// ledger attribution (nothing else on the path needs it).
type opContextKey struct{}

// WithOpID records the run's operation id for ledger attribution.
func WithOpID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, opContextKey{}, id)
}

func opIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(opContextKey{}).(string); ok {
		return v
	}
	return ""
}

// awaitReadiness is the readiness gate (contract's bounded-wait rule):
// inspect until the service reports deployed AND running (and its last
// build not failed), or the deadline passes → contained "readiness"
// failure with the last observed state. Image services gate additionally
// on the deployed image matching the desired reference when a deploy was
// executed (Build services: the source-build seam reports its own
// revision; the gate only sees Deployed+Running).
func (r *Reconciler) awaitReadiness(ctx context.Context, svc domain.Service, deployed bool) error {
	deadline := r.clock.Now().Add(r.readyGate.deadline)
	for {
		last, lerr := r.backend.Inspect(ctx, svc.Ref)
		if lerr == nil && last.Deployed && last.Running && last.LastBuild != domain.BuildFailed && revisionOK(svc, last) == nil {
			return nil
		}
		if lerr != nil && errors.Is(lerr, domain.ErrUnhealthy) {
			return fmt.Errorf("readiness %s: %w", svc.Ref, lerr)
		}
		now := r.clock.Now()
		if !now.Before(deadline) {
			if lerr != nil {
				return fmt.Errorf("readiness %s: %w (deadline %s)", svc.Ref, lerr, r.readyGate.deadline)
			}
			return fmt.Errorf("readiness %s: not ready within %s (deployed=%v running=%v build=%s image=%q revision=%q; %s)",
				svc.Ref, r.readyGate.deadline, last.Deployed, last.Running, last.LastBuild, last.Image, last.Revision, hintLogs)
		}
		if err := r.clock.Wait(ctx, r.readyGate.every); err != nil {
			return fmt.Errorf("readiness %s: %w", svc.Ref, err)
		}
	}
}

// revisionOK gate extension: when a deployment was just executed, the
// observed revision must already match what the plan wanted (image
// services: the pinned reference; git services: the pinned sha). A
// mutable reference is not comparable — the diff already warns about it.
func revisionOK(svc domain.Service, last domain.ObservedState) error {
	switch {
	case svc.Source.Image != "":
		want := wantRevisionImage(svc)
		if want == "" {
			return nil
		}
		if last.Image != want {
			return fmt.Errorf("image %q not yet %q", last.Image, want)
		}
	case svc.Source.Build != nil && isPinnedSHA(svc.Source.Build.Ref):
		if last.Revision != svc.Source.Build.Ref {
			return fmt.Errorf("revision %q not yet %q", last.Revision, svc.Source.Build.Ref)
		}
	}
	return nil
}

// wantRevisionImage returns the comparable image reference; mutable
// (unpinned) references return "" (not comparable).
func wantRevisionImage(svc domain.Service) string {
	switch {
	case svc.Source.Digest != "":
		return svc.Source.Image + "@" + svc.Source.Digest
	case svc.Source.Tag != "":
		return svc.Source.Image + ":" + svc.Source.Tag
	default:
		return "" // implicit latest: not pinning vocabulary
	}
}

func isPinnedSHA(ref string) bool {
	return (len(ref) == 40 || len(ref) == 64) && domain.ValidRevision(ref) == nil
}
