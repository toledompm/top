package obs

import (
	"context"
	"log/slog"
)

// Correlation: an operation's identity travels in the context, and every log
// line emitted through the correlated logger carries an "op" group with it
// (ADR-0006). The stored identity is also retrievable directly, e.g. to stamp
// operation records. Field values are operator/system-provided identifiers
// (operation ids, "ns/name" service refs, delivery ids); the logger renders
// them verbatim — no redaction happens here (ADR-0006, internal/exec).

type (
	ctxOpKey     struct{}
	ctxLoggerKey struct{}
)

// OpInfo is the correlation attached to a context.
type OpInfo struct {
	// ID is the operation id, e.g. "op_01J…" (UUID/generate upstream).
	ID string
	// Service is the affected service ref "ns/name", if the scope is one.
	Service string
	// DeliveryID is the webhook delivery id, if this run came from one.
	DeliveryID string
	// Actor is what triggered the run ("webhook", "poll", "topctl", …).
	Actor string
}

// empty reports whether nothing meaningful is set.
func (o OpInfo) empty() bool { return o == OpInfo{} }

// Ctx returns a child context carrying info, plus a correlated logger
// reachable via AppCtxLogger: every record it emits wraps the fields in an
// "op" group, skipping any field left empty.
func (l *Log) Ctx(parent context.Context, info OpInfo) context.Context {
	ctx := context.WithValue(parent, ctxOpKey{}, info)
	if info.empty() {
		return ctx
	}
	logger := slog.New(withOpGroup(l.logger.Handler(), info))
	return context.WithValue(ctx, ctxLoggerKey{}, logger)
}

// OpFromContext extracts the correlation from ctx, if any.
func OpFromContext(ctx context.Context) (OpInfo, bool) {
	info, ok := ctx.Value(ctxOpKey{}).(OpInfo)
	return info, ok && info.ID != ""
}

// AppCtxLogger returns the correlated logger stored in ctx, falling back to
// slog.Default() for paths that were never correlated by Ctx.
func AppCtxLogger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxLoggerKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// withOpGroup wraps h so every record gains an "op" group built
// from info. Empty fields are skipped entirely.
func withOpGroup(h slog.Handler, info OpInfo) slog.Handler {
	attrs := make([]slog.Attr, 0, 4)
	if info.ID != "" {
		attrs = append(attrs, slog.String("id", info.ID))
	}
	if info.Service != "" {
		attrs = append(attrs, slog.String("service", info.Service))
	}
	if info.DeliveryID != "" {
		attrs = append(attrs, slog.String("delivery", info.DeliveryID))
	}
	if info.Actor != "" {
		attrs = append(attrs, slog.String("actor", info.Actor))
	}
	if len(attrs) == 0 {
		return h
	}
	return h.WithGroup("op").WithAttrs(attrs)
}
