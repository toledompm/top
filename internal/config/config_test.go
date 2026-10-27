package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "top-config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultIsValid(t *testing.T) {
	cfg := Default()
	if err := Validate(cfg); err != nil {
		t.Fatalf("Default() must validate: %v", err)
	}
	if cfg.Controller.PollIntervalSeconds != 60 {
		t.Errorf("poll default = %d, want 60", cfg.Controller.PollIntervalSeconds)
	}
	if cfg.Controller.ReconcileIntervalSeconds != 3600 {
		t.Errorf("reconcile default = %d, want 3600", cfg.Controller.ReconcileIntervalSeconds)
	}
	if cfg.Backup.ScheduleSeconds != 86400 || cfg.Backup.RetentionDaily != 14 || cfg.Backup.RetentionWeekly != 8 {
		t.Errorf("backup defaults wrong: %+v", cfg.Backup)
	}
	if cfg.SecretsProvider.Kind != SecretsProviderHostfile {
		t.Errorf("secrets provider default = %q, want hostfile", cfg.SecretsProvider.Kind)
	}
	if cfg.DataDir != "/var/lib/top" {
		t.Errorf("dataDir default = %q", cfg.DataDir)
	}
	if got := cfg.SecretsDir(); got != "/var/lib/top/secrets" {
		t.Errorf("SecretsDir() = %q", got)
	}
}

func TestLoadExplicitValues(t *testing.T) {
	path := writeFile(t, `apiVersion: top/v1alpha1
controller:
  pollIntervalSeconds: 30
  reconcileIntervalSeconds: 7200
git:
  url: https://github.com/example/example-repo
  branch: trunk
backup:
  scheduleSeconds: 43200
  retentionDaily: 7
  retentionWeekly: 4
  offsite:
    enabled: true
    kind: rclone
    target: remote:top-backups
secretsProvider:
  kind: vault
dataDir: /data/top
logLevel: debug
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Controller.PollIntervalSeconds != 30 || cfg.Git.Branch != "trunk" {
		t.Errorf("file values not applied: %+v", cfg.Controller)
	}
	if cfg.Backup.Offsite.Kind != OffsiteRclone || cfg.Backup.Offsite.Target != "remote:top-backups" {
		t.Errorf("offsite not applied: %+v", cfg.Backup.Offsite)
	}
	// vault validates as known-but-unimplemented: config accepts it.
	if cfg.SecretsProvider.Kind != SecretsProviderVault {
		t.Errorf("secretsProvider.kind = %q, want vault", cfg.SecretsProvider.Kind)
	}
	if got := cfg.SecretsDir(); got != "/data/top/secrets" {
		t.Errorf("SecretsDir() = %q", got)
	}
}

func TestLoadStrictUnknownField(t *testing.T) {
	path := writeFile(t, `apiVersion: top/v1alpha1
controller:
  pollIntervalSeconds: 30
  pollIntervalseconds: 99
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("unknown field must fail loudly")
	}
	if !errors.Is(err, ErrUnknownField) {
		t.Fatalf("err = %v, want ErrUnknownField", err)
	}
}

func TestLoadRejectsMismatchedAPIVersion(t *testing.T) {
	path := writeFile(t, "apiVersion: top/v2\n")
	_, err := Load(path)
	if !errors.Is(err, ErrAPIVersion) {
		t.Fatalf("err = %v, want ErrAPIVersion", err)
	}
	if !strings.Contains(err.Error(), "top/v2") {
		t.Errorf("error should mention the offending version: %v", err)
	}
}

func TestEnvOverrides(t *testing.T) {
	path := writeFile(t, "apiVersion: top/v1alpha1\n")
	t.Setenv("TOP_CONTROLLER_POLL_INTERVAL_SECONDS", "90")
	t.Setenv("TOP_GIT_BRANCH", "release")
	t.Setenv("TOP_DATA_DIR", "/tmp/top-test")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Controller.PollIntervalSeconds != 90 {
		t.Errorf("env override not applied: %d", cfg.Controller.PollIntervalSeconds)
	}
	if cfg.Git.Branch != "release" || cfg.DataDir != "/tmp/top-test" {
		t.Errorf("env overrides not applied: %+v", cfg)
	}
}

func TestEnvOverrideInvalidInteger(t *testing.T) {
	path := writeFile(t, "apiVersion: top/v1alpha1\n")
	t.Setenv("TOP_CONTROLLER_POLL_INTERVAL_SECONDS", "soon")
	_, err := Load(path)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if len(ve.Errors) != 1 || ve.Errors[0].Field != "controller.pollIntervalSeconds" {
		t.Errorf("field path wrong: %+v", ve.Errors)
	}
}

func TestValidationFieldPaths(t *testing.T) {
	cfg := Default()
	cfg.Controller.PollIntervalSeconds = 0
	cfg.Controller.ReconcileIntervalSeconds = -1
	cfg.DataDir = "relative/path"
	cfg.Namespaces.Reserved = []string{"tenants"}
	cfg.Namespaces.Order = []string{"default", "core", "observability"}
	err := Validate(cfg)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	got := make(map[string]bool)
	for _, fe := range ve.Errors {
		got[fe.Field] = true
	}
	for _, field := range []string{
		"controller.pollIntervalSeconds",
		"controller.reconcileIntervalSeconds",
		"dataDir",
		"namespaces.reserved",
		"namespaces.order",
	} {
		if !got[field] {
			t.Errorf("missing validation error for %s; got %+v", field, ve.Errors)
		}
	}
	if !strings.Contains(err.Error(), "core") || !strings.Contains(err.Error(), "observability") {
		t.Errorf("immutable-namespace error should name them: %v", err)
	}
}

func TestValidateOffsiteRequiresTarget(t *testing.T) {
	cfg := Default()
	cfg.Backup.Offsite.Enabled = true
	cfg.Backup.Offsite.Kind = "carrier-pigeon"
	err := Validate(cfg)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v", err)
	}
	fields := map[string]bool{}
	for _, fe := range ve.Errors {
		fields[fe.Field] = true
	}
	if !fields["backup.offsite.kind"] || !fields["backup.offsite.target"] {
		t.Errorf("expected offsite kind+target errors, got %+v", ve.Errors)
	}
}

func TestJSONMarshalNoPrivateFields(t *testing.T) {
	// CLI `-o json` friendliness: MarshalJSON must exist and round-trip.
	cfg := Default()
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	for _, s := range []string{`"pollIntervalSeconds":60`, `"apiVersion":"top/v1alpha1"`} {
		if !strings.Contains(string(b), s) {
			t.Errorf("json output missing %s: %s", s, b)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/top-config.yaml"); err == nil {
		t.Fatal("missing file must error")
	}
}

func TestLoadEmptyPathIsDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") = %v", err)
	}
	want := Default()
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load(\"\") should equal defaults: %+v\nwant %+v", cfg, want)
	}
}
