// Package fakeexec hosts tier-2 test plumbing (ADR-0012): helpers around
// argv logs and run fixtures shared by tests that exercise the exec.Runner
// seam through the dokku shim in test/shim. It is NOT imported by production
// code.
//
// Two things live here:
//
//   - argv-log helpers: parsing the newline-JSON call logs the shim (or any
//     recorder) writes, plus EqualArgv golden comparisons for table tests.
//   - Run fixtures: {name, args → stdout, stderr, exitCode} entries loaded
//     from YAML files (test/fixtures/dokku/), and an installer that
//     materializes them into the shim's file-based response registry (see
//     test/shim/README.md for the registry layout and precedence).
//
// Composition note: for unit tests of command construction without processes
// at all, use internal/exec/exectest's FakeRunner directly — the seam types
// overlap on Command-style records but exectest responds in-process. This
// package is for the tier-2 loop where a real child process (the shim
// script) is spawned via PATH.
package fakeexec

import (
	"bufio"
	"crypto/sha1" //nolint:gosec // not a secret digest: stable, short registry keys
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Command is one recorded invocation: binary name plus argv.
type Command struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
}

// String renders the canonical one-liner: "dokku apps:list --format json".
func (c Command) String() string { return strings.Join(append([]string{c.Name}, c.Args...), " ") }

// Equal compares two recorded invocations field by field.
func (c Command) Equal(other Command) bool {
	return c.Name == other.Name && strings.Join(c.Args, "\x1f") == strings.Join(other.Args, "\x1f")
}

// EqualArgv compares a golden argv (table-test want) against a recorded
// Command. Name and every arg must match in order; extra/missing args fail.
func EqualArgv(wantName string, wantArgs []string, got Command) bool {
	if got.Name != wantName || len(got.Args) != len(wantArgs) {
		return false
	}
	for i := range wantArgs {
		if got.Args[i] != wantArgs[i] {
			return false
		}
	}
	return true
}

// ParseLog parses a newline-JSON argv log (one {name,args} object per line,
// as test/shim/bin/dokku writes). Blank lines are ignored.
func ParseLog(r io.Reader) ([]Command, error) {
	cmds := []Command{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c Command
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("fakeexec: bad log line %q: %w", line, err)
		}
		cmds = append(cmds, c)
	}
	return cmds, sc.Err()
}

// ParseLogFile is ParseLog over a file path.
func ParseLogFile(path string) ([]Command, error) {
	f, err := os.Open(path) //nolint:gosec // test fixture path provided by the test
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return ParseLog(f)
}

// Signature returns the shim registry key for a call: hex-encoded sha1 of
// `name + " " + strings.Join(args, " ")`. The shim shell script derives the
// identical key from "$*" (space-joined argv); therefore the con is that
// test args must be space-free (dokku args are).
func Signature(name string, args ...string) string {
	sum := sha1.Sum([]byte(name + " " + strings.Join(args, " ")))
	return hex.EncodeToString(sum[:])
}

// Fixture is one canned response in the run-fixture format. LoadFixtures
// reads these from YAML; InstallShimFixtures materializes them into the
// shim's registry directory.
type Fixture struct {
	Name   string   `yaml:"name"   json:"name"`
	Args   []string `yaml:"args"   json:"args"`
	Stdout string   `yaml:"stdout,omitempty"`
	Stderr string   `yaml:"stderr,omitempty"`
	// Exit is the process exit status; zero (the Go zero value) means success.
	Exit int `yaml:"exit,omitempty"`
}

// Key is the exact argv key this fixture answers: name + " " + space-joined
// args (the string Signature hashes).
func (f Fixture) Key() string { return f.Name + " " + strings.Join(f.Args, " ") }

// Signature is Signature(f.Name, f.Args...).
func (f Fixture) Signature() string { return Signature(f.Name, f.Args...) }

// LoadFixtures reads every *.yaml/*.yml file in dir. Each file holds either
// a single fixture mapping or a sequence of them, in the format:
//
//   - name: dokku
//     args: [apps:list, --format, json]
//     stdout: |
//     ["top-test-app"]
//     stderr: ""
//     exit: 0
//
// Exit defaults to 0 when omitted. Files are read in sorted name order; use
// distinct args rather than relying on order.
func LoadFixtures(dir string) ([]Fixture, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("fakeexec: load fixtures: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch filepath.Ext(e.Name()) {
		case ".yaml", ".yml":
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var all []Fixture
	for _, n := range names {
		var node yaml.Node
		if err := yaml.Unmarshal(mustRead(filepath.Join(dir, n)), &node); err != nil {
			return nil, fmt.Errorf("fakeexec: fixture %s: %w", n, err)
		}
		// Unmarshal into a Node yields the document node; unwrap it.
		if node.Kind == yaml.DocumentNode {
			if len(node.Content) == 0 {
				return nil, fmt.Errorf("fakeexec: fixture %s: empty document", n)
			}
			node = *node.Content[0]
		}
		switch node.Kind {
		case yaml.SequenceNode:
			var list []Fixture
			if err := node.Decode(&list); err != nil {
				return nil, fmt.Errorf("fakeexec: fixture %s: %w", n, err)
			}
			all = append(all, list...)
		case yaml.MappingNode:
			var one Fixture
			if err := node.Decode(&one); err != nil {
				return nil, fmt.Errorf("fakeexec: fixture %s: %w", n, err)
			}
			all = append(all, one)
		default:
			return nil, fmt.Errorf("fakeexec: fixture %s: empty or unparseable", n)
		}
	}
	return all, nil
}

func mustRead(path string) []byte {
	b, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		panic(err) // ReadDir already proved the file exists
	}
	return b
}

// WriteShimFixtures writes each fixture into dir under its exact-signature
// key (dir/<sig>.stdout, .stderr, .exit), creating dir. A fixture whose exit
// is 0 with empty output still gets an explicit .exit file so the registry
// distinguishes "present, empty" from "absent" (absent falls through to
// subcommand/family/default).
func WriteShimFixtures(dir string, fxs []Fixture) error {
	for _, f := range fxs {
		sig := f.Signature()
		if err := writeResponse(filepath.Join(dir, sig), f); err != nil {
			return err
		}
	}
	return nil
}

// WriteShimSubcommand installs a response for every invocation of one
// subcommand regardless of remaining args (registry files subcmd.<sub>.…).
func WriteShimSubcommand(dir, sub string, f Fixture) error {
	return writeResponse(filepath.Join(dir, "subcmd."+sub), f)
}

// WriteShimFamily installs a response for a subcommand family — the part of
// the first arg before ':' (registry files prefix.<family>.…). Families only
// match when neither the exact signature nor the full subcommand matched.
func WriteShimFamily(dir, family string, f Fixture) error {
	return writeResponse(filepath.Join(dir, "prefix."+family), f)
}

func writeResponse(base string, f Fixture) error {
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		return err
	}
	for name, content := range map[string][]byte{
		"stdout": []byte(f.Stdout),
		"stderr": []byte(f.Stderr),
		"exit":   []byte(strconv.Itoa(f.Exit) + "\n"),
	} {
		if err := os.WriteFile(base+"."+name, content, 0o644); err != nil { //nolint:gosec // test fixture dir
			return fmt.Errorf("fakeexec: write shim registry %s: %w", filepath.Base(name), err)
		}
	}
	return nil
}
