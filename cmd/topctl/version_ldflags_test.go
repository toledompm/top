// SPDX-License-Identifier: MIT
package main

// T-507: the release recipe (.goreleaser.yaml) injects the build stamp via
// -ldflags "-X main.version={{ .Version }}", and the same string feeds
// top_build_info{version} through internal/cli -> internal/obs. This test
// pins the WIRE PATH: building with the exact release flag must surface the
// injected version through `topctl version`. Tier-1 (hermetic, no host).
import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestVersionIsLdflagsInjectable(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "topctl")
	want := "v9.9.9-ldflags-test"
	cmd := exec.Command("go", "build", "-o", bin,
		"-ldflags", "-X main.version="+want, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out2, err := exec.Command(bin, "version", "-o", "json").Output()
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	var env struct {
		SchemaVersion int    `json:"schemaVersion"`
		Command       string `json:"command"`
		Data          struct {
			Topctl string `json:"topctl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out2, &env); err != nil {
		t.Fatalf("unmarshal version envelope: %v (%s)", err, out2)
	}
	if env.Command != "version" || env.SchemaVersion != 1 {
		t.Errorf("version envelope: command=%q schemaVersion=%d", env.Command, env.SchemaVersion)
	}
	if env.Data.Topctl != want {
		t.Errorf("version data[topctl] = %q, want %q (ldflags -X main.version)", env.Data.Topctl, want)
	}
}
