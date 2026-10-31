// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/toledompm/top/internal/backend/dokku"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/gitsource"
	"github.com/toledompm/top/internal/inventory"
	"github.com/toledompm/top/internal/reconcile"
	"github.com/toledompm/top/internal/secrets"
	"github.com/toledompm/top/internal/spec"
	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/store/sqlite"
)

// Wiring for the four T-309 command bodies (diff / reconcile / deploy /
// history). The bodies live in stubs.go next to their sibling commands;
// this file is what every one of them shares: desired-state resolution
// (gitsource + spec), the state-store opener, reconciler wiring, the
// exit-code mapping, and the text/JSON renderers. One binary, one
// contract (docs/contracts/cli.md).

// ---------------------------------------------------------------------------
// Desired-state resolution (ADR-0002: git is the source of truth)
// ---------------------------------------------------------------------------

// cliGitShim adapts the CLI's injected exec.Runner (Options.Runner — the
// test seam) to gitsource's env-carrying Runner. PRODUCTION config (no
// Options.Runner) keeps gitsource's own default FastRunner, which carries
// the SSH env transport properly (ADR-0008); this shim drops env, which
// only a scripted fake can afford (tests configure https/file answers
// keyed on argv, never ssh). The production path never uses this shim.
type cliGitShim struct{ r exec.Runner }

// Run implements gitsource.Runner.
func (s cliGitShim) Run(ctx context.Context, _ []string, name string, args ...string) ([]byte, []byte, error) {
	return s.r.Run(ctx, name, args...)
}

// RunStream implements gitsource.Runner.
func (s cliGitShim) RunStream(ctx context.Context, _ []string, stdout, stderr io.Writer, name string, args ...string) error {
	return s.r.RunStream(ctx, stdout, stderr, name, args...)
}

// orGitSource resolves the platform-repo Source for this invocation. An
// explicitly injected Options.Runner (tests) wraps the shim; otherwise the
// gitsource default (its production env-carrying FastRunner) applies.
func (c *cctx) orGitSource() *gitsource.Source {
	src := gitsource.New(c.cfg)
	if c.opts.Runner != nil {
		src.Runner = cliGitShim{r: c.runner}
	}
	return src
}

// desired is one resolved revision of the platform repo: its sha, its
// parsed+strict-validated services, the spec sources for reporting, and
// the non-fatal spec warnings (rendered in text mode).
type desired struct {
	revision string
	specs    []domain.Service
	sources  map[domain.ServiceRef]reconcile.SourcePos
	warnings []spec.Warning
	snap     *gitsource.Snapshot // callers `defer func(){ _ = snap.Close() }()`
}

// resolveDesired resolves which revision + services a run or diff targets.
// rev "" means the configured branch head (`git ls-remote`, no clone);
// otherwise the sha is fetched and materialized (force-push detection and
// the archive extraction live in gitsource). Spec loading is STRICT
// (internal/spec); the platform repo option allows reserved namespaces.
func (c *cctx) resolveDesired(ctx context.Context, rev string) (desired, error) {
	src := c.orGitSource()
	if rev == "" {
		var err error
		rev, err = src.ResolveLatest(ctx)
		if err != nil {
			return desired{}, err
		}
	}
	snap, err := src.FetchRevision(ctx, rev)
	if err != nil {
		return desired{}, err
	}
	res, err := spec.LoadDir(snap.Dir, spec.WithPlatformRepo(true))
	if err != nil {
		_ = snap.Close()
		return desired{}, fmt.Errorf("loading specs from revision %s: %w", rev, err)
	}
	return desired{
		revision: rev,
		specs:    res.Services,
		sources:  specSources(snap.Dir, res.Services),
		warnings: res.Warnings,
		snap:     snap,
	}, nil
}

// specSources builds the reporting source map (reconcile.SourcePos):
// repo-relative file + the yaml line of the service's `metadata:` key.
// Layout rules mirror internal/spec's walk (non-hidden, depth ≤ 2,
// .yaml/.yml; dir-level namespace for <ns>/<name>.yaml; root-level files
// decode their own metadata). Best-effort by construction: LoadDir
// (strict, same layout) has already spoken, so a file that cannot be
// mapped here just renders without a source, never misleads.
func specSources(dir string, svcs []domain.Service) map[domain.ServiceRef]reconcile.SourcePos {
	out := map[domain.ServiceRef]reconcile.SourcePos{}
	for _, rel := range listSpecFiles(dir) {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		var node yaml.Node
		if err := yaml.Unmarshal(data, &node); err != nil {
			continue
		}
		line, ok := metaKeyLine(node)
		if !ok {
			continue
		}
		var meta struct {
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal(data, &meta); err != nil {
			continue
		}
		if meta.Metadata.Name == "" {
			continue
		}
		ns := meta.Metadata.Namespace
		if ns == "" {
			// dir-level: "<ns>/<name>.yaml" (spec's rule); a bare root
			// file with no metadata.namespace has no identity to name.
			ns, _, _ = strings.Cut(rel, "/")
			if ns == rel { // root-level file without explicit ns
				continue
			}
		}
		ref := domain.Ref(domain.Namespace(ns), domain.Name(meta.Metadata.Name))
		out[ref] = reconcile.SourcePos{File: rel, Line: line}
	}
	return out
}

// listSpecFiles mirrors internal/spec's walkStripFile layout rules for
// source attribution only (non-hidden, symlink-free, depth ≤ 2 yaml).
func listSpecFiles(dir string) []string {
	var out []string
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		switch {
		case e.IsDir():
			sub, err := os.ReadDir(dir + "/" + name)
			if err != nil {
				continue
			}
			for _, f := range sub {
				if strings.HasPrefix(f.Name(), ".") || f.Type()&os.ModeSymlink != 0 ||
					f.IsDir() || !isYAMLName(f.Name()) {
					continue
				}
				out = append(out, name+"/"+f.Name())
			}
		case isYAMLName(name):
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func isYAMLName(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

// metaKeyLine finds the line of the top-level `metadata:` key (the same
// anchor internal/spec's error line resolution uses).
func metaKeyLine(node yaml.Node) (int, bool) {
	if len(node.Content) == 0 {
		return 0, false
	}
	doc := node.Content[0]
	if doc.Kind != yaml.MappingNode {
		return 0, false
	}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "metadata" {
			return doc.Content[i].Line, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// State store + reconciler wiring
// ---------------------------------------------------------------------------

// orStore opens the state store (<dataDir>/top.db) when one exists. A
// store that never existed (fresh install, fresh host) returns nil with NO
// error: diff scores no orphan candidates and history lists none — reads
// refuse to invent infrastructure, and sqlite.Open would otherwise CREATE
// a database from a read-only command. A store that exists but fails to
// open (corruption, downgrade protection) is a real failure the caller
// maps to exit 4 (docs/contracts/cli.md "backend/store unavailable").
func (c *cctx) orStore(ctx context.Context) (store.Store, error) {
	path := c.cfg.DataDir + "/top.db"
	if _, err := os.Stat(path); err != nil {
		return nil, nil // absent state: honest empty read
	}
	return sqlite.Open(ctx, path)
}

// orReconciler builds the pipeline driver: the production dokku backend
// (structurally the reconcile.Backend seam, dual-pinned by dokku's
// seamcheck test), the given store, the host-file SECRET resolver for
// specs with secretRefs (ADR-0008: values resolve at apply time; only
// names travel in specs and plans; other provider kinds fail contained at
// apply with "no secret provider configured"), and the inventory-backed
// ADR-0009 two-key prune gate (used only behind --prune).
func (c *cctx) orReconciler(st store.Store) (*reconcile.Reconciler, error) {
	runner := c.orRunner()
	opts := reconcile.Options{
		Backend: dokku.New(runner),
		Store:   st,
		Snapshot: func(ctx context.Context) (*inventory.HostInventory, error) {
			return inventory.Snapshot(ctx, runner, inventory.SnapshotOptions{
				Namespaces: c.cfg.Namespaces.Order,
			})
		},
		Namespaces: c.cfg.Namespaces.Order,
	}
	if c.cfg.SecretsProvider.Kind == "hostfile" {
		opts.Secrets = secrets.NewHostFile(c.cfg.SecretsDir())
	}
	return reconcile.New(opts)
}

// runInput assembles the reconciler's input from resolved desired state.
// Actor is user (a human through the CLI); the lease holder identifies the
// CLI (T-310 owns the final holder-id canon). Confirm is the --confirm
// side of the ADR-0009 consent pair; VolumeRoot deliberately stays unset —
// config has no volume root field yet, so storage changes fail contained
// with the ADR-0009 refusal (open item, M3 review).
func (c *cctx) runInput(d desired, prune, confirm bool) reconcile.RunInput {
	return reconcile.RunInput{
		Specs:         d.specs,
		Revision:      d.revision,
		Actor:         domain.ActorUser,
		CorrelationID: "",
		HolderID:      "topctl",
		Prune:         prune,
		Confirm:       confirm,
		Namespaces:    c.cfg.Namespaces.Order,
		Sources:       d.sources,
	}
}

// execReconcile runs the pipeline (shared by reconcile and the real
// deploy) and maps the outcome onto contract exit codes:
//
//   - run succeeded            → 0 (rendered)
//   - any failed service / RunError (run-level) → 3 (operation failed)
//   - lease conflict           → 3, code "lease-conflict" (typed
//     LeaseConflictError; docs/contracts/cli.md has no dedicated exit row
//     for conflicts — mapping to the contract's broad "operation failed"
//     space with a specific envelope code; a dedicated code is proposed
//     for T-310's exit-code canon)
//   - validation failure       → 3, code "validation-failed" (the run IS
//     recorded failed; invalid spec content is not argv usage)
//   - store open/schema errors → 4 (backend/store unavailable)
//
// res is rendered in BOTH success and (contained-)failure outcomes before
// the error envelope is printed.
func (c *cctx) execReconcile(cmd *cobra.Command, d desired, in reconcile.RunInput, st store.Store) error {
	recon, err := c.orReconciler(st)
	if err != nil {
		return c.emitErrCode(cmd, "reconciler-unavailable", ExitUnreachable, err.Error())
	}
	res, err := recon.Run(cmd.Context(), in)
	// A run that STARTED (an operation id exists) always renders —
	// success and contained failure both print their outcomes before any
	// error envelope.
	if res.OperationID != uuid.Nil {
		if c.format != FormatJSON { // advisory spec warnings (mutable tags/latest) must not fail a run
			_ = warnSpecText(c.out, d.warnings)
		}
		if rerr := c.renderRunResult(cmd, res); rerr != nil {
			return rerr
		}
	}
	if err == nil {
		return nil
	}
	var lce reconcile.LeaseConflictError
	var re *reconcile.RunError
	switch {
	case errors.As(err, &lce):
		return c.emitErrCode(cmd, "lease-conflict", ExitFailed,
			fmt.Sprintf("reconcile lease %q is held by %q (nothing was mutated; wait for the holder to finish or for lease expiry, or reconcile with --now)", lce.Lease, lce.Holder))
	case errors.Is(err, domain.ErrInvalid):
		return c.emitErrCode(cmd, "validation-failed", ExitFailed, err.Error())
	case errors.Is(err, store.ErrSchemaNewer):
		return c.emitErrCode(cmd, "store-schema-newer", ExitUnreachable, err.Error())
	case errors.As(err, &re):
		return c.emitErrCode(cmd, "failed", ExitFailed, err.Error())
	default:
		return c.emitErrCode(cmd, "failed", ExitFailed, err.Error())
	}
}

// renderDiff prints the diff report and returns the boolean drift verdict
// (any create/update/delete/drifted(self) row or orphaned service). JSON
// mirrors cli.md's illustrative envelope; text is the plan vocabulary.
func (c *cctx) renderDiff(cmd *cobra.Command, rep reconcile.DiffReport) (bool, error) {
	drift := false
	for _, s := range rep.Services {
		if s.Kind != domain.PlanNoop {
			drift = true
		}
	}
	if len(rep.Orphaned) > 0 {
		drift = true
	}
	if c.format == FormatJSON {
		if err := c.emitOk(cmd, diffMessage(drift), diffData(rep)); err != nil {
			return drift, err
		}
		return drift, nil
	}
	if err := renderDiffText(c.out, rep); err != nil {
		return drift, &ExitError{Code: ExitFailed, Err: err}
	}
	return drift, nil
}

func diffMessage(drift bool) string {
	if drift {
		return "drift present (exit 1 for scripting)"
	}
	return "no drift"
}

// diffData builds the JSON payload in docs/contracts/cli.md's shape
// (revision / services / summary); secret-reference field entries carry
// path + marker only — never a value (ADR-0008).
func diffData(rep reconcile.DiffReport) map[string]any {
	rows := make([]map[string]any, 0, len(rep.Services))
	summary := map[string]int{
		"create": 0, "update": 0, "noop": 0, "delete": 0, "driftedSelf": 0, "orphaned": len(rep.Orphaned),
	}
	for _, s := range rep.Services {
		switch s.Kind {
		case domain.PlanCreate:
			summary["create"]++
		case domain.PlanUpdate:
			summary["update"]++
		case domain.PlanNoop:
			summary["noop"]++
		case domain.PlanDelete:
			summary["delete"]++
		case domain.PlanDriftedSelf:
			summary["driftedSelf"]++
		}
		row := map[string]any{"service": s.Ref.String(), "change": s.Kind.String()}
		if s.Source.File != "" {
			row["source"] = s.Source.String()
		}
		fields := make([]map[string]any, 0, len(s.Changes))
		for _, ch := range s.Changes {
			f := map[string]any{"path": ch.Path}
			if ch.SecretRef {
				f["secretRef"] = true
				switch {
				case ch.From == nil && ch.To != nil:
					f["op"] = "+"
				case ch.From != nil && ch.To == nil:
					f["op"] = "-"
				default:
					f["op"] = "~"
				}
			} else {
				switch {
				case ch.From == nil && ch.To != nil:
					f["from"] = "<absent>"
					f["to"] = deref(ch.To)
				case ch.From != nil && ch.To == nil:
					f["from"] = deref(ch.From)
					f["to"] = "<absent>"
				default:
					f["from"] = deref(ch.From)
					f["to"] = deref(ch.To)
				}
			}
			fields = append(fields, f)
		}
		row["fields"] = fields
		rows = append(rows, row)
	}
	return map[string]any{"revision": rep.Revision, "services": rows, "summary": summary}
}

// renderDiffText prints the plan in internal/plan's vocabulary plus the
// spec origin and the orphaned report.
func renderDiffText(w io.Writer, rep reconcile.DiffReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "diff %s\n", rep.Revision)
	for _, s := range rep.Services {
		fmt.Fprintf(&b, "  %s: %s\n", s.Ref, s.Kind)
		if s.Source.File != "" {
			fmt.Fprintf(&b, "    spec: %s\n", s.Source.String())
		}
		for _, ch := range s.Changes {
			fmt.Fprintf(&b, "    %s\n", ch)
		}
	}
	if len(rep.Orphaned) > 0 {
		names := make([]string, 0, len(rep.Orphaned))
		for _, ref := range rep.Orphaned {
			names = append(names, ref.String())
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "  orphaned (present on host, absent from git): %s\n", strings.Join(names, ", "))
		fmt.Fprintf(&b, "  (removal only via `topctl reconcile --prune` with --confirm)\n")
	}
	_, err := w.Write([]byte(b.String()))
	if err != nil {
		return fmt.Errorf("writing diff output: %w", err)
	}
	return nil
}

func deref(v *string) string {
	if v == nil {
		return "<absent>"
	}
	return *v
}

// renderRunResult prints one run's per-service outcomes (both modes).
// JSON follows the reconciler contract's example history record shape.
func (c *cctx) renderRunResult(cmd *cobra.Command, res reconcile.RunResult) error {
	if c.format == FormatJSON {
		return c.emitOk(cmd, runMessage(res), runData(res))
	}
	return renderRunText(c.out, res)
}

func runMessage(res reconcile.RunResult) string {
	if res.Status == domain.RunFailed {
		return "reconcile failed (exit 3)"
	}
	return "reconcile succeeded"
}

// runData mirrors the reconciler contract's record vocabulary:
// operationId / actor / revision / state / services[{service, result,
// changes, error, hint, durationMs, skipReason}].
func runData(res reconcile.RunResult) map[string]any {
	rows := make([]map[string]any, 0, len(res.Outcomes))
	for _, o := range res.Outcomes {
		row := map[string]any{
			"service":    o.Ref.String(),
			"result":     o.Result.String(),
			"changes":    o.Changes,
			"durationMs": o.DurationMs,
		}
		if o.Err != "" {
			row["error"] = o.Err
		}
		if o.Hint != "" {
			row["hint"] = o.Hint
		}
		if o.Skip.IsValid() {
			row["skipReason"] = o.Skip.String()
		}
		rows = append(rows, row)
	}
	data := map[string]any{
		"operationId": "op_" + res.OperationID.String(),
		"revision":    res.Revision,
		"state":       res.Status.String(),
		"services":    rows,
	}
	if len(res.Orphaned) > 0 {
		refs := make([]string, 0, len(res.Orphaned))
		for _, r := range res.Orphaned {
			refs = append(refs, r.String())
		}
		sort.Strings(refs)
		data["orphaned"] = refs
	}
	if len(res.Pruned) > 0 {
		refs := make([]string, 0, len(res.Pruned))
		for _, r := range res.Pruned {
			refs = append(refs, r.String())
		}
		sort.Strings(refs)
		data["pruned"] = refs
	}
	if len(res.PruneRefusals) > 0 {
		bl := make([]map[string]any, 0, len(res.PruneRefusals))
		for _, pr := range res.PruneRefusals {
			bl = append(bl, map[string]any{"service": pr.Ref.String(), "reason": pr.Reason})
		}
		data["pruneRefusals"] = bl
	}
	if res.ValidationErr != "" {
		data["validationError"] = res.ValidationErr
	}
	return data
}

// renderRunText prints the run outcome per service with the failure hint
// (the reconciler contract's actionable record).
func renderRunText(w io.Writer, res reconcile.RunResult) error {
	var b strings.Builder
	mode := "reconcile"
	if res.Status == domain.RunFailed {
		mode = "reconcile (FAILED)"
	}
	fmt.Fprintf(&b, "%s: %s toward %s\n", mode, "op_"+res.OperationID.String(), res.Revision)
	for _, o := range res.Outcomes {
		fmt.Fprintf(&b, "  %s: %s", o.Ref, o.Result)
		if o.Skip.IsValid() {
			fmt.Fprintf(&b, " (%s)", o.Skip)
		}
		b.WriteByte('\n')
		for _, ch := range o.Changes {
			fmt.Fprintf(&b, "    ~ %s\n", ch)
		}
		if o.Err != "" {
			fmt.Fprintf(&b, "    error: %s\n", o.Err)
		}
		if o.Hint != "" {
			fmt.Fprintf(&b, "    hint: %s\n", o.Hint)
		}
	}
	if len(res.Orphaned) > 0 {
		refs := make([]string, 0, len(res.Orphaned))
		for _, r := range res.Orphaned {
			refs = append(refs, r.String())
		}
		sort.Strings(refs)
		fmt.Fprintf(&b, "  orphaned: %s (removal only via reconcile --prune + --confirm)\n", strings.Join(refs, ", "))
	}
	_, err := w.Write([]byte(b.String()))
	if err != nil {
		return fmt.Errorf("writing reconcile output: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// history reading + rendering
// ---------------------------------------------------------------------------

// storeExit maps state-store open failures onto exit 4
// (docs/contracts/cli.md "backend/store unavailable"), including the
// schema-newer downgrade-protection miss (state-store contract).
func (c *cctx) storeExit(cmd *cobra.Command, err error) error {
	code := "store-unavailable"
	if errors.Is(err, store.ErrSchemaNewer) {
		code = "store-schema-newer"
	}
	return c.emitErrCode(cmd, code, ExitUnreachable, err.Error())
}

// resolveExit maps desired-state resolution failures (gitsource geometry,
// strict spec loading, DiffOnly's validate phase) onto the contract exit
// table: git side → 4 (unavailable / auth / revision-gone), spec content
// → 3 (validation-failed; the spec set is data, not argv — the run IS
// recorded failed for mutating callers).
func (c *cctx) resolveExit(cmd *cobra.Command, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gitsource.ErrRevisionGone):
		return c.emitErrCode(cmd, "revision-gone", ExitUnreachable, err.Error())
	case errors.Is(err, gitsource.ErrGitAuth), errors.Is(err, gitsource.ErrHostKey):
		return c.emitErrCode(cmd, "git-auth", ExitUnreachable, err.Error())
	case errors.Is(err, gitsource.ErrGitUnavailable):
		return c.emitErrCode(cmd, "git-unavailable", ExitUnreachable, err.Error())
	case errors.Is(err, gitsource.ErrGitInternal), errors.Is(err, gitsource.ErrInvalidSHA):
		return c.emitErrCode(cmd, "git-internal", ExitUnreachable, err.Error())
	default:
		return c.emitErrCode(cmd, "validation-failed", ExitFailed, err.Error())
	}
}

// deploySpecs returns the closed dependsOn set for a single-service
// deploy (docs/contracts/cli.md deploy row: the reconciler, single
// service). A deploy pulls its transitive dependencies — the pipeline
// reads a service's dependents AFTER the dependency's own readiness, so a
// bare spec with an unresolved dependsOn would fail the whole run for a
// reason the operator can fix only by deploying the dependency too.
// Unknown targets (service or dependency) are usage errors.
func deploySpecs(specs []domain.Service, want domain.ServiceRef) ([]domain.Service, error) {
	byRef := make(map[domain.ServiceRef]domain.Service, len(specs))
	for _, s := range specs {
		byRef[s.Ref] = s
	}
	if _, ok := byRef[want]; !ok {
		return nil, fmt.Errorf("%w: unknown service %s: no spec in the platform repo defines it", domain.ErrInvalid, want)
	}
	seen := make(map[domain.ServiceRef]bool, len(specs))
	var stack []domain.ServiceRef
	push := func(refs []domain.ServiceRef) error {
		for _, r := range refs {
			if _, ok := byRef[r]; !ok {
				return fmt.Errorf("%w: deploy dependency %s is not defined by any spec in the repo", domain.ErrInvalid, r)
			}
			stack = append(stack, r)
		}
		return nil
	}
	stack = append(stack, want)
	var out []domain.Service
	for len(stack) > 0 {
		ref := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[ref] {
			continue
		}
		seen[ref] = true
		svc := byRef[ref]
		if err := push(svc.DependsOn); err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, nil
}

// historyRows applies the optional service-ref filter. TODAY (T-309):
// operation
// records are all the store offers (no run-summary lister), so a service
// filter is matched against the evidence RECORDED in each row (redacted
// failure text nominating "ns/name: ..."). Refactor with T-310's
// run-entry canon.
func historyRows(ctx context.Context, st store.Store, limit int, ref domain.ServiceRef) ([]*domain.Operation, error) {
	if st == nil { // absent state (`orStore`'s fresh-host contract): honest empty read
		return nil, nil
	}
	ops, err := st.ListOperations(ctx, limit, nil)
	if err != nil {
		return nil, err
	}
	if ref.IsZero() {
		return ops, nil
	}
	needle := ref.String()
	var out []*domain.Operation
	for _, op := range ops {
		if strings.Contains(op.Err, needle) {
			out = append(out, op)
		}
	}
	return out, nil
}

// historyData builds the JSON payload: one row per operation, contract
// vocabulary, with an actionable hint for failed rows whose redacted
// failure text names the service (T-308's FailedLogs retrieval is the
// `topctl logs` body's surface, not history's).
func historyData(ops []*domain.Operation) map[string]any {
	rows := make([]map[string]any, 0, len(ops))
	for _, op := range ops {
		row := map[string]any{
			"operationId":   "op_" + op.ID.String(),
			"kind":          op.Kind.String(),
			"state":         op.State.String(),
			"actor":         op.Actor.String(),
			"correlationId": op.CorrelationID,
			"revision":      op.Revision,
			"startedAt":     tstamp(op.StartedAt),
			"finishedAt":    tstamp(op.FinishedAt),
		}
		if op.Err != "" {
			row["error"] = op.Err
			if ref := failedServiceRef(op.Err); !ref.IsZero() {
				row["hint"] = "check `topctl logs " + ref.String() + "` for the failed deploy; last known-good still serving"
			}
		}
		rows = append(rows, row)
	}
	return map[string]any{"operations": rows}
}

// failedServiceRef parses the "ns/name: reason" shape the reconciler
// records in outcome error texts; anything else yields the zero ref.
func failedServiceRef(errText string) domain.ServiceRef {
	head, _, ok := strings.Cut(errText, ":")
	if !ok {
		return domain.ServiceRef{}
	}
	ref, perr := domain.ParseServiceRef(strings.TrimSpace(head))
	if perr != nil {
		return domain.ServiceRef{}
	}
	return ref
}

// renderHistoryText prints the history view (newest first, failed rows
// flagged with their hint). Renders structurally, not through the sorted
// Data-map envelope, because the shape is row-per-op.
func renderHistoryText(w io.Writer, ops []*domain.Operation) error {
	var b strings.Builder
	if len(ops) == 0 {
		b.WriteString("history: no operations recorded yet\n")
	}
	for _, op := range ops {
		fmt.Fprintf(&b, "op_%s  %-9s %-9s actor=%s revision=%s started=%s\n",
			op.ID.String(), op.Kind, op.State, op.Actor, op.Revision, tstamp(op.StartedAt))
		if op.Err != "" {
			fmt.Fprintf(&b, "  error: %s\n", op.Err)
			if ref := failedServiceRef(op.Err); !ref.IsZero() {
				fmt.Fprintf(&b, "  hint: check `topctl logs %s`; last known-good still serving\n", ref)
			}
		}
	}
	_, err := w.Write([]byte(b.String()))
	if err != nil {
		return fmt.Errorf("writing history output: %w", err)
	}
	return nil
}

func tstamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
