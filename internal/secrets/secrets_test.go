package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newStore(t *testing.T) (*HostFile, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "secrets") // under t.TempDir(): hermetic
	return NewHostFile(dir), dir
}

func TestResolveRoundTrip(t *testing.T) {
	h, dir := newStore(t)
	if err := h.Write("api-key", "s3cr3t!", false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// File perms must be 0600 and dir 0700, per ADR-0008.
	fi, err := os.Stat(filepath.Join(dir, "api-key"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret file perm = %04o, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("store dir perm = %04o, want 0700", di.Mode().Perm())
	}

	v1, ver1, err := h.Resolve(context.Background(), "api-key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v1 != "s3cr3t!" {
		t.Errorf("value = %q", v1)
	}
	if len(ver1) != 12 || ver1 != Version([]byte("s3cr3t!")) {
		t.Errorf("version = %q, want sha256(content)[:12]", ver1)
	}

	// Read again: identical version (stability under re-resolution).
	_, ver2, err := h.Resolve(context.Background(), "api-key")
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if ver1 != ver2 {
		t.Errorf("version unstable: %q vs %q", ver1, ver2)
	}

	// Overwrite: version must change (rotation semantics).
	if err := h.Write("api-key", "r0tated!", true); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	_, ver3, err := h.Resolve(context.Background(), "api-key")
	if err != nil {
		t.Fatalf("Resolve #3: %v", err)
	}
	if ver3 == ver1 {
		t.Errorf("version did not change after value change")
	}
}

func TestResolveWrongPermsIsTypedError(t *testing.T) {
	h, dir := newStore(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "api-key")
	if err := os.WriteFile(path, []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.Resolve(context.Background(), "api-key")
	if !errors.Is(err, ErrPerms) {
		t.Fatalf("err = %v, want ErrPerms", err)
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error must carry the fix hint: %v", err)
	}

	// Loose directory is also a typed perms error.
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.MkdirAll(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(loose, "api-key"), []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = NewHostFile(loose).Resolve(context.Background(), "api-key")
	if !errors.Is(err, ErrPerms) || !strings.Contains(err.Error(), "chmod 700") {
		t.Fatalf("loose dir: err = %v", err)
	}
}

func TestResolveMissing(t *testing.T) {
	h, dir := newStore(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.Resolve(context.Background(), "nope")
	if !errors.Is(err, ErrMissing) || !errors.Is(err, SecretErrNotFound) {
		t.Fatalf("err = %v, want ErrMissing", err)
	}
	if !strings.Contains(err.Error(), "secret:nope") {
		t.Errorf("error must use SafeRef, not raw naming: %v", err)
	}
}

func TestResolveEmptyFileIsMissing(t *testing.T) {
	h, dir := newStore(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api-key"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.Resolve(context.Background(), "api-key"); !errors.Is(err, ErrMissing) {
		t.Fatalf("empty secret: err = %v, want ErrMissing", err)
	}
}

func TestInvalidNames(t *testing.T) {
	h, _ := newStore(t)
	for _, name := range []string{"", "UPPER", "../escape", "-lead", "with.dot", strings.Repeat("a", 65)} {
		if _, _, err := h.Resolve(context.Background(), name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Resolve(%q) err = %v, want ErrInvalidName", name, err)
		}
		if err := h.Write(name, "v", false); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Write(%q) err = %v, want ErrInvalidName", name, err)
		}
	}
	for _, name := range []string{"a", "db-password", "gh_token-x9", strings.Repeat("a", 64)} {
		ctx := context.Background()
		if _, _, err := h.Resolve(ctx, name); err != nil && !errors.Is(err, ErrMissing) {
			// valid shape; only ErrMissing acceptable on empty store
			t.Errorf("Resolve(%q) unexpected err: %v", name, err)
		}
	}
}

func TestWriteRefusesExistingWithoutOverwrite(t *testing.T) {
	h, _ := newStore(t)
	if err := h.Write("api-key", "v1", false); err != nil {
		t.Fatal(err)
	}
	err := h.Write("api-key", "v2", false)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	v, _, err := h.Resolve(context.Background(), "api-key")
	if err != nil || v != "v1" {
		t.Fatalf("value mutated on refused overwrite: %q %v", v, err)
	}
}

func TestDelete(t *testing.T) {
	h, _ := newStore(t)
	if err := h.Write("api-key", "v", false); err != nil {
		t.Fatal(err)
	}
	if err := h.Delete("api-key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := h.Resolve(context.Background(), "api-key"); !errors.Is(err, ErrMissing) {
		t.Errorf("post-delete Resolve err = %v", err)
	}
	if err := h.Delete("api-key"); !errors.Is(err, ErrMissing) {
		t.Errorf("double delete err = %v", err)
	}
}

func TestListManifestHasNoValues(t *testing.T) {
	h, _ := newStore(t)
	want := map[string]string{"api-key": "alpha!", "gh-token": "beta!"}
	for n, v := range want {
		if err := h.Write(n, v, false); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := h.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(manifest) != 2 {
		t.Fatalf("len = %d, want 2", len(manifest))
	}
	for _, e := range manifest {
		if e.Name == "" || len(e.Version) != 12 || len(e.Checksum) != 64 || e.Mtime.IsZero() {
			t.Errorf("bad manifest entry: %+v", e)
		}
		wantCS := want[e.Name]
		if e.Checksum != Checksum([]byte(wantCS)) {
			t.Errorf("checksum mismatch for %s", e.Name)
		}
		if e.Version != e.Checksum[:12] {
			t.Errorf("version must be checksum[:12]")
		}
	}

	// The struct's JSON has no value field by construction, and marshal-error-free.
	b, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	for _, leaked := range want {
		if strings.Contains(string(b), leaked) {
			t.Errorf("manifest JSON leaked a value: %s", b)
		}
	}
}

func TestListIgnoresTempFilesAndSubdirs(t *testing.T) {
	h, dir := newStore(t)
	if err := h.Write("api-key", "v", false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tmp-secret-123"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest, err := h.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 1 || manifest[0].Name != "api-key" {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestListOnMissingDirIsEmpty(t *testing.T) {
	h, _ := newStore(t)
	manifest, err := h.List()
	if err != nil {
		t.Fatalf("List on absent dir: %v", err)
	}
	if len(manifest) != 0 {
		t.Errorf("expected empty, got %+v", manifest)
	}
}

func TestSafeRef(t *testing.T) {
	if got := SafeRef("api-key"); got != "secret:api-key" {
		t.Errorf("SafeRef = %q", got)
	}
}

func TestResolveParallelIsolation(t *testing.T) {
	// Struct-value safety: the resolved value lives only in the returned
	// string; there is no package-level state or buffer a concurrent
	// Resolve could leak through. Race detector must stay green under -race.
	h, _ := newStore(t)
	names := []string{"alpha", "beta", "gamma", "delta"}
	for i, name := range names {
		if err := h.Write(name, strings.Repeat(string(rune('a'+i)), 8), false); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i, name := range names {
		want := strings.Repeat(string(rune('a'+i)), 8)
		wg.Add(1)
		go func(name, want string) {
			defer wg.Done()
			for range 20 {
				v, _, err := h.Resolve(context.Background(), name)
				if err != nil || v != want {
					t.Errorf("parallel Resolve(%s): %q %v", name, v, err)
					return
				}
			}
		}(name, want)
	}
	wg.Wait()
}

func TestVaultProviderUnimplemented(t *testing.T) {
	_, _, err := VaultProvider{}.Resolve(context.Background(), "x")
	if !errors.Is(err, ErrUnimplemented) {
		t.Fatalf("err = %v, want ErrUnimplemented", err)
	}
}
