package dokku

// SPDX-License-Identifier: MIT

// setters.go — the granular convergence surface the reconciler seam
// (internal/reconcile/seam.go) expects on top of the batched Apply. Each
// method maps exactly ONE Dokku command class (the seam's "convergence
// method contract") and reuses Apply's helpers/types so argv behavior
// cannot drift between the batched and granular paths.

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/toledompm/top/internal/domain"
)

// appNameOf resolves the flat dokku app name for a ref.
func appNameOf(ref domain.ServiceRef) string {
	name, _ := domain.AppName(ref.Namespace, ref.Name) // valid by construction on this path
	return name
}

// SetConfigEnv performs an ADDITIVE `config:set --no-restart` of the given
// map (seam contract: additive, values are plain, no redeploy). Diffing
// against the observed env keys mirrors the Apply path so both converge
// identically; unset of dropped keys is the Apply path's job, per seam.
func (c *Client) SetConfigEnv(ctx context.Context, ref domain.ServiceRef, env map[string]string) error {
	if len(env) == 0 {
		return nil
	}
	app := appNameOf(ref)
	obs, err := c.Inspect(ctx, ref)
	if err != nil {
		return wrapBackend("set env inspect", app, err)
	}
	var setArgs []string
	for _, k := range slices.Sorted(maps.Keys(env)) {
		if managedEnvKeys[k] || k == archiveFingerprintKey {
			continue // dokku-managed / adapter-owned bookkeeping: never the spec path
		}
		if !slices.Contains(obs.EnvKeys, k) {
			setArgs = append(setArgs, k+"="+env[k])
		}
	}
	if len(setArgs) == 0 {
		return nil // converged: config:set --no-restart is cheap but still a mutation
	}
	if _, err := c.run(ctx, "set env", app, mutateTimeout,
		append([]string{"config:set", "--no-restart", app}, setArgs...)...); err != nil {
		return wrapBackend("set env", app, err)
	}
	return nil
}

// SetSecretEnv writes ONE resolved secret via `config:set --no-restart`
// (seam contract: carries a RESOLVED value; never logged — redaction is
// the exec layer's commitment, unchanged here).
func (c *Client) SetSecretEnv(ctx context.Context, ref domain.ServiceRef, name, value string) error {
	app := appNameOf(ref)
	if _, err := c.run(ctx, "set secret env", app, mutateTimeout,
		"config:set", "--no-restart", app, name+"="+value); err != nil {
		return wrapBackend("set secret env", app, err)
	}
	return nil
}

// SetPorts converges the host-exposure map (seam contract: full replace).
// ports:set triggers a redeploy, so a converged app is left alone — the
// same guard the Apply path applies via applyPorts.
func (c *Client) SetPorts(ctx context.Context, ref domain.ServiceRef, ports []domain.Port) error {
	app := appNameOf(ref)
	obs, err := c.Inspect(ctx, ref)
	if err != nil {
		return wrapBackend("set ports inspect", app, err)
	}
	if len(ports) == 0 {
		if len(obs.Ports) > 0 {
			if _, err := c.run(ctx, "set ports clear", app, mutateTimeout,
				"ports:clear", app); err != nil {
				return wrapBackend("set ports clear", app, err)
			}
		}
		return nil
	}
	var desired []string
	seen := map[int]bool{}
	for _, p := range ports {
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
	if portsEqual(desired, obs.Ports) {
		return nil // converged; ports:set always redeploys — never call it converged
	}
	if _, err := c.run(ctx, "set ports", app, mutateTimeout,
		append([]string{"ports:set", app}, desired...)...); err != nil {
		return wrapBackend("set ports", app, err)
	}
	return nil
}

// SetDomains performs a FULL-REPLACE `domains:set` (seam contract). The
// clear path (empty → domains:set with no args) is unverified upstream —
// the same deliberate gap the Apply path records; empty desired = no-op.
func (c *Client) SetDomains(ctx context.Context, ref domain.ServiceRef, domains []string) error {
	app := appNameOf(ref)
	obs, err := c.Inspect(ctx, ref)
	if err != nil {
		return wrapBackend("set domains inspect", app, err)
	}
	if len(domains) == 0 {
		return nil
	}
	if slices.Equal(sortedCopy(domains), sortedCopy(obs.Domains)) {
		return nil // converged; domains:set re-orders the proxy config
	}
	if _, err := c.run(ctx, "set domains", app, mutateTimeout,
		append([]string{"domains:set", app}, domains...)...); err != nil {
		return wrapBackend("set domains", app, err)
	}
	return nil
}

// SetRestartPolicy performs `ps:set restart-policy` only when the observed
// policy differs (ps:set is cheap but still a mutation; converged apps
// must not re-mutate).
func (c *Client) SetRestartPolicy(ctx context.Context, ref domain.ServiceRef, policy domain.RestartPolicy) error {
	app := appNameOf(ref)
	obs, err := c.Inspect(ctx, ref)
	if err != nil {
		return wrapBackend("set restart-policy inspect", app, err)
	}
	if obs.RestartPolicy == policy {
		return nil
	}
	// Unreported policy + always (the spec default) is converged: dokku's
	// storage default IS always; ps:set on it would only churn records.
	if obs.RestartPolicy == "" && policy == domain.RestartAlways {
		return nil
	}
	if _, err := c.run(ctx, "set restart-policy", app, mutateTimeout,
		"ps:set", app, "restart-policy", string(policy)); err != nil {
		return wrapBackend("set restart-policy", app, err)
	}
	return nil
}

// AttachNetwork attaches the app to a network (seam contract: attach-only,
// no redeploy) when it is not already attached. network:set re-deploys, so
// a converged app is never poked.
func (c *Client) AttachNetwork(ctx context.Context, ref domain.ServiceRef, network string) error {
	app := appNameOf(ref)
	obs, err := c.Inspect(ctx, ref)
	if err != nil {
		return wrapBackend("attach network inspect", app, err)
	}
	if slices.Contains(obs.Networks, network) {
		return nil // already attached: network:set re-deploys, so guard
	}
	if _, err := c.run(ctx, "attach network", app, mutateTimeout,
		"network:set", app, "attach-post-deploy", network); err != nil {
		return wrapBackend("attach network", app, err)
	}
	return nil
}

// sortedCopy is a small sort helper for domain-equality checks.
func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}
