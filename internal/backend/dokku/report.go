// SPDX-License-Identifier: MIT

// Parsing of Dokku plugin report output into typed values. Verified flag /
// JSON-key facts (Dokku 0.38.x docs on dokku.com, read date 2026-10-27):
//
//   - ps:report <app> --format json — keys are the report flags with the
//     leading "--ps-" stripped (e.g. "computed-restart-policy"); the
//     read-only flags are prefix-less ("deployed", "running", "processes").
//     During the 0.38.x deprecation window legacy "ps-" prefixed aliases
//     are also emitted. Read-only "--deployed"/"--running" have no prefix.
//   - builds:report <app> --format json — status keys keep their "build-"
//     form ("build-status"); plugin properties strip "--builds-".
//   - git:report <app> --format json — properties strip "--git-"
//     ("source-image", "deploy-branch"); read-only flags "--git-sha" and
//     "--git-last-updated-at" surface likewise. Docs do not state the
//     stripped form for the read-only flags, so both spellings are
//     accepted defensively below.
//   - apps:report <app> --format json — keys strip "--app-" ("deploy-source",
//     "deploy-source-metadata", "locked"), legacy "app-" aliases emitted.
//   - ports:report <app> --ports-map-json — JSON array of
//     {"scheme","host_port","container_port"} objects.
//   - storage:list <app> --format json — JSON array of
//     {"entry_name","host_path","container_path"} (+ optional "readonly",
//     "volume_options") objects, mirroring the underlying attachment.
//   - network:report <app> --format json — properties strip "--network-";
//     "computed-attach-post-create"/"computed-attach-post-deploy"/
//     "computed-initial-network" hold the effective attach lists
//     (space-separated) and "--" prefixes are all verified flags.
//   - resource:report <app> --format json — per-proctype dotted keys
//     "<proctype>.limit.<kind>" (stripped form; legacy
//     "resource-<proctype>.limit.<kind>" aliases during the window).
//   - config:keys <app> — plain-text keys, one per line (config plugin has
//     no --format json for keys; ADR-0008: key NAMES only are ever read).
package dokku

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// parseObject decodes a report stdout that is either a single JSON object
// (per-app invocation) or an array of such objects (plugin-framework
// variants); an array selects its sole object. A zero-length array parses
// as an empty map so callers can render "unreported" rather than error.
func parseObject(stdout []byte) (map[string]any, error) {
	var (
		obj  map[string]any
		arr  []map[string]any
		anyv any
	)
	if err := json.Unmarshal(stdout, &arr); err == nil {
		if len(arr) == 0 {
			return map[string]any{}, nil
		}
		if len(arr) == 1 {
			return arr[0], nil
		}
		return nil, fmt.Errorf("report: unexpected %d objects", len(arr))
	}
	if err := json.Unmarshal(stdout, &obj); err == nil {
		return obj, nil
	}
	if err := json.Unmarshal(stdout, &anyv); err != nil {
		return nil, fmt.Errorf("report: not JSON: %w", err)
	}
	return nil, fmt.Errorf("report: unexpected JSON shape %T", anyv)
}

// reportString returns the first non-empty string among the candidate
// keys (0.38.x emits stripped + legacy-prefixed aliases). Values are
// often plain strings; numbers/bools render via fmt.
func reportString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		s := strings.TrimSpace(fmt.Sprintf("%v", v))
		if s != "" {
			return s
		}
	}
	return ""
}

// reportBool reads a boolean report value that Dokku may emit as a real
// JSON bool or as the strings "true"/"false"/"1"/"0".
func reportBool(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case bool:
			return t
		case string:
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "true", "1", "yes":
				return true
			case "false", "0", "no":
				return false
			}
		}
	}
	return false
}

// portEntry mirrors ports:report --ports-map-json.
type portEntry struct {
	Scheme        string `json:"scheme"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
}

// parsePorts decodes the ports map array; an empty body is an empty slice.
func parsePorts(stdout []byte) ([]portEntry, error) {
	entries := []portEntry{}
	if err := json.Unmarshal(stdout, &entries); err == nil {
		return entries, nil
	}
	// Some framings may omit the array wrapper for a single object.
	obj := map[string]any{}
	if err := json.Unmarshal(stdout, &obj); err != nil {
		return nil, fmt.Errorf("ports report: not JSON: %w", err)
	}
	return nil, fmt.Errorf("ports report: unexpected shape")
}

// storageEntry mirrors storage:list <app> --format json.
type storageEntry struct {
	EntryName     string `json:"entry_name"`
	HostPath      string `json:"host_path"`
	ContainerPath string `json:"container_path"`
}

func parseStorage(stdout []byte) ([]storageEntry, error) {
	entries := []storageEntry{}
	if err := json.Unmarshal(stdout, &entries); err == nil {
		return entries, nil
	}
	obj := map[string]any{}
	if err := json.Unmarshal(stdout, &obj); err != nil {
		return nil, fmt.Errorf("storage report: not JSON: %w", err)
	}
	return nil, fmt.Errorf("storage report: unexpected shape")
}

// defaultProctype is the single process type the v0 modeled slice inspects
// resources for; per-proctype overlays are an M3 concern.
const defaultProctype = "web"

// resourceLimit reads the configured limit for the default web proctype
// from resource:report JSON ("web.limit.cpu" etc., plus legacy aliases).
func resourceLimit(m map[string]any, kind string) string {
	return reportString(m,
		defaultProctype+".limit."+kind,
		"_default_.limit."+kind,
		"resource-"+defaultProctype+".limit."+kind,
		"resource-_default_.limit."+kind)
}

// networkLists collects the networks an app is attached to from the
// network:report computed values (each may be space-separated list).
func networkLists(m map[string]any) []string {
	var nets []string
	for _, key := range []string{
		"computed-attach-post-create", "computed-attach-post-deploy",
		"computed-initial-network",
		"network-computed-attach-post-create", "network-computed-attach-post-deploy",
		"network-computed-initial-network",
	} {
		for _, net := range strings.Fields(reportString(m, key)) {
			if !slices.Contains(nets, net) {
				nets = append(nets, net)
			}
		}
	}
	slices.Sort(nets)
	return nets
}

// envKeysRe matches plausible environment-variable key lines in
// `config:keys` plain-text output; all other lines (dokku headers
// "-----> ...", warnings "! ...") are skipped.
var envKeysRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseEnvKeys turns `config:keys <app>` output into the sorted set of key
// NAMES. Values are never read here (ADR-0008).
func parseEnvKeys(stdout []byte) []string {
	var keys []string
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !envKeysRe.MatchString(line) {
			continue
		}
		if !slices.Contains(keys, line) {
			keys = append(keys, line)
		}
	}
	slices.Sort(keys)
	return keys
}
