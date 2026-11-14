// SPDX-License-Identifier: MIT

// Package dokku is TOP's PlatformBackend implementation over the Dokku CLI —
// the ONLY package that knows Dokku command syntax (ADR-0001). Boundary
// rules enforced here: dokku CLI only (via the internal/exec Runner), never
// out-of-band Docker mutation of apps, never `dokku`/`com.dokku.*` labels
// on TOP resources, minimum supported Dokku 0.38.x (preflight-verified up
// the stack, see ADR-0001).
//
// Every command is invoked as argv on one binary ("dokku" by default) with
// per-class timeouts (read 30s / mutate 2m / destroy 5m) supplied through
// the context so exec and cancellation semantics stay uniform (ADR-0012,
// platform-backend.md). Exit status decides success; a non-empty stderr
// with exit 0 is a warning logged, never a failure (contract error
// semantics). Redaction of secret values is the exec layer's job (ADR-0008);
// this package passes args unredacted and logs no values.
package dokku

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
)

// DefaultBinary is the Dokku CLI binary this adapter invokes.
const DefaultBinary = "dokku"

// Per-command-timeout classes (platform-backend.md; today's slice: reads
// 30s, mutations 2m, destroys 5m; the 15-min deploy class arrives with M3's
// Apply).
const (
	readTimeout    = 30 * time.Second
	mutateTimeout  = 2 * time.Minute
	destroyTimeout = 5 * time.Minute
)

// Client is the Dokku PlatformBackend. It is stateless and safe for
// concurrent use as far as the Runner is.
type Client struct {
	// Binary defaults to DefaultBinary; overridable for shims (tier 2).
	Binary string
	// InternalNetwork is the shared internal docker network the adapter
	// attaches `routing.mode: internal` services to during Apply (ADR-0002).
	// Empty falls back to DefaultInternalNetwork ("top-internal"); the e2e
	// suite overrides it with its disposable per-container network.
	InternalNetwork string
	// Runner is the exec seam (internal/exec.FastExec in production,
	// exectest.FakeRunner in tier-1 tests, a docker-exec wrapper in tier-3).
	Runner exec.Runner
	// Log receives stderr warnings from successful commands (contract:
	// exit 0 with stderr → log, do not fail). Nil means discard.
	Log *slog.Logger
}

// New returns a Client with defaults applied.
func New(runner exec.Runner) *Client {
	return &Client{Binary: DefaultBinary, Runner: runner}
}

// run executes argv with the given timeout class and returns trimmed
// stdout. It logs (never fails on) exit-0 stderr warnings and maps every
// failure to a *BackendError — see errors.go for the wrapping semantics.
func (c *Client) run(ctx context.Context, op, app string, timeout time.Duration, args ...string) (string, error) {
	if c.Runner == nil {
		return "", wrapBackend(op, app, exec.ErrNotFound)
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, stderr, err := c.Runner.Run(rctx, c.Binary, args...)
	if err == nil && len(bytes.TrimSpace(stderr)) > 0 {
		if l := c.Log; l != nil {
			// stderr is already redacted by the exec layer; log as-is.
			l.WarnContext(ctx, "dokku stderr on success",
				"op", op, "app", app, "stderr", string(bytes.TrimSpace(stderr)))
		}
	}
	if err != nil {
		// stdout/stderr still return on failure — callers classify on the
		// child's OUTPUT (DeployImage's no-changes marker rides whichever
		// stream dokku chose), so dropping it here made the bootstrap
		// resume path blind (homelab media-server, 2026-11-14).
		return strings.TrimSpace(string(stdout)), wrapBackend(op, app, err)
	}
	return strings.TrimSpace(string(stdout)), nil
}

// existsApp runs the app-existence probe. `dokku apps:exists <app>` exits
// zero when the app exists; its documented semantics (application-management.md)
// make ANY non-zero exit the "does not exist" answer (with the command
// itself already having run). Errors that are not exit statuses (timeout,
// binary missing, exec I/O) are wrapped and returned as-is.
func (c *Client) existsApp(ctx context.Context, op, app string) (bool, error) {
	if _, err := c.run(ctx, op, app, readTimeout, "apps:exists", app); err != nil {
		// Non-zero = "not exist" per application-management.md. Verified
		// tier-3 on real 0.38.x: exit 20 ("!  App <app> does not exist").
		// Accept the observed 20 plus loose 10/1 aliases; anything else is an
		// infra failure and is surfaced.
		if exec.IsExitCode(err, 20) || exec.IsExitCode(err, 10) || exec.IsExitCode(err, 1) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// resolve validates the ref and derives the Dokku app name, wrapping any
// validation failure as a *BackendError (ErrInvalid class via re-wrap).
func resolve(ref domain.ServiceRef) (domain.ServiceRef, string, error) {
	app, err := domain.AppName(ref.Namespace, ref.Name)
	if err != nil {
		return ref, "", wrapBackend("resolve app", ref.String(), err)
	}
	return ref, app, nil
}

// parseRestartPolicy is implemented in map.go.

// parseBuildStatus is implemented in map.go.

// EnsureApp implements backend.PlatformBackend: create when absent, adopt
// when present ("apps:exists" exit code → "apps:create"). Adoption is by
// name prefix per ADR-0009's naming layer; ledger-verified adoption is the
// caller's concern (T-204/T-402) — see the interface doc note.
func (c *Client) EnsureApp(ctx context.Context, ref domain.ServiceRef) error {
	_, app, err := resolve(ref)
	if err != nil {
		return err
	}
	ok, err := c.existsApp(ctx, "ensure app", app)
	if err != nil {
		return err
	}
	if ok {
		return nil // adopt
	}
	_, err = c.run(ctx, "ensure app", app, mutateTimeout, "apps:create", app)
	return err
}

// Inspect implements backend.PlatformBackend: compose the verified reports
// into a domain.ObservedState. The app not existing is domain.ErrNotFound.
//
// Composition (all reads, one call each):
//
//	apps:exists (method above) → not-found short-circuit
//	ps:report <app> --format json        → deployed/running/restart-policy
//	builds:report <app> --format json    → last build status
//	git:report <app> --format json       → git sha / source-image
//	apps:report <app> --format json      → deploy-source(+metadata)/locked
//	ports:report <app> --ports-map-json  → ports
//	storage:list <app> --format json     → mounts
//	config:keys <app>                    → env KEYS (values never read)
//	network:report <app> --format json   → attached networks
//	resource:report <app> --format json  → web-proctype limits
//
// Deliberate omissions in this slice (documented, see README):
//   - Domains: no domains:report flag verified this wave — left unset.
//   - Deploy lock: apps:report "locked" is read but ObservedState has no
//     field for it (fix comes with M3's Apply, which needs it first).
func (c *Client) Inspect(ctx context.Context, ref domain.ServiceRef) (domain.ObservedState, error) {
	ref, app, err := resolve(ref)
	if err != nil {
		return domain.ObservedState{}, err
	}
	exists, err := c.existsApp(ctx, "inspect", app)
	if err != nil {
		return domain.ObservedState{}, err
	}
	if !exists {
		return domain.ObservedState{}, domain.NewNotFound("dokku app", ref.String())
	}

	// read all reports; any report failure aborts the snapshot.
	ps, err := c.readJSON(ctx, app, "ps:report")
	if err != nil {
		return domain.ObservedState{}, wrapBackend("inspect ps", app, err)
	}
	builds, err := c.readJSON(ctx, app, "builds:report")
	if err != nil {
		return domain.ObservedState{}, wrapBackend("inspect builds", app, err)
	}
	git, err := c.readJSON(ctx, app, "git:report")
	if err != nil {
		return domain.ObservedState{}, wrapBackend("inspect git", app, err)
	}
	apps, err := c.readJSON(ctx, app, "apps:report")
	if err != nil {
		return domain.ObservedState{}, wrapBackend("inspect apps", app, err)
	}
	network, err := c.readJSON(ctx, app, "network:report")
	if err != nil {
		return domain.ObservedState{}, wrapBackend("inspect network", app, err)
	}
	resources, err := c.readJSON(ctx, app, "resource:report")
	if err != nil {
		return domain.ObservedState{}, wrapBackend("inspect resource", app, err)
	}
	portJSON, err := c.run(ctx, "inspect ports", app, readTimeout, "ports:report", app, "--ports-map-json")
	if err != nil {
		return domain.ObservedState{}, wrapBackend("inspect ports", app, err)
	}
	ports, err := parsePorts([]byte(portJSON))
	if err != nil {
		return domain.ObservedState{}, wrapBackend("inspect ports", app, err)
	}
	storageJSON, err := c.run(ctx, "inspect storage", app, readTimeout, "storage:list", app, "--format", "json")
	if err != nil {
		return domain.ObservedState{}, wrapBackend("inspect storage", app, err)
	}
	storage, err := parseStorage([]byte(storageJSON))
	if err != nil {
		return domain.ObservedState{}, wrapBackend("inspect storage", app, err)
	}
	envOut, err := c.run(ctx, "inspect env keys", app, readTimeout, "config:keys", app)
	if err != nil {
		return domain.ObservedState{}, wrapBackend("inspect env keys", app, err)
	}
	// Domains read (tier-4 uncommented 2026-11-03): apply sets proxy
	// domains (`domains:set`) but Inspect never read them back, so a
	// green apply was followed by a perpetual "+domains" drift row (the
	// "after a green run the same diff is empty" invariant broke on real
	// proxy specs). `domains:report <app> --format json` reports
	// {"app-enabled","app-vhosts","global-enabled","global-vhosts"}; the
	// vhost string is space-separated.
	domJSON, err := c.run(ctx, "inspect domains", app, readTimeout, "domains:report", app, "--format", "json")
	if err != nil {
		return domain.ObservedState{}, wrapBackend("inspect domains", app, err)
	}
	domains := parseDomains([]byte(domJSON), app)

	obs := domain.ObservedState{
		Ref:           ref,
		AppName:       app,
		Deployed:      reportBool(ps, "deployed", "ps-deployed"),
		Running:       reportBool(ps, "running", "ps-running"),
		RestartPolicy: parseRestartPolicy(reportString(ps, "computed-restart-policy", "ps-computed-restart-policy")),
		LastBuild:     parseBuildStatus(reportString(builds, "build-status", "builds-build-status")),
		EnvKeys:       parseEnvKeys([]byte(envOut)),
		Networks:      networkLists(network),
		Domains:       domains,
		InspectedAt:   time.Now().UTC(),
	}

	// Deployed revision/image: a docker-image deploy surface sets
	// deploy-source=docker-image with the image ref as metadata
	// (docker-image deployment docs); git/source deploys carry the commit
	// sha there and in git:report --git-sha. A never-deployed app's
	// git:report reports sha="HEAD" (fresh-repo pseudo-sha, verified on
	// 0.38.32) — the committed facts are gated on being a valid git sha,
	// so an undeployed app reads Revision "" (domain.ValidRevision is the
	// shared definition of "is a sha").
	source := reportString(apps, "deploy-source", "app-deploy-source")
	metadata := reportString(apps, "deploy-source-metadata", "app-deploy-source-metadata")
	image := reportString(git, "source-image", "git-source-image", "source_image")
	gitSha := reportString(git, "sha", "git-sha")
	switch {
	case source == "docker-image" || image != "":
		obs.Image = firstNonEmpty(metadata, image)
	case shaLike(metadata):
		obs.Revision = metadata
	case shaLike(gitSha):
		obs.Revision = gitSha
	}

	for _, p := range ports {
		obs.Ports = append(obs.Ports, domain.Port{
			Name:          p.Scheme, // scheme is the closest stable label
			ContainerPort: p.ContainerPort,
			HostPort:      p.HostPort,
		})
	}
	for _, s := range storage {
		obs.StorageMounts = append(obs.StorageMounts, domain.StorageMount{
			Name:      s.EntryName,
			HostPath:  s.HostPath,
			MountPath: s.ContainerPath,
		})
	}
	if resources != nil {
		obs.Resources = domain.Resources{Limits: domain.ResourceLimits{
			CPU:    resourceLimit(resources, "cpu"),
			Memory: resourceLimit(resources, "memory"),
		}}
	}
	return obs, nil
}

// readJSON runs `<plugin>:report <app> --format json` and parses one object.
func (c *Client) readJSON(ctx context.Context, app, report string) (map[string]any, error) {
	out, err := c.run(ctx, "inspect "+report, app, readTimeout, report, app, "--format", "json")
	if err != nil {
		return nil, err
	}
	m, err := parseObject([]byte(out))
	if err != nil {
		return nil, err
	}
	return m, nil
}

// LastGoodRevision implements backend.PlatformBackend: the deploy-source
// metadata of the last deploy (apps:report --app-deploy-source-metadata),
// falling back to git:report --git-source-image for image deploys. An app
// that was never deployed returns ("", nil); an app that does not exist is
// domain.ErrNotFound.
func (c *Client) LastGoodRevision(ctx context.Context, ref domain.ServiceRef) (string, error) {
	ref, app, err := resolve(ref)
	if err != nil {
		return "", err
	}
	exists, err := c.existsApp(ctx, "last revision", app)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", domain.NewNotFound("dokku app", ref.String())
	}
	meta, err := c.reportFlag(ctx, app, "apps:report", "--app-deploy-source-metadata")
	if err != nil {
		return "", err
	}
	if meta != "" {
		return meta, nil
	}
	img, err := c.reportFlag(ctx, app, "git:report", "--git-source-image")
	if err != nil {
		return "", err
	}
	return img, nil
}

// reportFlag single-flag read: <report> <app> --<flag>. The flag is the
// exact documented spelling (no invented stripped variants); the caller
// supplies the fallback flag as a second call.
func (c *Client) reportFlag(ctx context.Context, app, report, flag string) (string, error) {
	out, err := c.run(ctx, "inspect "+report, app, readTimeout, report, app, flag)
	if err != nil {
		return "", wrapBackend("inspect "+report, app, err)
	}
	return out, nil
}

// Stop implements backend.PlatformBackend: "ps:stop <app>". An app that
// does not exist is a no-op success (idempotent, per QR: stop semantics);
// anything else failing is a *BackendError.
func (c *Client) Stop(ctx context.Context, ref domain.ServiceRef) error {
	_, app, err := resolve(ref)
	if err != nil {
		return err
	}
	exists, err := c.existsApp(ctx, "stop", app)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	_, err = c.run(ctx, "stop", app, mutateTimeout, "ps:stop", app)
	return err
}

// Destroy implements backend.PlatformBackend: "apps:destroy --force <app>".
//
// OWNERSHIP SPLIT (ADR-0009): the destructive gate — ledger match AND
// top.dev/* label/naming two-key match — is proven and logged by the
// CALLER (uninstall/prune, T-204/T-208); this adapter's defense-in-depth is
// the naming assertion only: it refuses a ref that cannot derive a
// well-formed TOP app name and never destroys anything else. An app that
// does not exist is a no-op success.
func (c *Client) Destroy(ctx context.Context, ref domain.ServiceRef) error {
	_, app, err := resolve(ref)
	if err != nil {
		return err
	}
	exists, err := c.existsApp(ctx, "destroy", app)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	_, err = c.run(ctx, "destroy", app, destroyTimeout, "apps:destroy", "--force", app)
	return err
}

// HealthyBackend implements backend.PlatformBackend: "dokku version" plus a
// cheap "apps:list --format json". Both must exit zero; any failure (or
// timeout, or missing binary) yields (false, err) wrapping
// domain.ErrUnhealthy via BackendError.
func (c *Client) HealthyBackend(ctx context.Context) (bool, error) {
	if _, err := c.run(ctx, "health", DefaultBinary, readTimeout, "version"); err != nil {
		return false, wrapHealth("health", DefaultBinary, err)
	}
	if _, err := c.run(ctx, "health", DefaultBinary, readTimeout, "apps:list", "--format", "json"); err != nil {
		return false, wrapHealth("health", DefaultBinary, err)
	}
	return true, nil
}

// EnsureNetwork implements backend.PlatformBackend: "network:exists" exit
// code → "network:create <name>" (attachable bridge network, network-management.md).
func (c *Client) EnsureNetwork(ctx context.Context, name string) error {
	if _, err := c.run(ctx, "ensure network", name, readTimeout, "network:exists", name); err == nil {
		return nil // exists
	} else if !exec.IsExitCode(err, 1) {
		return err
	}
	_, err := c.run(ctx, "ensure network", name, mutateTimeout, "network:create", name)
	return err
}

// Close implements backend.PlatformBackend; the CLI adapter holds nothing.
func (c *Client) Close() error { return nil }

// --- integrator surface (T-205..T-208, internal/install) --------------------

// deployTimeout is the deploy-time class of platform-backend.md's error
// semantics: image deploys are the long pole of any operation. 10 minutes
// is the installer contract's bootstrap-deploy budget (ADR-0005 slice,
// T-206); M3's Apply reuses the same class.
const deployTimeout = 10 * time.Minute

// DeployImage implements backend.PlatformBackend: "git:from-image <app>
// <imageRef>" (docker-image deployment docs) under the deploy timeout
// class. The deploy is idempotent-safe to re-run (a re-run redeploys the
// same image), but exit-nonzero means a real failure — the installer
// records it as the bootstrap-failed marker.
func (c *Client) DeployImage(ctx context.Context, ref domain.ServiceRef, imageRef string) error {
	_, app, err := resolve(ref)
	if err != nil {
		return err
	}
	if strings.TrimSpace(imageRef) == "" {
		return wrapBackend("deploy image", app, fmt.Errorf("%w: empty image ref", domain.ErrInvalid))
	}
	stdout, err := c.run(ctx, "deploy image", app, deployTimeout, "git:from-image", app, imageRef)
	if err != nil {
		// dokku's idempotence posture (verified live 2026-11-13/14,
		// media-server): git:from-image with an image whose source context
		// the app ALREADY has prints "! No changes detected, skipping git
		// commit" + "Call 'ps:rebuild' …" and exits 1 — the deployed state
		// already matches the requested ref. That is success for TOP's
		// contract (the deploy is current; ps:rebuild would build the SAME
		// bytes again). Classify: exit 1 + the no-changes marker ⇒ nil.
		// The marker rides STDERR in tty contexts and STDOUT under
		// sudo/non-TTY (dokku's own stream choice) — check BOTH; the
		// stdout-only case is why the stderr-only match kept missing.
		var ee *exec.ExitError
		if errors.As(err, &ee) && exec.IsExitCode(err, 1) &&
			(strings.Contains(string(ee.Stderr), "No changes detected") ||
				strings.Contains(stdout, "No changes detected")) {
			return nil
		}
		return err
	}
	return nil
}

// EnsureStorageEntry implements backend.PlatformBackend: "storage:info
// <name>" (dokku.com persistent-storage docs, 0.38+: named entries) →
// "storage:create <name> <hostDir> --chown false". The --chown false the
// contract wants: TOP's hostDir is installer-managed (0700, purge-data
// tier) — Dokku must not take ownership of it.
// A storage:info exit-status failure means "absent"; any other error class
// (timeout, missing binary) surfaces.
func (c *Client) EnsureStorageEntry(ctx context.Context, name, hostDir string) error {
	if !validStorageName(name) {
		return wrapBackend("ensure storage entry", name, fmt.Errorf("%w: storage entry name %q must be a DNS-1123 label (dokku storage docs)", domain.ErrInvalid, name))
	}
	if _, serr := c.run(ctx, "ensure storage entry", name, readTimeout, "storage:info", name); serr == nil {
		return nil // entry exists
	} else if !exec.IsExitCode(serr, 1) {
		return serr
	}
	_, cerr := c.run(ctx, "ensure storage entry", name, mutateTimeout, "storage:create", name, hostDir, "--chown", "false")
	return cerr
}

// AttachStorage implements backend.PlatformBackend: mount a named storage
// entry into the app at containerPath — "storage:mount <app> <name>
// --container-dir <containerPath>" (the named-entry form; the legacy
// host:container colon form stays out of this adapter so every deploy
// anchor is a dokku-tracked entry).
func (c *Client) AttachStorage(ctx context.Context, ref domain.ServiceRef, name, containerPath string) error {
	_, app, err := resolve(ref)
	if err != nil {
		return err
	}
	if !validStorageName(name) {
		return wrapBackend("attach storage", app, fmt.Errorf("%w: storage entry name %q must be a DNS-1123 label", domain.ErrInvalid, name))
	}
	if !strings.HasPrefix(containerPath, "/") {
		return wrapBackend("attach storage", app, fmt.Errorf("%w: container path %q must be absolute (dokku storage docs)", domain.ErrInvalid, containerPath))
	}
	_, err = c.run(ctx, "attach storage", app, mutateTimeout, "storage:mount", app, name, "--container-dir", containerPath)
	return err
}

// DetachStorage implements backend.PlatformBackend: "storage:unmount <app>
// <name> --container-dir <containerPath>". No-op-safe when the app is
// gone or the mounts are absent: idempotent teardown per T-208.
func (c *Client) DetachStorage(ctx context.Context, ref domain.ServiceRef, name, containerPath string) error {
	_, app, err := resolve(ref)
	if err != nil {
		return err
	}
	exists, err := c.existsApp(ctx, "detach storage", app)
	if err != nil {
		return err
	}
	if !exists {
		return nil // nothing mounted anywhere; apps:destroy already cleaned it
	}
	_, err = c.run(ctx, "detach storage", app, mutateTimeout, "storage:unmount", app, name, "--container-dir", containerPath)
	// "no-op rather than an error" per the 0.38 docs for scope-less
	// unmounts; tolerate the nothing-attached exit like the exists probe.
	if err != nil && exec.IsExitCode(err, 1) {
		return nil
	}
	return err
}

// DestroyStorageEntry implements backend.PlatformBackend: "storage:destroy
// <name> --force". The host dir backing the entry is deliberately left
// alone here: purge-data removes it topctl-side with its own confirmation
// (ADR-0009 teardown tiers) — see the interface doc.
func (c *Client) DestroyStorageEntry(ctx context.Context, name string) error {
	if _, err := c.run(ctx, "destroy storage entry", name, mutateTimeout, "storage:destroy", name, "--force"); err != nil {
		// Missing entries are fine on re-run (idempotent teardown).
		if exec.IsExitCode(err, 1) {
			return nil
		}
		return err
	}
	return nil
}

// validStorageName is dokku's DNS-1123-label constraint on entry names.
func validStorageName(name string) bool {
	if name == "" || len(name) > 45 {
		return false
	}
	for i := 0; i < len(name); i++ {
		b := name[i]
		ok := b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-'
		if b == '-' && (i == 0 || i == len(name)-1) {
			return false
		}
		if !ok {
			return false
		}
	}
	return true
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// shaLike reports whether s satisfies domain.ValidRevision (a resolved
// git sha); pseudo-values such as git's fresh-repo "HEAD" are not SHA and
// read as "no revision".
func shaLike(s string) bool { return s != "" && domain.ValidRevision(s) == nil }
