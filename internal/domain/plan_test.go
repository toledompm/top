package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestFieldChangeOps(t *testing.T) {
	t.Parallel()
	v := "bar"
	add, err := NewFieldChange("env.FOO", nil, &v)
	if err != nil || add.op() != opAdd {
		t.Fatalf("add: %v %v", add, err)
	}
	if s := add.String(); s != "+env.FOO: <absent> → bar" {
		t.Fatalf("add render = %q", s)
	}
	rm, err := NewFieldChange("env.FOO", &v, nil)
	if err != nil || rm.op() != opRemove {
		t.Fatalf("remove: %v %v", rm, err)
	}
	mod, err := NewFieldChange("revision", strPtr("a1b2c3d"), strPtr("9f8e7d6"))
	if err != nil || mod.op() != opModify {
		t.Fatalf("modify: %v %v", mod, err)
	}
	if _, err := NewFieldChange("env.X", nil, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("both-absent change should be invalid, got %v", err)
	}
}

func strPtr(s string) *string { return &s }

func TestSecretFieldChangeRenderingIsValueFree(t *testing.T) {
	t.Parallel()
	//-proof plumbing: even if a caller mistakes a *value* for a name and
	// stuffs it into the change the secret-safe path renders nothing but
	// the path.
	c, err := NewSecretFieldChange("secretRefs.SMTP_PASSWORD", "")
	if err != nil {
		t.Fatalf("NewSecretFieldChange: %v", err)
	}
	rendered := c.String()
	if rendered != "~secretRefs.SMTP_PASSWORD (secret ref)" {
		t.Fatalf("render = %q", rendered)
	}
	for _, marker := range []string{"hunter2", "actual-password"} {
		if strings.Contains(rendered, marker) {
			t.Fatalf("render leaks %q: %q", marker, rendered)
		}
	}
	if c.From != nil || c.To != nil {
		t.Fatal("secret field change must carry nil From/To")
	}
	if _, err := NewSecretFieldChange("", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty path = %v", err)
	}
	if _, err := NewSecretFieldChange("x", "rocket"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad op = %v", err)
	}
	if add, err := NewSecretFieldChange("secretRefs.NEW", "+"); err != nil || add.op() != opAdd {
		t.Fatalf("forced add: %v %v", add, err)
	}
	if rm, err := NewSecretFieldChange("secretRefs.OLD", "-"); err != nil || rm.op() != opRemove {
		t.Fatalf("forced remove: %v %v", rm, err)
	}
}

func TestPlanRendering(t *testing.T) {
	t.Parallel()
	p := Plan{
		Revision: "a1b2c3d4e5f6",
		Services: []ServicePlan{
			{Ref: Ref("core", "top-controller"), Kind: PlanNoop},
			{
				Ref:  Ref("observability", "prometheus"),
				Kind: PlanUpdate,
				Changes: []FieldChange{
					{Path: "revision", From: strPtr("a1b2c3d"), To: strPtr("9f8e7d6")},
					mustChange(t, NewSecretFieldChange("secretRefs.SMTP_PASSWORD", "+")),
				},
			},
		},
		Orphaned: []ServiceRef{Ref("default", "stale")},
	}
	out := p.String()
	for _, want := range []string{
		"plan @a1b2c3d4e5f6:",
		"observability/prometheus: update",
		"~revision: a1b2c3d → 9f8e7d6",
		"+secretRefs.SMTP_PASSWORD (secret ref)",
		"orphaned: default/stale (delete requires --prune)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "core/top-controller: noop") {
		t.Fatalf("empty service plan should not render:\n%s", out)
	}
}

func mustChange(t *testing.T, c FieldChange, err error) FieldChange {
	t.Helper()
	if err != nil {
		t.Fatalf("change: %v", err)
	}
	return c
}

func TestPlanEmptyAndDestructive(t *testing.T) {
	t.Parallel()
	if !(Plan{}).Empty() {
		t.Fatal("zero plan should be empty")
	}
	p := Plan{
		Services: []ServicePlan{
			{Ref: Ref("default", "web"), Kind: PlanNoop},
			{Ref: Ref("default", "api"), Kind: PlanDelete},
		},
	}
	if p.Empty() {
		t.Fatal("plan with delete is not empty")
	}
	destr := p.Destructive()
	if len(destr) != 1 || destr[0].Kind != PlanDelete {
		t.Fatalf("Destructive = %v", destr)
	}
	if !(ServicePlan{Kind: PlanNoop}).Empty() {
		t.Fatal("noop service plan should be empty")
	}
	if (ServicePlan{Kind: PlanNoop, Changes: []FieldChange{{Path: "x", From: strPtr("1"), To: strPtr("2")}}}).Empty() {
		t.Fatal("noop with changes is a caller bug and must not report empty")
	}
	if !(ChangeKind("delete")).IsValid() || ChangeKind("misc").IsValid() {
		t.Fatal("ChangeKind.IsValid broken")
	}
	// Service lookup + not-found
	sp, err := p.Service(Ref("default", "api"))
	if err != nil || sp.Kind != PlanDelete {
		t.Fatalf("Plan.Service: %v %v", sp, err)
	}
	if _, err := p.Service(Ref("default", "zzz")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing service lookup = %v", err)
	}
}

func TestChangeKindStrings(t *testing.T) {
	t.Parallel()
	kinds := map[ChangeKind]string{
		PlanCreate: "create", PlanUpdate: "update", PlanDelete: "delete",
		PlanNoop: "noop", PlanDriftedSelf: "drifted(self)",
	}
	for k, s := range kinds {
		if k.String() != s {
			t.Fatalf("%s != %s", k, s)
		}
	}
	if !PlanDriftedSelf.IsDestructive() || PlanUpdate.IsDestructive() {
		t.Fatal("IsDestructive broken")
	}
}
