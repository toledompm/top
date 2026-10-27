// SPDX-License-Identifier: MIT

package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// Render helpers — mirror internal/cli's envelope style in SPIRIT only
// (doctor cannot import internal/cli without an import cycle: cli wires
// the doctor command body). The tiny shared shape, documented here:
//
//	{"schemaVersion":1,"command":"doctor","ok":<bool>,"findings":[…]}
//
// JSON is a single compact line (agent-parsable), text is one line per
// finding plus a hint block for non-ok findings (human-readable). cli's
// own envelope uses its Envelope.Data for the same payload.

// EnvelopeData returns the payload cli nests into its own Envelope.Data
// (the versioned JSON object with schemaVersion/reportOk/findings).
func (r Report) EnvelopeData() map[string]any {
	return map[string]any{
		"ok":        r.OK(),
		"okCount":   r.Count(SeverityOK),
		"infoCount": r.Count(SeverityInfo),
		"warnCount": r.Count(SeverityWarn),
		"failCount": r.Count(SeverityFail),
		"findings":  r.Findings,
		"minDokku":  MinDokkuVersion,
		"minDocker": MinDockerVersion,
	}
}

// Summary is the single human sentence for the envelope Message field.
func (r Report) Summary() string {
	switch {
	case !r.OK():
		return fmt.Sprintf("doctor found %d failure(s), %d warning(s) — fix the failures before installing",
			r.Count(SeverityFail), r.Count(SeverityWarn))
	case r.Count(SeverityWarn) > 0:
		return "doctor found no failures, " + strconv.Itoa(r.Count(SeverityWarn)) + " warning(s)"
	default:
		return "doctor found no issues"
	}
}

// RenderText writes the human-readable report: a header, one line per
// finding, and a hint line for every non-ok finding.
func (r Report) RenderText(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "doctor: %s\n", r.Summary()); err != nil {
		return fmt.Errorf("writing text report: %w", err)
	}
	for _, f := range r.Findings {
		line := fmt.Sprintf("  %-4s %-22s %s: %s\n",
			string(f.Sev()), "["+f.ID+"]", f.Check, f.Message)
		if _, err := io.WriteString(w, line); err != nil {
			return fmt.Errorf("writing text report: %w", err)
		}
		if f.Hint != "" {
			if _, err := fmt.Fprintf(w, "        hint: %s\n", f.Hint); err != nil {
				return fmt.Errorf("writing text report: %w", err)
			}
		}
	}
	return nil
}

// RenderJSON writes the standalone doctor JSON object as one compact
// line (cli's Envelope is the wrapper when doctor runs under topctl).
func (r Report) RenderJSON(w io.Writer) error {
	type doctorJSON struct {
		SchemaVersion int       `json:"schemaVersion"`
		Command       string    `json:"command"`
		OK            bool      `json:"ok"`
		Findings      []Finding `json:"findings"`
	}
	Object := doctorJSON{SchemaVersion: 1, Command: "doctor", OK: r.OK(), Findings: r.Findings}
	if r.Findings == nil {
		Object.Findings = []Finding{}
	}
	b, err := json.Marshal(Object)
	if err != nil {
		return fmt.Errorf("rendering doctor json: %w", err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("writing doctor json: %w", err)
	}
	return nil
}
