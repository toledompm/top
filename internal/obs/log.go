// Package obs holds TOP's observability plumbing: slog setup with
// correlation (ADR-0006) and the Prometheus metrics helpers (ADR-0010).
//
// Scope discipline: this package only formats strings it is given. Secret
// redaction is enforced upstream in internal/exec (ADR-0006); nothing here
// inspects or rewrites log/metric content.
package obs

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Level names accepted by New and Log.SetLevel.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Log bundles the process logger with a runtime-toggleable level variable.
// The controller holds one Log and hands out correlated loggers via Ctx /
// OpLogger; operators flip verbosity without a restart.
type Log struct {
	logger *slog.Logger
	level  *slog.LevelVar
}

// New builds the process logger: JSON on stderr, level parsed from level
// (default "info" on an empty value).
func New(level string) (*Log, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	l := &Log{level: &slog.LevelVar{}}
	l.level.Set(lvl)
	l.build(os.Stderr)
	return l, nil
}

// SetLevel switches the live level at runtime. An empty value resets to
// info. Unrecognized values are an error and leave the previous level in
// place.
func (l *Log) SetLevel(level string) error {
	lvl, err := parseLevel(level)
	if err != nil {
		return err
	}
	l.level.Set(lvl)
	return nil
}

// Logger returns the base process logger. Records from it carry no "op"
// group; use Ctx/OpLogger for correlated logging.
func (l *Log) Logger() *slog.Logger { return l.logger }

// parseLevel maps a level name to a slog.Level. Empty means info.
func parseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "":
		return slog.LevelInfo, nil
	case LevelDebug:
		return slog.LevelDebug, nil
	case LevelInfo:
		return slog.LevelInfo, nil
	case LevelWarn:
		return slog.LevelWarn, nil
	case LevelError:
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("obs: unknown log level %q", level)
	}
}

// build constructs the JSON handler over w with l.level governing
// enabledness. Split out so tests can swap the writer.
func (l *Log) build(w io.Writer) {
	l.logger = slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l.level}))
}

// newLogForTest is the test seam for capturing output.
func newLogForTest(level string, w io.Writer) *Log {
	l := &Log{level: &slog.LevelVar{}}
	_ = l.SetLevel(level)
	l.build(w)
	return l
}
