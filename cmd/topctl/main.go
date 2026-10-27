// topctl is TOP's single binary: it provides the CLI surface that installs,
// configures, and operates the TOP platform on a Dokku host (see ADR-0006
// and ADR-0011 for the architecture and CLI contract).
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Version stamps the binary. Overridden at build time via:
//
//	-ldflags "-X main.version=v0.x.y"
var version = "dev"

// schemaVersion is the version of TOP's own output/config schemas that this
// binary speaks (see docs/contracts/). Bumped when the wire format changes.
const schemaVersion = "v0"

// minDokkuVersion is the oldest Dokku release TOP supports.
const minDokkuVersion = "0.38"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "topctl",
		Short:         "TOP control CLI — GitOps reconciliation for Dokku",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version, schema version, and minimum supported Dokku version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := fmt.Sprintf("topctl %s (schemaVersion %s, min Dokku %s)\n", version, schemaVersion, minDokkuVersion)
			_, err := cmd.OutOrStdout().Write([]byte(out))
			if err != nil {
				return fmt.Errorf("writing version output: %w", err)
			}
			return nil
		},
	}
}
