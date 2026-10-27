// SPDX-License-Identifier: MIT
package inventory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/toledompm/top/internal/config"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
)

// Reason is a typed, stable decision cause over a TOP-identifiable host
// resource. The values are referenced by the README truth table and
// surfaced to the uninstall harness (task B-1) so it can log
// skip-with-reason, never delete-by-guess.
type Reason string

const (
	// ReasonOK: the rule's keys agree; the operation is permitted.
	ReasonOK Reason = "ok"
	// ReasonRefusedPreExisting: the ledger records the resource as
	// pre-existing — it was on the host before TOP touched it. Adoption
	// via the bootstrap inventory is that row's only legal transition
	// (store.MarkManaged refuses it); deletion is refused.
	ReasonRefusedPreExisting Reason = "refused-pre-existing"
	// ReasonRefusedLedgerMissing: no ledger row records this identifier
	// as TOP-managed; key 1 of the two-key rule fails.
	ReasonRefusedLedgerMissing Reason = "refused-ledger-missing"
	// ReasonRefusedLabelMismatch: the ledger knows the resource, but the
	// fresh snapshot no longer shows the top.dev/managed label or the
	// namespace prefix (renamed, relabeled, replaced) — key 2 fails.
	ReasonRefusedLabelMismatch Reason = "refused-label-mismatch"
	// ReasonAbsentSnapshot: the identifier is not present in the fresh
	// snapshot at all; nothing to delete and nothing to adopt.
	ReasonAbsentSnapshot Reason = "absent-from-snapshot"
	// ReasonRefusedForeignIdentifier: the identifier carries neither the
	// managed label nor a known namespace prefix, so it does not even
	// enter TOP's identification rules.
	ReasonRefusedForeignIdentifier Reason = "refused-foreign-identifier"
)

// String implements fmt.Stringer.
func (r Reason) String() string { return string(r) }

// Decision is the outcome of one Adoptable/Deletable evaluation.
type Decision struct {
	// Adoptable: the resource may be explicitly adopted into TOP
	// ownership via the bootstrap ledger walk (never silently).
	Adoptable bool
	// Deletable: both two-key keys agree — uninstall (task B-1) may
	// remove it.
	Deletable bool
	// Reason explains the outcome; never empty.
	Reason Reason
	// Detail carries the offending name/label/state for audit output.
	// Identifiers only, redaction-free by construction.
	Detail string
}

// String renders "reason: detail" for logs and CLI output.
func (d Decision) String() string {
	if d.Detail == "" {
		return d.Reason.String()
	}
	return string(d.Reason) + ": " + d.Detail
}

// IdentifiableKind reports which ledger kinds the decisions consult.
func IdentifiableKind(kind string) bool {
	switch kind {
	case KindApp, KindContainer, KindNetwork, KindVolume:
		return true
	}
	return false
}

// NameOwnedBy checks layer-1 identification: does the identifier carry the
// "<namespace>-<name>" agreement for one of the known namespaces? The
// controller's own app (core-top-controller) qualifies via the core
// namespace. Returns the matching namespace.
func NameOwnedBy(name string, namespaces []string) (string, bool) {
	for _, ns := range namespaces {
		if ns == "" {
			continue
		}
		prefix := ns + "-"
		if strings.HasPrefix(name, prefix) && len(strings.TrimPrefix(name, prefix)) > 0 {
			return ns, true
		}
	}
	return "", false
}

// SnapshotOwned evaluates key 2 (the live-read half of the two-key rule):
// does the FRESH snapshot identify this resource as TOP's, via layer 2
// (top.dev/managed label) or layer 1 (naming agreement)? It never consults
// the ledger.
func SnapshotOwned(nss []string, kind, identifier string, snap *HostInventory) bool {
	switch kind {
	case KindApp:
		// Apps come from Dokku; the documented CLI cannot label apps
		// themselves, so layer 1 (naming) is the app check.
		if !snap.HasApp(identifier) {
			return false
		}
		_, ok := NameOwnedBy(identifier, nss)
		return ok
	case KindContainer:
		c, found := snap.FindContainer(identifier)
		if !found {
			return false
		}
		if IsTOPManaged(c.Labels) {
			return true
		}
		_, ok := NameOwnedBy(strings.TrimPrefix(c.Name, "/"), nss)
		return ok
	case KindNetwork:
		n, found := snap.FindNetwork(identifier)
		return found && IsTOPManaged(n.Labels)
	case KindVolume:
		v, found := snap.FindVolume(identifier)
		return found && IsTOPManaged(v.Labels)
	default:
		return false
	}
}

// SnapshotPresent reports whether the identifier exists in the snapshot at
// all, regardless of ownership marks.
func SnapshotPresent(kind, identifier string, snap *HostInventory) bool {
	switch kind {
	case KindApp:
		return snap.HasApp(identifier)
	case KindContainer:
		_, ok := snap.FindContainer(identifier)
		return ok
	case KindNetwork:
		_, ok := snap.FindNetwork(identifier)
		return ok
	case KindVolume:
		_, ok := snap.FindVolume(identifier)
		return ok
	default:
		return false
	}
}

// Adoptable implements the best-effort bootstrap-adoption rule (ADR-0009):
//
//	Adoptable(kind, id, snap) ⟺ identifier matches TOP prefixes/labels
//	(layer 1 or 2) AND the resource is recorded in the snapshot.
//
// Adoption is always explicit: callers mark rows through
// CreateLedgerFromSnapshot (state pre-existing) and the store's MarkManaged
// handoff; nothing here mutates anything.
func Adoptable(kind, identifier string, snap *HostInventory, nss []string) Decision {
	if !IdentifiableKind(kind) {
		return Decision{Reason: ReasonRefusedForeignIdentifier, Detail: "unknown ledger kind " + kind}
	}
	if !SnapshotPresent(kind, identifier, snap) {
		return Decision{Reason: ReasonAbsentSnapshot, Detail: kind + " " + identifier}
	}
	if !SnapshotOwned(nss, kind, identifier, snap) {
		return Decision{Reason: ReasonRefusedForeignIdentifier, Detail: kind + " " + identifier}
	}
	detail := kind + " " + identifier
	if ns, ok := NameOwnedBy(identifier, nss); ok {
		detail = detail + " (namespace " + ns + ")"
	}
	return Decision{Adoptable: true, Reason: ReasonOK, Detail: detail}
}

// Deletable implements the two-key deletion rule (ADR-0009 teardown):
//
//	key 1 — the ledger (store) records kind/identifier in state managed
//	         or bootstrap-owned (pre-existing refuses);
//	key 2 — a FRESH HostInventory re-read at teardown time still
//	         identifies the resource as TOP's (label/prefix agreement).
//
// Both must agree, else skip-with-reason. This function only decides —
// deletion EXECUTION is the M2 uninstall task (B-1); it also never mutates
// the ledger (B-1 removes rows via store.RemoveLedgerEntry only after
// successful removal).
func Deletable(ctx context.Context, st store.Store, kind, identifier string, freshSnap *HostInventory, nss []string) (Decision, error) {
	if !IdentifiableKind(kind) {
		return Decision{Reason: ReasonRefusedForeignIdentifier, Detail: "unknown ledger kind " + kind}, nil
	}
	entries, err := st.ListLedger(ctx, kind)
	if err != nil {
		return Decision{}, fmt.Errorf("deletable %s %q: list ledger: %w", kind, identifier, err)
	}
	var entry *store.Resource
	for _, e := range entries {
		if e.Identifier == identifier {
			entry = e
			break
		}
	}
	if entry == nil {
		return Decision{Reason: ReasonRefusedLedgerMissing, Detail: kind + " " + identifier}, nil
	}
	if entry.State == store.ResourcePreExisting {
		return Decision{Reason: ReasonRefusedPreExisting, Detail: kind + " " + identifier}, nil
	}
	if !SnapshotPresent(kind, identifier, freshSnap) {
		return Decision{Reason: ReasonAbsentSnapshot, Detail: kind + " " + identifier}, nil
	}
	if !SnapshotOwned(nss, kind, identifier, freshSnap) {
		return Decision{Reason: ReasonRefusedLabelMismatch, Detail: kind + " " + identifier}, nil
	}
	byOp := entry.CreatedByOp
	if byOp == "" {
		byOp = "-"
	}
	return Decision{
		Deletable: true,
		Reason:    ReasonOK,
		Detail:    fmt.Sprintf("%s %s (state %s, created by op %s)", kind, identifier, entry.State, byOp),
	}, nil
}

// MarkCreated records a resource TOP's bootstrap created right now: a
// bootstrap-owned ledger row attributed to the creating operation
// (ADR-0009). It is theruar thin wrapper callers use instead of composing
// store.Resource themselves; later handoff to steady state goes through
// store.MarkManaged.
func MarkCreated(ctx context.Context, st store.Store, kind, identifier, opID string, labels map[string]string, now time.Time) error {
	if !IdentifiableKind(kind) {
		return fmt.Errorf("mark created %s %q: %w: unknown ledger kind", kind, identifier, domain.ErrInvalid)
	}
	if identifier == "" || opID == "" {
		return fmt.Errorf("mark created %s %q: %w: empty identifier or operation id", kind, identifier, domain.ErrInvalid)
	}
	r := &store.Resource{
		Kind:        kind,
		Identifier:  identifier,
		Labels:      labels,
		CreatedByOp: opID,
		State:       store.ResourceBootstrapOwned,
		CreatedAt:   now,
	}
	if err := st.InsertLedgerEntry(ctx, r); err != nil {
		return fmt.Errorf("mark created %s %q (op %s): %w", kind, identifier, opID, err)
	}
	return nil
}

// LedgerSourceEntry is one snapshot-sourced ledger decision reported by
// CreateLedgerFromSnapshot: a print of what was recorded and what was
// already there is required output, not a nicety.
type LedgerSourceEntry struct {
	Kind       string `json:"kind"`
	Identifier string `json:"identifier"`
	Recorded   bool   `json:"recorded"`
	Skipped    string `json:"skipped,omitempty"` // why nothing changed ("" when recorded)
}

// CreateLedgerFromSnapshot walks a fresh snapshot and records every
// TOP-identifiable resource not yet in the ledger as pre-existing
// (store.ResourcePreExisting). Recording is best-effort per row:
//
//   - only resources the snapshot identifies as TOP-controlled by layer 1
//     (naming agreement) or layer 2 (top.dev/managed label) are recorded;
//   - an already-recorded row (the store's ErrConflict on duplicate
//     (kind, identifier)) is skipped and reported, not an error;
//   - any other store error aborts with the first failure.
//
// The returned report and inserted count are the caller's audit trail.
func CreateLedgerFromSnapshot(ctx context.Context, st store.Store, snap *HostInventory, namespaces []string, createdByOp string, now time.Time) ([]LedgerSourceEntry, int, error) {
	if len(namespaces) == 0 {
		namespaces = config.NamespacePolicyOrder
	}
	inserted := 0
	report := []LedgerSourceEntry{}

	record := func(kind, identifier string, labels map[string]string) {
		if !IdentifiableKind(kind) || identifier == "" {
			return
		}
		// The snapshot is the record here: skip anything it does not
		// positively identify as TOP-controlled.
		if !SnapshotPresent(kind, identifier, snap) || !SnapshotOwned(namespaces, kind, identifier, snap) {
			return
		}
		entry := LedgerSourceEntry{Kind: kind, Identifier: identifier}
		ierr := st.InsertLedgerEntry(ctx, &store.Resource{
			Kind:        kind,
			Identifier:  identifier,
			Labels:      effectiveLabels(labels, identifier, namespaces),
			CreatedByOp: createdByOp,
			State:       store.ResourcePreExisting,
			CreatedAt:   now,
		})
		switch {
		case ierr == nil:
			entry.Recorded = true
			inserted++
		case errors.Is(ierr, domain.ErrConflict):
			entry.Skipped = "already recorded"
		default:
		}
		report = append(report, entry)
	}

	for _, c := range snap.Containers {
		record(KindContainer, strings.TrimPrefix(c.Name, "/"), c.Labels)
	}
	for _, n := range snap.Networks {
		record(KindNetwork, n.Name, n.Labels)
	}
	for _, v := range snap.Volumes {
		record(KindVolume, v.Name, v.Labels)
	}
	for _, a := range snap.DokkuApps {
		record(KindApp, a, nil)
	}
	return report, inserted, nil
}

// effectiveLabels is the top.dev/* evidence view the ledger stores for a
// resource: layer 2 labels when present, otherwise the namespace inferred
// from the naming agreement — so every pre-existing row still says why TOP
// would claim it.
func effectiveLabels(labels map[string]string, name string, namespaces []string) map[string]string {
	if IsTOPManaged(labels) {
		return labels
	}
	ns, ok := NameOwnedBy(name, namespaces)
	if !ok {
		return nil
	}
	if labels == nil {
		labels = map[string]string{}
	}
	if _, present := labels[LabelNamespace]; !present {
		labels[LabelNamespace] = ns
	}
	return labels
}
