// SPDX-License-Identifier: MIT

package cli

// Interactive pre-install bootstrap (T-600, owner 2026-11-14: "unify the
// bootstrap script with topctl install — same pattern for everything,
// override flags, exit 1 when the operator declines"): ON a TTY,
// `topctl install` fills its own missing preconditions:
//
//   - repo missing          → create via gh, or deep-link + wait; the
//     --no-repo-create override refuses creation (exit 1 when the repo
//     is then still missing — never fail silently into a worse state);
//   - --deploy-key missing  → generate ~/.ssh/top_deploy_key + pinned
//     known_hosts, register via gh, or deep-link + wait;
//   - visibility            → prompted for the creation deep-link
//     (GitHub's /new form takes &name= and &visibility=).
//
// Non-interactive runs (piped, CI, --confirm) skip ALL of this: the
// strict preflight behavior is unchanged. The script
// scripts/bootstrap-repo.sh is retired — this IS the flow now.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// repoExists reports whether the GitHub repo is reachable: gh first
// (authoritative even for private repos), then a plain ls-remote with
// every credential prompt disabled — the https fallback on a MISSING
// private repo makes git ask for a username and hang the TTY (live
// probe, 2026-11-14). A prompt-refused probe = "not reachable".
func repoExists(ctx context.Context, r runnerArgs, ownerRepo string) bool {
	if _, err := exec.LookPath("gh"); err == nil {
		out, err := exec.Command("gh", "repo", "view", ownerRepo, "--json", "name").CombinedOutput()
		_ = out
		if err == nil {
			return true
		}
	}
	cmd := exec.Command("git", "ls-remote", "https://github.com/"+ownerRepo, "HEAD")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=echo")
	_, err := cmd.CombinedOutput()
	return err == nil
}

// createRepoViaGH creates the private repo through gh (may print a
// browser hint on first use).
func createRepoViaGH(ownerRepo string) error {
	cmd := exec.Command("gh", "repo", "create", ownerRepo, "--private")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// runnerArgs is the minimal call shape the reachability probe needs
// (the CLI's Runner seam, argv-only).
type runnerArgs func(ctx context.Context, name string, args ...string) error

// ghAvailable reports a logged-in gh CLI.
func ghAvailable() bool {
	if _, err := exec.LookPath("gh"); err != nil {
		return false
	}
	return exec.Command("gh", "auth", "status").Run() == nil
}

// deepLinkNewRepo is the pre-filled create form (owner-side /new 404s —
// the global form is the one that takes name+visibility, verified live
// 2026-11-14).
func deepLinkNewRepo(name string, visibility string) string {
	return "https://github.com/new?name=" + name + "&visibility=" + visibility
}

// interactiveBootstrap fills installOptions' repo/key preconditions on a
// TTY. Returns (deployKey, knownHosts, nil) when it made changes the
// caller must thread into Options; empty strings mean "preconditions
// were already satisfied / overridden". declined is true when the
// operator declined a creation the strict path then needs (the caller
// exits 1 with the honest message).
func interactiveBootstrap(ctx context.Context, r runnerArgs, repoURL string, noRepoCreate bool) (deployKey, knownHosts string, declined bool, err error) {
	if !isInteractiveTTY() {
		return "", "", false, nil
	}
	ownerRepo, ok := ownerRepoOf(repoURL)
	if !ok {
		return "", "", false, nil // not a GitHub repo — nothing to bootstrap
	}
	needRepo := !repoExists(ctx, r, ownerRepo)
	if needRepo && noRepoCreate {
		declined = true
		return "", "", declined, nil
	}
	if needRepo {
		visibility := askChoice("Repo "+ownerRepo+" does not exist. Create it — visibility?", []string{"private", "public"}, "private")
		created := false
		if ghAvailable() {
			fmt.Printf("▸ creating %s (%s) via gh…\n", ownerRepo, visibility)
			created = createRepoViaGH(ownerRepo) == nil
		}
		if !created {
			fmt.Printf("────────────────────────────────────────────────────────────\n")
			fmt.Printf("Create the %s repo by hand (empty, no README):\n  %s\n", visibility, deepLinkNewRepo(strings.SplitN(ownerRepo, "/", 2)[1], visibility))
			waitForEnter("Press ENTER once the repo exists")
		}
		if !repoExists(ctx, r, ownerRepo) {
			declined = true
			return "", "", declined, nil
		}
	}

	// Deploy key: only when the caller gave no --deploy-key AND the
	// default material is absent (an operator with existing material
	// passes --deploy-key as before; nothing here overrides them).
	home, _ := os.UserHomeDir()
	key := filepath.Join(home, ".ssh", "top_deploy_key")
	kh := filepath.Join(home, ".ssh", "top_known_hosts")
	if fileExists(key) && fileExists(kh) {
		return key, kh, false, nil
	}
	fmt.Printf("▸ generating deploy key %s + pinned %s\n", key, kh)
	if err := generateDeployKeyMaterial(key, kh); err != nil {
		return "", "", false, fmt.Errorf("deploy key generation: %w", err)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return "", "", false, fmt.Errorf("read %s.pub: %w", key, err)
	}
	registered := false
	if ghAvailable() {
		err := exec.Command("gh", "api", "-X", "POST", "/repos/"+ownerRepo+"/keys",
			"-f", "title=media-server-top", "-f", "key="+strings.TrimSpace(string(pub)),
			"-F", "read_only=false").Run()
		registered = err == nil
		if registered {
			fmt.Printf("▸ registered deploy key (read-write; flip to read-only after the install's --init push)\n")
		}
	}
	if !registered {
		fmt.Printf("────────────────────────────────────────────────────────────\n")
		fmt.Printf("Add the deploy key by hand — ALLOW WRITE ACCESS for now\n(flip to read-only after the install's --init push):\n  key:  %s\n  page: https://github.com/%s/settings/keys/new\n", strings.TrimSpace(string(pub)), ownerRepo)
		waitForEnter("Press ENTER once the key is added")
	}
	if err := verifyDeployKey(key, kh); err != nil {
		return "", "", false, fmt.Errorf("GitHub did not accept the deploy key (read-write? right repo?): %w", err)
	}
	fmt.Println("✓ GitHub reachable with the deploy key")
	return key, kh, false, nil
}

// ownerRepoOf extracts "owner/repo" from a github remote URL (ssh or
// https). Non-GitHub remotes return ok=false — nothing to bootstrap.
func ownerRepoOf(repoURL string) (string, bool) {
	u := strings.TrimSuffix(repoURL, ".git")
	for _, prefix := range []string{"git@github.com:", "ssh://git@github.com/", "https://github.com/"} {
		if rest, trimmed := strings.CutPrefix(u, prefix); trimmed {
			parts := strings.SplitN(rest, "/", 2)
			if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
				return parts[0] + "/" + parts[1], true
			}
			return "", false
		}
	}
	return "", false
}

// ── interactive primitives (TTY-only; the installer itself never
// prompts — ADR-0009's requireConfirm owns the confirm surface) ──

func isInteractiveTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func waitForEnter(msg string) {
	fmt.Printf("%s ", msg)
	_, _ = fmt.Fscanln(os.Stdin)
}

func askChoice(question string, choices []string, def string) string {
	fmt.Printf("%s [%s] (default %q): ", question, strings.Join(choices, "/"), def)
	var line string
	_, _ = fmt.Fscanln(os.Stdin, &line)
	if line == "" {
		return def
	}
	for _, c := range choices {
		if line == c {
			return c
		}
	}
	fmt.Printf("  (unknown answer %q — using %q)\n", line, def)
	return def
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// generateDeployKeyMaterial writes the ed25519 deploy keypair (0600) and
// the pinned github.com known_hosts — exactly what scripts/bootstrap-
// repo.sh did, now inside the verb.
func generateDeployKeyMaterial(key, knownHosts string) error {
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		return err
	}
	if err := exec.Command("ssh-keygen", "-t", "ed25519", "-f", key, "-N", "", "-C", "top-platform-repo", "-q").Run(); err != nil {
		return err
	}
	out, err := exec.Command("ssh-keyscan", "-t", "ed25519", "github.com").Output()
	if err != nil {
		return err
	}
	return os.WriteFile(knownHosts, out, 0o600)
}

// verifyDeployKey runs the exact ssh identity check the operator flow
// used (BatchMode — never a prompt/hang).
func verifyDeployKey(key, knownHosts string) error {
	cmd := exec.Command("ssh", "-i", key, "-o", "IdentitiesOnly=yes",
		"-o", "UserKnownHostsFile="+knownHosts, "-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10", "-T", "git@github.com")
	out, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(string(out), "successfully authenticated") &&
		!strings.Contains(string(out), "does not provide shell access") {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}
