package plan

import (
	"sort"
	"time"

	"github.com/toledompm/top/internal/domain"
)

// SynthesizeObserved mirrors what a correct PlatformBackend.Inspect returns
// for an app that was converged exactly to the given desired Service — the
// shape the idempotency invariant speaks of (the backend itself arrives in
// M3; the generator must mirror the ObservedState contract, not Compute's
// internals: it is written from docs/contracts/platform-backend.md).
//
// Secrets: each secretRef is present as an env key (the name) with a
// non-empty checksum — no value ships anywhere.
//
// This is **test-support API** (conformance_check T-111 and internal/plan's
// own idempotency invariant share it); it has no reconciler caller and must
// stay free of Compute-internal coupling.
func SynthesizeObserved(d domain.Service) domain.ObservedState {
	o := domain.ObservedState{
		Ref:             d.Ref,
		AppName:         mustAppName(d.Ref),
		Deployed:        true,
		Running:         true,
		LastBuild:       domain.BuildSucceeded,
		InspectedAt:     time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC),
		EnvKeys:         []string{},
		SecretChecksums: map[string]string{},
		Resources: domain.Resources{Limits: domain.ResourceLimits{
			CPU: d.Resources.Limits.CPU, Memory: d.Resources.Limits.Memory,
		}},
	}
	if d.Source.Image != "" {
		o.Image = d.Source.Reference()
	}
	if d.Source.Build != nil {
		o.Revision = d.Source.Build.Ref
	}
	o.EnvKeys = append(o.EnvKeys, sortedKeys(d.Env)...)
	// A converged host reports the secretRef's MAPPED env key (the
	// T-403 dash-kebab rule, domain.SecretEnvKey) in its config, never
	// the ref name — mirror that exactly (validated specs cannot carry
	// underscore names, so the skip below is unreachable for real input).
	for _, ref := range d.SecretRefs {
		if key, err := domain.SecretEnvKey(ref); err == nil {
			o.EnvKeys = append(o.EnvKeys, key)
		}
	}
	sort.Strings(o.EnvKeys)
	for _, ref := range d.SecretRefs {
		o.SecretChecksums[ref] = "sha256:example-checksum-" + ref
	}
	// Ports are host exposure: only proxy mode declares mappings (the
	// changePorts/applyPorts rule). none/internal converge to none — a
	// converged internal-mode host reports no port mappings even though
	// the spec declares ports for its healthcheck references.
	if d.Routing.Mode == domain.RoutingProxy {
		o.Ports = append(o.Ports, d.Ports...)
		o.Domains = append([]string(nil), d.Routing.Domains...)
	}
	if d.Routing.Mode == domain.RoutingInternal {
		o.Networks = []string{internalNetwork}
	}
	for _, s := range d.Storage {
		// Storage entries are named "<app>-<name>" (ADR-0004).
		o.StorageMounts = append(o.StorageMounts, domain.StorageMount{
			Name:      mustAppName(d.Ref) + "-" + s.Name,
			MountPath: s.MountPath,
		})
	}
	// The spec layer defaults restart policy to "always"; the backend
	// reports what is set.
	o.RestartPolicy = d.RestartPolicy
	if o.RestartPolicy == "" {
		o.RestartPolicy = domain.RestartAlways
	}
	return o
}

// mustAppName is the panic-free wrapper around domain.AppName for fixtures
// whose identity is already validated upstream (internal/spec).
func mustAppName(ref domain.ServiceRef) string {
	app, err := domain.AppName(ref.Namespace, ref.Name)
	if err != nil {
		panic(err)
	}
	return app
}
