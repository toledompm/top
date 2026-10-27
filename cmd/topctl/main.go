// topctl is TOP's single binary. It is intentionally thin: the whole
// command tree, output envelope, exit-code mapping, and stub behavior
// live in internal/cli (docs/contracts/cli.md, ADR-0011). This file only
// injects the build stamp and defers to cli.Execute.
package main

import (
	"os"

	"github.com/toledompm/top/internal/cli"
)

// Version stamps the binary. Overridden at build time via:
//
//	-ldflags "-X main.version=v0.x.y"
var version = "dev"

func main() {
	os.Exit(cli.Execute(cli.Options{Version: version}, nil))
}
