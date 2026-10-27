// SPDX-License-Identifier: MIT

package recover

import (
	"fmt"
	"io"
)

// Render helpers for the diagnose report — the doctor Finding/Report
// TYPES are reused (imported), so recovery prints exactly the preflight
// shape; the TEXT header and hint wording follow the backup-recovery
// contract's own example instead (a "recover: …" header and
// "→ suggested: …" hint lines), because the contract pins the example
// output shape for this command and doctor pins its own for `doctor`.
//
//	Header:               recover diagnose: <summary>
//	Finding line (doctor-aligned columns):
//	  fail [controller/state] controller running state: …
//	Hint line:            → suggested: `topctl recover restart`
//
// For the JSON envelope, doctor's Report.EnvelopeData (ok / *_count /
// findings / floors) IS reused directly — the payload shape is the same
// object under both commands, multiplication avoided by design.

// Summary is the single human sentence for the envelope Message.
func Summary(r Report) string {
	switch {
	case !r.OK():
		return fmt.Sprintf("diagnose found %d failure(s), %d warning(s) — every fix below is a suggested command; recovery output doubles as a runbook",
			r.Count(sevFail), r.Count(sevWarn))
	default:
		return fmt.Sprintf("diagnose found no failures (%d warning(s)); nothing here blocks recovery",
			r.Count(sevWarn))
	}
}

// RenderText writes the human report in doctor's column layout with the
// recovery contract's suggestion arrows.
func RenderText(w io.Writer, r Report) error {
	if _, err := fmt.Fprintf(w, "recover %s\n", Summary(r)); err != nil {
		return fmt.Errorf("writing text report: %w", err)
	}
	for _, f := range r.Findings {
		line := fmt.Sprintf("  %-4s %-24s %s: %s\n",
			string(f.Sev()), "["+f.ID+"]", f.Check, f.Message)
		if _, err := io.WriteString(w, line); err != nil {
			return fmt.Errorf("writing text report: %w", err)
		}
		if f.Hint != "" {
			if _, err := fmt.Fprintf(w, "        → suggested: %s\n", f.Hint); err != nil {
				return fmt.Errorf("writing text report: %w", err)
			}
		}
	}
	return nil
}

// RenderResultText writes a successful action's human output (the
// runbook view: what was decided, what ran, what came out of it).
func RenderResultText(w io.Writer, verb string, res *Result) error {
	if _, err := fmt.Fprintf(w, "recover %s: %s decided via %s\n", verb, res.App, res.Source); err != nil {
		return fmt.Errorf("writing text report: %w", err)
	}
	for _, c := range res.Commands {
		if _, err := fmt.Fprintf(w, "  ran: %s\n", c); err != nil {
			return fmt.Errorf("writing text report: %w", err)
		}
	}
	for _, n := range res.Notes {
		if _, err := fmt.Fprintf(w, "  note: %s\n", n); err != nil {
			return fmt.Errorf("writing text report: %w", err)
		}
	}
	return nil
}
