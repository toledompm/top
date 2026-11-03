// SPDX-License-Identifier: MIT

// Mapping helpers: Dokku report values → internal/domain enums.
package dokku

import (
	"bytes"
	"encoding/json"
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

// parseDomains decodes `domains:report <app> --format json` (tier-4
// verified 2026-11-03) into the proxy's domains:
//
//	{"app-enabled":"true","app-vhosts":"grafana.example.com other.example.com",
//	 "global-enabled":"true","global-vhosts":"deploy.example.internal"}
//
// The app's domains ride app-vhosts space-separated; a proxy-disabled or
// undeployed app reports an empty string. Unknown/absent keys degrade to
// []string{}.
func parseDomains(raw []byte) []string {
	var doc map[string]string
	if err := json.Unmarshal(bytes.TrimSpace(raw), &doc); err != nil {
		return nil // malformed report reads as no domains; never stalls a run
	}
	if strings.EqualFold(doc["app-enabled"], "false") {
		return nil
	}
	split := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' })
	}
	return split(doc["app-vhosts"])
}
