// SPDX-License-Identifier: MIT

package recover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/toledompm/top/internal/config"
	"github.com/toledompm/top/internal/doctor"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/store/sqlite"
)

// Diagnose — the fully-real M2 half of the backup-recovery contract's
// `diagnose` line (independent recovery, ADR-0005). Read-only by
// contract; documented single exception: the state-store open is
// migrate-on-open by ADR-0007 (store README: recover paths that open
// the DB run forward-only migrations; ErrSchemaNewer is surfaced as its
// own finding instead).
//
// Checks (each finding carries a suggested-action Hint; all checks run
// even when some fail — the doctor Run discipline):
//
//   - dokku presence/version + apps:list healthy-ish verdict;
//   - controller app existence + ps:report state + logs evidence;
//   - SQLite file: exists, opens, integrity (PRAGMA via store.Check),
//     staleness of the newest recorded operation;
//   - config parses/validates (missing → "fresh install assumed");
//   - /var/lib/top{,/secrets,/git} permission facts (ADR-0008);
//   - disk headroom at the dataDir's filesystem.
//
// Listener-conflict probing is deliberately NOT here: binding 80/443 is
// an install-time concern that `topctl doctor` owns (see README).
func Diagnose(ctx context.Context, opts Options) Report {
	opts.applyDefaults()
	r := Report{}

	apps := checkDokku(&r, ctx, opts)
	controllerPresent := checkControllerApps(&r, ctx, opts, apps)
	checkStore(&r, ctx, opts, controllerPresent)
	checkConfigFile(&r, opts)
	checkPermissions(&r, opts)
	checkDisk(&r, opts)
	return r
}

// checkDokku is the recovery-graded Dokku probe. Unlike doctor's (dokku
// absent → info, a guided install supplies it), recovery FAILS here:
// ADR-0005 makes the Dokku CLI the recovery path's only external
// dependency. Returns the apps list, or nil when unavailable.
func checkDokku(r *Report, ctx context.Context, opts Options) []string {
	out, _, err := opts.Runner.Run(ctx, "dokku", "version")
	if errors.Is(err, exec.ErrNotFound) {
		fail(r, "dokku/present", "dokku CLI present",
			"dokku is not on PATH: the recovery path runs the Dokku CLI directly (ADR-0005), so without it nothing here can act externally",
			"install Dokku per docs.dokku.com (or run topctl from the root/dokku-group user on the host); manual check: `which dokku`")
		return nil
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			fail(r, "dokku/health", "dokku healthy",
				fmt.Sprintf("`dokku version` failed (exit %d): %s", ee.ExitCode, excerpt(err.Error())),
				"check group membership (`sudo usermod -aG dokku <you>`) and run `dokku version` by hand")
			return nil
		}
		fail(r, "dokku/health", "dokku healthy",
			"`dokku version` could not run: "+excerpt(err.Error()),
			"try `dokku version` by hand; a timeout points at a wedged dokku CLI process")
		return nil
	}
	version := dokkuVersionFrom(string(out))
	if version == "" {
		version = "<unknown>"
	}

	// Healthy-ish verdict: version AND apps:list both answer.
	appsOut, _, err := opts.Runner.Run(ctx, "dokku", "apps:list")
	if err != nil {
		ok(r, "dokku/health", "dokku healthy",
			fmt.Sprintf("dokku %s answers `version` (healthy-ish)", version))
		warn(r, "dokku/apps", "dokku app inventory",
			"`dokku apps:list` failed: "+excerpt(err.Error()),
			"run `dokku apps:list` by hand; while it fails, the controller-state check below runs degraded (from ps:report only). ADR-0005: recovery operates without the app list if necessary")
		return nil
	}
	versionOK := engineAtLeast(version, doctor.MinDokkuVersion)
	apps := splitApps(string(appsOut))
	if versionOK {
		ok(r, "dokku/health", "dokku healthy",
			fmt.Sprintf("dokku %s healthy (version ≥ %s and apps:list ok; %d app(s))", version, doctor.MinDokkuVersion, len(apps)))
	} else {
		fail(r, "dokku/health", "dokku healthy",
			fmt.Sprintf("dokku %s is below the supported floor %s (ADR-0001); the recovery contract's commands may misbehave on older dokku", version, doctor.MinDokkuVersion),
			fmt.Sprintf("upgrade Dokku to ≥ %s (docs.dokku.com), then re-run diagnose", doctor.MinDokkuVersion))
	}
	return apps
}

// checkControllerApps reads the controller app's existence and running
// state, plus logs evidence. Returns whether the app exists.
func checkControllerApps(r *Report, ctx context.Context, opts Options, apps []string) bool {
	app := controllerApp()
	if apps == nil {
		info(r, "controller/state", "controller app "+app,
			"dokku apps:list was unavailable so controller existence could not be verified (this does not compromise the independence guarantee: diagnose itself ran)",
			"fix the dokku/… finding above and re-run; `dokku apps:list` names the controller app when it exists")
		return false
	}
	found := false
	for _, a := range apps {
		if a == app {
			found = true
			break
		}
	}
	if !found {
		fail(r, "controller/missing", "controller app "+app,
			fmt.Sprintf("no Dokku app named %s exists: never deployed or destroyed", app),
			fmt.Sprintf("`topctl recover redeploy %s` redeploys from the last-known-good revision (store checkpoint) or the pinned bootstrap image", app))
		return false
	}

	status, okStatus := psReportState(r, ctx, opts, app)
	unhealthy := okStatus && (!status.deployed || !status.running)
	logLines, logOK := controllerLogs(r, ctx, opts, app, unhealthy)
	failedDeploys(r, ctx, opts, app)

	switch {
	case okStatus && status.deployed && status.running:
		ok(r, "controller/state", "controller running state",
			fmt.Sprintf("%s is deployed and running (ps:report)", app))
		return true
	case okStatus && !status.deployed:
		fail(r, "controller/state", "controller running state",
			fmt.Sprintf("%s has no current deployment (deployed=false, running=%t)", app, status.running),
			fmt.Sprintf("`topctl recover redeploy %s` (default: last-known-good from the store, else pinned bootstrap image)", app))
		suggestLogTail(r, app, logLines, logOK)
		return true
	case okStatus && !status.running:
		fail(r, "controller/state", "controller running state",
			fmt.Sprintf("%s is deployed but not running (crashed or stopped)", app),
			fmt.Sprintf("`topctl recover restart` — restarts %s and waits bounded (contract cap 3m) for ps:reported health", app))
		suggestLogTail(r, app, logLines, logOK)
		return true
	default:
		// ps:report failed: state unknown; the failure was already warned
		// by psReportState. Degrade honestly instead of guessing.
		warn(r, "controller/state", "controller running state",
			fmt.Sprintf("could not determine %s's run state (ps:report unavailable)", app),
			fmt.Sprintf("run `dokku ps:report %s --format json` by hand; do not restart blind — check the failure cause first", app))
		return true
	}
}

// suggestLogTail attaches the last controller log line to the report as
// the contract example does ("last log lines suggest: …"), only when
// something is already failing (a healthy run stays clean).
func suggestLogTail(r *Report, app string, lines []string, logOK bool) {
	if !logOK || len(lines) == 0 {
		return
	}
	last := lines[len(lines)-1]
	info(r, "controller/log-tail", "controller recent logs (last 200)",
		fmt.Sprintf("last log line: %s", last),
		fmt.Sprintf("visible above; rerun diagnose after the fix to confirm the tail stops implicating the same cause. Full: `dokku logs %s --num 200`", app))
}

// psState is the minimal ps:report state recover needs.
type psState struct {
	deployed bool
	running  bool
}

// psReportState reads `ps:report --format json` deployed/running. The
// read-only flags are prefix-less on Dokku 0.38.x (internal/backend/
// dokku's verified parsing notes); legacy aliases accepted defensively.
// Parsing is local — the recovery path deliberately does not import the
// full backend stack (ADR-0005's minimal-dependency recovery).
func psReportState(r *Report, ctx context.Context, opts Options, app string) (psState, bool) {
	st := psState{}
	out, _, err := opts.Runner.Run(ctx, "dokku", "ps:report", app, "--format", "json")
	if err != nil {
		warn(r, "controller/report", "controller ps:report",
			fmt.Sprintf("`dokku ps:report %s --format json` failed: %s", app, excerpt(err.Error())),
			fmt.Sprintf("run `dokku ps:report %s --format json` by hand; the run-state verdict is degraded to unknown (never guessed)", app))
		return st, false
	}
	m, jerr := parseReportObject(out)
	if jerr != nil {
		warn(r, "controller/report", "controller ps:report",
			fmt.Sprintf("ps:report output was not parseable JSON: %s", excerpt(jerr.Error())),
			fmt.Sprintf("run `dokku ps:report %s --format json` by hand; the run-state verdict is degraded to unknown", app))
		return st, false
	}
	st.deployed = reportBool(m, "deployed", "ps-deployed", "deployed-state")
	st.running = reportBool(m, "running", "ps-running", "running-state")
	return st, true
}

// controllerLogs pulls the controller's last 200 lines as outage
// evidence (backup-recovery contract's logs --num 200 probe). Returns
// the parsed lines and whether the probe itself succeeded. `unhealthy`
// gates whether the lines travel onward (a healthy app keeps the report
// clean; the contract's diagnose example only surfaces log tails to
// explain the failure at hand).
func controllerLogs(r *Report, ctx context.Context, opts Options, app string, unhealthy bool) ([]string, bool) {
	out, _, err := opts.Runner.Run(ctx, "dokku", "logs", app, "--num", "200")
	if err != nil {
		warn(r, "controller/logs", "controller recent logs",
			fmt.Sprintf("`dokku logs %s --num 200` failed: %s", app, excerpt(err.Error())),
			fmt.Sprintf("run `dokku logs %s --num 200` by hand; log evidence is unavailable", app))
		return nil, false
	}
	lines := nonEmptyLines(string(out))
	if len(lines) == 0 {
		info(r, "controller/logs", "controller recent logs (last 200)",
			fmt.Sprintf("`dokku logs %s --num 200` returned no lines", app),
			fmt.Sprintf("an app that has never logged is consistent with never-started/restart-clean; verify via `dokku ps:report %s --format json`", app))
		return nil, true
	}
	if !unhealthy {
		return nil, true
	}
	return lines, true
}

// failedDeploys reads `logs:failed` (Dokku logs plugin's failed-deploy
// viewer); non-empty output → warn with the newest line as evidence.
func failedDeploys(r *Report, ctx context.Context, opts Options, app string) {
	out, _, err := opts.Runner.Run(ctx, "dokku", "logs:failed", app)
	if err != nil {
		info(r, "controller/failed-deploys", "failed deploy log",
			fmt.Sprintf("`dokku logs:failed %s` did not run cleanly (it is a Dokku logs-plugin feature; older hosts or never-failed deploys raise a benign error): %s", app, excerpt(err.Error())),
			fmt.Sprintf("if a deploy failed before the outage, run `dokku logs:failed %s` by hand", app))
		return
	}
	lines := nonEmptyLines(string(out))
	if len(lines) == 0 {
		ok(r, "controller/failed-deploys", "failed deploy log",
			fmt.Sprintf("`dokku logs:failed %s` is empty: no failed deploy recorded", app))
		return
	}
	warn(r, "controller/failed-deploys", "failed deploy log",
		fmt.Sprintf("`dokku logs:failed %s` has content; newest line: %s", app, lines[len(lines)-1]),
		fmt.Sprintf("fix the underlying cause (disk, image pull — see other findings), then `dokku ps:rebuild %s`; full log: `dokku logs:failed %s`", app, app))
}

// checkStore opens <dataDir>/top.db via the store and reports
// existence, schema version, integrity, and staleness.
func checkStore(r *Report, ctx context.Context, opts Options, controllerPresent bool) {
	path := dbPath(opts)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			if !controllerPresent {
				info(r, "store/missing", "state store "+path,
					path+" does not exist; with no controller app and no config produced above, this host looks like a fresh install (nothing to recover from)",
					"if you expected an installed TOP here: the state volume is detached or wiped — check backups (`topctl backup list`) and the restore runbook (docs/operations/recovery.md)")
				return
			}
			fail(r, "store/missing", "state store "+path,
				path+" is missing while the controller app exists: the state volume is detached or wiped",
				"redeploy the controller from the pinned image (`topctl recover redeploy`) to a healthy process, verify the volume mount (`dokku storage:list "+controllerApp()+"`), then restore metadata (M5 restore; meanwhile docs/operations/recovery.md)")
			return
		}
		warn(r, "store/missing", "state store "+path,
			"could not stat "+path+": "+excerpt(err.Error()),
			"check dataDir permissions — the recovery path must at least READ the state file (read-only for diagnose)")
		return
	}

	st, err := sqlite.Open(ctx, path)
	if err != nil {
		var sn store.SchemaNewerError
		switch {
		case errors.As(err, &sn):
			fail(r, "store/schema-newer", "state store openable",
				fmt.Sprintf("state store at %s is schema v%d; this build knows up to v%d — opening read-only-forward would be a silent DOWNGRADE (downgrade protection fired correctly)", path, sn.Current, sn.Known),
				string(sn.Error())+"; downgrade is never automatic — recover with the newer build or an older backup")
		default:
			fail(r, "store/open", "state store openable",
				fmt.Sprintf("could not open %s: %s", path, excerpt(err.Error())),
				"corruption/lock/permission — the docs/operations/recovery.md runbook decides; a full disk or a live controller hold surfaces here")
		}
		return
	}
	defer func() { _ = st.Close() }()

	schemaVersion, vErr := st.SchemaVersion(ctx)
	varSchema := "unknown"
	if vErr == nil {
		varSchema = fmt.Sprintf("v%d", schemaVersion)
	}

	if err := st.Check(ctx); err != nil {
		var corrupt domain.CorruptError
		if errors.As(err, &corrupt) {
			fail(r, "store/integrity", "state store integrity ("+varSchema+")",
				fmt.Sprintf("%s: PRAGMA integrity_check failed: %s", path, excerpt(corrupt.Error())),
				"restore from the newest verified backup (`topctl backup list`; `topctl recover restore` lands in M5 — the safeguards come first, ADR-0007); record which integrity failure occurred")
			return
		}
		fail(r, "store/integrity", "state store integrity ("+varSchema+")",
			fmt.Sprintf("integrity_check could not run: %s", excerpt(err.Error())),
			"permissions or disk failure — fix the named cause and re-run diagnose")
		return
	}

	age, hasOp := newestOpAge(ctx, st, opts.Now())
	if !hasOp {
		ok(r, "store/integrity", "state store integrity ("+varSchema+")",
			fmt.Sprintf("%s integrity ok (no operations recorded yet — fresh store)", path))
		return
	}
	msg := fmt.Sprintf("%s integrity ok (newest op %s old)", path, formatDuration(age))
	if age >= staleAfter {
		warn(r, "store/integrity", "state store integrity ("+varSchema+")",
			msg+" — BUT stale: no reconcile has recorded since",
			fmt.Sprintf("an outage-sized gap. expected while the controller is down; if it stays stale after restart/redeploy, that is the next thread to pull (`topctl recover diagnose` again, then `dokku ps:report %s`)", controllerApp()))
		return
	}
	ok(r, "store/integrity", "state store integrity ("+varSchema+")", msg)
}

// staleAfter is the staleness threshold for the newest recorded
// operation (contract example: warn stale even when integrity passes).
// Reconcile runs at least hourly (config default), so a >6h gap means
// no reconcile has recorded since — an outage-sized window.
const staleAfter = 6 * time.Hour

// newestOpAge returns the newest recorded operation's completion age
// (FinishedAt, else StartedAt); false when none exists or no timestamp
// was ever set.
func newestOpAge(ctx context.Context, st store.Store, now time.Time) (time.Duration, bool) {
	ops, err := st.ListOperations(ctx, 1, nil)
	if err != nil || len(ops) == 0 {
		return 0, false
	}
	op := ops[0]
	at := op.FinishedAt
	if at.IsZero() {
		at = op.StartedAt
	}
	if at.IsZero() {
		return 0, false
	}
	age := now.Sub(at)
	if age < 0 {
		return 0, true
	}
	return age, true
}

// formatDuration renders an age compactly and deterministically
// ("41h", "5m") — the contract's prose style, golden-test friendly.
func formatDuration(d time.Duration) string {
	return d.Truncate(time.Minute).String()
}

// checkConfigFile validates TOP's config when present.
func checkConfigFile(r *Report, opts Options) {
	path := opts.ConfigFile
	if _, err := os.Stat(path); os.IsNotExist(err) {
		info(r, "config/missing", "config file "+path,
			path+" not found: fresh install assumed (defaults from internal/config; TOP_* env still apply)",
			"no action needed — an install phase writes the real config; do not hand-create one during recovery")
		return
	}
	cfg, err := config.Load(path)
	if err != nil {
		fail(r, "config/invalid", "config "+path,
			path+" does not load/validate: "+excerpt(err.Error()),
			fmt.Sprintf("fix %s by hand (strict YAML: unknown apiVersion or unknown field is a hard error — internal/config); until fixed, recovery runs on defaults + TOP_* env", path))
		return
	}
	ok(r, "config/valid", "config "+path,
		fmt.Sprintf("%s parses and validates strictly (top/v1alpha1; dataDir %s, git url set: %t)", path, cfg.DataDir, cfg.Git.URL != ""))
}

// checkPermissions inspects the dataDir tree's permission facts
// (ADR-0008): dataDir 0700; secrets/ and git/ 0700 with 0600 files.
// Read-only: the fix (chmod) is suggested for the operator, never
// applied here.
func checkPermissions(r *Report, opts Options) {
	checkDirPerms(r, opts.DataDir, "top", "dataDir")
	for _, sub := range []string{"secrets", "git"} {
		dir := opts.DataDir + "/" + sub
		checkDirPerms(r, dir, sub, sub+" directory")
		checkLooseFiles(r, dir, sub, sub)
	}
}

func checkDirPerms(r *Report, dir, idSuffix, label string) {
	st, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		info(r, "perms/"+idSuffix, label+" "+dir,
			dir+" does not exist (fresh install, not yet provisioned by the installer)",
			"the installer phases create it at 0700; nothing to fix during a genuinely fresh recovery (an operator may pass here before install)")
	case err != nil:
		warn(r, "perms/"+idSuffix, label+" "+dir,
			"could not stat "+dir+": "+excerpt(err.Error()),
			"check mount/ownership; diagnose only ever reads this tree")
	case !st.IsDir():
		warn(r, "perms/"+idSuffix, label+" "+dir,
			dir+" exists but is not a directory",
			"move the offending path aside so the installer can provision "+dir+" (0700)")
	case st.Mode().Perm()&^0o700 != 0:
		warn(r, "perms/"+idSuffix, label+" "+dir,
			fmt.Sprintf("%s has mode %o; the ADR-0008 contract is 0700", dir, st.Mode().Perm()),
			fmt.Sprintf("`chmod 700 %s` (diagnose suggests; it never chmods)", dir))
	default:
		ok(r, "perms/"+idSuffix, label+" "+dir,
			fmt.Sprintf("%s has mode %o (0700, ADR-0008-safe)", dir, st.Mode().Perm()))
	}
}

// checkLooseFiles flags any file directly inside dir with a mode looser
// than 0600 (secret dalliance under secrets/; deploy keys under git/ —
// ADR-0008). Names only; the file VALUE is never read.
func checkLooseFiles(r *Report, dir, idSuffix, label string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // dir readability already surfaced by checkDirPerms
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.Mode().Perm()&^0o600 == 0 {
			continue
		}
		warn(r, "perms/"+idSuffix+"-file", label+" file "+dir+"/"+e.Name(),
			fmt.Sprintf("%s/%s has mode %o; the ADR-0008 contract is 0600", dir, e.Name(), fi.Mode().Perm()),
			fmt.Sprintf("`chmod 600 %s/%s`; consider rotating the credential if its exposure window is unknown (names only are shown — the value is never read here)", dir, e.Name()))
	}
}

// checkDisk applies internal/doctor's disk thresholds to the recovery
// question ("can SQLite and a container even write here now?").
// ≥ 20 GiB ok, ≥ 2 GiB warn, < 2 GiB fail — same thresholds as the
// preflight so one page of docs covers both tools. The probe type is
// the doctor's DiskProbe (reused seam, no duplicated statfs); when nil
// the local systemDiskProbe (probe.go) stands in.
func checkDisk(r *Report, opts Options) {
	anchor := anchorDir(opts.DataDir)
	free, err := opts.DiskProbe(anchor)
	if err != nil {
		warn(r, "disk/headroom", "disk headroom",
			"could not stat filesystem free space at "+anchor+": "+excerpt(err.Error()),
			fmt.Sprintf("run `df -h %s` by hand", anchor))
		return
	}
	gib := free >> 30
	switch {
	case gib >= 20:
		ok(r, "disk/headroom", "disk headroom",
			fmt.Sprintf("%d GiB free on %s (≥ 20 GiB recommended)", gib, anchor))
	case gib >= 2:
		warn(r, "disk/headroom", "disk headroom",
			fmt.Sprintf("%d GiB free on %s; below the 20 GiB recommended headroom", gib, anchor),
			"free space — a full disk also wedges SQLite (disk I/O errors) and the controller container; docs/operations/recovery.md lists safe-to-delete targets")
	default:
		fail(r, "disk/headroom", "disk headroom",
			fmt.Sprintf("only %d GiB free on %s: a near-full disk is the most common cause of both a dead controller and a failing SQLite", gib, anchor),
			"free disk space (old Dokku images/logs; NOT TOP's registered backups), then `topctl recover restart` and re-run diagnose to watch integrity recover")
	}
}

// ---- tiny shared helpers ---------------------------------------------------

// anchorDir walks dataDir (or its nearest existing ancestor) so statfs
// answers for the filesystem that actually holds TOP's state, without
// creating anything (same discipline as internal/doctor's helper; kept
// local — doctor's is unexported and one for-line loop isn't worth an
// API request across packages).
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

// dokkuVersionFrom pulls a numeric version token out of "dokku version
// 0.38.13 …" (same tolerance as doctor's usage; local to keep recover
// independent of doctor's internals). "" = unrecognized output.
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

// engineAtLeast compares "maj.min[.patch]" actual against "maj.min"
// floor (doctor's algorithm, local copy kept deliberately tiny for the
// same independence reason).
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

// splitApps converts `dokku apps:list` output into app names, handling
// plain lines and "=====> app" banners (same tolerance as doctor).
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

// nonEmptyLines splits command output into trimmed, non-empty lines.
func nonEmptyLines(out string) []string {
	var lines []string
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(strings.TrimSpace(ln), "\r")
		if ln != "" {
			lines = append(lines, ln)
		}
	}
	return lines
}

// parseReportObject decodes psycopg-style plugin report stdout (single
// JSON object, or an array selecting its sole object — same shape rules
// as internal/backend/dokku/report.go, parsed locally for independence).
func parseReportObject(out []byte) (map[string]any, error) {
	var obj map[string]any
	var arr []map[string]any
	if err := json.Unmarshal(out, &arr); err == nil {
		switch len(arr) {
		case 0:
			return map[string]any{}, nil
		case 1:
			return arr[0], nil
		default:
			return nil, fmt.Errorf("report: unexpected %d objects", len(arr))
		}
	}
	if err := json.Unmarshal(out, &obj); err == nil {
		return obj, nil
	}
	return nil, errors.New("report: not JSON")
}

// reportBool reads a boolean report value that Dokku may emit as a real
// JSON bool or as "true"/"false"/"1"/"0" strings.
func reportBool(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case bool:
			return t
		case string:
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "true", "1", "yes":
				return true
			case "false", "0", "no":
				return false
			}
		}
	}
	return false
}

// excerpt keeps error text short for the one-line message contract
// (doctor's discipline; recover prints single-line messages).
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

// ---- free finding constructors (doctor's are methods on its local
// Report; the alias report needs free functions) ----------------------------

func ok(r *Report, id, check, msg string) { add(r, id, sevOK, check, msg, "") }

func info(r *Report, id, check, msg, hint string) {
	add(r, id, sevInfo, check, msg, hint)
}

func warn(r *Report, id, check, msg, hint string) {
	add(r, id, sevWarn, check, msg, hint)
}

func fail(r *Report, id, check, msg, hint string) {
	add(r, id, sevFail, check, msg, hint)
}

// add appends a finding; every non-ok severity must carry a hint
// (doctor's invariant).
func add(r *Report, id string, sev Severity, check, msg, hint string) {
	r.Findings = append(r.Findings, Finding{
		ID: id, Severity: sev, Check: check, Message: msg, Hint: hint,
	})
}
