// SPDX-License-Identifier: MIT

package backup

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/store"
)

// This file is TOP's offsite copy step (T-502, BackupProvider.CopyOffsite,
// docs/contracts/backup-recovery.md): after a successful local backup
// exists, copy its single file to an SSH destination. One rsync invocation,
// argv-only, configured from internal/config's backup.offsite surface.
//
// Decisions this file commits to (documented for the M5 review; ADR-0007
// untouched):
//
//   - Mechanism: rsync over SSH, NOT rclone. The backlog names rsync/SSH as
//     the instrument and it is the smallest contract-fitting option: rsync
//     and ssh are already on every TOP host (no new dependency, no config
//     daemon). `backup.offsite.kind: rclone` remains config-known but
//     unimplemented — it fails as ErrUnimplemented at resolution time,
//     mirroring the v0 vault posture (ADR-0008 pattern).
//   - Failure alarms but must NOT fail the local backup: Copy reports the
//     failure to Recorder + Log and returns it, and every composed caller
//     (CLI/controller, T-503 wiring) must treat the returned error as
//     non-fatal for the local backup already committed.
//   - Copied-state alarm surface in v0 = METRICS ONLY. The store surface is
//     frozen for T-502 (no registry marker/migration): copied-state is
//     process-level (top_backup_offsite_last_success_timestamp,
//     top_backup_offsite_total{result}), not per registry row. No
//     op-record is inserted either (same evidence rule as Create: a record
//     inserted before/after the copy is neither an admission trail nor the
//     watermark). A per-row marker (store.Backup.CopiedOffsiteAt +
//     SetBackupCopiedOffsite) is the documented open seam for the M5
//     review — see README §Offsite.
//
// Security posture: rsync is invoked ONLY through the exec.Runner seam
// (argv slices, no shell string — shell interpolation of config values is
// structurally impossible). No key or password ever appears in this
// package: authentication is SSH's own (agent/keys/config; BatchMode
// guarantees prompt-less key auth and a clean failure instead of a hung
// password prompt), and stderr echoed into errors/logs carries no secret
// by construction.

// DefaultOffsiteConnectTimeoutSeconds is the SSH connection-established
// timeout when Offsite.TimeoutSeconds is unset — enough to fail a
// destination-down fast; never part of the copy-time budget (bound callers
// with a context deadline for that).
const DefaultOffsiteConnectTimeoutSeconds = 15

// offsiteBinary is the rsync binary name resolved on PATH.
const offsiteBinary = "rsync"

// whitelistFlags is the restricted rsync flag allowlist: value-less flags
// only, each individually pinned. Anything else (transport options,
// remote-shell selection, value-taking flags, unknown futures) is rejected
// before exec — Copy must never translate arbitrary config into rsync
// behavior.
var whitelistFlags = map[string]bool{
	"--archive":    true,
	"-a":           true,
	"--compress":   true,
	"-z":           true,
	"--checksum":   true,
	"-c":           true,
	"--whole-file": true,
	"-W":           true,
}

// OffsiteRecorder is the offsite-copy metrics seam (obs-symmetric to
// Recorder, kept a separate interface so small Recorder implementations
// stay valid and *obs.Metrics is the only production satisfier).
// Nil-tolerant: an offsite copy must be alarmable even where metrics
// cannot be recorded — never the reverse.
type OffsiteRecorder interface {
	// SetBackupOffsiteSuccessAt stamps the last successful copy timestamp
	// (Unix seconds); the gauge stays at 0 until the first success —
	// "no fresh timestamp" IS the alarm condition, mirroring ADR-0007's
	// stale-backup alerting.
	SetBackupOffsiteSuccessAt(t time.Time)
	// RecordBackupOffsite records one copy attempt by outcome
	// (ResultOK/ResultError, ADR-0010 bounded labels — never an error
	// string).
	RecordBackupOffsite(d time.Duration, result string)
}

// Offsite copies one committed backup file off-host over SSH. Zero value
// is not usable; build it from config.Backup.Offsite (see README §Offsite
// for the field mapping).
type Offsite struct {
	// Target is the SSH destination spec: exactly "<user>@<host>:<path>"
	// (rsync remote syntax; the file lands inside <path>). No IPv6, no
	// whitespace, no extra colons in v0. Required.
	Target string
	// ExtraArgs are additional rsync flags, whitelist-validated
	// (surfaced as backup.offsite.options). Empty is fine — the default
	// argv needs no flags for a single-file copy.
	ExtraArgs []string
	// Port is the SSH port; 0 = ssh default. Surfaced inside the -e
	// command only.
	Port int
	// TimeoutSeconds bounds SSH connection establishment (0 =
	// DefaultOffsiteConnectTimeoutSeconds). Total copy time is the
	// caller's context deadline.
	TimeoutSeconds int
	// Runner is the exec seam (the only way rsync runs); required.
	Runner exec.Runner
	// Recorder receives the offsite metrics; optional.
	Recorder OffsiteRecorder
	// Log receives one info/warn line per copy; optional (discarded when
	// nil).
	Log *slog.Logger
	// Now is the clock (injected for hermetic tests; defaults to
	// time.Now).
	Now func() time.Time
}

// remote parses Target into its user/host/path triple. The grammar is
// exactly user@host:path: one colon, one @, no empty part, no IPv6, no
// whitespace. validate calls it before anything else happens.
func (o *Offsite) remote() (user, host, path string, err error) {
	spec := o.Target
	if strings.ContainsAny(spec, " \t\n\r") {
		return "", "", "", fmt.Errorf("offsite target %q: whitespace is not part of the grammar: %w", spec, domain.ErrInvalid)
	}
	colon := strings.IndexByte(spec, ':')
	if colon < 0 {
		return "", "", "", fmt.Errorf("offsite target %q: want user@host:path: %w", spec, domain.ErrInvalid)
	}
	remote, path := spec[:colon], spec[colon+1:]
	at := strings.IndexByte(remote, '@')
	if at <= 0 {
		return "", "", "", fmt.Errorf("offsite target %q: want user@host:path: %w", spec, domain.ErrInvalid)
	}
	user, host = remote[:at], remote[at+1:]
	if path == "" || host == "" {
		return "", "", "", fmt.Errorf("offsite target %q: empty user/host/path: %w", spec, domain.ErrInvalid)
	}
	if strings.IndexByte(spec[colon+1:], ':') >= 0 {
		return "", "", "", fmt.Errorf("offsite target %q: more than one colon (IPv6 literals are not supported in v0): %w", spec, domain.ErrInvalid)
	}
	return user, host, path, nil
}

// validate rejects broken wiring and malformed/unsafe configuration BEFORE
// anything executes (malformed config rejected pre-exec).
func (o *Offsite) validate() error {
	if o.Runner == nil {
		return fmt.Errorf("offsite copier: %w: nil Runner", domain.ErrInvalid)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if _, _, _, err := o.remote(); err != nil {
		return err
	}
	for _, f := range o.ExtraArgs {
		if !whitelistFlags[f] {
			return fmt.Errorf("offsite rsync flag %q: not whitelisted (allowed: -a/--archive, -z/--compress, -c/--checksum, -W/--whole-file): %w", f, domain.ErrInvalid)
		}
	}
	if o.Port < 0 || o.Port > 65535 {
		return fmt.Errorf("offsite ssh port %d: want 1-65535 (0 = default): %w", o.Port, domain.ErrInvalid)
	}
	if o.TimeoutSeconds < 0 {
		return fmt.Errorf("offsite timeout %d: want >= 0 (0 = default): %w", o.TimeoutSeconds, domain.ErrInvalid)
	}
	return nil
}

// sshCommand builds the value passed to rsync's -e argument: a plain ssh
// argv string, BatchMode (never prompt, never hang), the configured
// connect timeout, and the optional port. Key/password handling is SSH's
// own — this package never sees a secret.
func (o *Offsite) sshCommand() string {
	timeout := o.TimeoutSeconds
	if timeout == 0 {
		timeout = DefaultOffsiteConnectTimeoutSeconds
	}
	parts := []string{"ssh", fmt.Sprintf("-o ConnectTimeout=%d", timeout), "-o BatchMode=yes"}
	if o.Port > 0 {
		parts = append(parts, fmt.Sprintf("-p %d", o.Port))
	}
	return strings.Join(parts, " ")
}

// argv builds the single rsync invocation: rsync [whitelisted flags...]
// -e "<ssh>" <local-file> <user@host:path>. One file per copy (not
// dir-sync semantics); config values travel as argv atoms — never through
// a shell, never interpolated.
func (o *Offsite) argv(b *store.Backup) []string {
	args := append([]string{}, o.ExtraArgs...)
	return append(args, "-e", o.sshCommand(), b.Path, o.Target)
}

// Copy copies ONE committed local backup file to the configured offsite
// target (BackupProvider.CopyOffsite, backup-recovery contract): validate
// → one rsync run → metrics + log on either outcome.
//
// Return semantics (the contract's alarm clause): Copy FAILS with the
// wrapped rsync error, but the local backup is already committed and MUST
// be treated as intact — composed callers log and continue. Copy never
// touches the registry, never deletes or mutates the local file, and never
// panics on destination-down.
//
// Idempotence: Copy is deliberately re-runnable — two copies of the same
// file overwrite the same remote file and re-stamp the same gauge
// (process-level state, idempotent by overwrite; there is no registry
// write to duplicate in v0).
func (o *Offsite) Copy(ctx context.Context, b *store.Backup) error {
	if err := o.validate(); err != nil {
		return fmt.Errorf("offsite copy: %w", err)
	}
	start := o.Now()

	runErr := o.run(ctx, b)

	if o.Recorder != nil {
		if runErr == nil {
			o.Recorder.SetBackupOffsiteSuccessAt(o.Now().UTC())
		}
		o.Recorder.RecordBackupOffsite(o.Now().Sub(start), resultOf(runErr))
	}
	if runErr != nil {
		err := fmt.Errorf("offsite copy %s → %s: %w", b.Path, o.Target, runErr)
		o.Log.Warn("offsite copy failed (local backup unaffected)", "target", o.Target, "err", err)
		return err
	}
	o.Log.Info("offsite copy done", "target", o.Target, "path", b.Path)
	return nil
}

// run performs the single rsync invocation through the exec seam. The
// destination being down is just a non-zero exit here — classification and
// alarming belong to Copy.
func (o *Offsite) run(ctx context.Context, b *store.Backup) error {
	_, stderr, err := o.Runner.Run(ctx, offsiteBinary, o.argv(b)...)
	if err != nil {
		// FastExec already redacts registered secrets from captured
		// buffers and error strings; only bounded, non-secret framing is
		// added here.
		if len(stderr) > 0 {
			err = fmt.Errorf("%w (stderr: %s)", err, truncate(strings.TrimSpace(string(stderr)), exec.ExcerptLimit))
		}
		return err
	}
	return nil
}

// Pull stages ONE remote file from an offsite target onto the local
// filesystem and returns the staged path (T-503 restore lifeline: when the
// local backup directory is gone, the offsite copy is the recoverable
// source — docs/operations/recovery.md "whole host lost" row + ADR-0007).
//
// The spec argument is the exact same grammar as Target ("<user>@<host>:<path>").
// The caller passes the operator's `--backup` argument here, so a
// `topctl recover restore --backup user@host:…` flows through the identical
// security posture as Copy: rsync over the BatchMode ssh command, argv
// atoms only, whitelisted ExtraArgs, no shell, no secret handling. The
// ONLY difference from Copy is direction: remote → local staging dir
// (mode 0700, created on demand). The staged name keeps its remote
// basename; a collision overwrites (staging is scratch space owned by the
// single restore in flight).
//
// Pull is plain file movement: it never verifies, never registers, and
// never qualifies a file for restore by itself — restore's admission
// (watermark + integrity) happens after staging. Metrics are NOT stamped
// (a pulled file is not a CopyOffsite backup attempt in the scheduling
// sense); the log line keeps the runbook trail.
func (o *Offsite) Pull(ctx context.Context, spec, stagingDir string) (string, error) {
	if o.Runner == nil {
		return "", fmt.Errorf("offsite pull: %w: nil Runner", domain.ErrInvalid)
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if _, _, _, err := parseRemoteSpec(spec); err != nil {
		return "", fmt.Errorf("offsite pull: %w", err)
	}
	for _, f := range o.ExtraArgs {
		if !whitelistFlags[f] {
			return "", fmt.Errorf("offsite pull rsync flag %q: not whitelisted (allowed: -a/--archive, -z/--compress, -c/--checksum, -W/--whole-file): %w", f, domain.ErrInvalid)
		}
	}
	if o.Port < 0 || o.Port > 65535 {
		return "", fmt.Errorf("offsite ssh port %d: must be 1-65535 (0 = default): %w", o.Port, domain.ErrInvalid)
	}
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return "", fmt.Errorf("offsite pull staging dir %s: %w", stagingDir, err)
	}
	local := filepath.Join(stagingDir, filepath.Base(remotePath(spec)))

	// argv mirrors Copy's exactly with source/target swapped and a .part
	// landing name: rsync [flags] -e <ssh> <user@host:path> <local>.part
	// (the .part avoids claiming a valid staged file after a mid-run
	// failure on filesystems without rename-atomicity guarantees).
	args := append([]string{}, o.ExtraArgs...)
	args = append(args, "-e", o.sshCommand(), spec, local+".part")

	_, stderr, rerr := o.Runner.Run(ctx, offsiteBinary, args...)
	if rerr != nil {
		err := fmt.Errorf("offsite pull %s → %s: %w", spec, local, rerr)
		if len(stderr) > 0 {
			err = fmt.Errorf("%w (stderr: %s)", err, truncate(strings.TrimSpace(string(stderr)), exec.ExcerptLimit))
		}
		o.Log.Warn("offsite pull failed", "target", spec, "err", err)
		return "", err
	}
	if rerr2 := os.Rename(local+".part", local); rerr2 != nil {
		return "", fmt.Errorf("offsite pull staging %s: %w", local, rerr2)
	}
	o.Log.Info("offsite pull staged", "target", spec, "staged", local)
	return local, nil
}

// parseRemoteSpec validates one "<user>@<host>:<path>" file spec — the
// same grammar Offsite.remote() enforces for Target, factored through a
// zero Offsite so the restore path can validate the operator's argument
// without touching the copier's wiring.
func parseRemoteSpec(spec string) (user, host, path string, err error) {
	o := Offsite{Target: spec}
	return o.remote()
}

// remotePath extracts the :-separated path part of a validated spec.
func remotePath(spec string) string {
	if i := strings.IndexByte(spec, ':'); i >= 0 {
		return spec[i+1:]
	}
	return spec
}

// resultOf maps an error to the bounded metric label.
func resultOf(err error) string {
	if err != nil {
		return ResultError
	}
	return ResultOK
}
