package domain

import (
	"errors"
	"testing"
	"time"
)

func TestOperationLifecycleTransitions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		from, to    OperationState
		wantInvalid bool
	}{
		{name: "pending to running", from: OpPending, to: OpRunning},
		{name: "pending aborted", from: OpPending, to: OpAborted},
		{name: "running succeeded", from: OpRunning, to: OpSucceeded},
		{name: "running failed", from: OpRunning, to: OpFailed},
		{name: "running aborted", from: OpRunning, to: OpAborted},
		// invalid: skips, reversals, terminal exits
		{name: "pending failed skip", from: OpPending, to: OpFailed, wantInvalid: true},
		{name: "pending succeeded skip", from: OpPending, to: OpSucceeded, wantInvalid: true},
		{name: "running back to pending", from: OpRunning, to: OpPending, wantInvalid: true},
		{name: "succeeded to running", from: OpSucceeded, to: OpRunning, wantInvalid: true},
		{name: "failed to running", from: OpFailed, to: OpRunning, wantInvalid: true},
		{name: "aborted to pending", from: OpAborted, to: OpPending, wantInvalid: true},
		{name: "succeeded repeated", from: OpSucceeded, to: OpSucceeded, wantInvalid: true},
		{name: "unknown state", from: OpPending, to: OperationState("weird"), wantInvalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			op := mustOp(t, OpReconcile, ActorWebhook, "del-1", "a1b2c3d4e5f6")
			// reach tt.from through valid transitions only
			if tt.from != OpPending {
				if err := op.Next(OpRunning); err != nil {
					t.Fatalf("setup running: %v", err)
				}
				if tt.from != OpRunning {
					if err := op.Next(tt.from); err != nil {
						t.Fatalf("setup reach(%s): %v", tt.from, err)
					}
				}
			}
			err := op.Next(tt.to)
			if tt.wantInvalid {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("Next(%s→%s) err = %v, want ErrConflict", tt.from, tt.to, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Next(%s→%s) unexpected: %v", tt.from, tt.to, err)
			}
			if op.State != tt.to {
				t.Fatalf("state = %s, want %s", op.State, tt.to)
			}
		})
	}
}

func TestOperationStartFinish(t *testing.T) {
	t.Parallel()
	op := mustOp(t, OpDeploy, ActorUser, "", "")
	if op.IsTerminal() {
		t.Fatal("fresh op not terminal")
	}
	if err := op.Start(); err != nil || op.State != OpRunning {
		t.Fatalf("Start: %v", err)
	}
	if err := op.Next(OpSucceeded); err != nil || !op.IsTerminal() {
		t.Fatalf("finish: %v", err)
	}
}

func mustOp(t *testing.T, kind OperationKind, actor Actor, corr, rev string) *Operation {
	t.Helper()
	op, err := NewOperation(kind, actor, corr, rev)
	if err != nil {
		t.Fatalf("NewOperation: %v", err)
	}
	return op
}

func TestOperationTimestampBookkeeping(t *testing.T) {
	t.Parallel()
	op := mustOp(t, OpDeploy, ActorUser, "", "")
	if op.IsTerminal() {
		t.Fatal("fresh op not terminal")
	}
	var zero time.Time
	if !op.StartedAt.Equal(zero) || !op.FinishedAt.Equal(zero) {
		t.Fatal("fresh op should have zero timestamps")
	}
	if err := op.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if op.StartedAt.IsZero() || !op.FinishedAt.IsZero() {
		t.Fatal("start should set StartedAt only")
	}
	if err := op.Fail("readiness: checks failed (3 attempts): http 502"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if op.State != OpFailed || op.Err == "" || op.FinishedAt.IsZero() {
		t.Fatalf("fail bookkeeping: %s %q %v", op.State, op.Err, op.FinishedAt)
	}
	if err := op.Fail("again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("re-fail should conflict, got %v", err)
	}
}

func TestOperationFailRequiresMessage(t *testing.T) {
	t.Parallel()
	op := mustOp(t, OpDeploy, ActorSchedule, "", "")
	_ = op.Start()
	if err := op.Fail(""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty fail message = %v", err)
	}
}

func TestNewOperationValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewOperation(OperationKind("brew"), ActorUser, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad kind = %v", err)
	}
	if _, err := NewOperation(OpReconcile, Actor("ghost"), "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad actor = %v", err)
	}
	op, err := NewOperation(OpDeploy, ActorSchedule, "sched-1", "")
	if err != nil {
		t.Fatalf("NewOperation: %v", err)
	}
	if op.State != OpPending || op.ID.String() == "" {
		t.Fatalf("op = %+v", op)
	}
}

func TestOperationEnums(t *testing.T) {
	t.Parallel()
	for _, k := range []OperationKind{OpInstall, OpReconcile, OpDeploy, OpUninstall, OpOther} {
		if !k.IsValid() || k.String() == "" {
			t.Fatalf("kind %q invalid", k)
		}
	}
	if OperationKind("chore").IsValid() {
		t.Fatal("unknown kind valid")
	}
	for _, a := range []Actor{ActorUser, ActorController, ActorWebhook, ActorSchedule} {
		if !a.IsValid() {
			t.Fatalf("actor %q invalid", a)
		}
	}
	for _, s := range []OperationState{OpPending, OpRunning, OpSucceeded, OpFailed, OpAborted} {
		if !s.IsValid() {
			t.Fatalf("state %q invalid", s)
		}
	}
	if OperationState("archived").IsValid() {
		t.Fatal("unknown state valid")
	}
}

func TestInvalidTransitionErrorShape(t *testing.T) {
	t.Parallel()
	err := InvalidTransitionError{From: OpSucceeded, To: OpRunning}
	var target InvalidTransitionError
	if !errors.As(errors.Join(err, errors.New("wrapped")), &target) {
		t.Fatal("As(InvalidTransitionError) failed")
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatal("InvalidTransitionError must unwrap to ErrConflict")
	}
}
