// SPDX-License-Identifier: MIT
//
// Package inventory implements the host-ownership snapshot and resource
// ledger bridge of ADR-0009: a read-only capture of a host's relevant state
// (Docker resources, listening ports, Dokku apps) and the decision logic
// that connects that snapshot to the SQLite resource ledger
// (docs/contracts/state-store.md, store.Resource).
//
// Three identification layers (ADR-0009):
//
//  1. Naming — TOP-created things are "<namespace>-<name>"; namespaces come
//     from internal/config (core/observability/default, plus anything the
//     operator registered). The controller's own app is core-top-controller.
//  2. Labels — every Docker resource TOP creates carries top.dev/managed=true,
//     top.dev/namespace, top.dev/service. (TOP never applies dokku or
//     com.dokku.* labels — ADR-0001 hard rule 2.)
//  3. Ledger — store.Resource rows in SQLite: what TOP created, in which
//     operation, in which state (bootstrap-owned | managed | pre-existing).
//
// Deletability is the two-key rule: the ledger must record the resource as
// TOP-managed AND a fresh snapshot must still agree (label or namespace
// prefix). Any disagreement means skip-with-reason, never delete. Deletion
// EXECUTION lives elsewhere (M2 uninstall task B-1); this package only
// refuses or permits, with typed reasons.
package inventory

import "time"

// top.dev/* label keys (ADR-0009 layer 2, ADR-0001 hard rule 2).
const (
	LabelPrefix    = "top.dev/"
	LabelManaged   = "top.dev/managed"
	LabelNamespace = "top.dev/namespace"
	LabelService   = "top.dev/service"
	// ManagedTrue is the only managed value TOP writes.
	ManagedTrue = "true"
)

// Ledger resource kinds used by the store (store.Resource.Kind). Kept here
// so the snapshot and the ledger agree by construction.
const (
	KindApp       = "app"
	KindContainer = "container"
	KindNetwork   = "network"
	KindVolume    = "volume"
)

// HostInventory is one read-only capture of the host's relevant state at a
// point in time. Everything is plain data: JSON-serializable, no pointers to
// live state, safe to store in /var/lib/top or embed in a report.
type HostInventory struct {
	// CreatedAt is when the snapshot ran (UTC).
	CreatedAt time.Time `json:"createdAt"`

	// HostID is a stable, anonymous per-host identifier: the first 16 hex
	// bytes of SHA-256 over the host's /etc/machine-id content. The
	// machine-id itself is not a secret, but a hash keeps upstream
	// timelines/exports keyed by something TOP owns instead of an upstream
	// identifier. Empty when the seed source is unavailable (see Finding).
	HostID string `json:"hostId,omitempty"`

	// Containers is every container on the host (docker ps -a), not merely
	// TOP-related ones: the ledger comparison needs the foreign set too.
	Containers []Container `json:"containers,omitempty"`
	// Networks is docker's network list.
	Networks []Network `json:"networks,omitempty"`
	// Volumes is docker's volume list.
	Volumes []Volume `json:"volumes,omitempty"`
	// Listeners is what was listening on TCP ports when the snapshot ran.
	Listeners []Listener `json:"listeners,omitempty"`
	// DokkuApps lists the app names Dokku knows (dokku apps:list).
	DokkuApps []string `json:"dokkuApps,omitempty"`

	// Findings records every degraded capture (tooling missing, unparseable
	// output): what it was and why a section may be incomplete. A Finding is
	// never silently dropped from the JSON.
	Findings []Finding `json:"findings,omitempty"`
}

// Container is one docker ps -a line.
type Container struct {
	// ID is the short container ID as reported by docker ps.
	ID string `json:"id"`
	// Name is the leading name docker ps reports.
	Name string `json:"name"`
	// Image as reported.
	Image string `json:"image,omitempty"`
	// State is docker's state ("running", "exited", ...).
	State string `json:"state,omitempty"`
	// Labels as parsed from docker's comma-joined label string.
	Labels map[string]string `json:"labels,omitempty"`
}

// Network is one docker network ls line.
type Network struct {
	Name   string            `json:"name"`
	Driver string            `json:"driver,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Volume is one docker volume ls line.
type Volume struct {
	Name   string            `json:"name"`
	Driver string            `json:"driver,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Listener is one TCP socket in the listen state, from ss -tlnH. The parse
// is deliberately coarse (see README fragility notes): it is a
// pre-install/pre-teardown observation, never a source of truth.
type Listener struct {
	// LocalAddress is host:port as ss renders it ("0.0.0.0:22", "[::]:443").
	LocalAddress string `json:"localAddress"`
}

// Finding records one degraded or noteworthy capture during Snapshot.
// Subject may be empty for component-wide notes; Reason is a short stable
// tag followed by a human detail, e.g. "ss-missing: binary not in PATH".
type Finding struct {
	// Component: "containers" | "networks" | "volumes" | "listeners" |
	// "dokku" | "host".
	Component string `json:"component"`
	// Subject is the specific line/steps affected, empty for the whole part.
	Subject string `json:"subject,omitempty"`
	// Reason starts with a stable lowercase tag then ": " then detail.
	Reason string `json:"reason"`
}

// IsTOPManaged reports whether the label map carries the managed label
// (layer 2). Unknown or empty maps are false.
func IsTOPManaged(labels map[string]string) bool {
	return labels[LabelManaged] == ManagedTrue
}

// IsTOPLabelKey reports whether a label key lives in the top.dev/ space.
func IsTOPLabelKey(key string) bool {
	return len(key) > len(LabelPrefix) && key[:len(LabelPrefix)] == LabelPrefix
}
