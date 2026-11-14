// SPDX-License-Identifier: MIT

package doctor

import (
	"bytes"
	"testing"
)

// findingFixture covers every severity so both goldens exercise the full
// shape (hint emission, alignment, counts).
func findingFixture() []Finding {
	return []Finding{
		{ID: "os/distro", Severity: SeverityOK, Check: "operating system distribution", Message: "ubuntu 24.04 is a supported distribution"},
		{
			ID: "dokku/state", Severity: SeverityInfo, Check: "dokku present", Message: "dokku is not installed; TOP can install it (approval-gated)",
			Hint: "re-run `topctl install --install-dokku` to install via the documented bootstrap",
		},
		{
			ID: "resources/disk", Severity: SeverityWarn, Check: "disk headroom", Message: "12 GiB free on /; below the 20 GiB recommended headroom",
			Hint: "free space or expect tuning",
		},
		{
			ID: "docker/daemon", Severity: SeverityFail, Check: "docker daemon reachable", Message: "docker is installed but the daemon is not reachable (exit 1)",
			Hint: "on a host distro: `sudo systemctl start docker`",
		},
	}
}

// TestRenderTextGolden pins the human report shape (ADR-0009 spirit: the
// fix is printed next to the finding).
func TestRenderTextGolden(t *testing.T) {
	t.Parallel()
	r := Report{Findings: findingFixture()}
	var b bytes.Buffer
	if err := r.RenderText(&b); err != nil {
		t.Fatal(err)
	}
	want := "doctor: doctor found 1 failure(s) [docker/daemon], 1 warning(s) \u2014 fix the failures before installing\n" +
		"  ok   [os/distro]            operating system distribution: ubuntu 24.04 is a supported distribution\n" +
		"  info [dokku/state]          dokku present: dokku is not installed; TOP can install it (approval-gated)\n" +
		"        hint: re-run `topctl install --install-dokku` to install via the documented bootstrap\n" +
		"  warn [resources/disk]       disk headroom: 12 GiB free on /; below the 20 GiB recommended headroom\n" +
		"        hint: free space or expect tuning\n" +
		"  fail [docker/daemon]        docker daemon reachable: docker is installed but the daemon is not reachable (exit 1)\n" +
		"        hint: on a host distro: `sudo systemctl start docker`\n"
	if got := b.String(); got != want {
		t.Fatalf("text render:\n%q\nwant:\n%q", got, want)
	}
}

// TestRenderJSONGolden pins the standalone doctor JSON object shape
// (one compact line, schemaVersion 1, command "doctor").
func TestRenderJSONGolden(t *testing.T) {
	t.Parallel()
	r := Report{Findings: findingFixture()}
	var b bytes.Buffer
	if err := r.RenderJSON(&b); err != nil {
		t.Fatal(err)
	}
	want := `{"schemaVersion":1,"command":"doctor","ok":false,"findings":[` +
		`{"id":"os/distro","severity":"ok","check":"operating system distribution","message":"ubuntu 24.04 is a supported distribution"},` +
		`{"id":"dokku/state","severity":"info","check":"dokku present","message":"dokku is not installed; TOP can install it (approval-gated)","hint":"re-run ` + "`topctl install --install-dokku`" + ` to install via the documented bootstrap"},` +
		`{"id":"resources/disk","severity":"warn","check":"disk headroom","message":"12 GiB free on /; below the 20 GiB recommended headroom","hint":"free space or expect tuning"},` +
		`{"id":"docker/daemon","severity":"fail","check":"docker daemon reachable","message":"docker is installed but the daemon is not reachable (exit 1)","hint":"on a host distro: ` + "`sudo systemctl start docker`" + `"}` + `]}` + "\n"
	if got := b.String(); got != want {
		t.Fatalf("json render:\n%q\nwant:\n%q", got, want)
	}
}

// TestRenderJSONEmpty: a nil Findings slice renders as [] not null.
func TestRenderJSONEmpty(t *testing.T) {
	t.Parallel()
	r := Report{}
	var b bytes.Buffer
	if err := r.RenderJSON(&b); err != nil {
		t.Fatal(err)
	}
	// visible: findings must be an array, not the JSON null
	if got := b.String(); got != `{"schemaVersion":1,"command":"doctor","ok":true,"findings":[]}`+"\n" {
		t.Fatalf("empty json render: %q", got)
	}
}

// TestEnvelopeDataShape pins the payload cli nests into its Envelope.Data
// (same keys both renderers speak).
func TestEnvelopeDataShape(t *testing.T) {
	t.Parallel()
	r := Report{Findings: findingFixture()}
	data := r.EnvelopeData()
	if got := data["ok"]; got != false {
		t.Errorf("ok = %v, want false", got)
	}
	if got := data["failCount"]; got != 1 {
		t.Errorf("failCount = %v, want 1", got)
	}
	if got := data["warnCount"]; got != 1 {
		t.Errorf("warnCount = %v, want 1", got)
	}
	if got := data["minDokku"]; got != MinDokkuVersion {
		t.Errorf("minDokku = %v, want %v", got, MinDokkuVersion)
	}
	findings, ok := data["findings"].([]Finding)
	if !ok || len(findings) != 4 {
		t.Fatalf("findings = %#v", data["findings"])
	}
}
