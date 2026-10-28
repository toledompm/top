// SPDX-License-Identifier: MIT

package dokku

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/toledompm/top/internal/backend"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
)

// Apply implements backend.PlatformBackend (T-305 image services, T-306
// build services, T-307 healthchecks). Structure:
//
//  1. resolve + sanity (exactly one of image/build — spec validation)
//  2. EnsureApp (create/adopt) + Inspect (the delta baseline)
//  3. converge non-deploy facets, each a no-op when already equal:
//     env (batched config:set --no-restart + config:unset), restart
//     policy (ps:set), storage entries+mounts (+unmount of removed),
//     network attach/detach, ports (proxy set / non-proxy clear),
//     domains (proxy set only; Inspect has no domains read this wave
//     — set unconditionally when proxy, skip otherwise), resource
//     limits (resource:limit / resource:limit-clear), checks timings
//     (checks:set wait).
//  4. deploy the source revision — SKIPPED entirely when the host
//     already carries it (image ref equal; or build sha equal), which
//     guarantees the Tier-3 zero-mutation idempotency case. A build
//     with healthcheck probes NEVER skips on sha equality: the
//     checks must ship via the synthesized app.json archive, whose no-
//     change early-exit (verified: same archive → exit 0, no build
//     record) gives the same idempotency.
//
// Ordering notes (all verified against dokku.com 0.38.x):
//   - config/set/storage/network/policy/res Limits are set BEFORE the
//     deploy so the deployed containers start with them attached.
//   - restart-policy changes are picked up at deploy time (docs: "a
//     change in the restart policy must be followed by a ps:rebuild
//     call" — the deploy below is that rebuild).
//   - failed deploy ⇒ *BackendError; dokku restores the deploy branch +
//     source-image, so the old release keeps serving (ADR-0002 tier-3
//     AC).
func (c *Client) Apply(ctx context.Context, ref domain.ServiceRef, spec domain.Service, secrets map[string]string, opts backend.ApplyOptions) error {
	_, app, err := resolve(ref)
	if err != nil {
		return err
	}
	if err := spec.Source.IsValid(); err != nil {
		return wrapBackend("apply", app, fmt.Errorf("%w: source: %v", domain.ErrInvalid, err))
	}
	if spec.RestartPolicy == "" {
		spec.RestartPolicy = domain.RestartAlways
	}

	if err := c.EnsureApp(ctx, ref); err != nil {
		return wrapBackend("apply ensure app", app, err)
	}
	obs, err := c.Inspect(ctx, ref)
	if err != nil {
		return wrapBackend("apply inspect", app, err)
	}

	// Archive-with-checks services deploy via the synthesized archive
	// (deployArchive). The archive is synthesized once here so the SAME
	// bytes drive both the fingerprint env key (applyEnv) and the deploy
	// stream — convergence of check drift rides the fingerprint, and a
	// converged app skips the re-send entirely (dokku records a FAILED
	// build for an identical-archive poke, tier-3 2026-10-28).
	var archive []byte
	var archiveSHA string
	if spec.Source.Build == nil && spec.Source.Image != "" && probesPresent(spec.HealthCheck) {
		val, aErr := buildChecksArchive(spec.Source.Reference(), spec)
		if aErr != nil {
			return wrapBackend("apply", app, fmt.Errorf("%w: synthesize app.json archive: %v", domain.ErrInvalid, aErr))
		}
		sum := sha256.Sum256(val)
		archive = val
		archiveSHA = hex.EncodeToString(sum[:])
	}

	// --- env: one batched config:set --no-restart (never per-var restarts)
	if err := c.applyEnv(ctx, app, spec, secrets, obs); err != nil {
		return err
	}
	// --- restart policy (set before the deploy so the new containers are
	// created with it; docs: needs a rebuild — the deploy is one)
	if obs.RestartPolicy != spec.RestartPolicy {
		if _, err := c.run(ctx, "apply restart-policy", app, mutateTimeout,
			"ps:set", app, "restart-policy", string(spec.RestartPolicy)); err != nil {
			return wrapBackend("apply restart-policy", app, err)
		}
	}
	// --- storage: entries + mounts converge; removed mounts unmount
	if err := c.applyStorage(ctx, app, spec.Storage, obs.StorageMounts); err != nil {
		return err
	}
	// --- network: internal mode attaches the shared network (post-deploy
	// hook so scrapes only start after checks pass, ADR-0002)
	if err := c.applyNetwork(ctx, app, spec.Routing.Mode, obs.Networks); err != nil {
		return err
	}
	// --- ports: proxy mode DECLARES mappings; none/internal clear any
	if err := c.applyPorts(ctx, app, spec, obs.Ports); err != nil {
		return err
	}
	// --- domains: proxy declarations; unverified clear path skipped
	// (documented deviation: Inspect cannot read domains this wave)
	if spec.Routing.Mode == domain.RoutingProxy && len(spec.Routing.Domains) > 0 {
		if _, err := c.run(ctx, "apply domains", app, mutateTimeout,
			append([]string{"domains:set", app}, spec.Routing.Domains...)...); err != nil {
			return wrapBackend("apply domains", app, err)
		}
	}
	// --- resource limits
	if err := c.applyResources(ctx, app, spec.Resources.Limits, obs.Resources.Limits); err != nil {
		return err
	}
	// --- checks timings (wait); the check definitions themselves ship in
	// the deployed app.json (synthesized archive for image services)
	if err := c.applyCheckTimings(ctx, app, spec.HealthCheck); err != nil {
		return err
	}

	return c.deploy(ctx, app, spec, obs, opts, archive, archiveSHA)
}

// archiveFingerprintKey is the adapter-owned env key carrying the sha256
// of the last successfully converged synthesized app.json archive (see
// applyEnv). TOP_ prefix is the platform namespace (AGENTS.md naming); it
// is a config KEY by design — the value is only ever compared against the
// spec-derived fingerprint, never parsed as user config.
const archiveFingerprintKey = "TOP_ARCHIVE_SHA256"

// managedEnvKeys are dokku-managed app env keys: dokku injects but owns
// them, and they come back after every rebuild regardless of what the
// spec says, so they must never count as drift (verified 0.38.32
// tier-3-2026-10-28: GIT_REV appears in config:keys after ANY deploy and
// unsetting it would permanently mutate a converged app).
var managedEnvKeys = map[string]bool{"GIT_REV": true}

// applyEnv converges env with at most ONE config:set --no-restart and one
// config:unset. Value drift is deliberately NOT compared: the adapter
// never reads config values (ADR-0008), so the delta is key-set based —
// plain key sets and secret key sets (values merged into the same batched
// set; argv redaction is the exec layer's job). Removed keys are unset in
// a second idempotent call.
//
// applyEnv converges env with at most ONE config:set --no-restart and one
// config:unset. Value drift is deliberately NOT compared: the adapter
// never reads config values (ADR-0008), so the delta is key-set based —
// plain key sets and secret key sets (values merged into the same batched
// set; argv redaction is the exec layer's job). Removed keys are unset in
// a second idempotent call.
//
// The adapter-owned TOP_ARCHIVE_SHA256 fingerprint of the synthesized
// app.json archive (setArchiveFingerprint) is deliberately NOT written
// here: it must only land once the deploy itself has succeeded — a
// pre-deploy write would leave the NEW spec's sha in config after a
// FAILED deploy, and the next apply would read the match and wrongly
// skip convergence of an app still serving the OLD archive (tier-3
// 2026-10-28, TestE2EApplyFailingCheckPreservesOldRelease). minting the
// fingerprint after deploy success is the ONLY way Readiness can later
// distinguish converged from converged-with-check-drift, since dokku
// records a FAILED build for an identical-archive re-send and never
// exposes the stored app.json through the CLI.
func (c *Client) applyEnv(ctx context.Context, app string, spec domain.Service, secrets map[string]string, obs domain.ObservedState) error {
	env := maps.Clone(spec.Env)
	if env == nil {
		env = map[string]string{}
	}
	for _, name := range spec.SecretRefs {
		env[name] = secrets[name] // missing resolution is spec validation's job upstream
	}
	if len(env) == 0 && len(obs.EnvKeys) == 0 {
		return nil
	}
	desired := slices.Sorted(maps.Keys(env))
	existing := slices.Clone(obs.EnvKeys)
	slices.Sort(existing)

	var setArgs []string
	for _, k := range desired {
		if managedEnvKeys[k] {
			continue // dokku-managed; never set by the spec path
		}
		if !slices.Contains(existing, k) {
			setArgs = append(setArgs, k+"="+env[k])
		}
	}
	if len(setArgs) > 0 {
		if _, err := c.run(ctx, "apply env set", app, mutateTimeout,
			append([]string{"config:set", "--no-restart", app}, setArgs...)...); err != nil {
			return wrapBackend("apply env set", app, err)
		}
	}
	var unsetArgs []string
	for _, k := range existing {
		if !slices.Contains(desired, k) && !managedEnvKeys[k] && k != archiveFingerprintKey {
			unsetArgs = append(unsetArgs, k)
		}
	}
	if len(unsetArgs) > 0 {
		if _, err := c.run(ctx, "apply env unset", app, mutateTimeout,
			append([]string{"config:unset", app}, unsetArgs...)...); err != nil {
			return wrapBackend("apply env unset", app, err)
		}
	}
	return nil
}

// applyStorage converges mounts: create missing entries (docker volumes —
// no host path — except when the caller owns the dir, which does not
// happen on this path: the reconciler mounts are spec-local entries),
// mount missing mounts, unmount mounts the spec dropped.
func (c *Client) applyStorage(ctx context.Context, app string, desired []domain.Storage, mounted []domain.StorageMount) error {
	for _, s := range desired {
		entry, path := s.Name, s.MountPath
		if !validStorageName(entry) {
			return wrapBackend("apply storage", app, fmt.Errorf("%w: storage entry name %q must be a DNS-1123 label", domain.ErrInvalid, entry))
		}
		if err := c.EnsureStorageEntry(ctx, entry, defaultHostDir(entry)); err != nil {
			return wrapBackend("apply storage", app, err)
		}
		if !containsMount(mounted, entry, path) {
			if _, err := c.run(ctx, "apply storage mount", app, mutateTimeout,
				"storage:mount", app, entry, "--container-dir", path); err != nil {
				return wrapBackend("apply storage mount", app, err)
			}
		}
	}
	for _, m := range mounted {
		if !containsDesired(desired, m.Name, m.MountPath) {
			// not-idempotent-strict: a missing mount unmounts fine; exit-1
			// races are tolerated like DetachStorage does (teardown shows
			// the "no-op rather than an error" convention).
			if _, err := c.run(ctx, "apply storage unmount", app, mutateTimeout,
				"storage:unmount", app, m.Name, "--container-dir", m.MountPath); err != nil && !exec.IsExitCode(err, 1) {
				return wrapBackend("apply storage unmount", app, err)
			}
		}
	}
	return nil
}

func containsMount(mounts []domain.StorageMount, entry, path string) bool {
	for _, m := range mounts {
		if m.Name == entry && m.MountPath == path {
			return true
		}
	}
	return false
}

func containsDesired(desired []domain.Storage, entry, path string) bool {
	for _, s := range desired {
		if s.Name == entry && s.MountPath == path {
			return true
		}
	}
	return false
}

// defaultHostDir is dokku's own storage data dir for volume-style entries
// created without an explicit path (persistent-storage docs: creating
// without <path> names a docker volume / default dir under
// /var/lib/dokku/data/storage). The reconciler always mounts spec-local
// entries; host-managed dirs ride the installer tier instead.
func defaultHostDir(entry string) string { return "/var/lib/dokku/data/storage/" + entry }

// applyNetwork attaches (mode internal) or detaches (other modes) the
// shared internal network. `detach` argv per network-management docs.
func (c *Client) applyNetwork(ctx context.Context, app string, mode domain.RoutingMode, attached []string) error {
	net := c.InternalNetwork
	if net == "" {
		net = DefaultInternalNetwork
	}
	has := slices.Contains(attached, net)
	switch {
	case mode == domain.RoutingInternal && !has:
		if _, err := c.run(ctx, "apply network attach", app, mutateTimeout,
			"network:set", app, "attach-post-deploy", net); err != nil {
			return wrapBackend("apply network attach", app, err)
		}
	case mode != domain.RoutingInternal && has:
		if _, err := c.run(ctx, "apply network detach", app, mutateTimeout,
			"network:set", app, "detach", net); err != nil && !exec.IsExitCode(err, 1) {
			return wrapBackend("apply network detach", app, err)
		}
	}
	return nil
}

// applyPorts converges host exposure: routing proxy (or an explicit
// HostPort) GETS `ports:set <app> http:<host>:<container>` entries with
// host == container unless HostPort is declared; none/internal GET
// ports:clear when any mapping exists (verified: ports:clear exists for
// the platform-managed map).
func (c *Client) applyPorts(ctx context.Context, app string, spec domain.Service, obs []domain.Port) error {
	expose := spec.Routing.Mode == domain.RoutingProxy
	var desired []string
	if expose {
		seen := map[int]bool{}
		for _, p := range spec.Ports {
			if p.ContainerPort == 0 || seen[p.ContainerPort] {
				continue
			}
			seen[p.ContainerPort] = true
			host := p.ContainerPort
			if p.HostPort != 0 {
				host = p.HostPort
			}
			desired = append(desired, fmt.Sprintf("http:%d:%d", host, p.ContainerPort))
		}
	}
	if expose {
		if !portsEqual(desired, obs) {
			if _, err := c.run(ctx, "apply ports set", app, mutateTimeout,
				append([]string{"ports:set", app}, desired...)...); err != nil {
				return wrapBackend("apply ports set", app, err)
			}
		}
		return nil
	}
	if len(obs) > 0 {
		if _, err := c.run(ctx, "apply ports clear", app, mutateTimeout,
			"ports:clear", app); err != nil {
			return wrapBackend("apply ports clear", app, err)
		}
	}
	return nil
}

func portsEqual(desired []string, obs []domain.Port) bool {
	if len(desired) != len(obs) {
		return false
	}
	got := make([]string, 0, len(obs))
	for _, p := range obs {
		got = append(got, fmt.Sprintf("http:%d:%d", p.HostPort, p.ContainerPort))
	}
	slices.Sort(desired)
	slices.Sort(got)
	return slices.Equal(desired, got)
}

// applyResources converges per-proctype (web) limits. Inspect maps only
// the web row this wave (resource:report keys web.limit.*) — compare those
// SEMANTICALLY (quantity-translation, quantities.go): the spec's K8s-style
// spellings ("500m", "256Mi") differ from what dokku stores after a
// docker-native write ("0.5", "268435456") while meaning the same thing.
func (c *Client) applyResources(ctx context.Context, app string, want domain.ResourceLimits, got domain.ResourceLimits) error {
	if cpuSame(want.CPU, got.CPU) && memSame(want.Memory, got.Memory) {
		return nil
	}
	if want.CPU == "" && want.Memory == "" {
		if _, err := c.run(ctx, "apply resources clear", app, mutateTimeout,
			"resource:limit-clear", "--process-type", "web", app); err != nil && !exec.IsExitCode(err, 1) {
			return wrapBackend("apply resources clear", app, err)
		}
		return nil
	}
	if _, err := c.run(ctx, "apply resources set", app, mutateTimeout, resourceLimitArgs(app, want)...); err != nil {
		return wrapBackend("apply resources set", app, err)
	}
	return nil
}

// resourceLimitArgs composes `resource:limit [--cpu C] [--memory M]
// --process-type web <app>` (resource-management docs). Values are
// TRANSLATED spec quantities: K8s-style cores/memory are converted to
// docker-native forms ("500m"→"0.5", "256Mi"→"268435456") because dokku
// forwards the flags verbatim to `docker container create`, which rejects
// K8s spellings (tier-3 verified 2026-10-28, invalid suffix 'mi'). See
// quantities.go; unparseable spec values pass through verbatim.
func resourceLimitArgs(app string, lim domain.ResourceLimits) []string {
	args := []string{"resource:limit"}
	if lim.CPU != "" {
		if cores, ok := parseCPUCores(lim.CPU); ok {
			args = append(args, "--cpu", formatCPUCores(cores))
		} else {
			args = append(args, "--cpu", lim.CPU)
		}
	}
	if lim.Memory != "" {
		if mbytes, ok := parseMemoryBytes(lim.Memory); ok {
			args = append(args, "--memory", formatMemoryBytes(mbytes))
		} else {
			args = append(args, "--memory", lim.Memory)
		}
	}
	return append(args, "--process-type", "web", app)
}

// applyCheckTimings is VACATED on dokku 0.38 (tier-3 verified 2026-10-28):
// `checks:set <app> <key> <value>` accepts only `wait-to-retire` on this
// release — wait/timeout/attempts are app.json FIELDS in the healthcheck
// entries, not plugin keys (the zero-downtime docs' "available as
// properties" clause is not honored by the plugin's key validation on
// 0.38; cmd: `!  Invalid key specified, valid keys include: wait-to-retire`).
// The spec's IntervalSeconds therefore rides the synthesized app.json
// `wait` field, and a build service's timings live in its repo's own
// app.json. Remove once dokku's checks:set accepts check keys again.
func (c *Client) applyCheckTimings(context.Context, string, *domain.HealthCheck) error {
	return nil
}

// deploy sends the source revision to dokku. Skip rules:
//   - image, digest pinned: already equal ⇒ skip (Nothing-to-do; the
//     documented same-args early exit would ALSO be a no-op — skipping
//     avoids the gratuitous 15-min class call).
//   - image, mutable tag: NEVER skip on equality alone; `--force` is
//     passed only when the reconciler says movement was observed
//     (ApplyOptions.ForcePull); without force the same-args early exit
//     protects the host from a pointless redeploy.
//   - build: sha already deployed AND no healthcheck to ensure (checks
//     for build sources ride the repo's own app.json; the sha implies
//     it) ⇒ skip, guaranteeing "no-change poll leaves NO build record"
//     hardened in the adapter.
func (c *Client) deploy(ctx context.Context, app string, spec domain.Service, obs domain.ObservedState, opts backend.ApplyOptions, archive []byte, archiveSHA string) error {
	if spec.Source.Build != nil {
		b := *spec.Source.Build
		if b.Repo == "" {
			return wrapBackend("deploy build", app, fmt.Errorf("%w: build source: empty repo", domain.ErrInvalid))
		}
		if err := domain.ValidRevision(b.Ref); err != nil {
			return wrapBackend("deploy build", app, fmt.Errorf("%w: build source ref: %v", domain.ErrInvalid, err))
		}
		rev := b.Ref
		if obs.Revision == rev {
			if obs.Running {
				return nil // zero-mutation: already at rev and serving
			}
			// Same sha recorded but the release is not serving (failed
			// prior build/deploy): rebuild dokku's stored source —
			// re-running git:sync at the same sha is a no-op that exits 1
			// (dokku 0.38.32, tier-3 2026-10-28).
			if _, rerr := c.run(ctx, "deploy build rebuild", app, deployTimeout, "ps:rebuild", app); rerr != nil {
				return wrapBackend("deploy build rebuild", app, rerr)
			}
			return nil
		}
		args := []string{"git:sync", "--build-if-changes", app, b.Repo, rev}
		_, derr := c.run(ctx, "deploy build", app, deployTimeout, args...)
		return wrapMaybe(derr, "deploy build", app)
	}

	imageRef := spec.Source.Reference()
	if imageRef == "" {
		return wrapBackend("deploy image", app, fmt.Errorf("%w: empty image ref", domain.ErrInvalid))
	}
	force := !spec.Source.IsDigestPinned() && opts.ForcePull
	if probesPresent(spec.HealthCheck) {
		// Custom checks need app.json inside the deploy archive — the
		// contract's synthesized-archive path: Dockerfile FROM + app.json
		// (use-case-tar via stdin, `git:from-archive <app> --`). The same
		// bytes were already fingerprinted into env (TOP_ARCHIVE_SHA256).
		return c.deployArchive(ctx, app, imageRef, spec, obs, archive, archiveSHA)
	}
	// Zero-mutation skip REQUIRES the release to actually be serving: a
	// match with Running=false (failed prior deploy, retired container)
	// must re-deploy — dokku re-deploys an identical image cleanly
	// (exit 0, container started; tier-3 2026-10-28).
	if !force && obs.Image == imageRef && obs.Running {
		return nil
	}
	args := []string{"git:from-image"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, app, imageRef)
	_, derr := c.run(ctx, "deploy image", app, deployTimeout, args...)
	return wrapMaybe(derr, "deploy image", app)
}

// deployArchive streams the synthesized tar through the StdinRunner seam
// (`exec.StdinRunner`): production FastExec and the tier-3 docker-exec
// wrapper implement it; a Runner without the capability is an
// ErrUnhealthy-grade misconfiguration, reported as such rather than
// silently deploying without checks.
//
// Convergence vs. zero-mutation on real dokku (0.38.32, tier-3 verified
// 2026-10-28): resending an IDENTICAL archive exits 1 — "! No changes
// detected, skipping git commit / Call 'ps:rebuild' …" — AND dokku
// records a FAILED build for that no-op, which would poison Readiness
// (build-status gate). The adapter therefore converges via its
// TOP_ARCHIVE_SHA256 env fingerprint (spec-derived): if the observed
// fingerprint matches the one just computed from the spec, the deployed
// checks ARE the spec's checks and an already-serving app needs no
// re-send at all (fingerprint missing/stale ⇒ re-send, converging
// check drift and legacy apps; the fresh convergence writes the new
// fingerprint in the SAME apply's env batch).
//
// Same-content resend (fingerprint absent — pre-fingerprint legacy app,
// or the first apply after the deploy itself poisons in the bootstrap
// window) is classifiable: dokku's stderr marks it. If the release is
// serving, that is success-with-noise (it can only occur pre-fingerprint);
// if not serving, `ps:rebuild` heals — which fails when dokku holds
// NO commits (first attempt died before the source commit), reported as
// ErrBackend: the reconciler's re-apply (M3) then re-deploys from bourne
// scratch by unpicking the app. Deterministic archives keep this path
// rare: only the first-start window can enter it.
func (c *Client) deployArchive(ctx context.Context, app, imageRef string, spec domain.Service, obs domain.ObservedState, archive []byte, archiveSHA string) error {
	sr, ok := c.Runner.(exec.StdinRunner)
	if !ok {
		return wrapBackend("deploy archive", app, fmt.Errorf("%w: runner cannot feed stdin (need exec.StdinRunner for git:from-archive --)", domain.ErrUnhealthy))
	}
	if archive == nil {
		var aerr error
		archive, aerr = buildChecksArchive(imageRef, spec)
		if aerr != nil {
			return wrapBackend("deploy archive", app, fmt.Errorf("%w: synthesize app.json archive: %v", domain.ErrInvalid, aerr))
		}
	}
	fingerprintMatches := false
	envSHA, haveEnvSHA := "", false
	if archiveSHA != "" {
		if v, verr := c.envValue(ctx, app, archiveFingerprintKey); verr == nil {
			envSHA, haveEnvSHA = v, true
			fingerprintMatches = obs.Deployed && obs.Running && v == archiveSHA
		}
	}
	if fingerprintMatches {
		if c.Log != nil {
			c.Log.DebugContext(ctx, "archive fingerprint match; converged release already serving",
				"op", "deploy archive", "app", app)
		}
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, deployTimeout)
	defer cancel()
	stdout, stderr, rerr := sr.RunStdin(rctx, bytes.NewReader(archive), c.Binary, "git:from-archive", app, "--")
	if rerr == nil && len(bytes.TrimSpace(stderr)) > 0 && c.Log != nil {
		c.Log.WarnContext(ctx, "dokku stderr on success", "op", "deploy archive", "app", app)
	}
	if rerr == nil {
		// Deploy succeeded → the serving release now runs THIS spec's
		// checks; mint the fingerprint for future applies.
		c.setArchiveFingerprint(ctx, app, archiveSHA)
		return nil
	}
	// Classify the identical-archive case (dokku reports it on stderr,
	// stdout empty).
	combined := string(bytes.TrimSpace(stderr)) + "\n" + string(bytes.TrimSpace(stdout))
	if exec.IsExitCode(rerr, 1) && strings.Contains(combined, "No changes detected") {
		if obs.Deployed && obs.Running {
			if haveEnvSHA && envSHA != archiveSHA {
				// Serving release runs the OLD archive (env fingerprint ≠
				// computed) while dokku's stored source already matches the
				// new one: converge by rebuilding — ps:rebuild re-runs the
				// checks and surfacesErrBackend if they still fail (e.g.
				// re-apply of a spec whose checks keep failing; the old
				// release survives each failure).
				if _, rerr2 := c.run(ctx, "deploy archive rebuild", app, deployTimeout, "ps:rebuild", app); rerr2 != nil {
					return wrapBackend("deploy archive rebuild", app, rerr2)
				}
				c.setArchiveFingerprint(ctx, app, archiveSHA)
				return nil
			}
			// No fingerprint in env (pre-fingerprint legacy app or first
			// convergence after the bootstrap window): the archive IS
			// converged — dokku says so. Success-with-noise; mint the
			// fingerprint so the next apply skips cleanly.
			c.setArchiveFingerprint(ctx, app, archiveSHA)
			if c.Log != nil {
				c.Log.WarnContext(ctx, "archive unchanged; current release already serving",
					"op", "deploy archive", "app", app)
			}
			return nil
		}
		// Release recorded but NOT serving (first-start poisoned release,
		// retired container): dokku's directive — rebuild from stored
		// source. Fails when dokku holds no commits (first attempt died
		// mid-write, pre-fingerprint legacy); reported as ErrBackend.
		if _, rerr2 := c.run(ctx, "deploy archive rebuild", app, deployTimeout, "ps:rebuild", app); rerr2 != nil {
			return wrapBackend("deploy archive rebuild", app, rerr2)
		}
		c.setArchiveFingerprint(ctx, app, archiveSHA)
		return nil
	}
	return wrapBackend("deploy archive", app, fmt.Errorf("%w (dokku output tail: %.1200s)", rerr, combined))
}

// setArchiveFingerprint mints TOP_ARCHIVE_SHA256 = sha via config:set
// --no-restart, called ONLY after the archive deploy (fresh resend or
// ps:rebuild) has succeeded — see applyEnv for why the key must never be
// written speculatively before the deploy. A failed mint is logged and
// swallowed: the app is deployed and serving regardless, and the next
// apply simply re-sends the identical archive and lands on the
// success-with-noise path (harmless).
func (c *Client) setArchiveFingerprint(ctx context.Context, app, sha string) {
	if sha == "" {
		return
	}
	if _, err := c.run(ctx, "apply archive fingerprint", app, mutateTimeout,
		"config:set", "--no-restart", app, archiveFingerprintKey+"="+sha); err != nil {
		if c.Log != nil {
			c.Log.WarnContext(ctx, "archive fingerprint mint failed; next apply re-sends the archive",
				"op", "apply archive fingerprint", "app", app, "err", err)
		}
	}
}

// probesPresent reports whether the healthcheck defines probe actions
// (HTTP/TCP/Exec) in addition to timings.
func probesPresent(hc *domain.HealthCheck) bool {
	return hc != nil && (hc.HTTP != nil || hc.TCP != nil || hc.Exec != nil)
}

// envValue reads ONE config value by key: `config:get <app> <key>`
// (present ⇒ exit 0 + value; absent ⇒ exit 1). This is a CONTROLLED,
// documented deviation from ADR-0008's "the adapter never reads config
// values": it is used ONLY for the adapter's own TOP_ARCHIVE_SHA256
// fingerprint key — never for user/secret env — because there is no
// dokku CLI surface that exposes the deployed app.json's checks, and the
// fingerprint is what lets a converged image-with-checks service skip
// the archive re-send (an identical re-send would write a FAILED build
// record and break the Readiness gate; tier-3 verified 2026-10-28).
// Proposal to amend ADR-0008 with this narrow exception.
func (c *Client) envValue(ctx context.Context, app, key string) (string, error) {
	out, err := c.run(ctx, "inspect env value", app, readTimeout, "config:get", app, key)
	if exec.IsExitCode(err, 1) {
		return "", nil // absent
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func wrapMaybe(err error, op, app string) error {
	return wrapBackend(op, app, err)
}

// Readiness implements backend.PlatformBackend: contract composition
// (deployed + running + last build + an ACTIVE checks:run probe). An app
// that was never deployed/has nothing to probe still reports Checks=true
// (probe skipped — with zero processes checks:run would exit non-zero on
// "invalid container"); Deployed&&Running&&Build&&Checks compose the gate.
func (c *Client) Readiness(ctx context.Context, ref domain.ServiceRef) (backend.Readiness, error) {
	obs, err := c.Inspect(ctx, ref)
	if err != nil {
		return backend.Readiness{}, err
	}
	r := backend.Readiness{
		Deployed: obs.Deployed,
		Running:  obs.Running,
		Build:    obs.LastBuild,
		Revision: obs.Revision,
		Image:    obs.Image,
		Checks:   true, // probe skipped when nothing runs (see above)
	}
	if obs.Deployed && obs.Running {
		_, app, rerr := resolve(ref)
		if rerr != nil {
			return backend.Readiness{}, rerr
		}
		if _, cerr := c.run(ctx, "readiness checks", app, mutateTimeout, "checks:run", app); cerr != nil {
			// A failing check IS the answer, not an operation failure.
			r.Checks = false
			if c.Log != nil && !exec.IsExitCode(cerr, 1) {
				if l := c.Log; l != nil {
					l.WarnContext(ctx, "checks:run failed", "op", "readiness", "app", app, "err", cerr)
				}
			}
		}
	}
	return r, nil
}

// FailedLogs implements backend.PlatformBackend: `logs:failed <app>`
// (logs docs: output of the last failed deploy), redacted upstream,
// split to lines and truncated to the most recent `lines`. A missing app
// is ErrNotFound; exit-1 (no failed deploy output) is empty-success.
func (c *Client) FailedLogs(ctx context.Context, ref domain.ServiceRef, lines int) ([]string, error) {
	ref, app, err := resolve(ref)
	if err != nil {
		return nil, err
	}
	exists, err := c.existsApp(ctx, "failed logs", app)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, domain.NewNotFound("dokku app", ref.String())
	}
	out, err := c.run(ctx, "failed logs", app, readTimeout, "logs:failed", app)
	if err != nil && !exec.IsExitCode(err, 1) {
		return nil, wrapBackend("failed logs", app, err)
	}
	var rows []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) != "" {
			rows = append(rows, l)
		}
	}
	if lines > 0 {
		rows = rows[max(0, len(rows)-lines):]
	}
	return rows, nil
}

// DefaultInternalNetwork is the shared internal docker network this
// adapter attaches routing.mode: internal services to (ADR-0002;
// EnsureNetwork is the idempotent creator — callers create it first).
const DefaultInternalNetwork = "top-internal"
