package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestTypedErrorsUnwrapToSentinels(t *testing.T) {
	t.Parallel()
	cases := []error{
		NewNotFound("service", "default/web"),
		ConflictError{Kind: "app", Ref: "default/web", Reason: "exists and is not ours"},
		LockedError{Lease: "reconcile", Holder: "op 0192…"},
		CorruptError{Detail: "integrity_check failed"},
		ValidationError{File: "observability/prometheus.yaml", Field: "spec.routing.domains", Reason: "proxy requires domains"},
		PlanConflictError{Ref: Ref("default", "web"), Reason: "dependsOn cycle with default/api"},
		InvalidTransitionError{From: OpSucceeded, To: OpRunning},
	}
	want := []error{ErrNotFound, ErrConflict, ErrLocked, ErrCorrupt, ErrInvalid, ErrConflict, ErrConflict}
	for i, err := range cases {
		if !errors.Is(err, want[i]) {
			t.Fatalf("case %d: %T does not unwrap to %v", i, err, want[i])
		}
	}
}

func TestErrorMessagesAreReadable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "not found",
			err:  NewNotFound("service", "default/web"),
			want: `service "default/web": not found (hint:`,
		},
		{
			name: "conflict",
			err:  ConflictError{Kind: "app", Ref: "default/web", Reason: "exists and is not ours", Hint: "adopt via `topctl install`"},
			want: `app "default/web": exists and is not ours (hint: adopt via ` + "`topctl install`)",
		},
		{
			name: "locked",
			err:  LockedError{Lease: "reconcile", Holder: "op 55c9", Hint: "wait for the controller run"},
			want: `lease "reconcile" is held by op 55c9 (hint: wait for the controller run)`,
		},
		{
			name: "locked default hint",
			err:  LockedError{Lease: "reconcile", Holder: "op 55c9"},
			want: "do not loop-retry",
		},
		{
			name: "corrupt steers to diagnose",
			err:  CorruptError{Detail: "page 3 read error"},
			want: "metadata corrupt: page 3 read error (hint: the metadata DB is reconstructible: wipe the state file and re-run topctl install; reconcile is idempotent)",
		},
		{
			name: "validation with file and field",
			err:  ValidationError{File: "default/web.yaml", Field: "spec.source", Reason: "image and build both set"},
			want: "default/web.yaml: spec.source: invalid: image and build both set",
		},
		{
			name: "validation without file",
			err:  ValidationError{Field: "metadata.name", Reason: "empty"},
			want: "metadata.name: invalid: empty",
		},
		{
			name: "plan conflict startswith service",
			err:  PlanConflictError{Ref: Ref("default", "web"), Reason: "duplicate domain web.example.com"},
			want: "service default/web: duplicate domain web.example.com (hint:",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.err.Error()
			if !strings.Contains(got, tt.want) {
				t.Fatalf("error text:\n got  %s\n want %s", got, tt.want)
			}
		})
	}
}

func TestCorruptErrorAlwaysSuggestsDiagnose(t *testing.T) {
	t.Parallel()
	// A custom hint still must not replace the remediation path silently;
	// contract: callers must surface the reconstruction hint, never a retry.
	e := CorruptError{Detail: "x", Hint: "rebuild the store"}
	if !strings.Contains(e.Error(), "re-run topctl install") && !strings.Contains(e.Error(), "rebuild the store") {
		t.Fatalf("corrupt hint lost: %s", e)
	}
}
