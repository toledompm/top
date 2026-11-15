// SPDX-License-Identifier: MIT

package domain

import (
	"math"
	"strconv"
	"strings"
)

// Resource quantities in the spec are Kubernetes-flavored ("500m" cores,
// "256Mi"); dokku forwards values VERBATIM through to `docker container
// create`, which only understands docker-native forms ("0.5" cores,
// IEC "mi"/"gi"). Tier-3 verified 2026-10-28 (dokku 0.38.32): a spec value
// like "256Mi" is rejected outright by docker's -m flag.
//
// This module is SHARED VOCABULARY on purpose: the adapter translates
// spec quantities before `resource:limit`, and both apply paths'
// convergence guards compare OBSERVED (docker-native: "0.5",
// "268435456") against DESIRED (spec spelling: "500m", "256Mi")
// semantically — a literal compare re-proposes converged limits every
// run (live finding 2026-11-15, media-server: the resources phantom kept
// every plan non-empty and re-drove deploys that should have been
// no-ops).
//
// Documented as a deviation/proposal to docs/contracts/platform-backend.md
// (resource limits as K8s-style quantities in the spec; docker-native on
// the wire).

var memSuffixes = map[string]int64{
	"kb": 1e3, "k": 1e3,
	"mb": 1e6, "m": 1e6,
	"gb": 1e9, "g": 1e9,
	"tb": 1e12, "t": 1e12,
	"pb": 1e15, "p": 1e15,
	"ki": 1024, "mi": 1024 * 1024, "gi": 1024 * 1024 * 1024,
	"ti": 1024 * 1024 * 1024 * 1024,
	"b":  1,
}

// ParseMemoryBytes parses a K8s-style memory quantity ("256Mi", "1Gi",
// "1000k", "268435456") into bytes. Only SI/multiplier suffixes; invalid
// → (0, false).
func ParseMemoryBytes(q string) (int64, bool) {
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

// FormatMemoryBytes renders bytes for docker's -m (bare integer).
func FormatMemoryBytes(b int64) string { return strconv.FormatInt(b, 10) }

// ParseCPUCores parses a K8s-style CPU quantity ("500m", "0.5", "2")
// into cores. Invalid → (0, false).
func ParseCPUCores(q string) (float64, bool) {
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

// FormatCPUCores renders cores for docker's --cpus (decimal float).
func FormatCPUCores(c float64) string {
	return strconv.FormatFloat(c, 'f', -1, 64)
}

// CPUCoresSame reports semantic equality of two CPU quantities (falling
// back to string equality when unparseable).
func CPUCoresSame(a, b string) bool {
	if a == b {
		return true
	}
	pa, oka := ParseCPUCores(a)
	pb, okb := ParseCPUCores(b)
	return oka && okb && pa == pb
}

// MemorySame reports semantic equality of two memory quantities (falling
// back to string equality when unparseable).
func MemorySame(a, b string) bool {
	if a == b {
		return true
	}
	pa, oka := ParseMemoryBytes(a)
	pb, okb := ParseMemoryBytes(b)
	return oka && okb && pa == pb
}
