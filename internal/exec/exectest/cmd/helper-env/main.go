// helper-env prints the child's complete environment, one KEY=VALUE per
// line, sorted. Used to prove FastExec's env hygiene in hermetic tests
// (no /usr/bin/env assumptions on any platform). Built on demand by tests
// (see internal/exec's TestMain).
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

func main() {
	env := os.Environ()
	sort.Strings(env)
	_, _ = fmt.Fprint(os.Stdout, strings.Join(env, "\n"))
	_, _ = fmt.Fprintln(os.Stdout)
}
