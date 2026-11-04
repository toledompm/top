// SPDX-License-Identifier: MIT

package config_test

import (
	"testing"

	"github.com/toledompm/top/internal/config"
)

// T-502 additions: backup.offsite port/timeout/options are validated additively.
func TestOffsiteT502FieldValidation(t *testing.T) { //nolint:dupl // focused field matrix
	cfg := config.Default()
	cfg.Backup.Offsite.Enabled = true
	cfg.Backup.Offsite.Kind = config.OffsiteRsyncSSH
	cfg.Backup.Offsite.Target = "backup@offsite.example:/srv/top-copies"
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	cfg.Backup.Offsite.Port = 70000
	if err := config.Validate(cfg); err == nil {
		t.Fatal("port 70000 accepted")
	}
	cfg.Backup.Offsite.Port = 2222
	cfg.Backup.Offsite.TimeoutSeconds = -5
	if err := config.Validate(cfg); err == nil {
		t.Fatal("negative timeout accepted")
	}
	cfg.Backup.Offsite.TimeoutSeconds = 0
	cfg.Backup.Offsite.Options = []string{"--delete extra"}
	if err := config.Validate(cfg); err == nil {
		t.Fatal("multi-atom option accepted (argv-only rule)")
	}
}
