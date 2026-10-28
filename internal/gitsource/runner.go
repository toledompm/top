// SPDX-License-Identifier: MIT

package gitsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/toledompm/top/internal/exec"
)

// Runner is gitsource's seam over internal/exec. It is exec.Runner with one
// addition: an explicit child environment per call, because ADR-0008 carries
// the deploy key by PATH inside GIT_SSH_COMMAND — the key file is only ever
// referenced, never read by this package, and env is the ADR-blessed
// transport (never argv, never inside git config).
//
// Production transport: FastRunner (wraps a concrete *exec.FastExec, which
// supports ExtraEnv). Tests: wrap exectest.FakeRunner with an env-recording
// shim, or implement this small interface directly.
type Runner interface {
	// Run behaves like exec.Runner.Run with env appended to the child's
	// environment (KEY=VALUE form).
	Run(ctx context.Context, env []string, name string, args ...string) (stdout, stderr []byte, err error)
	// RunStream behaves like exec.Runner.RunStream with env appended.
	RunStream(ctx context.Context, env []string, stdout, stderr io.Writer, name string, args ...string) error
}

// FastRunner is the production Runner: a value wrapper around a base
// *exec.FastExec whose per-call configuration (ExtraEnv) cannot be mutated
// in place — each call clones the base so concurrent calls stay race-free.
type FastRunner struct {
	// Base is the underlying exec.FastExec configuration. Its Dir and
	// ExtraEnv are honored on every call; gitsource uses `-C` argv for
	// directories, so Dir may stay empty.
	Base *exec.FastExec
}

var _ Runner = FastRunner{}

// perCall clones Base with env appended to ExtraEnv.
func (r FastRunner) perCall(env []string) *exec.FastExec {
	fe := *r.Base
	fe.ExtraEnv = append(append([]string{}, r.Base.ExtraEnv...), env...)
	return &fe
}

// Run implements Runner.
func (r FastRunner) Run(ctx context.Context, env []string, name string, args ...string) (stdout, stderr []byte, err error) {
	return r.perCall(env).Run(ctx, name, args...)
}

// RunStream implements Runner.
func (r FastRunner) RunStream(ctx context.Context, env []string, stdout, stderr io.Writer, name string, args ...string) error {
	return r.perCall(env).RunStream(ctx, stdout, stderr, name, args...)
}

// -- work-surface helpers (perms, tempdir lifecycle, tmp files) -----------

// mkdir0700 creates dir (if missing) with 0700, tightening an existing
// looser one (directory reads fail loudly, so mkdir-only tightening is fine
// at the ADR-0008 posture: work areas are ours).
func mkdir0700(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("gitsource: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("gitsource: chmod %s: %w", dir, err)
	}
	return nil
}

// newTempDir creates a 0700 scratch directory inside workDir.
func newTempDir(workDir, pattern string) (string, error) {
	dir, err := os.MkdirTemp(workDir, pattern)
	if err != nil {
		return "", fmt.Errorf("gitsource: tempdir under %s: %w", workDir, err)
	}
	// MkdirTemp already uses 0700; assert it so a future os change can't
	// silently loosen the ADR-0008 posture.
	return dir, os.Chmod(dir, 0o700)
}

// writeTempFile writes data to a 0600 file matching pattern inside workDir,
// returning a cleanup func that removes it (idempotent, error ignored —
// leftovers in the git work area are only disk hygiene, not state).
func writeTempFile(workDir, pattern string, data []byte) (path string, cleanup func() error, err error) {
	f, err := os.CreateTemp(workDir, pattern)
	if err != nil {
		return "", nil, fmt.Errorf("gitsource: tempfile under %s: %w", workDir, err)
	}
	path = f.Name()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", nil, fmt.Errorf("gitsource: chmod %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", nil, fmt.Errorf("gitsource: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", nil, fmt.Errorf("gitsource: flush %s: %w", path, err)
	}
	once := sync.Once{}
	return path, func() error {
		var err error
		once.Do(func() { err = os.Remove(path) })
		return err
	}, nil
}

// Snapshot is one materialized revision: a private directory holding
// exactly the revision's tree (specs included, spec loading is
// internal/spec's job — this is paths only).
type Snapshot struct {
	SHA string // the revision materialized
	Dir string // private 0700 directory; consume then Close

	// release is wired by materialize before the snapshot escapes; Close
	// and the ctx-done hook both funnel through it once.
	release func() error
}

// init wires one-shot close semantics plus auto-close when ctx ends.
func (sn *Snapshot) init(ctx context.Context) {
	once := sync.Once{}
	base := sn.release
	sn.release = func() error {
		var err error
		once.Do(func() {
			err = os.RemoveAll(sn.Dir)
			if base != nil {
				if b := base(); err == nil {
					err = b
				}
			}
		})
		return err
	}
	context.AfterFunc(ctx, func() { _ = sn.release() })
}

// Close deletes the snapshot directory; repeated calls are no-ops, and the
// directory is also removed automatically when the source context ends.
func (sn *Snapshot) Close() error {
	if sn.release == nil {
		return errors.New("gitsource: snapshot not initialized")
	}
	return sn.release()
}
