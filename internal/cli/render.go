package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// JSONSchemaVersion is the version of the versioned JSON envelope that all
// topctl output speaks (docs/contracts/cli.md). It is the single source of
// truth; cmd/topctl's output schema derives from it, and --schema-version
// negotiation validates against it.
const JSONSchemaVersion = 1

// Format is the selected output mode: -o text (default) or -o json.
type Format string

// Output formats.
const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

// ParseFormat validates -o values.
func ParseFormat(s string) (Format, error) {
	switch Format(s) {
	case FormatText:
		return FormatText, nil
	case FormatJSON:
		return FormatJSON, nil
	case "":
		return "", fmt.Errorf("%w: empty -o value; want text or json", ErrUsage)
	default:
		return "", fmt.Errorf("%w: -o %q: want text or json", ErrUsage, s)
	}
}

// Envelope is the versioned output object for successful command
// responses. Errors use ErrEnvelope. Data is a free-form, per-command
// payload; in early milestones it carries plain facts (version stamps,
// stub notices) validated by golden tests.
type Envelope struct {
	SchemaVersion int            `json:"schemaVersion"`
	Command       string         `json:"command"`
	Message       string         `json:"message,omitempty"`
	Data          map[string]any `json:"data,omitempty"`
}

// ErrEnvelope is the versioned output object for command failures. Code is
// a stable machine string ("usage", "not-implemented", "failed", …) that
// pairs with the process exit code; agents read code, humans read Message.
type ErrEnvelope struct {
	SchemaVersion int    `json:"schemaVersion"`
	Command       string `json:"command"`
	Error         struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// RenderEnvelope writes env to w in the requested format. JSON output is
// always a single compact line: agent-parsable, zero ambiguity.
func RenderEnvelope(w io.Writer, f Format, env Envelope) error {
	switch f {
	case FormatJSON:
		b, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("rendering json envelope: %w", err)
		}
		return writeLine(w, b)
	case FormatText:
		var out []byte
		out = append(out, env.Command...)
		out = append(out, '\n')
		if env.Message != "" {
			out = append(out, "  message: "...)
			out = append(out, env.Message...)
			out = append(out, '\n')
		}
		for _, k := range sortedKeys(env.Data) {
			out = append(out, fmt.Sprintf("  %s: %v\n", k, env.Data[k])...)
		}
		return writeBytes(w, out)
	default:
		return fmt.Errorf("%w: unknown format %q", ErrUsage, f)
	}
}

// RenderErrEnvelope writes a failure envelope to w (JSON compact line, or
// plain text for humans).
func RenderErrEnvelope(w io.Writer, f Format, env ErrEnvelope) error {
	switch f {
	case FormatJSON:
		b, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("rendering json error envelope: %w", err)
		}
		return writeLine(w, b)
	case FormatText:
		return writeBytes(w, []byte(fmt.Sprintf("topctl %s: %s (code: %s)\n",
			env.Command, env.Error.Message, env.Error.Code)))
	default:
		return fmt.Errorf("%w: unknown format %q", ErrUsage, f)
	}
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func writeLine(w io.Writer, b []byte) error {
	b = append(b, '\n')
	return writeBytes(w, b)
}

func writeBytes(w io.Writer, b []byte) error {
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}
