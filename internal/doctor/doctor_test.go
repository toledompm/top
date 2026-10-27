// SPDX-License-Identifier: MIT

package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/exec/exectest"
)

// seams builds Options with every host-touching seam faked: no test
// binds a real port, reads /proc, stats the host's dataDir, or shells
// out. tests never run against the ambient machine state.
type seams struct {
	fake    *exectest.FakeRunner
	osRel   string                         // temp os-release path ("" → skip by pointing at missing file)
	ports   func() map[string]func() error // fake listener outcome per port
	mem     uint64
	memErr  error
	disk    uint64
	diskErr error
	goos    string
	arch    string
}

func (s *seams) options(t *testing.T) Options {
	t.Helper()
	o := Options{
		Runner: s.fake,
		OSProbe: func() (string, string) {
			return s.goos, s.arch
		},
		OSReleasePath: s.osRel,
		MemProbe: func() (uint64, error) {
			return s.mem, s.memErr
		},
		DiskProbe: func(string) (uint64, error) {
			return s.disk, s.diskErr
		},
		DataDir: "/nonexistent-top-state/root",
	}
	if s.goos == "" {
		s.goos = "linux"
	}
	o.OSProbe = func() (string, string) { return s.goos, s.arch }
	probe := s.ports
	o.Probe = func(network, addr string) (func(), error) {
		if probe == nil {
			return func() {}, nil
		}
		port := strings.TrimPrefix(addr, ":")
		rel, ok := probe()[port]
		if !ok {
			return func() {}, errNotBound
		}
		return func() {}, rel()
	}
	return o
}

// errNotBound is the fake "port free" outcome.
var errNotBound = errors.New("not bound")

// TestRunAllCheckIDsPresent pins the Phase 0 check inventory: every
// installer contract check shows up exactly once (by ID family) in a
// clean-machine report — nothing silently vanishes.
func TestRunAllCheckIDsPresent(t *testing.T) {
	t.Parallel()
	s := &seams{fake: exectest.NewFakeRunner(), goos: "linux", arch: "amd64"}
	s.mem, s.disk = 8<<30, (48 << 30)
	// All exec probes succeed; fake sudo/docker/dokku registers below.
	s.fake.SetPrefix([]string{"docker", "version"}, exectest.Response{Stdout: []byte("27.4.0\n")})
	s.fake.SetPrefix([]string{"dokku", "version"}, exectest.Response{Stdout: []byte("dokku version 0.38.1 (git commit abc)\n")})
	s.fake.SetPrefix([]string{"dokku", "apps:list"}, exectest.Response{Stdout: []byte("blog\n")})
	s.fake.SetPrefix([]string{"sudo"}, exectest.Response{})
	rep := Run(context.Background(), s.options(t))
	ids := map[string]bool{}
	for _, f := range rep.Findings {
		ids[strings.SplitN(f.ID, "/", 2)[0]] = true
	}
	for _, want := range []string{"os", "docker", "dokku", "ports", "resources", "sudo", "names", "store"} {
		if !ids[want] {
			t.Errorf("report missing check family %q; ids=%v", want, ids)
		}
	}
	if !rep.OK() {
		t.Errorf("clean-machine report should be OK (fails=%v)", reportOf(rep, SeverityFail))
	}
}

func reportOf(r Report, sev Severity) []string {
	var ids []string
	for _, f := range r.Findings {
		if f.Sev() == sev {
			ids = append(ids, f.ID)
		}
	}
	return ids
}

func finding(r Report, id string) (Finding, bool) {
	for _, f := range r.Findings {
		if f.ID == id {
			return f, true
		}
	}
	return Finding{}, false
}

func testOptions(base Options, mutate func(*Options)) Options {
	mutate(&base)
	return base
}

// TestCheckOSTable pins the OS/arch fan-out: linux ok, darwin dev-run
// warn, windows fail, arch fail, distro windows.
func TestCheckOSTable(t *testing.T) {
	t.Parallel()
	rel := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "os-release")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := Options{Runner: exectest.NewFakeRunner(), DataDir: "/definitely/not/here", Probe: func(string, string) (func(), error) { return func() {}, nil }, MemProbe: func() (uint64, error) { return 8 << 30, nil }, DiskProbe: func(string) (uint64, error) { return 40 << 30, nil }}
	tests := []struct {
		name     string
		goos     string
		arch     string
		osRel    string
		skipDist bool
		wantSev  Severity
		wantID   string
	}{
		{"linux-amd64-ubuntu24", "linux", "amd64", "ID=ubuntu\nVERSION_ID=\"24.04\"\n", false, SeverityOK, "os/distro"},
		{"linux-arm64-ubuntu22", "linux", "arm64", "ID=ubuntu\nVERSION_ID=\"22.04\"\n", false, SeverityOK, "os/distro"},
		{"linux-ubuntu26", "linux", "amd64", "ID=ubuntu\nVERSION_ID=\"26.04\"\n", false, SeverityOK, "os/distro"},
		{"linux-debian12", "linux", "amd64", "ID=debian\nVERSION_ID=\"12\"\n", false, SeverityOK, "os/distro"},
		{"linux-ubuntu20-outside", "linux", "amd64", "ID=ubuntu\nVERSION_ID=\"20.04\"\n", false, SeverityFail, "os/distro"},
		{"linux-debian13-ok", "linux", "amd64", "ID=debian\nVERSION_ID=\"13\"\n", false, SeverityOK, "os/distro"},
		{"linux-unknown-distro-warn", "linux", "amd64", "ID=centos\nVERSION_ID=\"9\"\n", false, SeverityWarn, "os/distro"},
		{"linux-bad-arch", "linux", "riscv64", "", true, SeverityFail, "os/arch"},
		{"darwin-dev-warn", "darwin", "arm64", "", true, SeverityWarn, "os/os"},
		{"windows-fail", "windows", "amd64", "", true, SeverityFail, "os/os"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var osRel string
			if !tc.skipDist {
				osRel = rel(t, tc.osRel)
			}
			r := Report{}
			opts := testOptions(base, func(o *Options) {
				o.OSProbe = func() (string, string) { return tc.goos, tc.arch }
				o.OSReleasePath = osRel
			})
			r.checkOS(opts)
			f, ok := finding(r, tc.wantID)
			if !ok {
				t.Fatalf("no finding %q; got %v", tc.wantID, r.Findings)
			}
			if f.Sev() != tc.wantSev {
				t.Fatalf("severity = %q, want %q (finding %+v)", f.Severity, tc.wantSev, f)
			}
			if tc.wantSev != SeverityOK && f.Hint == "" {
				t.Errorf("non-ok finding without hint: %+v", f)
			}
		})
	}
}

// TestCheckDockerTable: FakeRunner drives binary-missing (ErrNotFound),
// daemon-down (exit code), version parse, below-floor, and ok.
func TestCheckDockerTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		resp    exectest.Response
		err     error
		wantID  string
		wantSev Severity
	}{
		{"missing", exectest.Response{}, exec.ErrNotFound, "docker/binary", SeverityFail},
		{"daemon-down", exectest.Response{ExitCode: 1, Stderr: []byte("Cannot connect to the Docker daemon")}, nil, "docker/daemon", SeverityFail},
		{"old-engine", exectest.Response{Stdout: []byte("19.03.13\n")}, nil, "docker/version", SeverityFail},
		{"borderline-24.0", exectest.Response{Stdout: []byte("24.0.7\n")}, nil, "docker/version", SeverityOK},
		{"new-engine", exectest.Response{Stdout: []byte("27.4.0\n")}, nil, "docker/version", SeverityOK},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := exectest.NewFakeRunner()
			if tc.err != nil {
				f.SetPrefix([]string{"docker", "version"}, exectest.Response{Err: tc.err})
			} else {
				f.SetPrefix([]string{"docker", "version"}, tc.resp)
			}
			r := Report{}
			r.checkDocker(context.Background(), f)
			got, ok := finding(r, tc.wantID)
			if !ok {
				t.Fatalf("no finding %q; got %v", tc.wantID, r.Findings)
			}
			if got.Sev() != tc.wantSev {
				t.Fatalf("%s: severity %q, want %q", tc.wantID, got.Severity, tc.wantSev)
			}
			if got.Severity != SeverityOK && got.Hint == "" {
				t.Errorf("fail without hint: %+v", got)
			}
			// ADR-0009: the doctor runs exactly the documented read probe.
			if cmds := f.RecordedCommands(); len(cmds) != 1 || !strings.HasPrefix(cmds[0], "docker version") {
				t.Errorf("expected exactly one docker version probe, got %v", cmds)
			}
		})
	}
}

// TestCheckDokkuClassification pins the three-phase classification from
// installer.md plus the apps:list reuse.
func TestCheckDokkuClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		verResp  exectest.Response
		verErr   error
		appsResp exectest.Response
		appsErr  error
		wantID   string
		wantSev  Severity
	}{
		{"absent", exectest.Response{}, exec.ErrNotFound, resend(exectest.Response{}), nil, "dokku/state", SeverityInfo},
		{"present-ok", exectest.Response{Stdout: []byte("dokku version 0.38.13\n")}, nil, exectest.Response{Stdout: []byte("")}, nil, "dokku/state", SeverityOK},
		{"below-floor", exectest.Response{Stdout: []byte("dokku version 0.35.4\n")}, nil, exectest.Response{}, nil, "dokku/state", SeverityFail},
		{"broken", exectest.Response{ExitCode: 1, Stderr: []byte("dokku: command not found")}, nil, exectest.Response{}, nil, "dokku/state", SeverityFail},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := exectest.NewFakeRunner()
			f.SetPrefix([]string{"dokku", "version"}, grab(tc.verResp, tc.verErr))
			f.SetPrefix([]string{"dokku", "apps:list"}, tc.appsResp)
			r := Report{}
			opts := Options{Runner: f}
			opts.applyDefaults()
			apps := r.checkDokku(context.Background(), opts)
			got, ok := finding(r, tc.wantID)
			if !ok {
				t.Fatalf("no finding; got %v", r.Findings)
			}
			if got.Sev() != tc.wantSev {
				t.Fatalf("severity %q, want %q", got.Severity, tc.wantSev)
			}
			if tc.name == "absent" && apps != nil {
				t.Errorf("absent dokku should yield nil app list, got %v", apps)
			}
		})
	}
}

func grab(resp exectest.Response, err error) exectest.Response {
	if err != nil {
		return exectest.Response{Err: err}
	}
	return resp
}

func resend(r exectest.Response) exectest.Response { return r }

// TestCheckNamesChecksReservedCollision — the two-key rule's read-only
// pre-check: controller name → fail; reserved-prefix app → warn; other
// app → neither.
func TestCheckNamesReservedCollision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		apps      []string
		wantFails int
		wantWarns int
	}{
		{"no-apps", []string{}, 0, 0},
		{"controller-name", []string{"core-top-controller", "other"}, 1, 1},
		{"reserved-prefix", []string{"core-prometheus"}, 0, 2},
		{"unrelated-app", []string{"blog", "internal-stats"}, 0, 0},
		{"nil-apps-skipped", nil, 0, 0},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := Report{}
			opts := Options{ReservedNamespaces: []string{"core", "observability"}}
			r.checkNames(tc.apps, opts)
			if got := len(reportOf(r, SeverityFail)); got != tc.wantFails {
				t.Fatalf("fails = %d (%v), want %d", got, reportOf(r, SeverityFail), tc.wantFails)
			}
			if got := len(reportOf(r, SeverityWarn)); got != tc.wantWarns {
				t.Fatalf("warns = %d (%v), want %d", got, reportOf(r, SeverityWarn), tc.wantWarns)
			}
		})
	}
}

// TestCheckPortsFake probes never touch real TCP: injected outcomes map
// onto warn-free, warn-occupied, and warn-unprivileged paths, plus the
// skip flag path.
func TestCheckPortsFake(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		outcome func(port string) error
		skip    bool
		wantSev map[string]Severity
	}{
		{"both-free", nil, false, map[string]Severity{"ports/80": SeverityOK, "ports/443": SeverityOK}},
		{
			"both-occupied", func(port string) error { return errInUse }, false,
			map[string]Severity{"ports/80": SeverityWarn, "ports/443": SeverityWarn},
		},
		{
			"priv-bound", func(port string) error { return errPermission }, false,
			map[string]Severity{"ports/80": SeverityWarn, "ports/443": SeverityWarn},
		},
		{"skipped", nil, true, map[string]Severity{"ports/skipped": SeverityInfo}},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := Report{}
			opts := Options{SkipPorts: tc.skip}
			opts.applyDefaults()
			if tc.outcome != nil || !tc.skip {
				opts.Probe = func(_, addr string) (func(), error) {
					if tc.skip {
						t.Fatal("port probe ran while --skip-ports was set")
					}
					if tc.outcome != nil {
						if err := tc.outcome(strings.TrimPrefix(addr, ":")); err != nil {
							return nil, err
						}
					}
					return func() {}, nil
				}
			}
			r.checkPorts(opts)
			for id, sev := range tc.wantSev {
				f, ok := finding(r, id)
				if !ok {
					t.Fatalf("no %q finding; %v", id, r.Findings)
				}
				if f.Sev() != sev {
					t.Errorf("%s: severity %q, want %q", id, f.Severity, sev)
				}
			}
		})
	}
}

var errInUse = errors.New("bind: address already in use")

// TestCheckResourcesTable pins the resource floor fan-out.
func TestCheckResourcesTable(t *testing.T) {
	t.Parallel()
	gib := func(n uint64) uint64 { return n << 30 }
	tests := []struct {
		name       string
		disk       uint64
		mem        uint64
		memErr     error
		wantIDsSev map[string]Severity
	}{
		{"comfortable", gib(48), gib(8), nil, map[string]Severity{"resources/disk": SeverityOK, "resources/ram": SeverityOK}},
		{"small-host", gib(10), gib(2), nil, map[string]Severity{"resources/disk": SeverityWarn, "resources/ram": SeverityWarn}},
		{"disk-under-laydown", gib(1), gib(8), nil, map[string]Severity{"resources/disk": SeverityFail, "resources/ram": SeverityOK}},
		{"mem-under-floor", gib(48), 700 << 20, nil, map[string]Severity{"resources/ram": SeverityFail}},
		{"mem-probe-error", gib(48), 0, errors.New("no meminfo"), map[string]Severity{"resources/ram": SeverityWarn}},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := Report{}
			opts := Options{
				DataDir: "/some/anchor", MemProbe: func() (uint64, error) { return tc.mem, tc.memErr },
				DiskProbe: func(string) (uint64, error) { return tc.disk, nil },
			}
			opts.applyDefaults()
			r.checkResources(opts)
			for id, sev := range tc.wantIDsSev {
				f, ok := finding(r, id)
				if !ok {
					t.Fatalf("no %q; %v", id, r.Findings)
				}
				if f.Sev() != sev {
					t.Errorf("%s: %q, want %q", id, f.Severity, sev)
				}
			}
		})
	}
}

// TestCheckSudoTable: exit 0 ok; ErrNotFound and non-zero exit warn.
func TestCheckSudoTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		resp exectest.Response
		want Severity
	}{
		{"passwordless", exectest.Response{}, SeverityOK},
		{"nonzero", exectest.Response{ExitCode: 1}, SeverityWarn},
		{"missing-sudo", exectest.Response{}, SeverityWarn},
	}
	for i := range tests {
		tc := tests[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := exectest.NewFakeRunner()
			if tc.name == "missing-sudo" {
				f.SetPrefix([]string{"sudo"}, exectest.Response{Err: exec.ErrNotFound})
			} else {
				f.SetPrefix([]string{"sudo"}, tc.resp)
			}
			r := Report{}
			r.checkSudo(context.Background(), f)
			got, ok := finding(r, "sudo/passwordless")
			if !ok || got.Sev() != tc.want {
				t.Fatalf("finding = %+v, want severity %q", got, tc.want)
			}
			if got.Severity != SeverityOK && !strings.Contains(got.Hint, "sudo") {
				t.Errorf("hint should explain the sudo posture: %+v", got)
			}
		})
	}
}

// TestCheckDataDirNoWrites: doctor performs zero writes in the dataDir
// tree; permissions are observed, never adjusted.
func TestCheckDataDirNoWrites(t *testing.T) {
	t.Parallel()

	// absent → info
	r := Report{Findings: nil}
	opts := Options{DataDir: filepath.Join(t.TempDir(), "missing")}
	opts.applyDefaults()
	r.checkDataDir(opts)
	if f, ok := finding(r, "store/datadir"); !ok || f.Severity != SeverityInfo {
		t.Fatalf("absent: %+v", f)
	}

	// existing-but-0775 → warn (observed mode is looser than 0700)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "top"), 0o775); err != nil {
		t.Fatal(err)
	}
	r = Report{}
	opts = Options{DataDir: filepath.Join(dir, "top")}
	opts.applyDefaults()
	r.checkDataDir(opts)
	if f, ok := finding(r, "store/datadir"); !ok || f.Severity != SeverityWarn {
		t.Fatalf("0775: %+v", f)
	}

	// existing-0700 → ok
	if err := os.Chmod(filepath.Join(dir, "top"), 0o700); err != nil {
		t.Fatal(err)
	}
	r = Report{}
	opts = Options{DataDir: filepath.Join(dir, "top")}
	opts.applyDefaults()
	r.checkDataDir(opts)
	if f, ok := finding(r, "store/datadir"); !ok || f.Severity != SeverityOK {
		t.Fatalf("0700: %+v", f)
	}
}

// TestReportOKSeveritySemantics: OK flips only on fail; info/warn keep 0
// available (exit-code contract lives in internal/cli/doctor_test.go).
func TestReportOKSeveritySemantics(t *testing.T) {
	t.Parallel()
	clean := Report{Findings: []Finding{{ID: "x", Severity: SeverityOK}}}
	if !clean.OK() || clean.Count(SeverityOK) != 1 {
		t.Fatalf("clean: %+v", clean)
	}
	warnOnly := Report{Findings: []Finding{
		{ID: "a", Severity: SeverityWarn, Message: "m", Hint: "fix"},
		{ID: "b", Severity: SeverityInfo, Message: "i"},
	}}
	if !warnOnly.OK() {
		t.Errorf("warn/info must not flip OK (warns are advisory)")
	}
	failing := Report{Findings: []Finding{
		{ID: "a", Severity: SeverityWarn, Message: "m", Hint: "fix"},
		{ID: "b", Severity: SeverityFail, Message: "f", Hint: "fix harder"},
	}}
	if failing.OK() || failing.Count(SeverityFail) != 1 {
		t.Errorf("fail must flip OK to false: %+v", failing)
	}
	// Zero-value severity guards against a silent "" finding.
	z := Finding{ID: "z"}
	if z.Sev() != SeverityInfo {
		t.Errorf("zero-value severity should read as info, got %q", z.Sev())
	}
}

// TestEngineAtLeast covers floor comparison edge cases ("v" prefix,
// residue like "-ce", short forms).
func TestEngineAtLeast(t *testing.T) {
	t.Parallel()
	tests := []struct {
		actual, floor string
		want          bool
	}{
		{"24.0.7", "24.0", true},
		{"24.0", "24.0", true},
		{"23.0.6", "24.0", false},
		{"v27.4.0", "24.0", true},
		{"19.03.13", "24.0", false},
		{"", "0.38", false},
		{"", "", true},
		{"0.38.1-ce", "0.38", true},
	}
	for _, tc := range tests {
		if got := engineAtLeast(tc.actual, tc.floor); got != tc.want {
			t.Errorf("engineAtLeast(%q, %q) = %v, want %v", tc.actual, tc.floor, got, tc.want)
		}
	}
}

// TestDokkuVersionParse: version token extraction from real-world
// "dokku version 0.38.13 (git commit …)" shapes.
func TestDokkuVersionParse(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"dokku version 0.38.13\n": "0.38.13",
		"0.38.0\n":                "0.38.0",
		"dokku version 0.35.4":    "0.35.4",
		"garbage":                 "",
	}
	for in, want := range cases {
		if got := dokkuVersionFrom(in); got != want {
			t.Errorf("dokkuVersionFrom(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSplitApps parses `dokku apps:list` shapes (banner + app lines).
func TestSplitApps(t *testing.T) {
	t.Parallel()
	got := splitApps("blog\n(not a real app either)\ncore-prometheus\n=====> Other\n=====>\nplain note here\n")
	want := []string{"blog", "core-prometheus", "Other"}
	if len(got) != len(want) {
		t.Fatalf("splitApps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitApps = %v, want %v", got, want)
		}
	}
}
