// SPDX-License-Identifier: MIT

package backup_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/backup"
	"github.com/toledompm/top/internal/exec/exectest"
	"github.com/toledompm/top/internal/obs"
	"github.com/toledompm/top/internal/store"
)

// Tier-1 tests for T-502's offsite copy (T-502, backup-recovery contract
// CopyOffsite + "destination-down → local intact + metric" AC): a fake
// exectest runner stands in for rsync — never a live ssh; tier 3 (real
// SSH container) is specified for T-504, not run here (README §Offsite).

// offRecorder captures the offsite metrics seam (dedicated fake so the
// engine's fakeRecorder stays untouched).
type offRecorder struct {
	successAt time.Time
	stamped   bool
	results   []string
}

func (r *offRecorder) SetBackupOffsiteSuccessAt(t time.Time) { r.successAt, r.stamped = t, true }
func (r *offRecorder) RecordBackupOffsite(_ time.Duration, result string) {
	r.results = append(r.results, result)
}

// Compile-proof: *obs.Metrics satisfies the offsite seam (test-only
// dependency; production internal/backup never imports obs).
var _ backup.OffsiteRecorder = (*obs.Metrics)(nil)

// newOffsite builds a real env (temp store + engine) plus a copier wired
// to the fake runner and shared fake clock.
type offsiteEnv struct {
	*testEnv
	rec  *offRecorder
	fake *exectest.FakeRunner
	off  *backup.Offsite
	clk  time.Time
}

func newOffsiteEnv(t *testing.T) *offsiteEnv {
	t.Helper()
	env := &offsiteEnv{
		testEnv: newEnv(t),
		rec:     &offRecorder{},
		fake:    exectest.NewFakeRunner(),
		clk:     time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC),
	}
	env.off = newCopier(env, env.fake)
	return env
}

func newCopier(env *offsiteEnv, r *exectest.FakeRunner) *backup.Offsite {
	return &backup.Offsite{
		Target:   "backup@offsite.example:/srv/offsite-copies",
		Runner:   r,
		Recorder: env.rec,
		Now:      func() time.Time { return env.clk },
	}
}

func TestOffsiteCopyHappyPathArgv(t *testing.T) {
	env := newOffsiteEnv(t)
	b := mustCreate(t, env.engine)

	if err := env.off.Copy(context.Background(), b); err != nil {
		t.Fatalf("Copy: %v", err)
	}

	calls := env.fake.Recorded()
	if len(calls) != 1 {
		t.Fatalf("rsync calls = %d, want exactly one", len(calls))
	}
	c := calls[0]
	if c.Name != "rsync" {
		t.Fatalf("binary %q, want rsync", c.Name)
	}
	// Exact argv: rsync -e "<ssh>" <file> <target>; no flags by default.
	// Externals of the copy (rsync subtleties) stay outside the whitelist.
	want := []string{
		"-e",
		"ssh -o ConnectTimeout=15 -o BatchMode=yes",
		b.Path, "backup@offsite.example:/srv/offsite-copies",
	}
	if len(c.Args) != len(want) {
		t.Fatalf("argv = %q, want %q", c.Args, want)
	}
	for i := range want {
		if c.Args[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full argv %q)", i, c.Args[i], want[i], c.Args)
		}
	}
	// Success stamps the fresh timestamp (alarm-clears condition).
	if !env.rec.stamped || !env.rec.successAt.Equal(env.clk) {
		t.Fatalf("success gauge = %v stamped=%v, want %v/stamped", env.rec.successAt, env.rec.stamped, env.clk)
	}
	if len(env.rec.results) != 1 || env.rec.results[0] != backup.ResultOK {
		t.Fatalf("results = %v, want [ok]", env.rec.results)
	}
}

func TestOffsiteCopyCustomPortTimeoutAndWhitelistedFlags(t *testing.T) {
	env := newOffsiteEnv(t)
	b := mustCreate(t, env.engine)
	env.off.Port = 2222
	env.off.TimeoutSeconds = 30
	env.off.ExtraArgs = []string{"-z", "--checksum"}

	if err := env.off.Copy(context.Background(), b); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	c := env.fake.Recorded()[0]
	if c.Args[0] != "-z" || c.Args[1] != "--checksum" {
		t.Fatalf("flags not preserved: %q", c.Args[:2])
	}
	if got := c.Args[3]; got != "ssh -o ConnectTimeout=30 -o BatchMode=yes -p 2222" {
		t.Fatalf("ssh command %q", got)
	}
}

func TestOffsiteCopyDestinationDownLocalIntactAndAlarm(t *testing.T) {
	env := newOffsiteEnv(t)
	b := mustCreate(t, env.engine)
	env.fake.SetPrefix([]string{"rsync"},
		exectest.Response{ExitCode: 23, Stderr: []byte("rsync: connection unexpectedly closed")})

	cpy := newCopier(env, env.fake)
	if err := cpy.Copy(context.Background(), b); err == nil {
		t.Fatal("Copy succeeded with destination down, want error")
	} else if !strings.Contains(err.Error(), "offsite copy") {
		t.Fatalf("error not framed: %v", err)
	}

	// PRIMARY always: the local backup is intact — file exists, registry
	// row untouched, no panic.
	if len(backups(t, env.store)) != 1 {
		t.Fatalf("registry rows = %d, want the one local backup", len(backups(t, env.store)))
	}
	rows := backups(t, env.store)
	if rows[0].Path != b.Path || rows[0].WatermarkOp != b.WatermarkOp {
		t.Fatalf("registry row mutated: %+v", rows[0])
	}

	// Alarm: failure counted as "error", success gauge NOT stamped.
	if len(env.rec.results) != 1 || env.rec.results[0] != backup.ResultError {
		t.Fatalf("results = %v, want [error]", env.rec.results)
	}
	if env.rec.stamped {
		t.Fatal("success gauge stamped on failure — alien metal")
	}
}

func TestOffsiteCopyMalformedConfigRejectedPreExec(t *testing.T) {
	cases := map[string]*backup.Offsite{
		"no colon / no path segment": {
			Target: "backup@offsite.example",
			Runner: exectest.NewFakeRunner(),
		},
		"no user@ host part": {
			Target: "offsite.example:/srv/copies",
			Runner: exectest.NewFakeRunner(),
		},
		"empty path": {
			Target: "backup@offsite.example:",
			Runner: exectest.NewFakeRunner(),
		},
		"IPv6 double colon": {
			Target: "backup@2001:db8::1:/srv/copies",
			Runner: exectest.NewFakeRunner(),
		},
		"whitespace in target": {
			Target: "backup@offsite.example:/srv/cop ies",
			Runner: exectest.NewFakeRunner(),
		},
		"non-whitelisted rsync flag": {
			Target:    "backup@offsite.example:/srv/copies",
			ExtraArgs: []string{"--delete"},
			Runner:    exectest.NewFakeRunner(),
		},
		"rsync -e injection attempt": {
			Target:    "backup@offsite.example:/srv/copies",
			ExtraArgs: []string{"-e", "evil"},
			Runner:    exectest.NewFakeRunner(),
		},
		"out-of-range ssh port": {
			Target: "backup@offsite.example:/srv/copies",
			Port:   70000,
			Runner: exectest.NewFakeRunner(),
		},
		"negative timeout": {
			TimeoutSeconds: -1,
			Target:         "backup@offsite.example:/srv/copies",
			Runner:         exectest.NewFakeRunner(),
		},
		"nil Runner (broken wiring)": {
			Target: "backup@offsite.example:/srv/copies",
		},
	}
	for name, off := range cases {
		t.Run(name, func(t *testing.T) {
			err := off.Copy(context.Background(), &store.Backup{Path: "/var/lib/top/backups/top-x.db"})
			if err == nil {
				t.Fatal("malformed config accepted")
			}
			if r, ok := off.Runner.(*exectest.FakeRunner); ok && len(r.Recorded()) != 0 {
				t.Fatalf("executed despite invalid config: %v", r.RecordedCommands())
			}
		})
	}
}

func TestOffsiteCopyIdempotenceTwoCopiesOneRow(t *testing.T) {
	env := newOffsiteEnv(t)
	b := mustCreate(t, env.engine)

	env.clk = env.clk.Add(time.Second)
	if err := env.off.Copy(context.Background(), b); err != nil {
		t.Fatalf("Copy 1: %v", err)
	}
	first := env.rec.successAt
	env.clk = env.clk.Add(time.Second)
	if err := env.off.Copy(context.Background(), b); err != nil {
		t.Fatalf("Copy 2: %v", err)
	}

	// Two rsync invocations (both real copies), but process-level state is
	// idempotent: gauge = latest success, no registry write, no duplicate
	// row (the v0 surface has no copied marker at all — README §Offsite).
	if calls := env.fake.Recorded(); len(calls) != 2 {
		t.Fatalf("rsync calls = %d, want 2", len(calls))
	}
	if !env.rec.successAt.After(first) {
		t.Fatalf("gauge not re-stamped with latest: %v → %v", first, env.rec.successAt)
	}
	if rows := backups(t, env.store); len(rows) != 1 {
		t.Fatalf("registry rows = %d, want 1 (never duplicated)", len(rows))
	} else if rows[0].Path != b.Path {
		t.Fatalf("registry row path = %q, want untouched", rows[0].Path)
	}
}

func TestOffsiteCopyNoSecretInErrorOrLog(t *testing.T) {
	// Security posture check: a failing rsync carries no credential in the
	// reported error (stderr excerpt only, redacted by FastExec in prod;
	// BatchMode means ssh never even offers a prompt).
	env := newOffsiteEnv(t)
	b := mustCreate(t, env.engine)
	env.fake.SetPrefix([]string{"rsync"},
		exectest.Response{ExitCode: 255, Stderr: []byte("ssh: connect to host offsite.example port 22: Connection refused")})

	err := env.off.Copy(context.Background(), b)
	if err == nil {
		t.Fatal("want error")
	}
	for _, secret := range []string{"password", "id_rsa", "PEM"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error mentions credential: %v", err)
		}
	}
}

func TestBackupFileSurvivesCopyFailureOnDisk(t *testing.T) {
	env := newOffsiteEnv(t)
	b := mustCreate(t, env.engine)
	env.fake.SetPrefix([]string{"rsync"}, exectest.Response{Err: errors.New("network unreachable (simulated)")})

	if err := env.off.Copy(context.Background(), b); err == nil {
		t.Fatal("want error")
	}
	rows := backups(t, env.store)
	if len(rows) != 1 || rows[0].Size != b.Size || rows[0].VerifyResult != "" {
		t.Fatalf("local row damaged by failed copy: %+v vs %+v", rows[0], b)
	}
}
