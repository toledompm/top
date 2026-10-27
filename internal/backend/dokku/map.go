// SPDX-License-Identifier: MIT

// Mapping helpers: Dokku report values → internal/domain enums.
package dokku

import (
	"strings"

	"github.com/toledompm/top/internal/domain"
)

// parseRestartPolicy maps the dokku ps report's computed restart policy
// (docker values: no/always/unless-stopped/on-failure[:n]; built-in default
// on-failure:10) onto domain.RestartPolicy. Values with no domain
// counterpart map to "" (unreported) rather than being guessed.
func parseRestartPolicy(s string) domain.RestartPolicy {
	switch {
	case s == "":
		return ""
	case s == "always":
		return domain.RestartAlways
	case s == "no":
		return domain.RestartNever
	case strings.HasPrefix(s, "on-failure"):
		return domain.RestartOnFailure
	case strings.HasPrefix(s, "unless-stopped"):
		return "" // docker-only value; no domain counterpart (documented)
	default:
		return ""
	}
}

// parseBuildStatus maps builds:report's display build-status onto
// domain.BuildStatus. "canceled"/"abandoned" have no domain enum: they mean
// the last build did NOT succeed (same practical consequence as failed —
// the running revision is not backed by a successful build), so they map
// to BuildFailed (documented in the README mapping table).
func parseBuildStatus(s string) domain.BuildStatus {
	switch s {
	case "queued":
		return domain.BuildQueued
	case "running":
		return domain.BuildRunning
	case "succeeded":
		return domain.BuildSucceeded
	case "failed", "canceled", "abandoned":
		return domain.BuildFailed
	default:
		return domain.BuildNone
	}
}
