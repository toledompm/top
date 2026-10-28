// SPDX-License-Identifier: MIT

package dokku

import "testing"

func TestParseMemoryBytes(t *testing.T) {
	cases := map[string]struct {
		want   int64
		plural bool
	}{
		"256Mi": {want: 268435456, plural: true},
		"1Gi":   {want: 1073741824, plural: true},
		"1000k": {want: 1_000_000, plural: true},
		"1g":    {want: 1_000_000_000, plural: true},
		"512":   {want: 512, plural: true},
		"0":     {want: 0, plural: true},
		"256m":  {want: 256_000_000, plural: true},
		"":      {plural: false},
		"256Qi": {plural: false},
		"Mi":    {plural: false},
		"-1Gi":  {plural: false},
	}
	for q, c := range cases {
		got, ok := parseMemoryBytes(q)
		if ok != c.plural {
			t.Errorf("%q: ok=%v want %v", q, ok, c.plural)
		}
		if ok && got != c.want {
			t.Errorf("%q:=%d want %d", q, got, c.want)
		}
	}
}

func TestParseCPUCores(t *testing.T) {
	cases := map[string]struct {
		want   float64
		plural bool
	}{
		"500m":  {want: 0.5, plural: true},
		"2":     {want: 2, plural: true},
		"0.5":   {want: 0.5, plural: true},
		"1500m": {want: 1.5, plural: true},
		"":      {plural: false},
		"x":     {plural: false},
	}
	for q, c := range cases {
		got, ok := parseCPUCores(q)
		if ok != c.plural {
			t.Errorf("%q: ok=%v want %v", q, ok, c.plural)
		}
		if ok && got != c.want {
			t.Errorf("%q:=%v want %v", q, got, c.want)
		}
	}
}

func TestQuantitySameCPU(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"500m", "0.5", true},
		{"1", "1000m", true},
		{"", "", true},
		{"", "1", false},
		{"weird", "weird", true},
		{"weird", "odd", false},
	} {
		if got := cpuSame(c.a, c.b); got != c.want {
			t.Errorf("cpuSame(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestQuantitySameMem(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"256Mi", "268435456", true},
		{"256Mi", "268435460", false},
		{"1Gi", "1073741824", true},
		{"", "", true},
		{"", "1", false},
		{"weird", "weird", true},
		{"weird", "odd", false},
	} {
		if got := memSame(c.a, c.b); got != c.want {
			t.Errorf("memSame(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestFormatQuantities(t *testing.T) {
	if s := formatMemoryBytes(parseMust(t, "256Mi")); s != "268435456" {
		t.Errorf("memory: %q", s)
	}
	if s := formatCPUCores(parseMustCPU(t, "500m")); s != "0.5" {
		t.Errorf("cpu: %q", s)
	}
}

func parseMust(t *testing.T, q string) int64 {
	t.Helper()
	v, ok := parseMemoryBytes(q)
	if !ok {
		t.Fatalf("%q unparseable", q)
	}
	return v
}

func parseMustCPU(t *testing.T, q string) float64 {
	t.Helper()
	v, ok := parseCPUCores(q)
	if !ok {
		t.Fatalf("%q unparseable", q)
	}
	return v
}
