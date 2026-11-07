// Package secrets implements TOP's SecretProvider seam (ADR-0008): name →
// (value, version) resolution for the v0 host-file store, with the vault
// kind reserved as known-but-unimplemented.
//
// Hard boundaries (ADR-0008):
//   - Secret values never enter Git, SQLite, logs, metrics, or JSON output.
//     Persisted representations use SafeRef ("secret:<name>") and checksums.
//   - Secret files live one-per-name with mode 0600 in a 0700 directory.
//     A wrong-perms read is a loud, typed, fix-hinted error — never a
//     silent success with reduced guarantees.
//   - List() yields
//     names + SHA-256 checksums + mtimes only (names manifest for re-provisioning
//     runbooks, ADR-0013 boundary).
package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Provider is the SecretProvider seam from ADR-0008. Exactly one
// implementation (hostfile) ships in v0; Vault-style providers (T-602)
// must implement this same interface.
type Provider interface {
	// Resolve returns the secret's value and a version identifier. The
	// version must change when (and only when) the value changes, so the
	// reconciler can drive rotation via version comparison (M3).
	Resolve(ctx context.Context, name string) (value string, version string, err error)
}

// Name constraints: one per-secret file per name, filesystem-safe.
// ^[a-z][a-z0-9_-]{0,63}$
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Sentinel errors. Values and filesystem paths that contain secret data
// must never be embedded in these.
var (
	// ErrMissing is returned when a secret name does not exist.
	ErrMissing = errors.New("secret: not found")
	// ErrPerms is returned when a secret (or its directory) has unsafe
	// permissions; the error message carries the exact chmod fix hint.
	ErrPerms = errors.New("secret: unsafe permissions")
	// ErrExists is returned by Write when the name exists and overwrite
	// was not requested.
	ErrExists = errors.New("secret: already exists")
	// ErrInvalidName is returned for names outside ^[a-z][a-z0-9_-]{0,63}$.
	ErrInvalidName = errors.New("secret: invalid name")
	// ErrUnimplemented is returned by reserved providers (vault in v0).
	ErrUnimplemented = errors.New("secret: provider not implemented")

	// SecretErrNotFound is the alias used at call sites that speak in
	// "not found" terms (contract wording).
	SecretErrNotFound = ErrMissing
)

// SafeRef renders the only form of a secret ever allowed in logs, JSON,
// diffs, or manifests: a name, not a value (ADR-0008).
func SafeRef(name string) string { return "secret:" + name }

// Version computes the stable version identifier: first 12 hex chars of
// SHA-256 over the exact file content.
func Version(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])[:12]
}

// Checksum is the full SHA-256 hex digest of the secret content. It is the
// only value-derived string ever persisted or displayed (ADR-0013).
func Checksum(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// HostFile is the v0 secret provider: plain files, one per secret, under
// dir (default /var/lib/top/secrets — injectable for tests and tier-3 CI).
type HostFile struct {
	dir string
}

// NewHostFile returns a hostfile provider rooted at dir. The directory is
// NOT created here; Write creates it (0700) on demand.
func NewHostFile(dir string) *HostFile { return &HostFile{dir: dir} }

// Dir returns the store directory.
func (h *HostFile) Dir() string { return h.dir }

// Resolve implements Provider.
func (h *HostFile) Resolve(ctx context.Context, name string) (string, string, error) {
	if !nameRE.MatchString(name) {
		return "", "", fmt.Errorf("%w: %q must match %s", ErrInvalidName, name, nameRE.String())
	}
	path := filepath.Join(h.dir, name)

	if err := checkDirPerms(h.dir); err != nil {
		return "", "", err
	}
	content, err := os.ReadFile(path) //nolint:gosec // dir + regex-validated name
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", fmt.Errorf("%w: %s", ErrMissing, SafeRef(name))
		}
		// Unexpected I/O (e.g. EACCES on read) — wrap, never log.
		return "", "", fmt.Errorf("secret: read %s: %w", SafeRef(name), err)
	}
	if st, err := os.Lstat(path); err == nil && st.Mode().Perm() != 0o600 {
		return "", "", permsErr(path, st.Mode().Perm())
	}
	// Empty files are almost always accidents; treat as missing so a
	// typo'd secret set fails loudly rather than injecting "".
	if len(content) == 0 {
		return "", "", fmt.Errorf("%w: %s is empty", ErrMissing, SafeRef(name))
	}
	_ = ctx // reserved for provider-era timeouts/cancellation
	value := strings.TrimSuffix(string(content), "\n")
	return value, Version(content), nil
}

// Mode constants for the store: file 0600, directory 0700 (ADR-0008).
const (
	filePerm = 0o600
	dirPerm  = 0o700
)

func permsErr(path string, perm fs.FileMode) error {
	return fmt.Errorf("%w: %s is %04o — fix with: chmod 600 %s (directory must be chmod 700)",
		ErrPerms, path, perm, path)
}

func checkDirPerms(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: store directory %s does not exist yet", ErrMissing, dir)
		}
		return fmt.Errorf("secret: stat store directory: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("secret: %s is not a directory", dir)
	}
	if st.Mode().Perm() != dirPerm {
		return fmt.Errorf("%w: store directory %s is %04o — fix with: chmod 700 %s",
			ErrPerms, dir, st.Mode().Perm(), dir)
	}
	return nil
}

// Write stores a secret value, creating the store directory (0700) and the
// secret file (0600) as needed. With overwrite=false, an existing name is
// refused with ErrExists — values must never be silently replaced.
func (h *HostFile) Write(name, value string, overwrite bool) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%w: %q must match %s", ErrInvalidName, name, nameRE.String())
	}
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("secret: value for %s contains a NUL byte", SafeRef(name))
	}
	if err := os.MkdirAll(h.dir, dirPerm); err != nil {
		return fmt.Errorf("secret: create store: %w", err)
	}
	path := filepath.Join(h.dir, name)

	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%w: %s (pass overwrite to replace)", ErrExists, SafeRef(name))
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("secret: stat %s: %w", SafeRef(name), err)
		}
	}

	// Write via a 0600 temp file + rename so no window ever exposes a
	// wider mode and value bytes never hit the store under a lesser mode.
	tmp, err := os.CreateTemp(h.dir, ".tmp-secret-*")
	if err != nil {
		return fmt.Errorf("secret: temp file: %w", err)
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // best-effort cleanup
	if err := tmp.Chmod(filePerm); err != nil {
		tmp.Close() //nolint:errcheck // permissions failure aborts anyway
		return fmt.Errorf("secret: temp file chmod: %w", err)
	}
	if _, err := tmp.WriteString(value); err != nil {
		tmp.Close() //nolint:errcheck // write failure aborts anyway
		return fmt.Errorf("secret: temp write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("secret: temp close: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("secret: install %s: %w", SafeRef(name), err)
	}
	// Directory mode is correct-by-creation; enforce in case the dir
	// pre-existed with looser perms.
	if err := os.Chmod(h.dir, dirPerm); err != nil {
		return fmt.Errorf("secret: store dir chmod: %w", err)
	}
	return nil
}

// Delete removes a secret by name. Missing names are ErrMissing.
func (h *HostFile) Delete(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	if err := os.Remove(filepath.Join(h.dir, name)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrMissing, SafeRef(name))
		}
		return fmt.Errorf("secret: delete %s: %w", SafeRef(name), err)
	}
	return nil
}

// ManifestEntry describes one secret without revealing its value — the
// names-only listing operators use to re-provision the store (ADR-0008).
type ManifestEntry struct {
	Name     string    `json:"name"`
	Version  string    `json:"version"`
	Checksum string    `json:"checksum"`
	Mtime    time.Time `json:"mtime"`
}

// List returns every secret as name + version + checksum + mtime. Values
// are structurally impossible to include; there is no field for them.
func (h *HostFile) List() ([]ManifestEntry, error) {
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []ManifestEntry{}, nil
		}
		return nil, fmt.Errorf("secret: list store: %w", err)
	}
	out := []ManifestEntry{}
	for _, e := range entries {
		name := e.Name()
		if !nameRE.MatchString(name) || !e.Type().IsRegular() {
			continue // temp files and stranger things are not secrets
		}
		content, err := os.ReadFile(filepath.Join(h.dir, name)) //nolint:gosec // regex-checked name
		if err != nil {
			return nil, fmt.Errorf("secret: read %s: %w", SafeRef(name), err)
		}
		st, err := e.Info()
		if err != nil {
			return nil, fmt.Errorf("secret: stat %s: %w", SafeRef(name), err)
		}
		out = append(out, ManifestEntry{
			Name:     name,
			Version:  Version(content),
			Checksum: Checksum(content),
			Mtime:    st.ModTime().UTC(),
		})
	}
	return out, nil
}

// VaultProvider is the seam-reserved placeholder for external secret
// providers (T-602). Every operation returns ErrUnimplemented; config
// accepts the "vault" kind as valid so this is purely a resolution-time
// failure, per ADR-0008.
type VaultProvider struct{}

// Resolve implements Provider, always ErrUnimplemented in v0.
func (VaultProvider) Resolve(ctx context.Context, name string) (string, string, error) {
	return "", "", fmt.Errorf("%w: secretsProvider.kind=vault is reserved for post-v0 (use hostfile; ADR-0008)", ErrUnimplemented)
}
