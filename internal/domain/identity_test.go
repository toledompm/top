package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestAppName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		ns      Namespace
		svc     Name
		want    string
		wantErr error
		errMsg  string
	}{
		{name: "plain", ns: "default", svc: "web", want: "default-web"},
		{name: "reserved core", ns: "core", svc: "top-controller", want: "core-top-controller"},
		{name: "observability", ns: "observability", svc: "prometheus", want: "observability-prometheus"},
		{name: "hyphens", ns: "team-a", svc: "web-1", want: "team-a-web-1"},
		{name: "exactly 48 chars is ok", ns: "n", svc: Name(strings.Repeat("a", 46)), want: "n-" + strings.Repeat("a", 46)},
		{name: "49 chars total", ns: "nn", svc: Name(strings.Repeat("a", 46)), wantErr: ErrInvalid, errMsg: "exceeds"},
		{name: "way too long", ns: Namespace(strings.Repeat("a", 50)), svc: "web", wantErr: ErrInvalid, errMsg: "max 48"},
		{name: "upper case namespace", ns: "Team", svc: "web", wantErr: ErrInvalid, errMsg: "must match"},
		{name: "upper case name", ns: "team", svc: "Web", wantErr: ErrInvalid, errMsg: "must match"},
		{name: "leading hyphen name", ns: "team", svc: "-web", wantErr: ErrInvalid, errMsg: "must match"},
		{name: "trailing hyphen name", ns: "team", svc: "web-", wantErr: ErrInvalid, errMsg: "must match"},
		{name: "leading digit ok", ns: "0team", svc: "9web", want: "0team-9web"},
		{name: "empty namespace", ns: "", svc: "web", wantErr: ErrInvalid, errMsg: "empty"},
		{name: "empty name", ns: "team", svc: "", wantErr: ErrInvalid, errMsg: "empty"},
		{name: "underscore rejected", ns: "team", svc: "web_x", wantErr: ErrInvalid, errMsg: "must match"},
		{name: "dots rejected", ns: "team", svc: "web.x", wantErr: ErrInvalid, errMsg: "must match"},
		{name: "double hyphen ok", ns: "a--b", svc: "c--d", want: "a--b-c--d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := AppName(tt.ns, tt.svc)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("AppName(%s, %s) err = %v, want Is(%v)", tt.ns, tt.svc, err, tt.wantErr)
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Fatalf("error %q does not contain %q", err, tt.errMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("AppName(%s, %s) unexpected error: %v", tt.ns, tt.svc, err)
			}
			if got != tt.want {
				t.Fatalf("AppName = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAppNameUnderscore57(t *testing.T) {
	t.Parallel()
	// sanity guard on the 48-char budget arithmetic used above
	if _, err := AppName("n", Name(strings.Repeat("a", 48))); err == nil {
		t.Fatal("expected 50-char app name to be rejected")
	}
}

func TestServiceRefRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    ServiceRef
		wantErr bool
	}{
		{in: "default/web", want: Ref("default", "web")},
		{in: "observability/prometheus", want: Ref("observability", "prometheus")},
		// round trip through String
	}
	for _, tt := range tests {
		ref, err := ParseServiceRef(tt.in)
		if tt.wantErr {
			continue
		}
		if err != nil {
			t.Fatalf("ParseServiceRef(%q): %v", tt.in, err)
		}
		if !ref.Equal(tt.want) {
			t.Fatalf("ParseServiceRef(%q) = %v, want %v", tt.in, ref, tt.want)
		}
		back, err := ParseServiceRef(ref.String())
		if err != nil || !back.Equal(ref) {
			t.Fatalf("round trip %q → %v failed: %v/mismatch %v", tt.in, ref, err, back)
		}
	}
}

func TestServiceRefParseDefaults(t *testing.T) {
	t.Parallel()
	ref, err := ParseServiceRef("web")
	if err != nil {
		t.Fatalf("ParseServiceRef(web): %v", err)
	}
	if ref.Namespace != DefaultNamespace {
		t.Fatalf("bare name should default to %q, got %q", DefaultNamespace, ref.Namespace)
	}
}

func TestServiceRefParseErrors(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "a/b/c", "a//b", "UPPER/web", "core/UPPER", "web/", "/", "a/b/c/d"} {
		_, err := ParseServiceRef(in)
		if err == nil {
			t.Fatalf("ParseServiceRef(%q): want error", in)
		}
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("ParseServiceRef(%q) err not ErrInvalid: %v", in, err)
		}
	}
}

func TestNamespaceSemantics(t *testing.T) {
	t.Parallel()
	if !CoreNamespace.IsReserved() || !ObservabilityNamespace.IsReserved() {
		t.Fatal("core and observability must be reserved")
	}
	if DefaultNamespace.IsReserved() {
		t.Fatal("default must not be reserved")
	}
	if CoreNamespace.Tier() != 0 || ObservabilityNamespace.Tier() != 1 || Namespace("tenant").Tier() != 2 {
		t.Fatal("namespace tiers must be core=0, observability=1, rest=2")
	}
	for _, ns := range []string{"core", "observability", "default", "x-y9"} {
		if !Namespace(ns).IsValid() {
			t.Fatalf("namespace %q should be valid", ns)
		}
	}
	for _, ns := range []string{"", "Core", "-a", "a-", "a_b", strings.Repeat("a", 49)} {
		if Namespace(ns).IsValid() {
			t.Fatalf("namespace %q should be invalid", ns)
		}
	}
	if !Name("web").IsValid() || Name("web/x").IsValid() {
		t.Fatal("Name.IsValid broken")
	}
}

func TestValidRevision(t *testing.T) {
	t.Parallel()
	ok := []string{
		"a1b2c3d", // abbreviated
		"a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6a7b8c9d0", // sha1 full
		strings.Repeat("f", 64),                    // sha256 full
	}
	for _, s := range ok {
		if err := ValidRevision(s); err != nil {
			t.Fatalf("ValidRevision(%q): %v", s, err)
		}
	}
	bad := []string{"", "a", "ab", "A1B2C3D", "a1b2c3 ", "g1b2c3d", strings.Repeat("f", 65)}
	for _, s := range bad {
		if err := ValidRevision(s); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ValidRevision(%q) = %v, want ErrInvalid", s, err)
		}
	}
}

func TestServiceRefValidateAndDesiredValidate(t *testing.T) {
	t.Parallel()
	bad := Ref(Namespace(strings.Repeat("a", 25)), Name(strings.Repeat("b", 30)))
	if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long app ref should fail ErrInvalid, got %v", err)
	}
	d := DesiredState{
		Revision: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6a7b8c9d0",
		Services: []Service{
			{Ref: Ref("default", "web"), Source: Source{Image: "nginx", Digest: "sha256:aa"}},
			{Ref: Ref("default", "api"), Source: Source{Build: &BuildSource{Repo: "https://example", Ref: "main"}}},
		},
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("DesiredState.Validate: %v", err)
	}
	if got, err := d.Find(Ref("default", "web")); err != nil || !got.Ref.Equal(Ref("default", "web")) {
		t.Fatalf("Find: %v %v", got, err)
	}
	if _, err := d.Find(Ref("default", "nope")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Find(missing) = %v", err)
	}
	// duplicate + bad source + bad revision
	dup := d
	dup.Services = append(dup.Services, d.Services[0])
	if err := dup.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("dup validation = %v", err)
	}
	badSrc := d
	badSrc.Services[1] = Service{Ref: badSrc.Services[1].Ref, Source: Source{Image: "x", Build: &BuildSource{}}}
	if err := badSrc.Validate(); err == nil || !strings.Contains(err.Error(), "source") {
		t.Fatalf("source validation = %v", err)
	}
	badRev := d
	badRev.Revision = "not-a-sha"
	if err := badRev.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("revision validation = %v", err)
	}
}

func TestSourceReference(t *testing.T) {
	t.Parallel()
	if got := (Source{Image: "prom", Digest: "sha256:aa"}).Reference(); got != "prom@sha256:aa" {
		t.Fatalf("digest ref = %q", got)
	}
	if got := (Source{Image: "prom", Tag: "v3"}).Reference(); got != "prom:v3" {
		t.Fatalf("tag ref = %q", got)
	}
	if (Source{Image: "prom", Tag: "v3", Digest: "sha256:aa"}).IsValid() == nil {
		t.Fatal("tag+digest should be invalid")
	}
	if (Source{}).IsValid() == nil {
		t.Fatal("neither image nor build should be invalid")
	}
	if (Source{Image: "prom"}).IsDigestPinned() {
		t.Fatal("bare image is not digest-pinned")
	}
}

func TestRoutingAndRestartPolicyParse(t *testing.T) {
	t.Parallel()
	for _, m := range []RoutingMode{RoutingNone, RoutingInternal, RoutingProxy} {
		got, err := ParseRoutingMode(m.String())
		if err != nil || got != m {
			t.Fatalf("ParseRoutingMode(%s): %v %v", m, got, err)
		}
	}
	if _, err := ParseRoutingMode("misc"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad mode = %v", err)
	}
	for _, p := range []RestartPolicy{RestartAlways, RestartOnFailure, RestartNever} {
		if !p.IsValid() {
			t.Fatalf("policy %q invalid", p)
		}
	}
	if _, err := ParseRestartPolicy("sometimes"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad policy = %v", err)
	}
	if (&HealthCheck{}).Absent() != true {
		t.Fatal("zero HealthCheck should count as absent")
	}
	if (&HealthCheck{HTTP: &HTTPCheck{Path: "/"}}).Absent() != false {
		t.Fatal("HealthCheck with HTTP should not be absent")
	}
	if (Storage{Name: "data", MountPath: "/prometheus"}).String() != "data:/prometheus" {
		t.Fatal("Storage.String broken")
	}
}
