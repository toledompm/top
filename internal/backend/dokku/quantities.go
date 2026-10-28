// SPDX-License-Identifier: MIT

package dokku

import (
	"math"
	"strconv"
	"strings"
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
// Documented as a deviation/proposal to docs/contracts/platform-backend.md
// (quantity translation at the adapter boundary; ADR-0002 amendment).

var memSuffixes = map[string]int64{
	"ki": 1 << 10,
	"mi": 1 << 20,
	"gi": 1 << 30,
	"ti": 1 << 40,
	"pi": 1 << 50,
	"ei": 1 << 60,
	"k":  1e3, "kb": 1e3,
	"m": 1e6, "mb": 1e6,
	"g": 1e9, "gb": 1e9,
	"t": 1e12, "tb": 1e12,
	"p": 1e15, "pb": 1e15,
	"b": 1,
}

// parseMemoryBytes parses a K8s-style memory quantity ("256Mi", "1Gi",
// "1000k", "268435456") into bytes. Only SI/multiplier suffixes; invalid
// → (0, false).
func parseMemoryBytes(q string) (int64, bool) {
	s := strings.ToLower(strings.TrimSpace(q))
	n := 0
	for n < len(s) && (s[n] >= '0' && s[n] <= '9' || n > 0 && s[n] == '.') {
		n++
	}
	if n == 0 {
		return 0, false
	}
	num, err := strconv.ParseFloat(s[:n], 64)
	if err != nil || num < 0 || math.IsNaN(num) {
		return 0, false
	}
	var mult int64
	suffix := strings.TrimSpace(s[n:])
	if suffix == "" {
		mult = 1
	} else {
		m, ok := memSuffixes[suffix]
		if !ok || num*float64(m) > math.MaxInt64 {
			return 0, false
		}
		mult = m
	}
	return int64(num * float64(mult)), true
}

// formatMemoryBytes renders bytes for docker's -m (bare integer).
func formatMemoryBytes(b int64) string { return strconv.FormatInt(b, 10) }

// parseCPUCores parses a K8s-style CPU quantity ("500m", "0.5", "2")
// into cores. Invalid → (0, false).
func parseCPUCores(q string) (float64, bool) {
	s := strings.TrimSpace(q)
	milli := false
	if s != "" && strings.HasSuffix(s, "m") {
		milli = true
		s = s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	if milli {
		v /= 1000
	}
	return v, true
}

// formatCPUCores renders cores for docker's --cpus (decimal float).
func formatCPUCores(c float64) string {
	s := strconv.FormatFloat(c, 'f', -1, 64)
	return s
}

// cpuSame reports semantic equality of two CPU quantities (falling back
// to string equality when unparseable).
func cpuSame(a, b string) bool {
	if a == b {
		return true
	}
	pa, oka := parseCPUCores(a)
	pb, okb := parseCPUCores(b)
	return oka && okb && pa == pb
}

// memSame reports semantic equality of two memory quantities (falling
// back to string equality when unparseable).
func memSame(a, b string) bool {
	if a == b {
		return true
	}
	pa, oka := parseMemoryBytes(a)
	pb, okb := parseMemoryBytes(b)
	return oka && okb && pa == pb
}
