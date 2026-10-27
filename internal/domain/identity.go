package domain

import (
	"fmt"
	"regexp"
	"strings"
)

// Name and namespace rules (ADR-0004, docs/contracts/service-spec.md):
//
//   - A namespace is a logical grouping: a name prefix on the Dokku app, a
//     reconcile-order policy tier, and a status grouping. It is NOT resource
//     or network isolation in any Kubernetes sense.
//   - `core` and `observability` are reserved platform namespaces; tenant
//     repos may not claim them (enforced by spec validation, not here).
//   - `default` is the namespace used when a spec omits one.
//   - A name is DNS-label-like lowercase: [a-z0-9][a-z0-9-]* with no leading
//     or trailing hyphen.
const (
	// CoreNamespace is TOP's own platform namespace; drift pruning never
	// touches it (ADR-0004) and its drift on TOP's own controller is
	// reported, never auto-applied (ADR-0005).
	CoreNamespace Namespace = "core"

	// ObservabilityNamespace holds sibling platform services (metrics, and
	// later dashboards/logs) that reconcile before tenant workloads.
	ObservabilityNamespace Namespace = "observability"

	// DefaultNamespace is used when a ServiceSpec omits metadata.namespace.
	DefaultNamespace Namespace = "default"

	// maxAppNameChars is the Dokku-app name length budget for
	// "<namespace>-<name>" including the hyphen.
	maxAppNameChars = 48
)

// PlatformNamespaces returns the reserved, reconciled-first tiers in order
// (core, then observability); a fresh slice per call so callers cannot
// mutate shared state.
func PlatformNamespaces() []Namespace {
	return []Namespace{CoreNamespace, ObservabilityNamespace}
}

// nameRe matches a valid namespace or service name: lowercase
// alphanumeric-and-hyphen, starting and ending alphanumeric, not empty.
var nameRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

// Namespace is a TOP logical namespace: name prefix, policy tier, grouping.
type Namespace string

// IsValid reports whether ns is a syntactically valid namespace name.
func (ns Namespace) IsValid() bool { return ValidNamespace(string(ns)) == nil }

// IsReserved reports whether ns is a platform-reserved namespace (core,
// observability). Reserved namespaces require platform repo ownership
// (ADR-0004); enforcement lives in spec validation.
func (ns Namespace) IsReserved() bool {
	for _, r := range PlatformNamespaces() {
		if r == ns {
			return true
		}
	}
	return false
}

// Tier is the reconcile priority of a namespace: lower runs first.
// Core → 0, observability → 1, everything else → 2 (ADR-0004).
func (ns Namespace) Tier() int {
	switch ns {
	case CoreNamespace:
		return 0
	case ObservabilityNamespace:
		return 1
	default:
		return 2
	}
}

// String implements fmt.Stringer.
func (ns Namespace) String() string { return string(ns) }

// Name is the short name of one service within its namespace.
type Name string

// IsValid reports whether n is a syntactically valid service name.
func (n Name) IsValid() bool { return ValidName(string(n)) == nil }

// String implements fmt.Stringer.
func (n Name) String() string { return string(n) }

// ServiceRef qualifies a service by namespace and name — the canonical way
// services are addressed across contracts ("ns/name"). It is the identity
// that maps 1:1 to the Dokku app name (see AppName).
type ServiceRef struct {
	Namespace Namespace
	Name      Name
}

// Ref builds a ServiceRef from raw parts.
func Ref(ns Namespace, name Name) ServiceRef {
	return ServiceRef{Namespace: ns, Name: name}
}

// String renders the qualified form "ns/name".
func (r ServiceRef) String() string { return string(r.Namespace) + "/" + string(r.Name) }

// Validate checks namespace and name syntax and that the resulting Dokku
// app name fits the length budget.
func (r ServiceRef) Validate() error {
	if err := ValidNamespace(string(r.Namespace)); err != nil {
		return fmt.Errorf("service %q: namespace: %w", r.String(), err)
	}
	if err := ValidName(string(r.Name)); err != nil {
		return fmt.Errorf("service %q: name: %w", r.String(), err)
	}
	_, err := AppName(r.Namespace, r.Name)
	return err
}

// IsZero reports whether r is the uninitialized reference.
func (r ServiceRef) IsZero() bool {
	return r.Namespace == "" && r.Name == ""
}

// Equal reports identity equality (exact string match).
func (r ServiceRef) Equal(other ServiceRef) bool {
	return r.Namespace == other.Namespace && r.Name == other.Name
}

// ParseServiceRef parses the qualified "ns/name" form. It accepts exactly
// one slash; the namespace is defaulted to DefaultNamespace when omitted
// ("name" is shorthand for "default/name", mirroring spec defaults).
func ParseServiceRef(s string) (ServiceRef, error) {
	ref := ServiceRef{Namespace: DefaultNamespace}
	var namePart string
	switch {
	case s == "":
		return ref, fmt.Errorf("%w: empty service reference", ErrInvalid)
	case strings.ContainsRune(strings.TrimSuffix(s, "/"), '/'):
		ns, name, ok := strings.Cut(s, "/")
		if !ok || strings.ContainsRune(name, '/') {
			return ref, fmt.Errorf("%w: %q: too many slashes; want \"ns/name\"", ErrInvalid, s)
		}
		ref.Namespace, namePart = Namespace(ns), name
	default:
		namePart = s
	}
	ref.Name = Name(namePart)
	if err := ref.Validate(); err != nil {
		return ref, err
	}
	return ref, nil
}

// AppName is the Dokku app name for a service: "<namespace>-<name>"
// (ADR-0004). It validates both parts and the combined ≤48-char budget, so
// callers get the same validation regardless of where it is invoked.
func AppName(ns Namespace, name Name) (string, error) {
	if err := ValidNamespace(string(ns)); err != nil {
		return "", fmt.Errorf("app name from %s/%s: %w", ns, name, err)
	}
	if err := ValidName(string(name)); err != nil {
		return "", fmt.Errorf("app name from %s/%s: %w", ns, name, err)
	}
	app := string(ns) + "-" + string(name)
	if len(app) > maxAppNameChars {
		return "", fmt.Errorf("app name %q: %d chars exceeds %d-byte Dokku limit; hint: %w",
			app, len(app), maxAppNameChars, ErrInvalid)
	}
	return app, nil
}

// ValidNamespace validates a namespace string: name-syntax; empty is a
// spec-default matter, so it is still invalid here (callers apply
// DefaultNamespace before validating).
func ValidNamespace(s string) error {
	return validDNSLabel(s, "namespace")
}

// ValidName validates a service name string (same syntax as namespaces).
func ValidName(s string) error {
	return validDNSLabel(s, "name")
}

func validDNSLabel(s, kind string) error {
	switch {
	case s == "":
		return fmt.Errorf("%w: empty %s; want lowercase [a-z0-9-], no leading/trailing hyphen", ErrInvalid, kind)
	case len(s) > maxAppNameChars:
		return fmt.Errorf("%w: %s %q is %d chars, max %d", ErrInvalid, kind, s, len(s), maxAppNameChars)
	case !nameRe.MatchString(s):
		return fmt.Errorf("%w: %s %q: must match [a-z0-9][a-z0-9-]* with no leading or trailing hyphen", ErrInvalid, kind, s)
	}
	return nil
}

// revisionRe matches a resolved Git commit sha: 7..64 lowercase hex digits
// (sha1 full form is 40, sha256 is 64; shorter forms are accepted for
// display purposes but reconciliation pins the full sha from Git).
var revisionRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// ValidRevision validates a Git commit sha. Empty input is the caller's
// problem (no revision resolved yet) and reports ErrInvalid.
func ValidRevision(sha string) error {
	switch {
	case sha == "":
		return fmt.Errorf("%w: empty revision; want a git commit sha", ErrInvalid)
	case len(sha) < 7:
		return fmt.Errorf("%w: revision %q: too short to be a git sha (min 7 hex)", ErrInvalid, sha)
	case len(sha) > 64:
		return fmt.Errorf("%w: revision %q: longer than 64 hex chars", ErrInvalid, sha)
	case !revisionRe.MatchString(sha):
		return fmt.Errorf("%w: revision %q: want lowercase hex commit sha", ErrInvalid, sha)
	}
	return nil
}
