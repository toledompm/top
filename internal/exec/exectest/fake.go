// Package exectest provides the test double for internal/exec's Runner:
// FakeRunner, a tiny, thread-safe, configurable fake. It is THE seam every
// adapter task (backend/dokku, gitsource, install) uses to test
// behavior without spawning real processes.
//
// API surface is deliberately minimal:
//
//	f := exectest.NewFakeRunner()
//	f.SetOutput("dokku", exectest.Response{Stderr: []byte("created")},
//	    "apps:create", "my-app")
//	f.SetPrefix([]string{"git", "fetch"}, exectest.Response{ExitCode: 128})
//	// runner := f  — satisfies exec.Runner; every call is recorded:
//	// f.Calls -> []Call{Name, Args}
//
// SetOutput matches when name and every listed arg match in exact order
// (extra trailing args are allowed). SetPrefix matches against the ordered
// prefix of name + args. First (most recently registered) match wins; a call
// matching nothing gets the default Response. Reset() clears everything.
//
// Redaction: FakeRunner records exactly the argv it was handed. It never
// echoes captured stdout/stderr back into its log, so a secret can only
// appear in the recorded log if the caller put one in argv — and callers
// must never carry secret *values* in argv (ADR-0008); pass secrets via env
// and register them with FastExec.RedactSeed in production, and with no
// argv-valued secrets in tests.
package exectest

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/toledompm/top/internal/exec"
)

// Assert the fake satisfies the production seam.
var _ exec.Runner = (*FakeRunner)(nil)

// Call records one invocation.
type Call struct {
	Name  string
	Args  []string
	Stdin []byte // content fed to a RunStdin call; empty for Run
}

// String returns a readable form, safe for test output.
func (c Call) String() string {
	out := c.Name
	for _, a := range c.Args {
		out += " " + a
	}
	return out
}

// Response is the canned result for one matched call.
type Response struct {
	// Stdout/Stderr are returned to the caller as the captured buffers.
	Stdout []byte
	Stderr []byte

	// ExitCode, when non-zero, makes Run return an *exec.ExitError
	// carrying this stderr, mirroring FastExec semantics.
	ExitCode int

	// Err, when set, is returned verbatim (simulates errors that are not
	// exit statuses: binary missing, context deadline, I/O failure...).
	Err error
}

// FakeRunner implements exec.Runner behavior for tests. Create with
// NewFakeRunner; zero value is usable.
type FakeRunner struct {
	mu          sync.Mutex
	log         []Call
	byExact     map[string]Response // key: name + "\x00" + args joined by "\x00"
	byPrefix    [][]string          // ordered prefix of [name, args...]
	prefixResps map[int]Response
	defaultResp Response
	nextPrefix  int
	// seqScript support: per-prefix invocation counters + tail policy.
	seqIdx       []int        // index into byPrefix of the scripted entry
	seqParts     [][]Response // sequence, one element per scripted call
	seqTails     []Response   // answer AFTER the sequence is exhausted
	seqDone      map[int]int  // calls already served by each scripted entry
	nextSeqEntry int
}

// NewFakeRunner returns a ready-to-use fake.
func NewFakeRunner() *FakeRunner {
	return &FakeRunner{
		byExact:     map[string]Response{},
		prefixResps: map[int]Response{},
		seqDone:     map[int]int{},
	}
}

// SetOutput registers a canned response matching the given name/args
// combination (args in the call may continue past those listed). Later
// registrations win over earlier ones.
func (f *FakeRunner) SetOutput(name string, resp Response, args ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := exactKey(name, args)
	f.byExact[key] = resp
}

// SetPrefix registers a response matching any call whose name+args begin
// with the given ordered prefix (e.g. ["git", "fetch"]). Later registrations
// win over earlier ones.
func (f *FakeRunner) SetPrefix(prefix []string, resp Response) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byPrefix = append(f.byPrefix, slices.Clone(prefix))
	f.prefixResps[f.nextPrefix] = resp
	f.nextPrefix++
}

// SetDefault gives unmatched calls this response. Useful for asserting
// "nothing unexpected ran" or a blanket failure mode.
func (f *FakeRunner) SetDefault(resp Response) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaultResp = resp
}

// SetScriptedPrefix registers a sequence: the first len(script) calls
// matching the prefix receive script[0..n-1]; every later call receives
// tail. Thread-safe; calls Counter resets only with Reset(). Example
// (matches the shim's nth-failure injection): a dokku "apps:exists" that
// returns exit 20 while the app is missing, 0 after the create:
//
//	f.SetScriptedPrefix([]string{"dokku", "apps:exists"},
//	    []exectest.Response{{ExitCode: 20}}, exectest.Response{})
func (f *FakeRunner) SetScriptedPrefix(prefix []string, script []Response, tail Response) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byPrefix = append(f.byPrefix, slices.Clone(prefix))
	f.prefixResps[f.nextPrefix] = Response{} // never used directly
	idx := f.nextPrefix
	f.nextPrefix++
	f.seqIdx = append(f.seqIdx, idx)
	f.seqParts = append(f.seqParts, append([]Response(nil), script...))
	f.seqTails = append(f.seqTails, tail)
	f.seqDone[idx] = 0
	f.nextSeqEntry++
}

// Reset clears all canned responses, scripts, and the call log, keeping
// the fake reusable between table-test cases.
func (f *FakeRunner) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = nil
	f.byExact = map[string]Response{}
	f.byPrefix = nil
	f.prefixResps = map[int]Response{}
	f.defaultResp = Response{}
	f.nextPrefix = 0
	f.seqIdx = nil
	f.seqParts = nil
	f.seqTails = nil
	f.seqDone = map[int]int{}
	f.nextSeqEntry = 0
}

// Recorded returns a copy of the call log, oldest first.
func (f *FakeRunner) Recorded() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Call, len(f.log))
	copy(out, f.log)
	return out
}

// RecordedCommands returns just the printable command lines, oldest first.
func (f *FakeRunner) RecordedCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.log))
	for i, c := range f.log {
		out[i] = c.String()
	}
	return out
}

// Last returns the most recent call, with ok=false if none.
func (f *FakeRunner) Last() (Call, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.log) == 0 {
		return Call{}, false
	}
	return f.log[len(f.log)-1], true
}

// Run implements the Runner contract: any call (even unmatched or failing)
// is appended to the log first, then the canned response is returned.
// A non-zero ExitCode returns *exec.ExitError (exec.IsExitCode works);
// unmatched calls get the default response.
func (f *FakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	return f.run(ctx, nil, name, args...)
}

// RunStdin implements exec.StdinRunner: identical matching/response rules to
// Run, with the stdin content recorded on the Call for golden assertions
// (the adapter's synthesized healthcheck archive is proven by its bytes).
func (f *FakeRunner) RunStdin(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, []byte, error) {
	content, readErr := io.ReadAll(stdin)
	if readErr != nil {
		content = []byte(fmt.Sprintf("stdin read error: %v", readErr))
	}
	return f.run(ctx, content, name, args...)
}

// run is the shared Run/RunStdin body.
func (f *FakeRunner) run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	resp := f.matchLocked(name, args)
	f.log = append(f.log, Call{Name: name, Args: append([]string{}, args...), Stdin: stdin})
	f.mu.Unlock()

	if resp.Err != nil {
		return resp.Stdout, resp.Stderr, resp.Err
	}
	if resp.ExitCode != 0 {
		return resp.Stdout, resp.Stderr, &exec.ExitError{Name: name, Args: args, ExitCode: resp.ExitCode, Stderr: resp.Stderr}
	}
	return resp.Stdout, resp.Stderr, nil
}

// RunStream implements the Runner contract by writing canned output into
// the writers and returning the canned error path. Output is written in one
// chunk, simulating a fast stream. Errors and exit codes behave like Run.
// RunEnv satisfies install's envRunner seam: environment-carrying callers
// (the --init push, the phase-0 repo probe) delegate to the same argv-
// matched fake. The env itself is NOT recorded (the Call log is argv-only
// by design — env values are the credential channel and stay out of the
// golden surface); tests that must assert on the child env wrap
// FakeRunner with an env-recording shim instead (see install's
// envRecordRunner).
func (f *FakeRunner) RunEnv(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
	return f.Run(ctx, name, args...)
}

func (f *FakeRunner) RunStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	f.mu.Lock()
	resp := f.matchLocked(name, args)
	f.log = append(f.log, Call{Name: name, Args: append([]string{}, args...)})
	f.mu.Unlock()

	if stdout != nil {
		_, _ = stdout.Write(resp.Stdout)
	}
	if stderr != nil {
		_, _ = stderr.Write(resp.Stderr)
	}
	if resp.Err != nil {
		return resp.Err
	}
	if resp.ExitCode != 0 {
		return &exec.ExitError{Name: name, Args: args, ExitCode: resp.ExitCode, Stderr: resp.Stderr}
	}
	return nil
}

func (f *FakeRunner) matchLocked(name string, args []string) Response {
	// Prefixes last-registered-first, then exact, then scripted, then
	// default.
	for i := f.nextPrefix - 1; i >= 0; i-- {
		if matchesPrefix(f.byPrefix[i], name, args) {
			// scripted entries: consume one script step, then tail.
			for e := 0; e < len(f.seqIdx); e++ {
				if f.seqIdx[e] == i && len(f.seqParts[e]) > 0 {
					n := f.seqDone[i]
					if n < len(f.seqParts[e]) {
						f.seqDone[i] = n + 1
						return f.seqParts[e][n]
					}
					return f.seqTails[e]
				}
			}
			return f.prefixResps[i]
		}
	}
	if r, ok := f.byExact[exactKey(name, args)]; ok {
		return r
	}
	return f.defaultResp
}

func matchesPrefix(prefix []string, name string, args []string) bool {
	candidate := append([]string{name}, args...)
	if len(prefix) > len(candidate) {
		return false
	}
	return slices.Equal(prefix, candidate[:len(prefix)])
}

// exactKey builds a map key from name and args.
func exactKey(name string, args []string) string {
	key := name
	for _, a := range args {
		key += "\x00" + a
	}
	return key
}
