// SPDX-License-Identifier: MIT
package inventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/toledompm/top/internal/exec"
)

// The exact read-only commands Snapshot may run. Everything here observes;
// nothing mutates the host (ADR-0001: read-only Docker observations are
// legitimate, mutations only via the dokku CLI upstream of this package).
var (
	binDocker = "docker"
	binSS     = "ss"
	binDokku  = "dokku"
)

// SnapshotOptions configures one Snapshot run.
type SnapshotOptions struct {
	// Namespaces is the set of TOP logical namespaces whose "<ns>-<name>"
	// prefix marks layer-1 identification. Unset defaults to
	// config.NamespacePolicyOrder.
	Namespaces []string

	// MachineIDSeed is the pre-read machine identifier content. In
	// production HostID is derived from /etc/machine-id; tests inject a
	// fake seed here so they never depend on the local /etc.
	MachineIDSeed string

	// Now overrides the snapshot timestamp; zero means time.Now (UTC).
	Now time.Time
}

// Snapshot captures the host state relevant to ownership decisions via the
// exec Runner. Every command is read-only:
//
//	docker ps -a --format json          (newline-delimited JSON objects;
//	                                     the CLI-documented "json" shorthand)
//	docker network ls --format json
//	docker volume ls --format json
//	ss -tlnH                            (TCP listeners; degraded when absent)
//	dokku apps:list --format json
//
// The inventory composes only existing ad-hoc state: it runs the dokku CLI
// directly because on first install no backend adapter exists yet, and at
// teardown the store must be fed from live reads, not controller state.
//
// All sections degrade independently: a failed or unparseable command is
// recorded as a Finding (with a stable reason tag) and the rest of the
// snapshot still lands.
func Snapshot(ctx context.Context, r exec.Runner, opts SnapshotOptions) (*HostInventory, error) {
	snap := &HostInventory{
		Containers: []Container{},
		Networks:   []Network{},
		Volumes:    []Volume{},
		Listeners:  []Listener{},
		DokkuApps:  []string{},
		Findings:   []Finding{},
	}
	snap.CreatedAt = opts.Now
	if snap.CreatedAt.IsZero() {
		snap.CreatedAt = time.Now().UTC()
	}
	snap.HostID = hostID(opts.MachineIDSeed, snap)

	snap.Containers = captureContainers(ctx, r, snap)
	snap.Networks = captureNamedList(ctx, r, snap, "networks", "network", "ls")
	snap.Volumes = captureVolumesFromNamedList(ctx, r, snap)
	snap.Listeners = captureListeners(ctx, r, snap)
	snap.DokkuApps = captureDokkuApps(ctx, r, snap)
	return snap, nil
}

// hostID derives the anonymous host id (see HostInventory.HostID) from the
// seed, or /etc/machine-id when the caller has not supplied one. An
// unreadable/empty source becomes a Finding and an empty HostID — never an
// invented value.
func hostID(seed string, snap *HostInventory) string {
	data := seed
	if seed == "" {
		b, err := os.ReadFile("/etc/machine-id")
		if err != nil {
			snap.Findings = append(snap.Findings, Finding{Component: "host", Reason: "machine-id-unreadable: " + err.Error()})
			return ""
		}
		data = strings.TrimSpace(string(b))
	}
	if data == "" {
		snap.Findings = append(snap.Findings, Finding{Component: "host", Reason: "machine-id-empty: seed content empty"})
		return ""
	}
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:16])
}

// findingf appends a Finding with a stable reason tag and detail.
func findingf(snap *HostInventory, component, subject, tag, format string, a ...any) {
	f := Finding{Component: component, Reason: tag + ": " + fmt.Sprintf(format, a...)}
	if subject != "" {
		f.Subject = subject
	}
	snap.Findings = append(snap.Findings, f)
}

// --- docker sources ---------------------------------------------------------

// dockerPSLine is one object of `docker ps -a --format json`. Docker renders
// Names as "/name" and Labels as a comma-joined "k=v" string.
type dockerPSLine struct {
	ID     string `json:"ID"`
	Names  string `json:"Names"`
	Image  string `json:"Image"`
	State  string `json:"State"`
	Labels string `json:"Labels"`
}

func captureContainers(ctx context.Context, r exec.Runner, snap *HostInventory) []Container {
	containers := []Container{}
	out, _, err := r.Run(ctx, binDocker, "ps", "-a", "--format", "json")
	if err != nil {
		findingf(snap, "containers", "", "docker-ps-failed", "%v", err)
		return containers
	}
	for _, line := range jsonLines(out) {
		var l dockerPSLine
		if uerr := json.Unmarshal(line, &l); uerr != nil {
			findingf(snap, "containers", string(truncate(line)), "docker-ps-parse", "%v", uerr)
			continue
		}
		containers = append(containers, Container{
			ID:     l.ID,
			Name:   l.Names,
			Image:  l.Image,
			State:  l.State,
			Labels: parseDockerLabels(l.Labels),
		})
	}
	return containers
}

// dockerNamedLine is the shared shape of
// `docker network|volume ls --format json` rows: name, driver, and a Labels
// value whose shape varies across docker builds (object or "k=v" string).
type dockerNamedLine struct {
	Name   string          `json:"Name"`
	Driver string          `json:"Driver"`
	Labels json.RawMessage `json:"Labels"`
}

func captureNamedList(ctx context.Context, r exec.Runner, snap *HostInventory, component string, cmdArgs ...string) []Network {
	nets := []Network{}
	out, _, err := r.Run(ctx, binDocker, append(slices.Clone(cmdArgs), "--format", "json")...)
	if err != nil {
		findingf(snap, component, "", "docker-ls-failed", "%v", err)
		return nets
	}
	for _, line := range jsonLines(out) {
		var l dockerNamedLine
		if uerr := json.Unmarshal(line, &l); uerr != nil {
			findingf(snap, component, string(truncate(line)), "docker-parse", "%v", uerr)
			continue
		}
		nets = append(nets, Network{Name: l.Name, Driver: l.Driver, Labels: parseLabelsRaw(l.Labels)})
	}
	return nets
}

func captureVolumesFromNamedList(ctx context.Context, r exec.Runner, snap *HostInventory) []Volume {
	nets := captureNamedList(ctx, r, snap, "volumes", "volume", "ls")
	vols := make([]Volume, 0, len(nets))
	for _, n := range nets {
		vols = append(vols, Volume(n))
	}
	return vols
}

// parseDockerLabels splits docker's comma-joined "k=v,k2=v2" label string.
// Values containing commas are mangled (docker's ps output does not quote
// them); the limitation is acceptable because ownership decisions hinge on
// the top.dev/* keys' presence, not comma-carrying values.
func parseDockerLabels(joined string) map[string]string {
	if joined == "" {
		return nil
	}
	labels := map[string]string{}
	for _, pair := range strings.Split(joined, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if ok {
			labels[k] = v
		}
	}
	return labels
}

// parseLabelsRaw accepts a Labels field shaped as an object {"k":"v"} or a
// "k=v" string; anything else is nil (finding-free: no decision depends on
// foreign label shapes).
func parseLabelsRaw(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	asObj := map[string]string{}
	if err := json.Unmarshal(raw, &asObj); err == nil {
		return asObj
	}
	var asStr string
	if err := json.Unmarshal(raw, &asStr); err == nil {
		return parseDockerLabels(asStr)
	}
	return nil
}

// jsonLines splits newline-delimited JSON, skipping blank lines and
// removing surrounding whitespace per line.
func jsonLines(out []byte) [][]byte {
	lines := [][]byte{}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines = append(lines, []byte(line))
	}
	return lines
}

func truncate(line []byte) []byte {
	const keep = 64
	if len(line) <= keep {
		return line
	}
	return append(slices.Clone(line[:keep]), []byte("…")...)
}

// --- listeners (ss -tlnH) ---------------------------------------------------

// captureListeners records TCP listeners via ss -tlnH (headerless). The
// parse is deliberately shallow — see README fragility notes. When ss is
// missing entirely the fallback is honest degradation: a Finding marks the
// section unknown and capture proceeds without it (never an invented value).
func captureListeners(ctx context.Context, r exec.Runner, snap *HostInventory) []Listener {
	listeners := []Listener{}
	out, _, err := r.Run(ctx, binSS, "-tlnH")
	if err != nil {
		switch {
		case errors.Is(err, exec.ErrNotFound):
			findingf(snap, "listeners", "", "ss-missing", "ss binary not in PATH; listener capture skipped and ownership checks proceed without the port key")
		default:
			findingf(snap, "listeners", "", "ss-failed", "%v", err)
		}
		return listeners
	}
	for i, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// ss -tlnH rows: "LISTEN 0 128 0.0.0.0:22 0.0.0.0:*". Column 3 is
		// the local address.
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "LISTEN" {
			findingf(snap, "listeners", strconv.Itoa(i+1), "ss-parse", "unexpected row shape (%q)", truncate([]byte(line)))
			continue
		}
		listeners = append(listeners, Listener{LocalAddress: fields[3]})
	}
	slices.SortFunc(listeners, func(a, b Listener) int {
		return strings.Compare(a.LocalAddress, b.LocalAddress)
	})
	return listeners
}

// --- dokku -----------------------------------------------------------------

func captureDokkuApps(ctx context.Context, r exec.Runner, snap *HostInventory) []string {
	out, _, err := r.Run(ctx, binDokku, "apps:list", "--format", "json")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			findingf(snap, "dokku", "", "dokku-missing", "dokku binary not in PATH; app absence is unverified")
			return []string{}
		}
		findingf(snap, "dokku", "", "apps-list-failed", "%v", err)
		return []string{}
	}
	return parseDokkuApps(out, snap)
}

// parseDokkuApps accepts `dokku apps:list --format json`'s JSON array of
// strings; when an array of objects appears instead (dokku has shipped both
// shapes across releases) it reads the "name" keys. Unparseable output is a
// Finding, never an invented list.
func parseDokkuApps(out []byte, snap *HostInventory) []string {
	var asStrings []string
	if err := json.Unmarshal(out, &asStrings); err == nil {
		return nonEmpty(asStrings)
	}
	var asObjs []map[string]any
	if err := json.Unmarshal(out, &asObjs); err == nil {
		names := []string{}
		for _, o := range asObjs {
			if n, ok := o["name"].(string); ok {
				names = append(names, n)
			}
		}
		return nonEmpty(names)
	}
	findingf(snap, "dokku", "", "apps-list-parse", "unexpected JSON shape (%q)", strconv.Quote(string(truncate(out))))
	return []string{}
}

func nonEmpty(in []string) []string {
	out := []string{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// --- ownership queries over a snapshot -------------------------------------

// FindContainer looks a container up by name, tolerating docker's leading
// slash rendering.
func (h *HostInventory) FindContainer(name string) (Container, bool) {
	for _, c := range h.Containers {
		if c.Name == name || strings.TrimPrefix(c.Name, "/") == name {
			return c, true
		}
	}
	return Container{}, false
}

// FindNetwork looks a network up by identifier.
func (h *HostInventory) FindNetwork(name string) (Network, bool) {
	for _, n := range h.Networks {
		if n.Name == name {
			return n, true
		}
	}
	return Network{}, false
}

// FindVolume looks a volume up by identifier.
func (h *HostInventory) FindVolume(name string) (Volume, bool) {
	for _, v := range h.Volumes {
		if v.Name == name {
			return v, true
		}
	}
	return Volume{}, false
}

// HasApp reports whether dokku reported that app.
func (h *HostInventory) HasApp(name string) bool {
	for _, a := range h.DokkuApps {
		if a == name {
			return true
		}
	}
	return false
}

// JSON renders the snapshot for persistence (/var/lib/top and operation
// records). Marshaling plain data cannot fail here, but the error is
// surfaced rather than swallowed per repo convention.
func (h *HostInventory) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal host inventory: %w", err)
	}
	return b, nil
}
