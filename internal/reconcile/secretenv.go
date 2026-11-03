// SPDX-License-Identifier: MIT

package reconcile

import (
	"fmt"
	"strings"

	"github.com/toledompm/top/internal/domain"
)

// SecretEnvKey is the T-403 secretRef → env-var mapping rule (ADR-0008's
// "names use service-name syntax" stays; the platform repo README's
// "T-403 wiring" item 6 pins the convention): the secret NAME is the
// dash-kebab form of the env var it feeds — dashes become underscores,
// then the result is uppercased. So `gf-security-admin-password`
// injects as GF_SECURITY_ADMIN_PASSWORD, the exact env name Grafana
// requires; plain uppercase names (e.g. SMTP_PASSWORD) round-trip.
//
// The mapping is applied ONLY when the resolved value is written as
// container env (apply's secretRef path). Everything else — diff
// vocabulary ("secretRefs.<name>"), logs, records — carries the NAME
// verbatim; values never surface (ADR-0008).
func SecretEnvKey(name string) (string, error) {
	if strings.Contains(name, "_") {
		return "", fmt.Errorf("%w: secret names are service-name syntax ([a-z0-9-], ADR-0008): no underscore in %q; use the dash-kebab form of the env var (e.g. gf-security-admin-password)", domain.ErrInvalid, name)
	}
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_")), nil
}
