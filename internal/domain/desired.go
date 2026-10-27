package domain

import (
	"errors"
	"fmt"
)

// RoutingMode is how a service is exposed on the host (service-spec.md).
type RoutingMode string

const (
	// RoutingNone means no exposure beyond the container's own network.
	RoutingNone RoutingMode = "none"
	// RoutingInternal means reachable over TOP's shared internal docker
	// network (top-internal) — service-to-service, not host-exposed.
	RoutingInternal RoutingMode = "internal"
	// RoutingProxy means host-exposed through Dokku's nginx proxy with
	// Domains (required non-empty in this mode).
	RoutingProxy RoutingMode = "proxy"
)

// IsValid reports whether m is a known routing mode.
func (m RoutingMode) IsValid() bool {
	switch m {
	case RoutingNone, RoutingInternal, RoutingProxy:
		return true
	}
	return false
}

// String implements fmt.Stringer.
func (m RoutingMode) String() string { return string(m) }

// ParseRoutingMode parses a routing mode string.
func ParseRoutingMode(s string) (RoutingMode, error) {
	m := RoutingMode(s)
	if !m.IsValid() {
		return RoutingNone, fmt.Errorf("%w: routing mode %q: want none, internal, or proxy", ErrInvalid, s)
	}
	return m, nil
}

// RestartPolicy maps to `dokku ps:set restart-policy`.
type RestartPolicy string

const (
	// RestartAlways is the spec default (service-spec.md).
	RestartAlways RestartPolicy = "always"
	// RestartOnFailure restarts the container on unexpected exit.
	RestartOnFailure RestartPolicy = "on-failure"
	// RestartNever never restarts; batch/one-shot style workloads.
	RestartNever RestartPolicy = "no"
)

// IsValid reports whether p is a known restart policy.
func (p RestartPolicy) IsValid() bool {
	switch p {
	case RestartAlways, RestartOnFailure, RestartNever:
		return true
	}
	return false
}

// String implements fmt.Stringer.
func (p RestartPolicy) String() string { return string(p) }

// ParseRestartPolicy parses a restart policy string.
func ParseRestartPolicy(s string) (RestartPolicy, error) {
	p := RestartPolicy(s)
	if !p.IsValid() {
		return RestartAlways, fmt.Errorf("%w: restart policy %q: want always, on-failure, or no", ErrInvalid, s)
	}
	return p, nil
}

// BuildSource is the alternative to an image source: Dokku builds the app
// from a git repo (ADR-0002, `git:sync --build-if-changes`).
type BuildSource struct {
	Repo string
	Ref  string
	Path string
}

// Source is the resolved origin of a service's code/image. Exactly one of
// Image and Build is set (cross-field enforcement is spec validation; the
// resolved Service should already satisfy it).
type Source struct {
	// Image is the registry path, e.g. `docker.io/prom/prometheus`.
	Image string
	// Tag is the mutable tag, exactly one of Tag/Digest is set.
	Tag string
	// Digest is the content pin, e.g. `sha256:...` — the recommended form.
	Digest string
	// Build is non-nil for source builds (repo+ref+path).
	Build *BuildSource
}

// IsValid reports basic self-consistency: exactly one of image/build, and
// at most one of tag/digest when an image source.
func (s Source) IsValid() error {
	if (s.Image == "") == (s.Build == nil) {
		return errors.New("exactly one of image or build must be set")
	}
	if s.Image != "" && s.Tag != "" && s.Digest != "" {
		return errors.New("set at most one of tag or digest")
	}
	return nil
}

// Reference renders the pinned image reference for deploy: digest when
// pinned, image:tag otherwise, or the raw image when neither is set
// (mutable/implicit-latest is allowed but warned at diff time per contract).
func (s Source) Reference() string {
	if s.Image == "" {
		return ""
	}
	switch {
	case s.Digest != "":
		return s.Image + "@" + s.Digest
	case s.Tag != "":
		return s.Image + ":" + s.Tag
	default:
		return s.Image
	}
}

// IsDigestPinned reports whether the source pins an immutable digest.
func (s Source) IsDigestPinned() bool { return s.Digest != "" }

// Port is one declared container port. HostPort is discouraged in the
// contract and usually zero (absent).
type Port struct {
	Name          string
	ContainerPort int
	HostPort      int // 0 = not set
}

// Routing is the resolved routing intent of a service.
type Routing struct {
	Mode    RoutingMode
	Domains []string // required non-empty when Mode == RoutingProxy
}

// HTTPCheck probes over HTTP; Port is the port name from Ports (or its
// number as a string).
type HTTPCheck struct {
	Path string
	Port string
}

// TCPCheck asserts a port accepts connections.
type TCPCheck struct {
	Port string
}

// ExecCheck runs a command inside the container.
type ExecCheck struct {
	Command []string
}

// HealthCheck is the healthchecking intent. A nil *HealthCheck on the
// Service means "absent — use Dokku defaults" (service-spec.md).
type HealthCheck struct {
	HTTP *HTTPCheck
	TCP  *TCPCheck
	Exec *ExecCheck
	// IntervalSeconds between probes; 0 = Dokku default.
	IntervalSeconds int
}

// Absent reports whether h specifies nothing (Dokku defaults apply).
func (h *HealthCheck) Absent() bool {
	if h == nil {
		return true
	}
	return h.HTTP == nil && h.TCP == nil && h.Exec == nil && h.IntervalSeconds == 0
}

// Storage is one desired volume mount. Size is advisory (prereading the
// space; Dokku creates the host directory at EnsureApp time).
type Storage struct {
	Name      string // entry name: "{app}-{name}" style entry
	MountPath string // container path
	Size      string // advisory, e.g. "2Gi"
}

// String renders the mount as "name:/mountPath", matching the reconciler
// diff vocabulary ("storage: +data:/prometheus").
func (s Storage) String() string { return s.Name + ":" + s.MountPath }

// Rollout is the resolved rollout policy. MaxUnavailable == 0 is the
// default and means zero-downtime checks must pass before the switch.
type Rollout struct {
	MaxUnavailable int
}

// Resources are advisory resource limits mapped to Dokku's resource plugin.
type Resources struct {
	Limits ResourceLimits
}

// ResourceLimits are string quantities ("500m", "1Gi"); empty = unset.
type ResourceLimits struct {
	CPU    string
	Memory string
}

// Service is the resolved semantics of one ServiceSpec (YAML parsing and
// defaulting are internal/spec's job — this is what a spec *means*).
// A zero-value field means "unspecified"; spec defaults are applied by the
// parser, not here.
type Service struct {
	// Ref is the immutable identity; renaming = destroy + create (ADR-0004).
	Ref ServiceRef
	// Source is exactly one of image or build, already resolved.
	Source Source
	// Ports are declared container ports.
	Ports []Port
	// Routing is the exposure intent.
	Routing Routing
	// HealthCheck is nil (or Absent) when Dokku defaults are wanted.
	HealthCheck *HealthCheck
	// Storage are desired mounts.
	Storage []Storage
	// Env holds plain, non-secret values. Anything sensitive is a entry in
	// SecretRefs instead (ADR-0008): secret *values* never appear here.
	Env map[string]string
	// SecretRefs names, resolved at apply time by the SecretProvider and
	// injected as env. Only names travel through the platform.
	SecretRefs []string
	// DependsOn are intra-namespace ordering hints (namespace tier ordering
	// is separate, ADR-0004).
	DependsOn []ServiceRef
	// Rollout policy.
	Rollout Rollout
	// RestartPolicy; unset (empty) means the spec default `always` once
	// defaulted by internal/spec.
	RestartPolicy RestartPolicy
	// Resources limits.
	Resources Resources
	// Annotations carry metadata such as `top/special: bootstrap`
	// (core/top-controller self-management, ADR-0005).
	Annotations map[string]string
}

// Annotation returns the named annotation or "".
func (s Service) Annotation(key string) string { return s.Annotations[key] }

// DesiredState is the full resolved desired state at one revision: what a
// reconciliation run aims the host at. It is a value; copies are cheap and
// safe to share.
type DesiredState struct {
	// Revision is the resolved Git commit sha of the platform repo this
	// state was derived from (never a branch name, ADR-0002).
	Revision string
	// Services is the ordered set of resolved specs. Specialized consumers
	// (the reconciler) may re-sort by namespace tier + dependsOn, so order
	// here is only presentationally meaningful (repo order).
	Services []Service
}

// Find returns the service with the given ref, or ErrNotFound wrapped with
// the qualified name.
func (d DesiredState) Find(ref ServiceRef) (Service, error) {
	for _, svc := range d.Services {
		if svc.Ref.Equal(ref) {
			return svc, nil
		}
	}
	return Service{}, NewNotFound("service", ref.String())
}

// Refs returns the identity of every service; deterministic order follows
// the Services slice.
func (d DesiredState) Refs() []ServiceRef {
	refs := make([]ServiceRef, len(d.Services))
	for i, svc := range d.Services {
		refs[i] = svc.Ref
	}
	return refs
}

// Validate spot-checks the invariants the reconciler relies on: revision
// syntax, per-service identity, and basic source shape.
func (d DesiredState) Validate() error {
	if err := ValidRevision(d.Revision); err != nil {
		return fmt.Errorf("desired state: revision: %w", err)
	}
	seen := map[ServiceRef]int{}
	for i, svc := range d.Services {
		if err := svc.Ref.Validate(); err != nil {
			return fmt.Errorf("desired state: service %d: %w", i, err)
		}
		if prev, dup := seen[svc.Ref]; dup {
			return fmt.Errorf("desired state: services %d and %d: duplicate %v", prev, i, svc.Ref)
		}
		seen[svc.Ref] = i
		if err := svc.Source.IsValid(); err != nil {
			return fmt.Errorf("desired state: service %s: source: %w", svc.Ref, err)
		}
	}
	return nil
}
