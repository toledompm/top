// SPDX-License-Identifier: MIT

package install

import (
	"fmt"
	"io"
	"strconv"
)

// Render helpers — the cli envelope style, text side: a header sentence,
// then the contract's phase-6 lists. The JSON side is Result.DataMap
// inside the CLI envelope (no separate schema).

// Summary is the envelope Message for a Result (install or uninstall).
func Summary(res *Result) string {
	verbs := len(res.Created) + len(res.Removed)
	phrase := "install complete across all phases"
	if res.Resumed {
		phrase = "install resumed: " + strconv.Itoa(len(res.PhasesSkipped)) + " phase(s) skipped as already complete"
	}
	return phrase + "; " + strconv.Itoa(verbs) + " change(s) reported"
}

// RenderResultText writes the human report of one install/uninstall Result.
func RenderResultText(w io.Writer, res *Result) error {
	if _, err := fmt.Fprintf(w, "install: %s\n", Summary(res)); err != nil {
		return fmt.Errorf("writing text report: %w", err)
	}
	writeList := func(label string, items []string) error {
		if len(items) == 0 {
			return nil
		}
		if _, err := fmt.Fprintf(w, "  %s:\n", label); err != nil {
			return fmt.Errorf("writing text report: %w", err)
		}
		for _, i := range items {
			if _, err := fmt.Fprintf(w, "    - %s\n", i); err != nil {
				return fmt.Errorf("writing text report: %w", err)
			}
		}
		return nil
	}
	lists := []struct {
		label string
		items []string
	}{
		{"created", res.Created},
		{"removed", res.Removed},
		{"adopted", res.Adopted},
		{"left alone / kept", res.LeftAlone},
		{"skipped", res.Skipped},
		{"checked", res.Checked},
		{"secrets to provision", res.SecretsToProvision},
	}
	for _, l := range lists {
		if err := writeList(l.label, l.items); err != nil {
			return err
		}
	}
	for _, c := range res.Commands {
		if _, err := fmt.Fprintf(w, "  command: %s\n", c); err != nil {
			return fmt.Errorf("writing text report: %w", err)
		}
	}
	for _, note := range res.Notes {
		if _, err := fmt.Fprintf(w, "  note: %s\n", note); err != nil {
			return fmt.Errorf("writing text report: %w", err)
		}
	}
	for _, next := range res.NextCommands {
		if _, err := fmt.Fprintf(w, "  next: %s\n", next); err != nil {
			return fmt.Errorf("writing text report: %w", err)
		}
	}
	return nil
}
