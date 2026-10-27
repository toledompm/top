package domain

import (
	"fmt"
	"strings"
)

// ChangeKind is the per-service kind of change a plan carries (reconciler
// contract: `create | update | delete | noop | drifted(self)`).
type ChangeKind string

const (
	// PlanCreate: the app must be created and converged.
	PlanCreate ChangeKind = "create"
	// PlanUpdate: the app exists and needs convergence.
	PlanUpdate ChangeKind = "update"
	// PlanDelete: the app is orphaned from Git; deletion happens only via
	// explicit --prune on non-core namespaces (ADR-0004).
	PlanDelete ChangeKind = "delete"
	// PlanNoop: nothing to do — the tested idempotency invariant.
	PlanNoop ChangeKind = "noop"
	// PlanDriftedSelf: drift on TOP's own controller; reported, never
	// auto-applied (ADR-0005).
	PlanDriftedSelf ChangeKind = "drifted(self)"
)

// IsValid reports whether k is a known change kind.
func (k ChangeKind) IsValid() bool {
	switch k {
	case PlanCreate, PlanUpdate, PlanDelete, PlanNoop, PlanDriftedSelf:
		return true
	}
	return false
}

// String implements fmt.Stringer.
func (k ChangeKind) String() string { return string(k) }

// IsDestructive reports whether applying the kind removes a workload
// (delete) or defers to a human (drifted-self) — callers gate both on the
// confirmation/dry-run logic of ADR-0009.
func (k ChangeKind) IsDestructive() bool {
	return k == PlanDelete || k == PlanDriftedSelf
}

// changeOp is the character of a single field-level change: the field was
// added, removed, or modified between From and To.
type changeOp string

const (
	opAdd    changeOp = "+"
	opRemove changeOp = "-"
	opModify changeOp = "~"
)

// NewFieldChange builds an add/remove/modify change by pointer presence:
// From nil means the field did not exist; To nil means it is removed.
// At least one of From/To must be non-nil.
func NewFieldChange(path string, from, to *string) (FieldChange, error) {
	c := FieldChange{Path: path, From: from, To: to}
	if from == nil && to == nil {
		return c, fmt.Errorf("%w: field change %q: both from and to absent", ErrInvalid, path)
	}
	return c, nil
}

// FieldChange is one field-level difference between desired and observed.
//
// Secret safety (ADR-0008, contracts/README.md): a change on a
// secret-reference path MUST be constructed with NewSecretFieldChange and
// carries only the reference *name*; From/To stay nil so that no value —
// however the caller slices it — can leak through rendering or JSON.
// Renderers also refuse to print From/To on SecretRef entries as a
// belt-and-suspenders guard.
type FieldChange struct {
	// Path is a stable dotted path, e.g. "revision", "env.TZ",
	// "storage.data:/prometheus", "secretRefs.SMTP_PASSWORD".
	Path string
	// From is the observed value, nil when the field did not exist.
	From *string
	// To is the desired value, nil when the field is removed.
	To *string
	// SecretRef marks a secret-reference entry; value fields are nil.
	SecretRef bool
	// secretOp forces the rendered marker for secret-reference entries:
	// "" (modify), "+" (add), "-" (remove).
	secretOp string
}

func (c FieldChange) op() changeOp {
	if c.SecretRef {
		switch c.secretOp {
		case "+":
			return opAdd
		case "-":
			return opRemove
		default:
			return opModify
		}
	}
	switch {
	case c.From == nil && c.To != nil:
		return opAdd
	case c.From != nil && c.To == nil:
		return opRemove
	default:
		return opModify
	}
}

// NewSecretFieldChange builds the secret-safe change for a secret-bearing
// path (a secretRef entry, or an env key that is a secret reference): only
// the path name survives; From/To are deliberately nil, so no value can
// leak through rendering, JSON, or later edits. op is "" for modify, or a
// leading marker "+" / "-" to force add/remove rendering.
func NewSecretFieldChange(path, op string) (FieldChange, error) {
	if path == "" {
		return FieldChange{}, fmt.Errorf("%w: secret field change: empty path", ErrInvalid)
	}
	switch op {
	case "", "+", "-":
	default:
		return FieldChange{}, fmt.Errorf("%w: secret field change %q: op must be empty, +, or -", ErrInvalid, path)
	}
	return FieldChange{Path: path, SecretRef: true, secretOp: op}, nil
}

// String renders the change compactly. Secret-reference entries render as
// name + marker only — e.g. `~SMTP_PASSWORD (secret ref)` — matching the
// reconciler diff vocabulary `env: +FOO, ~BAR (secret ref), -BAZ`.
func (c FieldChange) String() string {
	if c.SecretRef {
		return fmt.Sprintf("%s%s (secret ref)", c.op(), c.Path)
	}
	return fmt.Sprintf("%s%s: %s → %s", c.op(), c.Path, renderVal(c.From), renderVal(c.To))
}

func renderVal(v *string) string {
	if v == nil {
		return "<absent>"
	}
	return *v
}

// ServicePlan is the plan for one service: its change kind plus the
// field-level changes that explain it.
type ServicePlan struct {
	Ref     ServiceRef
	Kind    ChangeKind
	Changes []FieldChange
}

// String renders one line per change after a header, e.g.:
//
//	observability/prometheus: update
//	  ~revision: a1b2c3d → 9f8e7d6
//	  +env.FOO: <absent> → bar
func (p ServicePlan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", p.Ref, p.Kind)
	for _, c := range p.Changes {
		fmt.Fprintf(&b, "\n  %s", c)
	}
	return b.String()
}

// Empty reports whether the plan is a genuine no-op: it must be PlanNoop
// AND carry no changes (a noop with changes is a caller bug).
func (p ServicePlan) Empty() bool {
	return p.Kind == PlanNoop && len(p.Changes) == 0
}

// Plan is the complete, zero-side-effect diff of a reconciliation — the
// same object `topctl diff` prints and apply executes (ADR-0002 parity
// guarantee). Apply iterates it; diff only renders it.
type Plan struct {
	Revision string // desired state revision the plan works toward
	Services []ServicePlan
	// Orphaned lists observed services absent from Git; they surface as
	// PlanDelete service plans on --prune and as reports otherwise.
	Orphaned []ServiceRef
}

// Service returns the plan for a ref, or ErrNotFound wrapped with its name.
func (p Plan) Service(ref ServiceRef) (ServicePlan, error) {
	for _, sp := range p.Services {
		if sp.Ref.Equal(ref) {
			return sp, nil
		}
	}
	return ServicePlan{}, NewNotFound("plan for service", ref.String())
}

// Empty reports whether the whole plan would change nothing.
func (p Plan) Empty() bool {
	for _, sp := range p.Services {
		if !sp.Empty() {
			return false
		}
	}
	return len(p.Orphaned) == 0
}

// Destructive lists every service plan whose application removes or
// re-creates work (delete / drifted-self) — the surface confirmation
// gates check (ADR-0009).
func (p Plan) Destructive() []ServicePlan {
	var out []ServicePlan
	for _, sp := range p.Services {
		if sp.Kind.IsDestructive() {
			out = append(out, sp)
		}
	}
	return out
}

// String renders the full plan; empty plans render as "noop (no changes)".
func (p Plan) String() string {
	if p.Empty() {
		return fmt.Sprintf("plan @%s: noop (no changes)", p.Revision)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "plan @%s:", p.Revision)
	first := true
	for _, sp := range p.Services {
		if sp.Empty() {
			continue
		}
		if first {
			b.WriteString("\n")
			first = false
		}
		for _, line := range strings.Split(sp.String(), "\n") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	for _, ref := range p.Orphaned {
		fmt.Fprintf(&b, "  orphaned: %s (delete requires --prune)\n", ref)
	}
	return strings.TrimRight(b.String(), "\n")
}
