// SPDX-License-Identifier: MIT

package dokku

import (
	"github.com/toledompm/top/internal/domain"
)

// Resource quantities in the spec are Kubernetes-flavored ("500m" cores,
// "256Mi" binary bytes), but dokku's resource:limit passes --cpu/--memory
// VERBATIM through to `docker container create`, which only understands
// plain core decimals and docker RAM suffixes (b/k/m/g/t/p — DEC, and no
// IEC "mi"/"gi"). Tier-3 verified 2026-10-28 (dokku 0.38.32): a spec value
// of "256Mi" reaches docker as -m 256Mi and the deploy dies with
//
//	invalid argument "256Mi" for "-m, --memory" flag: invalid suffix: 'mi'
//
// leaving no running container. The adapter therefore translates spec
// quantities into docker-native forms -- cores as a decimal float,
// memory as an integer byte count -- and compares limits SEMANTICALLY
// (both quantities parsed) so a converged app never re-mutes on
// re-apply even though the stored value's spelling differs from the
// spec's. Unparseable values fall back to verbatim strings.
//
// The translation itself is SHARED VOCABULARY (internal/domain/
// quantities.go): the plan's resource family needs the same semantic
// compare — a literal compare re-proposed converged limits every run and
// kept every plan non-empty (media-server 2026-11-15). These are thin
// unexported aliases so the adapter's callers stay untouched.

func parseMemoryBytes(q string) (int64, bool) { return domain.ParseMemoryBytes(q) }

func formatMemoryBytes(b int64) string { return domain.FormatMemoryBytes(b) }

func parseCPUCores(q string) (float64, bool) { return domain.ParseCPUCores(q) }

func formatCPUCores(c float64) string { return domain.FormatCPUCores(c) }

func cpuSame(a, b string) bool { return domain.CPUCoresSame(a, b) }

func memSame(a, b string) bool { return domain.MemorySame(a, b) }
