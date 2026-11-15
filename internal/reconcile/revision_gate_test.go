// SPDX-License-Identifier: MIT

package reconcile

import (
	"strings"
	"testing"

	"github.com/toledompm/top/internal/backend"
	"github.com/toledompm/top/internal/domain"
)

// The readiness gate's revision vocabulary: plain git:from-image deploys
// must show the pinned image reference; archive-deployed apps (the
// checks-carrying git:from-archive path) have NO Image fact by
// construction — dokku records the archive's commit sha — so their
// convergence is the adapter's fingerprint contract: the presence of
// backend.ArchiveFingerprintKey certifies the serving release (the key
// is minted ONLY after a successful archive deploy of that spec's
// archive; ADR-0002 amendment A1). Live finding 2026-11-15, media-server:
// the cadvisor conversion deployed fine and then failed readiness for
// the full 3m deadline on image="" because nothing could ever match.
func TestRevisionOKArchiveDeployedAppConvergesOnFingerprint(t *testing.T) {
	t.Parallel()
	svc := domain.Service{
		Ref:    domain.Ref("observability", "cadvisor"),
		Source: domain.Source{Image: "ghcr.io/google/cadvisor", Tag: "v0.60.6"},
	}

	// Plain image deploy: the Image comparison applies.
	plain := domain.ObservedState{Deployed: true, Running: true, Image: "ghcr.io/google/cadvisor:v0.60.6"}
	if err := revisionOK(svc, plain); err != nil {
		t.Fatalf("matching plain deploy must pass: %v", err)
	}
	mismatch := domain.ObservedState{Deployed: true, Running: true, Image: "ghcr.io/google/cadvisor:v0.59.0"}
	if err := revisionOK(svc, mismatch); err == nil || !strings.Contains(err.Error(), "not yet") {
		t.Fatalf("mismatched plain deploy must fail with the not-yet error, got %v", err)
	}

	// Archive deploy: Image is empty by construction; the fingerprint
	// key's presence certifies convergence.
	archive := domain.ObservedState{
		Deployed: true, Running: true,
		EnvKeys: []string{"TZ", backend.ArchiveFingerprintKey},
	}
	if err := revisionOK(svc, archive); err != nil {
		t.Fatalf("archive-deployed app must converge on the fingerprint key: %v", err)
	}

	// No fingerprint key: the empty Image is a REAL not-yet state (the
	// pre-conversion shape) and must still fail.
	preConversion := domain.ObservedState{Deployed: true, Running: true, EnvKeys: []string{"TZ"}}
	if err := revisionOK(svc, preConversion); err == nil {
		t.Fatal("image-less state without the fingerprint key must fail the gate")
	}

	// Mutable-tag specs are not comparable regardless (wantRevisionImage
	// returns "" ⇒ pass) — unchanged contract behavior.
	mutable := svc
	mutable.Source.Tag = ""
	mutable.Source.Digest = ""
	if err := revisionOK(mutable, domain.ObservedState{}); err != nil {
		t.Fatalf("mutable reference is not comparable: %v", err)
	}
}
