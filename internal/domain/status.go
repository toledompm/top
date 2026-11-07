package domain

import "fmt"

// ServiceStatus is the health/status summary carried in domain surfaces
// exported as `top_service_*` metric labels — a stable vocabulary shared by
// CLI and metrics (ADR-0010, ADR-0011).
type ServiceStatus string

const (
	// ServiceHealthy: converged and readiness passing.
	ServiceHealthy ServiceStatus = "healthy"
	// ServiceProgressing: a run is in flight for the service.
	ServiceProgressing ServiceStatus = "progressing"
	// ServiceDegraded: running but failed readiness/build, or drift pending.
	ServiceDegraded ServiceStatus = "degraded"
	// ServiceMissing: wanted in Git but absent on the host.
	ServiceMissing ServiceStatus = "missing"
	// ServiceOrphaned: on the host but absent from Git (prune candidates).
	ServiceOrphaned ServiceStatus = "orphaned"
	// ServiceSelfDrift: TOP's own controller drifted; human decides (ADR-0005).
	ServiceSelfDrift ServiceStatus = "self-drift"
)

// IsValid reports whether s is a known service status.
func (s ServiceStatus) IsValid() bool {
	switch s {
	case ServiceHealthy, ServiceProgressing, ServiceDegraded,
		ServiceMissing, ServiceOrphaned, ServiceSelfDrift:
		return true
	}
	return false
}

// String implements fmt.Stringer.
func (s ServiceStatus) String() string { return string(s) }

// ParseServiceStatus parses a status string.
func ParseServiceStatus(s string) (ServiceStatus, error) {
	st := ServiceStatus(s)
	if !st.IsValid() {
		return st, fmt.Errorf("%w: service status %q", ErrInvalid, s)
	}
	return st, nil
}

// BackendStatus is TOP's view of the backend itself
type BackendStatus string

const (
	// BackendHealthy: dokku reachable and responsive.
	BackendHealthy BackendStatus = "healthy"
	// BackendDegraded: reachable but reports anomalies (e.g. unhealthy checks).
	BackendDegraded BackendStatus = "degraded"
	// BackendUnreachable: cannot be reached at all.
	BackendUnreachable BackendStatus = "unreachable"
)

// IsValid reports whether s is a known backend status.
func (s BackendStatus) IsValid() bool {
	switch s {
	case BackendHealthy, BackendDegraded, BackendUnreachable:
		return true
	}
	return false
}

// String implements fmt.Stringer.
func (s BackendStatus) String() string { return string(s) }
