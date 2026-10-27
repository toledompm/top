//go:build conformance

package conformance_check

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docTokenCheck is one doc-lint spot-check: a short token that must keep
// living in the contract document itself. Guards *the doc* drifting away
// from code (T-111's AC: "harness fails on doc/interface drift").
//
// ⚠️ Over-fitting budget: tokens were deliberately kept FEW and STABLE —
// grammar-noise-adjacent tokens would break on harmless rewording. When a
// token legitimately moves, update the move here (the failure names the
// file), not the token list blindly.
var docTokens = []struct {
	// file relative to docs/contracts/
	file string
	// tokens the doc must contain (spot-check granularity, not full parse)
	tokens []string
}{
	{"service-spec.md", []string{
		"`apiVersion`", "spec.source.image", "spec.secretRefs[]", "spec.dependsOn[]",
	}},
	{"state-store.md", []string{
		"INSERT OR IGNORE", "ErrLocked", "ErrCorrupt", "downgrade protection",
	}},
	{"cli.md", []string{
		`"schemaVersion": 1`, "Exit codes", "`topctl`",
	}},
}

// TestContractDocsKeepTheirTokens pins the stable spot-check tokens above
// (8 token checks across the three contracts T-111's probes lean on hardest).
func TestContractDocsKeepTheirTokens(t *testing.T) {
	for _, tc := range docTokens {
		path := filepath.Join("..", "docs", "contracts", tc.file)
		blob, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(blob)
		for _, tok := range tc.tokens {
			if !strings.Contains(text, tok) {
				t.Errorf("%s no longer contains %q — doc drift (the conformance probe that pins it may be stale too)", tc.file, tok)
			}
		}
	}
}
