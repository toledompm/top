//go:build conformance

package conformance_check

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/toledompm/top/internal/secrets"
)

// TestSecretsHostFileProbe pins the secrets contract (ADR-0008) on the
// hostfile provider: round-trip, version identity, SafeRef, load-bearing
// perms, valueless manifest.
func TestSecretsHostFileProbe(t *testing.T) {
	ctx := context.Background()
	h := secrets.NewHostFile(secretsDir(t))
	const passtext = "PA$$WORD-t111"

	if err := h.Write("smtp-password", passtext, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	value, version, err := h.Resolve(ctx, "smtp-password")
	if err != nil || value != passtext || len(version) != 12 {
		t.Fatalf("Resolve = %q/%q/%v, want value + 12-hex version", value, version, err)
	}
	if _, v2, rerr := h.Resolve(ctx, "smtp-password"); rerr != nil || v2 != version {
		t.Fatalf("version unstable: %q vs %q", v2, version)
	}
	if got := secrets.SafeRef("smtp-password"); got != "secret:smtp-password" {
		t.Fatalf("SafeRef = %q, want secret:<name>", got)
	}

	// Manifest is names+checksums, structurally without a value field.
	manifest, err := h.List()
	if err != nil || len(manifest) != 1 {
		t.Fatalf("List = %+v err=%v", manifest, err)
	}
	blob, jerr := json.Marshal(manifest)
	if jerr != nil {
		t.Fatalf("marshal manifest: %v", jerr)
	}
	if strings.Contains(string(blob), passtext) {
		t.Fatalf("manifest leaked value: %s", blob)
	}
	if !strings.Contains(string(blob), `"checksum"`) {
		t.Fatalf("manifest missing checksum field: %s", blob)
	}

	// File perms are load-bearing: file mode on disk must be 0600;
	// loosening the file or the dir turns Resolve into typed ErrPerms.
	dir := h.Dir()
	st, serr := os.Stat(filepath.Join(dir, "smtp-password"))
	if serr != nil {
		t.Fatalf("stat secret file: %v", serr)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode = %o, want 600", st.Mode().Perm())
	}
	if dm, derr := os.Stat(dir); derr != nil || dm.Mode().Perm() != 0o700 {
		t.Fatalf("store dir mode = %v err=%v, want 700", dm, derr)
	}
	for _, loosen := range []func() func(){
		func() func() { // loosen file
			p := filepath.Join(dir, "smtp-password")
			os.Chmod(p, 0o644)                   //nolint:errcheck // probe makes it worse on purpose
			return func() { os.Chmod(p, 0o600) } //nolint:errcheck
		},
		func() func() { // loosen dir
			os.Chmod(dir, 0o755)                   //nolint:errcheck
			return func() { os.Chmod(dir, 0o700) } //nolint:errcheck
		},
	} {
		restore := loosen()
		_, _, rerr := h.Resolve(ctx, "smtp-password")
		restore()
		if !errors.Is(rerr, secrets.ErrPerms) {
			t.Fatalf("loosened perms → %v, want secrets.ErrPerms with chmod hint", rerr)
		}
		if !strings.Contains(fmt.Sprint(rerr), "chmod") {
			t.Fatalf("ErrPerms must carry the chmod fix hint, got %v", rerr)
		}
	}
}

// secretsDir is a temp dir with the load-bearing 0700 fix applied (macOS
// t.TempDir() is 0755, which the provider rightly refuses).
func secretsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod fixture dir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) }) //nolint:errcheck // lets t.TempDir clean up
	return dir
}

// TestSecretsMissingAndVault covers the negative class: missing name →
// ErrMissing; the reserved VaultProvider returns ErrUnimplemented (pure-
// addition seam, T-602).
func TestSecretsMissingAndVault(t *testing.T) {
	h := secrets.NewHostFile(secretsDir(t))
	if _, _, err := h.Resolve(context.Background(), "nope"); !errors.Is(err, secrets.ErrMissing) {
		t.Fatalf("missing → %v, want ErrMissing", err)
	}
	if _, _, err := (secrets.VaultProvider{}).Resolve(context.Background(), "x"); !errors.Is(err, secrets.ErrUnimplemented) {
		t.Fatalf("vault → %v, want ErrUnimplemented", err)
	}
}
