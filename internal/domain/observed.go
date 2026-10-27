package domain

import (
	"fmt"
	"time"
)

// BuildStatus is the backend-reported status of a service's last build
// (`builds:report --build-status` in the backend contract).
type BuildStatus string

const (
	BuildNone      BuildStatus = "none"
	BuildQueued    BuildStatus = "queued"
	BuildRunning   BuildStatus = "running"
	BuildSucceeded BuildStatus = "succeeded"
	BuildFailed    BuildStatus = "failed"
)

// IsValid reports whether b is a known build status.
func (b BuildStatus) IsValid() bool {
	switch b {
	case BuildNone, BuildQueued, BuildRunning, BuildSucceeded, BuildFailed:
		return true
	}
	return false
}

// String implements fmt.Stringer.
func (b BuildStatus) String() string { return string(b) }

// ParseBuildStatus parses a build status string; empty input is BuildNone.
func ParseBuildStatus(s string) (BuildStatus, error) {
	if s == "" {
		return BuildNone, nil
	}
	b := BuildStatus(s)
	if !b.IsValid() {
		return BuildNone, fmt.Errorf("%w: build status %q", ErrInvalid, s)
	}
	return b, nil
}

// StorageMount is an ObservedState mount: the storage entry and where it is
// mounted in the container.
type StorageMount struct {
	Name      string
	HostPath  string
	MountPath string
}

// String renders the mount as "name:/mountPath", matching Storage.
func (m StorageMount) String() string { return m.Name + ":" + m.MountPath }

// ObservedState is what PlatformBackend.Inspect returns (per
// docs/contracts/platform-backend.md): a structured snapshot of the host's
// actual state for one app. It is the input to the diff that builds a Plan.
//
// Redaction (ADR-0008): env carries *keys*, never values; secret
// references are represented only by their content checksum so the
// reconciler can detect "the referenced secret changed on the host"
// without ever seeing the value.
type ObservedState struct {
	// Ref is the service identity the app maps to (namespace + name from
	// the app-name prefix).
	Ref ServiceRef
	// AppName is the Dokku app name as reported/derived.
	AppName string
	// Deployed: the app exists and has at least one deployed release.
	Deployed bool
	// Running: the app's container(s) are up.
	Running bool
	// Revision is the deployed git sha (source builds), "" for image deploys.
	Revision string
	// Image is the deployed source-image reference (image deploys), "" otherwise.
	Image string
	// EnvKeys are the *names* of config vars; values are deliberately absent.
	EnvKeys []string
	// SecretChecksums maps a secret reference name to the checksum of its
	// current value ("" checksum = reference not resolvable on the host).
	SecretChecksums map[string]string
	// Ports as reported.
	Ports []Port
	// Domains attached to the proxy.
	Domains []string
	// StorageMounts as reported.
	StorageMounts []StorageMount
	// RestartPolicy as reported; empty when unreported.
	RestartPolicy RestartPolicy
	// Resources as reported.
	Resources Resources
	// Networks the app is attached to.
	Networks []string
	// LastBuild status, BuildNone when the app has no builds.
	LastBuild BuildStatus
	// InspectedAt is when the backend gathered this snapshot.
	InspectedAt time.Time
}

// Absent refines Deployed: an app that does not exist at all.
func (o ObservedState) Absent() bool { return !o.Deployed }

// Unhealthy refines Running for readiness purposes: it is running but its
// last build failed — the readiness gate (backend contract `Readiness`)
// decides what to do with this.
func (o ObservedState) Unhealthy() bool { return o.Running && o.LastBuild == BuildFailed }
