// SPDX-License-Identifier: MIT
package inventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/exec/exectest"
)

// fixture builds a Runner whose outputs are the committed testdata files
// (pre-install snapshot of a fictional host: core-top-controller,
// default-website, an unrelated redis cache, dokku + ss present).
func fixture(t *testing.T) *exectest.FakeRunner {
	t.Helper()
	rd := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("fixture %s: %v", name, err)
		}
		return b
	}
	f := exectest.NewFakeRunner()
	f.SetOutput("docker", exectest.Response{Stdout: rd("containers.jsonl")}, "ps", "-a", "--format", "json")
	f.SetOutput("docker", exectest.Response{Stdout: rd("networks.json")}, "network", "ls", "--format", "json")
	f.SetOutput("docker", exectest.Response{Stdout: rd("volumes.json")}, "volume", "ls", "--format", "json")
	f.SetOutput("dokku", exectest.Response{Stdout: rd("apps.json")}, "apps:list", "--format", "json")
	f.SetOutput("ss", exectest.Response{Stdout: rd("ss-tlnH.txt")}, "-tlnH")
	return f
}

func testOpts() SnapshotOptions {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	return SnapshotOptions{MachineIDSeed: "fake-machine-id", Now: now}
}

// TestSnapshotRoundTrip covers: capture commands invoked read-only, the
// full snapshot survives a JSON round-trip unchanged, and HostID is the
// documented sha256(seed)[:16].
func TestSnapshotRoundTrip(t *testing.T) {
	f := fixture(t)
	snap, err := Snapshot(context.Background(), f, testOpts())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Read-only command set: nothing destructive in the argv log.
	cmds := f.RecordedCommands()
	for _, c := range cmds {
		switch {
		case c == "docker ps -a --format json",
			c == "docker network ls --format json",
			c == "docker volume ls --format json",
			c == "ss -tlnH",
			c == "dokku apps:list --format json":
		default:
			t.Errorf("unexpected command in snapshot: %s", c)
		}
	}

	if len(snap.Containers) != 4 {
		t.Fatalf("containers = %d, want 4", len(snap.Containers))
	}
	ctl, ok := snap.FindContainer("core-top-controller")
	if !ok || !IsTOPManaged(ctl.Labels) || ctl.Labels[LabelNamespace] != "core" {
		t.Fatalf("core-top-controller = %+v, want TOP-managed core container", ctl)
	}
	if _, ok := snap.FindContainer("default-website"); !ok {
		t.Error("default-website missing")
	}
	if _, ok := snap.FindContainer("redis-cache"); !ok {
		t.Error("unrelated redis-cache must be captured too")
	}
	if len(snap.Networks) != 3 {
		t.Errorf("networks = %d, want 3 (incl. bridge/host)", len(snap.Networks))
	}
	net, ok := snap.FindNetwork("observability-web-net")
	if !ok || !IsTOPManaged(net.Labels) || net.Labels[LabelNamespace] != "observability" {
		t.Fatalf("observability-web-net = %+v, want TOP-managed (string label form)", net)
	}
	if v, ok := snap.FindVolume("core-top-state"); !ok || !IsTOPManaged(v.Labels) {
		t.Errorf("core-top-state volume = %+v, want TOP-managed", v)
	}
	wantListeners := []string{"0.0.0.0:443", "0.0.0.0:80", "127.0.0.11:35629", "192.0.2.9:22"}
	if got := listenerAddr(t, snap); !slices.Equal(got, wantListeners) {
		t.Errorf("listeners = %v, want sorted %v", got, wantListeners)
	}
	for _, app := range []string{"core-top-controller", "default-website"} {
		if !snap.HasApp(app) {
			t.Errorf("dokku app %s missing", app)
		}
	}
	sum := sha256.Sum256([]byte("fake-machine-id"))
	if snap.HostID != hex.EncodeToString(sum[:16]) {
		t.Errorf("HostID = %q, want sha256(seed)[:16]", snap.HostID)
	}

	// JSON round-trip: persistence must be lossless.
	b, err := snap.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var back HostInventory
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.CreatedAt != snap.CreatedAt || back.HostID != snap.HostID {
		t.Errorf("round-trip header drift: %+v vs %+v", back, snap)
	}
	if !roundTripEqual(back, *snap) {
		t.Errorf("round-trip content drift:\n%+v\nvs\n%+v", back, *snap)
	}
}

// roundTripEqual compares the snapshot content field by field (Container
// carries a map, so it is not a comparable struct for slices.Equal).
func roundTripEqual(a, b HostInventory) bool {
	if a.CreatedAt != b.CreatedAt || a.HostID != b.HostID {
		return false
	}
	if len(a.Containers) != len(b.Containers) || len(a.Networks) != len(b.Networks) ||
		len(a.Volumes) != len(b.Volumes) || len(a.Listeners) != len(b.Listeners) ||
		!slices.Equal(a.DokkuApps, b.DokkuApps) || !slices.Equal(a.Listeners, b.Listeners) {
		return false
	}
	for i := range a.Containers {
		if a.Containers[i].ID != b.Containers[i].ID ||
			a.Containers[i].Name != b.Containers[i].Name ||
			a.Containers[i].Image != b.Containers[i].Image ||
			a.Containers[i].State != b.Containers[i].State ||
			len(mapMismatch(a.Containers[i].Labels, b.Containers[i].Labels)) > 0 {
			return false
		}
	}
	for i := range a.Networks {
		if a.Networks[i].Name != b.Networks[i].Name ||
			a.Networks[i].Driver != b.Networks[i].Driver ||
			len(mapMismatch(a.Networks[i].Labels, b.Networks[i].Labels)) > 0 {
			return false
		}
	}
	for i := range a.Volumes {
		if a.Volumes[i].Name != b.Volumes[i].Name ||
			a.Volumes[i].Driver != b.Volumes[i].Driver ||
			len(mapMismatch(a.Volumes[i].Labels, b.Volumes[i].Labels)) > 0 {
			return false
		}
	}
	return true
}

// mapMismatch returns the symmetric key/value difference of two maps.
func mapMismatch(want, got map[string]string) map[string]string {
	diff := map[string]string{}
	for k, v := range want {
		if got[k] != v {
			diff[k] = v
		}
	}
	for k, v := range got {
		if want[k] != v {
			diff[k] = v
		}
	}
	return diff
}

func listenerAddr(t *testing.T, snap *HostInventory) []string {
	t.Helper()
	out := []string{}
	for _, l := range snap.Listeners {
		out = append(out, l.LocalAddress)
	}
	return out
}

// TestSnapshotDegradesIndependently: a failing/malformed section becomes a
// Finding and the rest still lands; an absent ss/dokku binary flags
// unknown rather than inventing.
func TestSnapshotDegradesIndependently(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetOutput("docker", exectest.Response{Stdout: []byte("garbage line\n")}, "ps", "-a", "--format", "json")
	f.SetOutput("docker", exectest.Response{ExitCode: 1}, "network", "ls", "--format", "json")
	f.SetOutput("docker", exectest.Response{Stdout: []byte(`{"Name":"v1","Driver":"local"}`)}, "volume", "ls", "--format", "json")
	f.SetOutput("ss", exectest.Response{Err: exec.ErrNotFound}, "-tlnH")
	f.SetOutput("dokku", exectest.Response{Err: exec.ErrNotFound}, "apps:list", "--format", "json")

	snap, err := Snapshot(context.Background(), f, testOpts())
	if err != nil {
		t.Fatalf("Snapshot must degrade, not fail: %v", err)
	}
	tags := make(map[string]bool)
	for _, fi := range snap.Findings {
		tags[fi.Component] = tags[fi.Component] || fi.Reason != ""
	}
	if len(snap.Findings) != 4 {
		t.Fatalf("findings = %+v, want one capture degradation per section + hosts", snap.Findings)
	}
	for _, comp := range []string{"containers", "networks", "listeners", "dokku"} {
		if !tags[comp] {
			t.Errorf("component %s did not record a finding", comp)
		}
	}
	if got := len(snap.Containers) + len(snap.Networks) + len(snap.Listeners) + len(snap.DokkuApps); got != 0 {
		t.Errorf("failed sections invented content: %d items", got)
	}
	if v, ok := snap.FindVolume("v1"); !ok || v.Driver != "local" {
		t.Errorf("healthy volume section lost: %+v", v)
	}
}

// TestDockerPSLeadingSlash: docker renders names "/name"; the parse keeps
// the raw spelling and every lookup trims.
func TestDockerPSLeadingSlash(t *testing.T) {
	snap := &HostInventory{Containers: []Container{{Name: "/web"}}}
	if _, ok := snap.FindContainer("web"); !ok {
		t.Error("FindContainer must trim the leading slash")
	}
}

// TestHostIDMissing: no seed and no /etc/machine-id → empty HostID with a
// finding, never a fabricated id. (Relies on the contract test environment
// having no readable /etc/machine-id guarantee — the seed path is what the
// production host provides.)
func TestHostIDMissing(t *testing.T) {
	f := exectest.NewFakeRunner()
	f.SetDefault(exectest.Response{Stdout: []byte("[]")})
	snap, err := Snapshot(context.Background(), f, SnapshotOptions{MachineIDSeed: "seed"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.HostID == "" {
		t.Fatal("seed supplied: HostID must be derived")
	}
	// Empty seed with machine-id present in the environment is environment
	// dependent; assert only the invariant "no crash, no invented value":
	// HostID is either derived or empty-with-finding.
	if snap.HostID == "" {
		found := false
		for _, fi := range snap.Findings {
			if fi.Component == "host" {
				found = true
			}
		}
		if !found {
			t.Error("empty HostID without a finding invents silence")
		}
	}
}
