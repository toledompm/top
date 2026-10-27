// helper-echo echoes its arguments to stdout, one space-joined line — the
// smallest possible portable "real subprocess" for hermetic exec tests.
// With -err, the joined line also goes to stderr. Built on demand by tests
// via `go build` (see internal/exec/tests' TestMain).
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	joined := strings.Join(os.Args[1:], " ")
	if len(os.Args) > 1 && (os.Args[1] == "-err" || os.Args[1] == "-fail") {
		rest := strings.Join(os.Args[2:], " ")
		fmt.Fprintln(os.Stderr, rest)
		if os.Args[1] == "-fail" {
			_, _ = fmt.Fprintln(os.Stdout, rest)
			os.Exit(1)
		}
		return
	}
	fmt.Println(joined)
}
