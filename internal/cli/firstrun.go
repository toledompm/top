// SPDX-License-Identifier: MIT

package cli

// T-406 — the composition root's install.Options.FirstRun wiring (the
// "T-406 slot" internal/install/adopt.go documents). The installer's
// phase 5 calls ONE reconciliation pass through the FirstRunner seam;
// production composes exactly the ADR-0002 chain:
//
//	gitsource (resolve seeded revision → fetch snapshot)
//	→ spec.LoadDir(WithPlatformRepo(true)) (strict; reserved namespaces ok)
//	→ reconcile.Reconciler.Run (the adopt-aware pipeline)
//
// This mirrors the proven reconHandoff reference in internal/install/
// adopt_test.go — the CLI version shares the CLI's orGitSource (so the
// injected Runner seam applies) and theLEN canonical state-store path;
// it opens its OWN sqlite connection lazily inside FirstRun (the phase-5
// call happens after the installer created the DataDir, so a read-only
// preface like repo-URL validation can never fail because of it — the
// earlier eager open would have mapped a preflight-space failure onto
// exit 4). The installer holds its own connection to the same file; the
// handoff finishes its run before the MarkManaged flip reads that state,
// so the short-lived connection serializes through the store's own
// locking discipline.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"github.com/toledompm/top/internal/backend/dokku"
	"github.com/toledompm/top/internal/config"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/gitsource"
	"github.com/toledompm/top/internal/install"
	"github.com/toledompm/top/internal/reconcile"
	"github.com/toledompm/top/internal/secrets"
	"github.com/toledompm/top/internal/spec"
	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/store/sqlite"
)

// installFirstRun is install.FirstRunner's production implementation.
type installFirstRun struct {
	src *gitsource.Source
	be  reconcile.Backend
	// stPath is the canonical state file (<dataDir>/top.db); the
	// connection is opened per-run (see the file comment about lifetimes).
	stPath string
	// st, when set (tests), is used instead of a fresh sqlite.Open.
	st  store.Store
	nss []string
	// secrets is the host-file provider when the caller's config uses it
	// (ADR-0008's v0); nil for other kinds (contained failure at apply).
	secrets reconcile.SecretResolver
	// cfg feeds the RunInput's VolumeRoot default (<dataDir>/volumes —
	// storage-bearing specs refuse un-owned host dirs, ADR-0009; live
	// homelab install, 2026-11-14).
	cfg config.Config
}

// FirstRun implements install.FirstRunner. It reports ControllerSpec and
// the run's own status even when the run itself fails (the missing
// controller spec is the interesting no-spec finding; internal/install's
// phase 5 handles both shapes).
func (h installFirstRun) FirstRun(ctx context.Context) (install.FirstRunReport, error) {
	st := h.st
	if st == nil {
		s, err := sqlite.Open(ctx, h.stPath)
		if err != nil {
			return install.FirstRunReport{}, fmt.Errorf("state store %s: %w", h.stPath, err)
		}
		defer func() { _ = s.Close() }()
		st = s
	}
	sha, err := h.src.ResolveLatest(ctx)
	if err != nil {
		return install.FirstRunReport{}, err
	}
	snap, err := h.src.FetchRevision(ctx, sha)
	if err != nil {
		return install.FirstRunReport{}, err
	}
	defer func() { _ = snap.Close() }()
	loaded, err := spec.LoadDir(snap.Dir, spec.WithPlatformRepo(true))
	if err != nil {
		return install.FirstRunReport{}, fmt.Errorf("loading specs from revision %s: %w", sha, err)
	}
	notes := h.ensureSpecSecrets(loaded.Services)
	selfSpec := false
	for _, svc := range loaded.Services {
		if svc.Ref.Equal(install.ControllerRef()) {
			selfSpec = true
		}
	}
	rec, err := reconcile.New(reconcile.Options{
		Backend: h.be,
		Store:   st,
		// Secrets: the host-file provider (ADR-0008's v0) — WITHOUT this
		// the first reconcile violently fails at the first secretRef
		// service("no secret provider configured"; tier-4 verified
		// 2026-11-03 on the seeded platform repo's grafana). The kind is
		// keyed from the caller's own config: other kinds fail contained
		// with the same ADR-0008 posture (no secret resolution without an
		// explicit provider).
		Secrets: h.secrets,
	})
	if err != nil {
		return install.FirstRunReport{}, err
	}
	out, err := rec.Run(ctx, reconcile.RunInput{
		Specs:      loaded.Services,
		Revision:   sha,
		Actor:      domain.ActorUser,
		Namespaces: h.nss,
		// Same default the controller daemon uses (controller.
		// OptionsFromConfig): storage-bearing specs need a TOP-owned
		// VolumeRoot or apply refuses un-owned host dirs (ADR-0009) —
		// the live homelab first-reconcile hit exactly that refusal.
		VolumeRoot: h.cfg.DataDir + "/volumes",
		// T-403: the materialized revision's host path feeds the
		// provisioning-supply sync (provision.go) — the repo's
		// config/provisioning trees (prometheus.yml, grafana
		// datasources/dashboards) must land in the VolumeRoot entry dirs
		// BEFORE the storage mounts, or prometheus boots with no config
		// file and fails its healthcheck (live homelab, 2026-11-14).
		// Valid through defer snap.Close() above (the sync copies, never
		// references).
		ProvisionSrc: snap.Dir,
	})
	rep := install.FirstRunReport{
		Revision:       sha,
		RunID:          "op_" + out.OperationID.String(),
		ControllerSpec: selfSpec,
		Succeeded:      err == nil && out.Status == domain.RunSucceeded,
		Status:         out.Status.String(),
		Notes:          notes,
	}
	if err != nil {
		return rep, err
	}
	return rep, nil
}

// ensureSpecSecrets generates a random host-file secret for every
// spec-declared secretRef the operator has not provisioned (T-600, live
// homelab 2026-11-14: the seeded grafana spec's admin-password ref made
// EVERY first reconcile fail until the operator hand-created a file
// whose path and name were only documented in a README). Values never
// print — the note names the file; the operator reads it when needed.
// Existing secrets are never touched (generation is first-miss only).
func (h installFirstRun) ensureSpecSecrets(svcs []domain.Service) []string {
	if h.secrets == nil {
		return nil
	}
	dir := h.cfg.SecretsDir()
	var notes []string
	seen := map[string]bool{}
	for _, svc := range svcs {
		for _, name := range svc.SecretRefs {
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			if _, _, rerr := h.secrets.Resolve(context.Background(), name); rerr == nil {
				continue
			}
			value, err := randomSecret()
			if err != nil {
				notes = append(notes, "secret "+name+": generation failed ("+err.Error()+"); create it at "+dir+"/"+name)
				continue
			}
			if werr := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); werr != nil {
				notes = append(notes, "secret "+name+": write failed ("+werr.Error()+")")
				continue
			}
			notes = append(notes, "secret "+name+" was missing — generated a random value at "+dir+"/"+name+" (0600; read it with e.g. sudo cat "+dir+"/"+name+")")
		}
	}
	return notes
}

// randomSecret is 32 bytes of crypto/rand, base64url (no padding churn
// in files or dashboards).
func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// wireFirstRun composes the T-406 bridge: the handoff reconciles from the
// SAME remote the bootstrap deployed from (the install command's
// positional argument, overriding the config's git URL), through the
// CLI's Runner seam (production gitsource default, or the injected test
// shim inside orGitSource), against the canonical state file, with the
// production dokku backend. No eager I/O: everything opens inside
// FirstRun.
func (c *cctx) wireFirstRun(repoURL string) install.FirstRunner {
	src := c.orGitSource()
	src.URL = repoURL
	var secretProvider reconcile.SecretResolver
	if c.cfg.SecretsProvider.Kind == config.SecretsProviderHostfile {
		secretProvider = secrets.NewHostFile(c.cfg.SecretsDir())
	}
	return installFirstRun{
		src:     src,
		be:      dokku.New(c.orRunner()),
		stPath:  c.cfg.DataDir + "/top.db",
		nss:     c.cfg.Namespaces.Order,
		secrets: secretProvider,
		cfg:     c.cfg,
	}
}
