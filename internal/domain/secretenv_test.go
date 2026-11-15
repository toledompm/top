// SPDX-License-Identifier: MIT

package domain

import (
	"strings"
	"testing"
)

// TestSecretEnvKeyDashKebabRule pins the secretRef → env-var mapping
// (T-403 wiring item 6): dashes become underscores, uppercased; the
// plain-uppercase round-trip; underscore in the NAME is a spec-validation
// error with the fix hint.
func TestSecretEnvKeyDashKebabRule(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"gf-security-admin-password": "GF_SECURITY_ADMIN_PASSWORD",
		"smtp-password":              "SMTP_PASSWORD",
		"api-token":                  "API_TOKEN",
		"x":                          "X",
	} {
		got, err := SecretEnvKey(name)
		if err != nil || got != want {
			t.Errorf("SecretEnvKey(%q) = %q,%v want %q,nil", name, got, err, want)
		}
	}
	if _, err := SecretEnvKey("has_underscore"); err == nil {
		t.Error("underscore in name must fail (names are [a-z0-9-], ADR-0008; use dash-kebab of the env var)")
	} else if !strings.Contains(err.Error(), "dash-kebab") {
		t.Errorf("error text should carry the fix hint, got: %v", err)
	}
}
