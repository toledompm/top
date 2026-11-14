// SPDX-License-Identifier: MIT

package install

// T-212: --init — ADR-0005's repo-initialization clause of installer-
// contract phase 3. The platform repo is ADR-0002's GitOps source of
// truth; when the configured remote resolves EMPTY and --init was passed,
// this file seeds it from the embedded template (the same content committed
// at examples/platform-repo — single source of truth via symlink): render
// the template into a private worktree beside <dataDir>/git, then push ONE
// initial commit to the configured remote.
//
// Credential semantics (ADR-0008/0005):
//
//   https remotes: the one-time write credential is the TOP_INIT_TOKEN
//     environment value (Options.InitToken; the CLI reads it at runtime
//     only). It reaches git ONLY through the child process environment via
//     a 0700 askpass shim written into the work dir and deleted after the
//     push. Threat model, tersely: the token exists at rest only in the
//     operator's own shell environment; it is never on argv, never in git
//     config, never written to disk, never logged; a root/same-user process
//     could in principle read the child env — the same ambient trust level
//     as the operator's own shell. This is write-once plumbing; the
//     steady-state transport remains the read-only deploy key.
//   ssh remotes: the operator's deploy-key material (dataDir/git/id_ed25519,
//     known_hosts copied by phase 3) referenced BY PATH inside
//     GIT_SSH_COMMAND — the same transport gitsource uses; TOP never reads
//     the key file. If that key is read-only the push fails loudly on the
//     provider's stderr; the operator can switch to an https remote with
//     TOP_INIT_TOKEN instead (documented in internal/install's README).
//
// Remote CREATION ("where the provider supports it", ADR-0005) is
// deliberately v0-deferred: the contract's "provider permitting" wording
// scopes it to an optional path, and no v0 provider API client exists.
// --init pushes into a remote the operator created EMPTY; an unreachable
// remote under --init is a clean typed refusal naming it.
//
// Zero-changes rule: the existing-repo path (no --init) touches the remote
// with NOTHING but `git ls-remote` (read-only, phase 0) — asserted in
// tests by the absence of every other git verb on the call log.
// Re-init refusal: --init on a NON-EMPTY remote refuses with the evidence
// it saw (no force, no merge, no overwrite), EXCEPT when a previous run of
// THIS install already recorded initPushed on the newest phase-3 operation
// (resume: the seed commit landed and a later step failed; redoing the
// phase must not double-refuse on the repo's now-non-empty state).
//
// Render discipline: the embedded template carries exactly one existential
// placeholder ({{CONTROLLER_IMAGE}}); nothing secret is ever baked into
// the seed commit.

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/gitsource"
	"github.com/toledompm/top/internal/store"
)

//go:embed platformrepo
var platformRepoFS embed.FS

// templateRoot is the embedded template fs with the directory prefix
// stripped (mirroring internal/store/sqlite's embed.go discipline).
func templateRoot() (fs.FS, error) { return fs.Sub(platformRepoFS, "platformrepo") }

// controllerImageToken is the template's single existential placeholder.
const controllerImageToken = "{{CONTROLLER_IMAGE}}"

// Init identity defaults (ADR-0005: "a deterministic identity"; the config
// canon carries URL/branch only, so the author is this documented default —
// Options overrides exist for future config work).
const (
	initAuthorNameDefault  = "TOP platform seeding"
	initAuthorEmailDefault = "top@example.invalid"
	initVersionDefault     = "unknown"
	askpassName            = "top-init-askpass"
)

// Typed init failures. The CLI maps them onto the exit matrix (both are
// decision-with-evidence → drift space, exit 1); the values are message
// text, the identity is classification.
var (
	// ErrRemoteUnreachable: --init could not even see the remote. The
	// remote must already exist (empty is fine) — creation is v0-deferred.
	ErrRemoteUnreachable = errors.New("install: --init: the platform remote is not reachable")
	// ErrRepoNotEmpty: --init saw content on the remote. TOP never forces,
	// merges, or overwrites a foreign repo — the evidence is in the message.
	ErrRepoNotEmpty = errors.New("install: --init: the platform remote already has content")
)

// envRunner extends exec.Runner with a per-call child environment — the
// credential transport of --init (askpass token, strict GIT_SSH_COMMAND)
// must never carry values on argv. Production carriers: *exec.FastExec
// (cloned per call with ExtraEnv appended — the gitsource.FastRunner
// discipline); anything implementing RunEnv is honored first. Tests inject
// an env-recording shim.
type envRunner interface {
	RunEnv(ctx context.Context, env []string, name string, args ...string) (stdout, stderr []byte, err error)
}

// fastEnvRunner is the production carrier: an immutable per-call clone so
// concurrent calls stay race-free.
type fastEnvRunner struct{ base *exec.FastExec }

func (r fastEnvRunner) RunEnv(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
	fe := *r.base
	fe.ExtraEnv = append(append([]string{}, r.base.ExtraEnv...), env...)
	return fe.Run(ctx, name, args...)
}

// plainEnvRunner is the test-double fallback. A non-empty env REFUSES:
// dropping a credential-bearing environment silently would be exactly the
// bug class this seam exists to prevent (tests that need env inspection
// wrap exectest.FakeRunner with a RunEnv shim instead).
type plainEnvRunner struct{ r exec.Runner }

func (r plainEnvRunner) RunEnv(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
	if len(env) > 0 {
		return nil, nil, fmt.Errorf("install: --init: the configured Runner cannot carry the child environment (need an envRunner or exec.FastExec); refusing to drop it")
	}
	return r.r.Run(ctx, name, args...)
}

// envRunnerOf resolves the Options.Runner into the env-capable seam.
func envRunnerOf(r exec.Runner) envRunner {
	switch v := r.(type) {
	case envRunner:
		return v
	case *exec.FastExec:
		return fastEnvRunner{base: v}
	default:
		return plainEnvRunner{r: r}
	}
}

// runRepoInit is phase 3's --init clause (docs/contracts/installer.md:
// "If the repo is missing/empty and --init was passed: render the embedded
// platform-repo template, create the remote (provider permitting), and
// push the initial commit"). Reachability/emptiness evidence is fresh HERE
// (not trusted from phase 0): the remote can change between phases.
func runRepoInit(ctx context.Context, opts Options, st store.Store, opID string, res *Result, extra phaseExtras) error {
	workDir := filepath.Join(opts.DataDir, "git")
	if err := mkdirLocal0700(workDir); err != nil {
		return err
	}
	seedDir, err := os.MkdirTemp(workDir, "top-init-")
	if err != nil {
		return fmt.Errorf("install: --init: worktree: %w", err)
	}
	// The worktree never escapes this function: deleted after the push
	// (best-effort — the phase record carries the work's outcome already).
	defer func() { _ = os.RemoveAll(seedDir) }()

	// 1. Fresh evidence: reachability + emptiness, with the same auth
	// environment the push will use (a probe that says "empty" through a
	// transport the push cannot use would be a lie).
	kind, pushEnv, cleanup, perr := initPushEnv(opts, workDir)
	if perr != nil {
		return perr
	}
	defer cleanup() // deletes the askpass shim (token transport), if any
	runner := envRunnerOf(opts.Runner)

	out, stderrOut, lerr := runner.RunEnv(ctx, pushEnv, "git", "ls-remote", opts.RepoURL, "refs/heads/"+opts.Branch)
	evidence := firstNonEmptyLine(stderrOut, out)
	if lerr != nil {
		return fmt.Errorf("%w: %s: %v (remote said: %s); FIX: create the empty repo on the provider yourself, then re-run `topctl install --init` (completed phases resume) — remote creation via provider API is deliberately v0-deferred (ADR-0005 \"provider permitting\")",
			ErrRemoteUnreachable, opts.RepoURL, lerr, evidence)
	}
	if strings.TrimSpace(string(out)) != "" {
		if ptErr := refuseOrSkipInit(ctx, opts, st, evidence); ptErr != nil {
			return ptErr
		}
		// A previous run of this install seeded it: resume-and-skip. The
		// registration record below (runWiring) still completes.
		res.Checked = append(res.Checked, "platform repo "+opts.RepoURL+" resolves non-empty, but a previous run of this install already pushed the seed (initPushed) — skipping re-init")
		return nil
	}

	// Empty confirmed. 2. Render the embedded template into the worktree.
	rendered, rerr := renderTemplate(seedDir, opts.ImageRef())
	if rerr != nil {
		return rerr
	}

	// 3. The deterministic verb sequence (golden-tested): init, add,
	// commit (identity via -c flags), remote add, push. Identity timing
	// details (author/committer dates) come from the child env so the
	// commit is reproducible for a given run's clock.
	run := func(args ...string) ([]byte, error) {
		stdout, stderr, gerr := runner.RunEnv(ctx, pushEnv, "git", append([]string{"-C", seedDir}, args...)...)
		if gerr != nil {
			return nil, fmt.Errorf("install: --init: git %s failed: %w (child said: %s)", args[0], gerr, firstNonEmptyLine(stderr, stdout))
		}
		return stdout, nil
	}
	if _, err := run("init", "-b", opts.Branch); err != nil { // git ≥ 2.28 names the initial branch
		return err
	}
	if _, err := run("add", "-A"); err != nil {
		return err
	}
	if _, err := run(
		"-c", "user.name="+opts.initAuthorName(), "-c", "user.email="+opts.initAuthorEmail(),
		"-c", "commit.gpgsign=false", "commit", "-m", initCommitMessage(opts),
	); err != nil {
		return err
	}
	if _, err := run("remote", "add", "origin", opts.RepoURL); err != nil {
		return err
	}
	if _, err := run("push", "-u", "origin", opts.Branch); err != nil {
		return err
	}

	extra["initPushed"] = true
	extra["initCredential"] = kind // transport KIND only — never the value
	extra["initFiles"] = rendered
	res.Checked = append(res.Checked, "platform repo "+opts.RepoURL+" resolved empty on branch "+opts.Branch+" — seeding proceeded")
	res.Created = append(res.Created, fmt.Sprintf("platform-repo seed commit pushed to %s (branch %s, %d file(s), transport %s), message %q",
		opts.RepoURL, opts.Branch, rendered, kind, initCommitMessage(opts)))
	switch kind {
	case "ssh-deploy-key":
		res.Notes = append(res.Notes, "init push authenticated by the deploy-key FILE PATH inside GIT_SSH_COMMAND (ADR-0008); the key itself was never read by TOP")
	case "askpass-token":
		res.Notes = append(res.Notes, "init push authenticated by a one-time credential from the environment (TOP_INIT_TOKEN), carried in the child env and answered through a deleted askpass shim; never argv, never on disk, never stored, never logged")
	default:
		res.Notes = append(res.Notes, "init push ran with no extra credential configured (local/anonymous remote)")
	}
	return nil
}

// refuseOrSkipInit classifies the non-empty evidence across the
// resume/foreign boundary: the NEWEST phase-3 operation record saying
// initPushed means this install pushed it on a previous attempt (skip);
// anything else is foreign content — the clean refusal with evidence.
func refuseOrSkipInit(ctx context.Context, opts Options, st store.Store, evidence string) error {
	if previouslySeeded(ctx, st) {
		return nil
	}
	return fmt.Errorf("%w: %s branch %q resolves with content (evidence: %s) — TOP never forces, merges, or overwrites a foreign repo; leave the content as-is, or if a PREVIOUS attempt of this install pushed the seed and was interrupted mid-install, resume by re-running `topctl install` (it recognizes its own seed)",
		ErrRepoNotEmpty, opts.RepoURL, opts.Branch, evidence)
}

// previouslySeeded reads the newest phase-3 operation record for the
// initPushed marker. An aborted op does NOT count as seeded (a superseded
// run's push may not have completed; emptiness is decided freshly anyway).
func previouslySeeded(ctx context.Context, st store.Store) bool {
	ops, err := st.ListOperations(ctx, 100, &store.OperationFilter{Kind: domain.OpInstall})
	if err != nil {
		return false // scan failures defer to the fresh empty check; never skip on a broken audit trail
	}
	for _, op := range ops {
		if p, ok := phaseOf(op); ok && p == phaseWiring {
			v, ok := op.Details["initPushed"]
			return ok && v == true && op.State != domain.OpAborted //nolint:staticcheck // direct bool literal: the details map is a JSON round-trip
		}
	}
	return false
}

// renderTemplate materializes the embedded template into seedDir, filling
// the existential placeholder. Deterministic per inputs (and therefore
// idempotent to re-run into a fresh directory).
func renderTemplate(seedDir, imageRef string) (int, error) {
	root, err := templateRoot()
	if err != nil {
		return 0, fmt.Errorf("install: --init: template fs: %w", err)
	}
	rendered := 0
	werr := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." {
				if err := os.MkdirAll(filepath.Join(seedDir, filepath.FromSlash(p)), 0o700); err != nil {
					return fmt.Errorf("mkdir %s: %w", p, err)
				}
			}
			return nil
		}
		raw, err := fs.ReadFile(root, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		content := bytes.ReplaceAll(raw, []byte(controllerImageToken), []byte(imageRef))
		if err := os.WriteFile(filepath.Join(seedDir, filepath.FromSlash(p)), content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
		rendered++
		return nil
	})
	if werr != nil {
		return 0, fmt.Errorf("install: --init: render template: %w", werr)
	}
	return rendered, nil
}

// initPushEnv builds the credential-bearing child environment for the init
// probe + push. Returns the transport kind (report vocabulary), the env,
// and the askpass shim's cleanup (removes the shim when written — the
// token itself lives only in the child env, never in the file).
func initPushEnv(opts Options, workDir string) (kind string, env []string, cleanup func(), err error) {
	cleanup = func() {}
	if isSSHRemote(opts.RepoURL) {
		// The same transport internal/gitsource uses (ADR-0008): the
		// operator's key material copied into <dataDir>/git/ during phase
		// 3 is referenced BY PATH; TOP never reads the key file.
		env = []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=" + gitsource.SSHCommand(
			filepath.Join(workDir, "id_ed25519"), filepath.Join(workDir, "known_hosts"))}
		return "ssh-deploy-key", env, cleanup, nil
	}
	if tok := strings.TrimSpace(opts.InitToken); tok != "" {
		askpassPath := filepath.Join(workDir, askpassName)
		if werr := os.WriteFile(askpassPath, []byte(askpassScript()), 0o700); werr != nil {
			return "", nil, cleanup, fmt.Errorf("install: --init: askpass shim: %w", werr)
		}
		once := false
		cleanup = func() {
			if !once {
				once = true
				_ = os.Remove(askpassPath)
			}
		}
		env = []string{
			"GIT_TERMINAL_PROMPT=0",
			"GIT_ASKPASS=" + askpassPath,
			"TOP_INIT_TOKEN=" + tok,
		}
		return "askpass-token", env, cleanup, nil
	}
	// Anonymous transport: local/file/anonymous-https remotes (the tier-1
	// test double; also an intranet mirror). Prompts can deadlock a
	// captured process — always off.
	return "anonymous", []string{"GIT_TERMINAL_PROMPT=0"}, cleanup, nil
}

// askpassScript carries no credential: git calls it with the prompt on
// argv[1]; the password answer reads the environment TOP handed to git.
func askpassScript() string {
	return `#!/bin/sh
# One-time askpass shim of TOP's installer (--init). The token is NEVER
# written in this file: it is read from the environment TOP passed to git.
case "$1" in
*sername*) printf 'top\n' ;;
*) printf '%s\n' "$TOP_INIT_TOKEN" ;;
esac
`
}

// initCommitMessage is the seed commit's subject.
func initCommitMessage(opts Options) string {
	return "top: platform repo initialized by TOP " + opts.initVersion()
}

// isSSHRemote — ssh remote shape detection. internal/gitsource has the
// same guard unexported; this bounded local twin stays re-verifiable
// against gitsource's tests (the same deliberate-duplicate discipline as
// install's atLeastVersion vs doctor's engineAtLeast).
func isSSHRemote(url string) bool {
	if strings.HasPrefix(url, "ssh://") {
		return true
	}
	// scp-style only when actually scp-shaped (gitsource's guard):
	// "user@host:path", never a bare path.
	if i := strings.IndexByte(url, '@'); i > 0 {
		head, rest := url[:i], url[i:]
		if !strings.Contains(head, ":") && strings.Contains(rest, ":") {
			return true
		}
	}
	return false
}

// firstNonEmptyLine picks the first non-empty trimmed line across the
// streams in priority order (the compact evidence for error messages).
func firstNonEmptyLine(streams ...[]byte) string {
	for _, v := range streams {
		for _, line := range strings.Split(string(v), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				return line
			}
		}
	}
	return "(empty)"
}

// mkdirLocal0700 mirrors gitsource's mkdir0700 (create 0700, tighten an
// existing looser directory — the ADR-0008 posture).
func mkdirLocal0700(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("install: --init: create %s: %w", dir, err)
	}
	return os.Chmod(dir, 0o700)
}
