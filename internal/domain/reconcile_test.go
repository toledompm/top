package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSkipAndOutcomeEnums(t *testing.T) {
	t.Parallel()
	for _, r := range []SkipReason{SkipDependencyFailed, SkipSelfDrift, SkipLocked} {
		if !r.IsValid() {
			t.Fatalf("skip reason %q invalid", r)
		}
	}
	if SkipReason("vibes").IsValid() {
		t.Fatal("unknown skip reason valid")
	}
	for _, r := range []OutcomeResult{OutcomeNoop, OutcomeSucceeded, OutcomeFailed, OutcomeSkipped, OutcomeOrphaned} {
		if !r.IsValid() {
			t.Fatalf("outcome %q invalid", r)
		}
	}
	for _, s := range []RunStatus{RunPending, RunRunning, RunSucceeded, RunFailed} {
		if !s.IsValid() {
			t.Fatalf("run status %q invalid", s)
		}
	}
}

func TestServiceOutcomeValidation(t *testing.T) {
	t.Parallel()
	good := []ServiceOutcome{
		{Ref: Ref("core", "top-controller"), Result: OutcomeNoop},
		{Ref: Ref("observability", "prometheus"), Result: OutcomeSucceeded, Changes: []string{"revision a1b2c3d→9f8e7d6"}, DurationMs: 42100},
		{Ref: Ref("observability", "grafana"), Result: OutcomeFailed, Err: "readiness: checks failed (3 attempts): http 502", Hint: "top logs observability/grafana; last known-good still serving"},
		{Ref: Ref("default", "worker"), Result: OutcomeSkipped, Skip: SkipDependencyFailed},
		{Ref: Ref("default", "stale"), Result: OutcomeOrphaned},
	}
	for i, o := range good {
		if err := o.Validate(); err != nil {
			t.Fatalf("outcome %d invalid: %v", i, err)
		}
	}
	bad := []ServiceOutcome{
		{Ref: Ref("default", "x"), Result: OutcomeResult("explode")},
	}
	bad = append(bad,
		ServiceOutcome{Ref: Ref("default", "y"), Result: OutcomeSkipped},
		ServiceOutcome{Ref: Ref("default", "z"), Result: OutcomeFailed},
	)
	for i, o := range bad {
		if err := o.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad outcome %d should fail ErrInvalid, got %v", i, err)
		} else if !strings.Contains(err.Error(), o.Ref.String()) {
			t.Fatalf("outcome error should name the service: %v", err)
		}
	}
}

func TestRevisionRefValidation(t *testing.T) {
	t.Parallel()
	good := []RevisionRef{
		{Kind: RevisionGit, Value: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6a7b8c9d0"},
		{Kind: RevisionImage, Value: "docker.io/prom/prometheus@sha256:aa"},
	}
	for _, r := range good {
		if err := r.Validate(); err != nil {
			t.Fatalf("RevisionRef %v: %v", r, err)
		}
	}
	bad := []RevisionRef{
		{Kind: RevisionKind("tree"), Value: "x"},
		{Kind: RevisionGit, Value: "nosha"},
		{Kind: RevisionImage, Value: ""},
	}
	for _, r := range bad {
		if err := r.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("RevisionRef %+v should fail ErrInvalid, got %v", r, err)
		}
	}
	if !RevisionGit.IsValid() || RevisionKind("hash").IsValid() {
		t.Fatal("RevisionKind.IsValid broken")
	}
}

func TestRunStatusAndCheckpointShape(t *testing.T) {
	t.Parallel()
	cp := Checkpoint{
		LastApplied:   "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6a7b8c9d0",
		LastKnownGood: "",
		LastKnownGoodPerService: map[ServiceRef]RevisionRef{
			Ref("default", "web"):   {Kind: RevisionGit, Value: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6a7b8c9d0"},
			Ref("default", "image"): {Kind: RevisionImage, Value: "registry/app@sha256:bb"},
		},
	}
	for ref, r := range cp.LastKnownGoodPerService {
		if err := r.Validate(); err != nil {
			t.Fatalf("checkpoint ref %s: %v", ref, err)
		}
	}
	// run record shape mirrors the contract example JSON
	run := ReconciliationRun{
		Actor:         ActorWebhook,
		CorrelationID: "gh-delivery-9f2",
		Revision:      cp.LastApplied,
		Status:        RunFailed,
		StartedAt:     time.Now().UTC(),
		Outcomes: []ServiceOutcome{
			{Ref: Ref("observability", "grafana"), Result: OutcomeFailed, Err: "readiness: http 502"},
		},
		ValidationErr: "",
	}
	if run.Status != RunFailed || len(run.Outcomes) != 1 {
		t.Fatalf("run = %+v", run)
	}
}

func TestObservedStateShape(t *testing.T) {
	t.Parallel()
	o := ObservedState{
		Ref:             Ref("observability", "prometheus"),
		AppName:         "observability-prometheus",
		Deployed:        true,
		Running:         true,
		Revision:        "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6a7b8c9d0",
		Image:           "docker.io/prom/prometheus@sha256:ab12",
		EnvKeys:         []string{"TZ"},
		SecretChecksums: map[string]string{"SMTP_PASSWORD": "chk:b3"},
		Ports:           []Port{{Name: "http", ContainerPort: 9090}},
		Domains:         []string{"prometheus.apps.example.com"},
		StorageMounts:   []StorageMount{{Name: "data", HostPath: "/var/lib/dokku/data/storage/prometheus-data", MountPath: "/prometheus"}},
		RestartPolicy:   RestartAlways,
		Networks:        []string{"top-internal"},
		LastBuild:       BuildSucceeded,
		InspectedAt:     time.Now().UTC(),
	}
	if o.Absent() || o.Unhealthy() {
		t.Fatalf("healthy observed state misread: %+v", o)
	}
	if (ObservedState{}).Absent() != true {
		t.Fatal("zero observed state should count as absent")
	}
	failing := o
	failing.LastBuild = BuildFailed
	if !failing.Unhealthy() {
		t.Fatal("running+failed build should be unhealthy")
	}
	if b, err := ParseBuildStatus(""); err != nil || b != BuildNone {
		t.Fatalf("empty build status = %s %v", b, err)
	}
	if _, err := ParseBuildStatus("bronze"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad build status = %v", err)
	}
	for _, b := range []BuildStatus{BuildNone, BuildQueued, BuildRunning, BuildSucceeded, BuildFailed} {
		if !b.IsValid() {
			t.Fatalf("build status %q invalid", b)
		}
	}
	if (StorageMount{Name: "data", MountPath: "/prometheus"}).String() != "data:/prometheus" {
		t.Fatal("StorageMount.String broken")
	}
	if !ServiceHealthy.IsValid() || !ServiceSelfDrift.IsValid() || ServiceStatus("halved").IsValid() {
		t.Fatal("ServiceStatus.IsValid broken")
	}
	if _, err := ParseServiceStatus("degraded"); err != nil {
		t.Fatalf("ParseServiceStatus: %v", err)
	}
	if _, err := ParseServiceStatus("great"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad service status = %v", err)
	}
	if !BackendHealthy.IsValid() || BackendStatus("unknown").IsValid() {
		t.Fatal("BackendStatus.IsValid broken")
	}
}

func TestServiceAnnotation(t *testing.T) {
	t.Parallel()
	svc := Service{Annotations: map[string]string{"top/special": "bootstrap"}}
	if got := svc.Annotation("top/special"); got != "bootstrap" {
		t.Fatalf("annotation = %q", got)
	}
	if svc.Annotation("nope") != "" {
		t.Fatal("missing annotation should be empty")
	}
}
