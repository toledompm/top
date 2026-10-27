# internal/secrets

TOP's `SecretProvider` seam (ADR-0008): resolving secret names to values at
apply time, with version identifiers so Milestone 3 can drive rotation by
version comparison. Exactly one implementation ships in v0 — `hostfile`,
the plain-file host store at `/var/lib/top/secrets/<name>` — so that adding
Vault-style providers later (T-602) is a pure addition.

## Provider contract

```go
type Provider interface {
    Resolve(ctx context.Context, name string) (value string, version string, err error)
}
```

- `version` changes if and only if the value changes
  (`version = sha256(content)[:12]`).
- Names must match `^[a-z][a-z0-9_-]{0,63}$`.
- Implementations must never log values or retain package-level state.

## Boundaries (ADR-0008, enforced here)

- **Values never in Git, SQLite, logs, metrics, or JSON output.** Anything
  persisted or displayed uses `SafeRef(name)` (`"secret:<name>"`) or a
  checksum. `ManifestEntry` (from `List()`) structurally cannot carry a
  value — it has no field for one.
- **Permissions are load-bearing**: secret files `0600`, store directory
  `0700`. A violation fails loudly and typed (`ErrPerms`) with the exact
  `chmod 600/700 <path>` fix hint — never a silent read of a loose file.
- **The store is excluded from metadata backups** (ADR-0007); `List()` is
  the names+checksums manifest recovery runbooks consume.
- `VaultProvider` is the reserved seam: every operation returns
  `ErrUnimplemented`. Config accepts `secretsProvider.kind: vault` as valid;
  the failure surfaces only at resolution (pure-addition guarantee).

## API (`NewHostFile(dir)` — dir injectable; default from `config.SecretsDir()`)

| Op | Signature | Errors |
| --- | --- | --- |
| Resolve | `(h *HostFile) Resolve(ctx, name) (value, version, error)` | `ErrInvalidName`, `ErrMissing` (alias `SecretErrNotFound`), `ErrPerms` |
| Write | `(h *HostFile) Write(name, value string, overwrite bool) error` | `ErrInvalidName`, `ErrExists` (no overwrite), `ErrPerms` |
| Delete | `(h *HostFile) Delete(name string) error` | `ErrInvalidName`, `ErrMissing` |
| List | `(h *HostFile) List() ([]ManifestEntry, error)` | I/O errors |
| SafeRef | `SafeRef(name) string` → `"secret:<name>"` | — |
| Version / Checksum | `sha256(content)[:]` (12 hex / 64 hex) | — |

## Semantics

- `Write` creates the file through a `0600` temp file + rename (no window
  with looser mode) and refuses an existing name unless `overwrite=true`.
- `Resolve` checks directory **and** file perms on every call; an empty
  secret file is treated as missing (a typo'd set must fail loudly).
- Trailing newline is trimmed from stored values so stdin/set helpers don't
  corrupt tokens; checksums are over raw file bytes.

## Future: Vault-style providers

Same `Provider` interface, config-selected via `secretsProvider.kind`.
Provider-side versioning drives rotation; the reconciler's
resolve-at-apply flow (inject via `dokku config:set --no-restart`,
ADR-0008) does not change. Rotation triggering restarting deploys is
Milestone 3 work, not here.
