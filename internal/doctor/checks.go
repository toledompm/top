// SPDX-License-Identifier: MIT

package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/toledompm/top/internal/exec"
)

// checkOS verifies the OS/arch is in the supported set
// (docs/operations/installation.md + ADR-0001): linux x86_64/arm64 —
// Ubuntu 22.04/24.04/26.04, Debian 11+. macOS is a documented DEV-only
// run (ADR-0012 dev story): WARN, not fail. Everything else: fail.
func (r *Report) checkOS(opts Options) {
	goos, arch := opts.OSProbe()
	switch goos {
	case "linux":
		switch arch {
		case "amd64", "arm64":
			r.checkDistro(opts.OSReleasePath)
		default:
			r.fail("os/arch", "OS architecture",
				fmt.Sprintf("architecture %q is not supported (want amd64 or arm64 on linux)", arch),
				"run TOP on a linux host with an x86_64 or arm64 CPU")
		}
	case "darwin":
		r.warn("os/os", "operating system",
			fmt.Sprintf("macOS (%s/%s) is not a supported host; TOP targets Linux — this counts as a development-only run", goos, arch),
			"use a Linux host (Ubuntu 22.04/24.04/26.04, Debian 11+) for a real install; on macOS only dev-tier tooling is expected to run")
	default:
		r.fail("os/os", "operating system",
			fmt.Sprintf("operating system %q is not supported (want linux; %q build detected)", goos, runtime.GOOS),
			"build topctl for linux (amd64/arm64) and run it on Ubuntu 22.04/24.04/26.04 or Debian 11+")
	}
}

// checkDistro reads the os-release file (linux only; best effort). The
// support window lives in distroSupported; path "" → /etc/os-release.
// With an injectable path (Options.OSReleasePath) tests write their own
// fixture in a temp dir instead of reading the host file.
func (r *Report) checkDistro(pathOpt string) {
	path := pathOpt
	if path == "" {
		path = "/etc/os-release"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		r.warn("os/distro", "operating system distribution",
			"could not read /etc/os-release; supported distribution could not be verified",
			"verify the host runs Ubuntu 22.04/24.04/26.04 or Debian 11+")
		return
	}
	id, ok1 := osReleaseField(raw, "ID")
	vers, ok2 := osReleaseField(raw, "VERSION_ID")
	switch {
	case ok1 && (id == "ubuntu" || id == "debian"):
		if !ok2 || vers == "" {
			r.warn("os/distro", "operating system distribution",
				"/etc/os-release lacked VERSION_ID; could not verify the supported version window",
				"verify the host runs Ubuntu 22.04/24.04/26.04 or Debian 11+")
			return
		}
		if distroSupported(id, vers) {
			r.ok("os/distro", "operating system distribution",
				fmt.Sprintf("%s %s is a supported distribution", id, vers))
		} else {
			r.fail("os/distro", "operating system distribution",
				fmt.Sprintf("%s %s is outside the supported window (want Ubuntu 22.04/24.04/26.04, Debian 11+)", id, vers),
				"install TOP on Ubuntu 22.04/24.04/26.04 or Debian 11+ (docs/operations/installation.md)")
		}
	default:
		friendly := id
		if !ok1 {
			friendly = "<unknown>"
		}
		r.warn("os/distro", "operating system distribution",
			fmt.Sprintf("distribution %q is not in the tested set (Ubuntu, Debian); TOP may still work", friendly),
			"the tested distributions are Ubuntu 22.04/24.04/26.04 and Debian 11+; proceed at your own risk")
	}
}

func distroSupported(id, versionID string) bool {
	major, _ := strconv.Atoi(strings.SplitN(versionID, ".", 2)[0])
	if id == "ubuntu" {
		return major >= 22 && major <= 26
	}
	return major >= 11 // debian 11+
}

// osReleaseField extracts a KEY=value field (values are quoted sometimes),
// returning (value, found).
func osReleaseField(raw []byte, key string) (string, bool) {
	prefix := key + "="
	for _, ln := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(ln, prefix) {
			v := strings.Trim(strings.TrimPrefix(ln, prefix), `"`)
			return v, true
		}
	}
	return "", false
}

// distroSupported answers whether ID/VERSION_ID sits inside the
// documented support window (installation.md).

// checkDocker verifies: binary present → daemon reachable → engine
// version ≥ MinDockerVersion (recorded assumption in README: no platform
// doc pins a Docker floor; 24.0 was the stable engine line at planning).
func (r *Report) checkDocker(ctx context.Context, runner exec.Runner) {
	out, _, err := runner.Run(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	var ee *exec.ExitError
	switch {
	case errors.Is(err, exec.ErrNotFound):
		r.fail("docker/binary", "docker present",
			"docker is not on PATH",
			"install Docker (docker-ce docs, or the gated `topctl install --install-dokku` bootstrap can bring Docker along)")
		return
	case errors.As(err, &ee):
		r.fail("docker/daemon", "docker daemon reachable",
			fmt.Sprintf("docker is installed but the daemon is not reachable (exit %d): %s", ee.ExitCode, excerpt(err.Error())),
			"on a host distro: `sudo systemctl start docker`; on a desktop: start Docker Desktop/OrbStack, then re-run")
		return
	}
	if err != nil {
		r.fail("docker/daemon", "docker daemon reachable",
			"docker probe failed: "+excerpt(err.Error()),
			"ensure docker works for this user (timeout? try `docker version` by hand)")
		return
	}
	version := strings.TrimSpace(string(out))
	if engineAtLeast(version, MinDockerVersion) {
		r.ok("docker/version", "docker engine version",
			fmt.Sprintf("docker engine %s satisfies the %s floor", version, MinDockerVersion))
	} else {
		r.fail("docker/version", "docker engine version",
			fmt.Sprintf("docker engine %s is below the %s floor (assumption recorded in internal/doctor/README.md)", version, MinDockerVersion),
			fmt.Sprintf("upgrade Docker to ≥ %s (docs/operations/installation.md)", MinDockerVersion))
	}
}

// engineAtLeast compares "maj.min" floors against "maj.min[.patch]" actual.
func engineAtLeast(actual, floor string) bool {
	a, f := versionParts(actual), versionParts(floor)
	for i := 0; i < len(f); i++ {
		av := 0
		if i < len(a) {
			av = a[i]
		}
		if av < f[i] {
			return false
		}
		if av > f[i] {
			return true
		}
	}
	return true
}

func versionParts(v string) []int {
	fields := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	out := make([]int, 0, len(fields))
	for _, fld := range fields {
		// Stop at non-numeric residue ("27.4.0-ce" style tags). Take the
		// leading digits and stop.
		n := 0
		for i := 0; i < len(fld); i++ {
			if fld[i] < '0' || fld[i] > '9' {
				break
			}
			n = n*10 + int(fld[i]-'0')
		}
		out = append(out, n)
	}
	return out
}

// checkDokku classifies Dokku per installer.md Phase 0:
// absent → INFO (a gated install exists; not a failure);
// present+healthy ≥ 0.38 → ok; present but broken / below floor / not
// runnable by this user → FAIL with a fix hint.
// Returns the app-name list (possibly nil) so checkNames can reuse the
// single read-only `dokku apps:list`.
func (r *Report) checkDokku(ctx context.Context, opts Options) []string {
	out, _, err := opts.Runner.Run(ctx, "dokku", "version")
	if errors.Is(err, exec.ErrNotFound) {
		r.info("dokku/state", "dokku present",
			"dokku is not installed; TOP can install it (approval-gated)",
			fmt.Sprintf("re-run `topctl install --install-dokku` to install via the documented bootstrap (it lists and confirms exact changes first); or install Dokku %s+ upstream by hand", MinDokkuVersion))
		return nil
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			r.fail("dokku/state", "dokku healthy",
				fmt.Sprintf("dokku is installed but `dokku version` failed (exit %d): %s", ee.ExitCode, excerpt(err.Error())),
				"if permissions: run `sudo -E dokku` or add the user to the dokku group (usermod -aG dokku <you>); if the binary is broken, reinstall Dokku per docs.dokku.com")
			return nil
		}
		r.fail("dokku/state", "dokku healthy",
			"dokku probe failed: "+excerpt(err.Error()),
			"timed out? try `dokku version` by hand")
		return nil
	}
	version := dokkuVersionFrom(string(out))
	if engineAtLeast(version, MinDokkuVersion) {
		r.ok("dokku/state", "dokku healthy",
			fmt.Sprintf("dokku %s is installed and healthy", version))
	} else {
		r.fail("dokku/state", "dokku healthy",
			fmt.Sprintf("dokku %s is below the supported floor %s (ADR-0001)", version, MinDokkuVersion),
			fmt.Sprintf("upgrade Dokku to ≥ %s (docs.dokku.com upgrade guide, then re-run)", MinDokkuVersion))
	}
	// Only inspect apps when the CLI itself is healthy.
	appsOut, _, err := opts.Runner.Run(ctx, "dokku", "apps:list")
	if err != nil {
		r.warn("dokku/apps", "dokku app inventory",
			"could not list Dokku apps (read-only check skipped): "+excerpt(err.Error()),
			"run `dokku apps:list` by hand if you plan to install now")
		return nil
	}
	return splitApps(string(appsOut))
}

// dokkuVersionFrom pulls a numeric version token out of
// "dokku version 0.38.13 (git commit …)". Falls back to "" (unknown → always
// below floor with an explanatory message? no: unknown → treated as
// modern and the honest message surfaces it).
func dokkuVersionFrom(out string) string {
	for _, tok := range strings.Fields(out) {
		if tok == "" {
			continue
		}
		if tok[0] >= '0' && tok[0] <= '9' {
			return tok
		}
	}
	return ""
}

// checkNames flags collisions with TOP's reserved names before Phase 1
// (ownership inventory) makes it a hard stop:
//   - an app literally named core-top-controller → FAIL (Phase 4 needs
//     that exact name);
//   - any other app sharing a reserved `<namespace>-` prefix → WARN
//     (installer will surface the inventory adoption question; it is not
//     a hard conflict today).
func (r *Report) checkNames(apps []string, opts Options) {
	if apps == nil {
		return // dokku absent or app-list unavailable; nothing to check
	}
	prefixed := make([]string, 0, len(opts.ReservedNamespaces))
	for _, ns := range opts.ReservedNamespaces {
		prefixed = append(prefixed, ns+"-")
	}
	collided := false
	for _, app := range apps {
		switch app {
		case ControllerAppName:
			collided = true
			if state, known := opts.LedgeredApps[app]; known {
				// The ledger says this IS TOP's own deployment — a
				// standalone `topctl doctor` after a successful install
				// must not call the operator's own controller a hostile
				// collision (live run 2026-11-14: doctor-after-install
				// FAILED on TOP's own app). INFO + the re-install note.
				r.info("names/top-controller", "TOP-reserved app names",
					fmt.Sprintf("app %q exists and is TOP's own deployment (ledger state: %s)", ControllerAppName, state),
					"re-running `topctl install` resumes/skips at completed phases; `topctl uninstall` removes it")
			} else {
				r.fail("names/top-controller", "TOP-reserved app names",
					fmt.Sprintf("a Dokku app named %q already exists on this host", ControllerAppName),
					"TOP's bootstrap deploy needs that exact name; check whether it is TOP's own leftover (`dokku ps:inspect core-top-controller`) and remove/rename it before installing")
			}
		default:
			for _, pre := range prefixed {
				if strings.HasPrefix(app, pre) {
					collided = true
					r.warn("names/reserved-prefix", "TOP-reserved app names",
						fmt.Sprintf("a Dokku app named %q shares a reserved TOP namespace prefix (%s)", app, strings.TrimSuffix(pre, "-")),
						"install may either adopt it or refuse; pre-check with `dokku apps:list` before installing to avoid surprises")
					break
				}
			}
		}
	}
	if collided {
		r.warn("dokku/apps", "dokku app inventory",
			fmt.Sprintf("dokku has %d app(s) and reserved-name collisions were reported above", len(apps)),
			"resolve the collisions (rename/remove the offending apps) before `topctl install`")
	} else {
		r.ok("dokku/apps", "dokku app inventory",
			fmt.Sprintf("dokku has %d app(s); none collide with TOP-reserved names", len(apps)))
		r.ok("names/clean", "TOP-reserved app names", "no reserved-name collisions")
	}
}

// splitApps converts `dokku apps:list` output into app names. Shapes
// handled: plain app-per-line, and "=====> app" banner lines (single
// token only); anything multi-token or decorative is skipped.
func splitApps(out string) []string {
	var apps []string
	for _, ln := range strings.Split(out, "\n") {
		if rest, isBanner := strings.CutPrefix(ln, "=====>"); isBanner {
			ln = rest
		}
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.ContainsAny(ln, " \t:()") {
			continue
		}
		apps = append(apps, ln)
	}
	return apps
}

// checkPorts probes 80 and 443 (docs: "free if HTTP proxying will be
// used"; brief-specified simplification: always check, occupied → WARN,
// never a firewall probe, never a require-root behavior — Sor the
// bind-close probe is best-effort and may be skipped with --skip-ports).
func (r *Report) checkPorts(opts Options) {
	if opts.SkipPorts {
		r.info("ports/skipped", "ports 80/443 free",
			"port probe skipped (--skip-ports)",
			"verify 80/443 are free or proxy-owned before an install that brings the proxy")
		return
	}
	for _, port := range []string{"80", "443"} {
		name := "ports/" + port
		release, err := opts.Probe("tcp", ":"+port)
		switch {
		case err == nil:
			release()
			r.ok(name, "port "+port+" free", "port "+port+" is bindable (not currently held)")
		case errors.Is(err, errPermission):
			r.warn(name, "port "+port+" free",
				"port "+port+" could not be probed: binding privileged ports requires root here",
				"TOP will report this host as proxy-occupied on this port; if Dokku's nginx is already the listener that is fine, otherwise free the port before installing")
		default:
			r.warn(name, "port "+port+" free",
				fmt.Sprintf("port %s is occupied (something is listening; if it is Dokku's nginx from a prior install, that is expected)", port),
				"identify the listener by hand (e.g. `sudo ss -lptn 'sport = :"+port+"''` on Linux) when unsure; a non-Dokku owner on 80/443 will clash with the proxy")
		}
	}
}

// checkResources: disk free ≥ 20 GB recommended (installation.md
// "20 GB free disk"); RAM ≥ 1 GB floor, < 4 GB warns (observability stack).
// Semantics choices (documented in README):
//   - disk: FAIL only below a hard lay-down floor (2 GiB); < 20 GiB → WARN
//     (recommended, with observability);
//   - RAM: FAIL under 1 GiB; WARN under 4 GiB.
func (r *Report) checkResources(opts Options) {
	// Disk — anchored at DataDir when it exists, else its nearest
	// existing ancestor (state dir doesn't exist pre-install).
	anchor := anchorDir(opts.DataDir)
	free, err := opts.DiskProbe(anchor)
	switch err {
	case nil:
		gib := free >> 30
		switch {
		case gib >= 20:
			r.ok("resources/disk", "disk headroom",
				fmt.Sprintf("%d GiB free on %s (≥ 20 GiB recommended)", gib, anchor))
		case gib >= 2:
			r.warn("resources/disk", "disk headroom",
				fmt.Sprintf("%d GiB free on %s; below the 20 GiB recommended headroom", gib, anchor),
				"TOP + Dokku + observability stack can fill 20 GiB quickly; free space or expect tuning (docs/operations/observability.md)")
		default:
			r.fail("resources/disk", "disk headroom",
				fmt.Sprintf("%d GiB free on %s; below the 2 GiB lay-down floor", gib, anchor),
				"free at least 20 GiB (docs/operations/installation.md recommends 20 GB post-laydown)")
		}
	default:
		r.warn("resources/disk", "disk headroom",
			"could not stat filesystem disk usage: "+excerpt(err.Error()),
			"run `df -h <dataDir's filesystem>` by hand to confirm headroom")
	}

	// RAM.
	total, err := opts.MemProbe()
	switch err {
	case nil:
		gib := total >> 30
		switch {
		case gib >= 4:
			r.ok("resources/ram", "memory headroom",
				fmt.Sprintf("%d GiB RAM (≥ 4 GiB recommended with the observability stack)", gib))
		case gib >= 1:
			r.warn("resources/ram", "memory headroom",
				fmt.Sprintf("%d GiB RAM; ≥ 1 GiB floor met, but the observability stack wants 4 GiB", gib),
				"either accept a small-host tuning (docs/operations/observability.md) or add RAM")
		default:
			r.fail("resources/ram", "memory headroom",
				fmt.Sprintf("%d MiB RAM is below the 1 GiB floor (v0 TOP + Dokku alone)", total>>20),
				"provide at least 1 GiB RAM (2 vCPU / 4 GiB / 20 GiB disk is the documented comfortable point)")
		}
	default:
		r.warn("resources/ram", "memory headroom",
			"could not read total RAM (accuracy note: on macOS the sysctl read is dev-run accuracy): "+excerpt(err.Error()),
			"run `free -h` (linux) or `sysctl hw.memsize` (macOS) by hand")
	}
}

// anchorDir walks DataDir (or its nearest existing ancestor) so statfs
// answers for the filesystem that will actually hold TOP's state, without
// creating anything.
func anchorDir(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

// checkSudo: ADR-0009 — TOP never assumes passwordless sudo; it prints
// exact sudo commands. A `sudo -n true` failure is a WARN, not a fail:
// interactive sudo is explicitly fine; passwordless sudo is only a
// scripting nicety.
func (r *Report) checkSudo(ctx context.Context, runner exec.Runner) {
	_, _, err := runner.Run(ctx, "sudo", "-n", "true")
	switch {
	case err == nil:
		r.ok("sudo/passwordless", "passwordless sudo",
			"`sudo -n true` succeeded: fully non-interactive installs are possible")
	case errors.Is(err, exec.ErrNotFound):
		r.warn("sudo/passwordless", "passwordless sudo",
			"sudo is not on PATH (or this system has no sudo)",
			"run topctl as a root-capable user, or apply the printed sudo commands manually")
	default:
		r.warn("sudo/passwordless", "passwordless sudo",
			"`sudo -n true` did not succeed; non-interactive install steps will need you at the keyboard once",
			"interactive sudo is fine (TOP prints exact sudo commands, never assumes passwordless — ADR-0009); for scripting, configure passwordless sudo for the install user")
	}
}

// checkDataDir differs from other checks in an important way: it performs
// NO filesystem writes (doctor contract: read-only; the actual
// data-dir check (typo "data-dir" fixed): the installer creates it.
// The contract mode is 0711 on the datadir itself (traverse-only, owner
// posture amendment 2026-11-14: the 0644 runtime config must be readable
// by non-sudo CLI verbs and dokku's storage plugin stats the volumes
// subtree) with 0700 subdirs; 0700 stays accepted (a pre-amendment
// install). Anything looser → WARN. It only reports the observed state:
//   - absent → INFO (installer creates it);
//   - not a dir → WARN;
//   - 0700 or 0711 → OK;
//   - anything else → WARN.
func (r *Report) checkDataDir(opts Options) {
	dir := opts.DataDir
	st, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		r.info("store/datadir", "data directory "+dir,
			dir+" does not exist yet; created by `topctl install` (0711)",
			"no action needed now")
	case err != nil:
		r.warn("store/datadir", "data directory "+dir,
			"could not stat "+dir+": "+excerpt(err.Error()),
			"check permissions; the install phase will need to (re)create it")
	case !st.IsDir():
		r.warn("store/datadir", "data directory "+dir,
			dir+" exists and is not a directory",
			"remove or move the offending path so topctl install can create the state directory")
	case st.Mode().Perm() == 0o700:
		r.ok("store/datadir", "data directory "+dir,
			dir+" exists with 0700 permissions")
	case st.Mode().Perm() == 0o711:
		r.ok("store/datadir", "data directory "+dir,
			dir+" exists with 0711 permissions (traverse-only: the runtime config is non-root-readable; contents stay 0700/0600)")
	case st.Mode().Perm()&^0o711 != 0 || st.Mode().Perm()&0o700 != 0o700:
		r.warn("store/datadir", "data directory "+dir,
			fmt.Sprintf("%s exists with mode %o (TOP's contract is 0711 traverse-only, subdirs 0700)", dir, st.Mode().Perm()),
			"`chmod 711 "+dir+"` (TOP re-tightens this during install)")
	default:
		r.ok("store/datadir", "data directory "+dir,
			dir+" exists with 0711 permissions")
	}
}

// ---- small finding constructors -------------------------------------------

func (r *Report) ok(id, check, msg string)         { r.add(id, SeverityOK, check, msg, "") }
func (r *Report) info(id, check, msg, hint string) { r.add(id, SeverityInfo, check, msg, hint) }
func (r *Report) warn(id, check, msg, hint string) { r.add(id, SeverityWarn, check, msg, hint) }
func (r *Report) fail(id, check, msg, hint string) { r.add(id, SeverityFail, check, msg, hint) }

// add appends a finding; on non-ok severities the hint must be non-empty.
func (r *Report) add(id string, sev Severity, check, msg, hint string) {
	r.Findings = append(r.Findings, Finding{
		ID: id, Severity: sev, Check: check, Message: msg, Hint: hint,
	})
}

// excerpt keeps error text short for the one-line message contract.
func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
