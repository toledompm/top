// SPDX-License-Identifier: MIT

// Package gitsource implements the GitSource half of
// docs/contracts/git-source.md: the relationship between TOP and the
// platform Git repo. It owns a bare mirror under <dataDir>/git/mirror.git,
// resolves the branch head to a sha (`git ls-remote`), fetches arbitrary
// revisions into the mirror (`git fetch origin <sha>`), and materializes a
// revision's files into a private temp directory (`git archive <sha>` +
// `tar -xf`) — never a shared working-tree checkout.
//
// Everything external runs through internal/exec (ADR-0006, ADR-0001: git
// via CLI, no Go git library). Deploy-key access follows ADR-0008: the key
// is a file path only, carried via GIT_SSH_COMMAND in the child
// environment; host-key checking is always strict and never bypassed.
//
// THIS IS NOT the whole contract: registration (ADR-0005 repo-exists /
// --init flows) belongs to the installer; polling-loop scheduling and
// webhook intake (ADR-0003) belong to the controller and internal/webhook
// respectively. This package provides the primitives they consume,
// including Poller for the 60 s poll loop.
package gitsource

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/toledompm/top/internal/config"
	"github.com/toledompm/top/internal/exec"
)

// VersionedShas lists hex sha lengths accepted for FetchRevision. A sha is
// the only argument that ever reaches argv from outside; accepting only
// fixed-length hex keeps forging refs/paths/options impossible.
func validSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !isHexDigit(c) {
			return false
		}
	}
	return true
}

// Source owns ONE platform repo: the configured URL/branch plus the
// deploy-key material paths beneath the data dir. Create with New; for
// tests supply a non-nil Runner.
//
// A Source is safe for concurrent use at the Runner level only insofar as
// the underlying Runner is: the exec layer is argv-stateless, but git
// operating on the same mirror concurrently is not coordinated here — the
// reconciler serializes fetch/materialize per repo (T-310).
type Source struct {
	// URL is the platform repo remote (ssh or https per ADR-0008).
	URL string
	// Branch is the trigger branch (poll target, ADR-0003).
	Branch string
	// KeyPath is the deploy-key file (0600 on disk; only ever referenced
	// by path). Empty means the remote needs no SSH key (https, or an
	// explicitly configured agent setup — never our default stance).
	KeyPath string
	// KnownHosts is the pinned host-key file (ADR-0008). Empty with an
	// ssh:// URL is an error at call time: strict host-key checking must
	// know what to check against.
	KnownHosts string
	// WorkDir holds the mirror and revision temp dirs (typically
	// <dataDir>/git). Created on demand, mode 0700.
	WorkDir string
	// Runner executes every external process. Nil means build one from
	// New's defaults (exec.FastExec under the hood).
	Runner Runner
}

// New builds a Source from TOP's own runtime configuration: URL/branch from
// cfg.Git, key material at the ADR-0008 layout (<dataDir>/git/id_ed25519 and
// <dataDir>/git/known_hosts), work area at <dataDir>/git.
func New(cfg config.Config) *Source {
	return &Source{
		URL:        cfg.Git.URL,
		Branch:     cfg.Git.Branch,
		KeyPath:    cfg.DataDir + "/git/id_ed25519",
		KnownHosts: cfg.DataDir + "/git/known_hosts",
		WorkDir:    cfg.DataDir + "/git",
		Runner:     nil,
	}
}

// MirrorDir is the bare mirror this Source maintains. ADR-0008 layout.
func (s *Source) MirrorDir() string {
	if s.WorkDir == "" {
		return "mirror.git"
	}
	return s.WorkDir + "/mirror.git"
}

// runner returns the effective Runner, defaulting to exec.FastExec lazily
// (never at construction, so sources built before tests set up doubles work).
func (s *Source) runner() Runner {
	if s.Runner == nil {
		return FastRunner{Base: exec.NewFastExec()}
	}
	return s.Runner
}

// env builds the child environment for git: git must never hang on a
// terminal prompt, and SSH URLs need the strict GIT_SSH_COMMAND (ADR-0008).
// Only PATHS appear here — never key contents; ssh reads the key file
// itself and this package never opens it.
func (s *Source) env() ([]string, error) {
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	if !isSSHRemote(s.URL) {
		return env, nil
	}
	if s.KnownHosts == "" {
		return nil, errors.New("gitsource: ssh remote without a pinned known_hosts file (ADR-0008 requires it)")
	}
	if s.KeyPath == "" {
		return nil, errors.New("gitsource: ssh remote without a deploy key path")
	}
	sshCmd := SSHCommand(s.KeyPath, s.KnownHosts)
	return append(env, "GIT_SSH_COMMAND="+sshCmd), nil
}

// isSSHRemote reports whether the URL uses SSH (git@host:path scp-style or
// ssh:// URLs). https (and plain local paths) do not.
func isSSHRemote(url string) bool {
	if strings.HasPrefix(url, "ssh://") {
		return true
	}
	// scp-style only when it is actually scp-shaped: something before
	// "user@host:path". A bare path or a plain directory name must not
	// count (a local path can contain '@' after a '/').
	if i := strings.IndexByte(url, '@'); i > 0 {
		head := url[:i]
		rest := url[i:]
		if !strings.Contains(head, ":") && strings.Contains(rest, ":") {
			return true
		}
	}
	return false
}

// SSHCommand renders the strict OpenSSH command line GIT_SSH_COMMAND asks
// for (ADR-0008 verbatim): the deploy key by PATH, identity-only, pinned
// known_hosts, strict host-key checking. There is deliberately no option to
// disable any of it.
func SSHCommand(keyPath, knownHosts string) string {
	return "ssh -i " + keyPath +
		" -o IdentitiesOnly=yes" +
		" -o UserKnownHostsFile=" + knownHosts +
		" -o StrictHostKeyChecking=yes"
}

// run executes one argv with this Source's child env. Failures are
// classified ONCE here — against the command's own (already exec-redacted)
// stderr — so the typed error carries the remote signature; later wrapping
// must never re-classify.
func (s *Source) run(ctx context.Context, op string, name string, args ...string) (stdout, stderr []byte, err error) {
	env, err := s.env()
	if err != nil {
		return nil, nil, wrapOp(op, err)
	}
	out, errOut, err := s.runner().Run(ctx, env, name, args...)
	if err != nil {
		return out, errOut, toTyped(op, err, firstLine(errOut))
	}
	return out, errOut, nil
}

// runStream is run for stream sinks (nothing here yet streams, but the
// Runner plumbing exists for the install/init push path's successor).

// fileExists is the mirror-presence check (HEAD exists ⇔ a clone happened).
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// EnsureMirror clones the bare mirror once (idempotent). ADR-0005's
// repo-exists contract: cloning creates NOTHING on the remote ("mirror
// clone"), and re-cloning is never attempted while the mirror exists.
func (s *Source) EnsureMirror(ctx context.Context) error {
	if fileExists(s.MirrorDir() + "/HEAD") {
		return nil
	}
	if err := mkdir0700(s.WorkDir); err != nil {
		return wrapOp("ensure-mirror", err)
	}
	_, _, err := s.run(ctx, "ensure-mirror",
		"git", "clone", "--mirror", s.URL, s.MirrorDir())
	return err
}

// ResolveLatest resolves the configured branch head to a sha via
// `git ls-remote` — no clone required, and the answer is mutable by
// definition (the branch can move under us; that is what a subsequent
// FetchRevision handles, including the force-push case, typed
// ErrRevisionGone).
func (s *Source) ResolveLatest(ctx context.Context) (string, error) {
	ref := "refs/heads/" + s.Branch
	stdout, _, err := s.run(ctx, "resolve", "git", "ls-remote", s.URL, ref)
	if err != nil {
		return "", err
	}
	// ls-remote prints "<sha>\t<ref>" lines, optionally followed by peeled
	// "<sha>\t<ref>^{}" entries. We asked for one ref; take the first line
	// that matches it exactly.
	for _, line := range strings.Split(string(stdout), "\n") {
		sha, refOut, ok := strings.Cut(line, "\t")
		if ok && refOut == ref {
			return sha, nil
		}
	}
	// An empty (or unmatched) reply means the branch disappeared from the
	// remote — the same force-push family as a vanished sha.
	return "", wrapOp("resolve", fmt.Errorf("%w: branch %q no longer on remote", ErrRevisionGone, s.Branch))
}

// FetchRevision makes sure `sha` is present in the local mirror (fetching
// it when new) and materializes its whole tree into a fresh, private temp
// directory. It never shares a checkout across runs and never applies the
// archive anywhere else.
//
// The returned Snapshot auto-closes when ctx is done and may be closed
// early via Snapshot.Close; both are no-ops after the first close.
func (s *Source) FetchRevision(ctx context.Context, sha string) (*Snapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !validSHA(sha) {
		return nil, wrapOp("fetch", fmt.Errorf("%w: %q is not a hex sha (40 or 64 hex characters)", ErrInvalidSHA, sha))
	}
	if err := s.EnsureMirror(ctx); err != nil {
		return nil, err
	}
	// A sha that just vanished (force-push) makes the remote stop
	// advertising it: fetch fails and must abort the run BEFORE any
	// mutation happens — this error is the loud signal (run() already
	// classified the stderr, e.g. "couldn't find remote ref" →
	// ErrRevisionGone).
	if _, _, err := s.run(ctx, "fetch", "git", "-C", s.MirrorDir(), "fetch", "--quiet", "origin", sha); err != nil {
		return nil, err
	}

	snap, err := s.materialize(ctx, sha)
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// materialize extracts the revision's tarball into a fresh temp dir:
// `git archive --format=tar <sha>` captures stdout, then `tar -xf` unpacks
// it into the snapshot dir. Two argv steps instead of one shell pipe (the
// exec layer is argv-only, ADR-0006) — functionally identical to the
// contract's illustrative `git archive <sha> | tar -x`.
func (s *Source) materialize(ctx context.Context, sha string) (*Snapshot, error) {
	if err := mkdir0700(s.WorkDir); err != nil {
		return nil, wrapOp("materialize", err)
	}
	dir, err := newTempDir(s.WorkDir, "top-extract-")
	if err != nil {
		return nil, wrapOp("materialize", err)
	}
	snap := &Snapshot{SHA: sha, Dir: dir}
	snap.init(ctx) // auto-close on ctx done

	ok := false
	defer func() {
		if !ok {
			_ = snap.Close() // best-effort cleanup; primary error is already returned
		}
	}()

	tarOut, _, err := s.run(ctx, "materialize-archive",
		"git", "-C", s.MirrorDir(), "archive", "--format=tar", sha)
	if err != nil {
		return nil, err
	}
	// Write the tarball as 0600 inside the work dir (not the snapshot dir,
	// so the snapshot tree is exactly the revision's tree).
	tarPath, cleanup, err := writeTempFile(s.WorkDir, "top-archive-*.tar", tarOut)
	if err != nil {
		return nil, wrapOp("materialize-archive", err)
	}
	defer func() { _ = cleanup() }()

	if _, _, err := s.run(ctx, "materialize-extract", "tar", "-xf", tarPath, "-C", snap.Dir); err != nil {
		return nil, err
	}
	ok = true
	return snap, nil
}

func isHexDigit(c rune) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' // lowercase hex only (git shas)
}
