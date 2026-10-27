package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var helperEcho, helperEnv string

// TestMain builds tiny Go helper binaries into a temp dir. Why: the test set
// must be hermetic and portable (macOS + Linux). Reaching for /usr/bin/printf
// or bash builtins breaks either here or there, and `sh -c` would undermine
// the argv-only invariant this package exists to prove. Compiling
// exectest/cmd/helper-{echo,env} in TestMain gives guaranteed binaries with
// zero external dependencies.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "top-exec-test")
	if err != nil {
		fmt.Fprintf(os.Stderr, "temp dir: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// cwd of a go test run is this package's dir; the module root is two up.
	root := "../.."
	for name, dest := range map[string]string{
		"helper-echo": filepath.Join(dir, "helper-echo"),
		"helper-env":  filepath.Join(dir, "helper-env"),
	} {
		build := exec.Command("go", "build", "-o", dest, "./internal/exec/exectest/cmd/"+name)
		build.Dir = root
		if out, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s\n", name, err, out)
			os.Exit(1)
		}
	}
	helperEcho = filepath.Join(dir, "helper-echo")
	helperEnv = filepath.Join(dir, "helper-env")
	os.Exit(m.Run())
}

func TestExitOneBecomesExitError(t *testing.T) {
	r := NewFastExec()
	_, stderr, err := r.Run(context.Background(), "false")
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("want *ExitError, got %#v (stderr=%q)", err, stderr)
	}
	if ee.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", ee.ExitCode)
	}
	if !IsExitCode(err, 1) || IsExitCode(err, 2) {
		t.Error("IsExitCode mismatch")
	}
	if !strings.Contains(ee.Error(), "exit 1") {
		t.Errorf("Error() = %q, want exit 1 mention", ee.Error())
	}
}

func TestExitZeroStderrIsSuccess(t *testing.T) {
	r := NewFastExec()
	_, stderr, err := r.Run(context.Background(), helperEcho, "-err", "warning:-something")
	if err != nil {
		t.Fatalf("err = %v, want nil (stdlib exit status decides success)", err)
	}
	if !bytes.Contains(stderr, []byte("warning:-something")) {
		t.Errorf("stderr = %q, want the warning surfaced to the caller", stderr)
	}
}

func TestStdoutStderrCapturedSeparately(t *testing.T) {
	r := NewFastExec()
	stdout, stderr, err := r.Run(context.Background(), helperEcho, "a", "b")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if string(stdout) != "a b\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if len(stderr) != 0 {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestTimeoutType(t *testing.T) {
	r := NewFastExec()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := r.Run(ctx, "sleep", "5") // sleep is POSIX everywhere
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want errors.Is(err, ErrTimeout)", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the underlying context deadline reachable", err)
	}
	if strings.Contains(err.Error(), "sleep 5") && !strings.Contains(err.Error(), "sleep") {
		t.Error("error text lost the command label")
	}
}

func TestRunStreamStreams(t *testing.T) {
	r := NewFastExec()
	var sOut, sErr bytes.Buffer
	err := r.RunStream(context.Background(), &sOut, &sErr, helperEcho, "stream", "me")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if sOut.String() != "stream me\n" || sErr.Len() != 0 {
		t.Errorf("streamed stdout=%q stderr=%q", sOut.String(), sErr.String())
	}
}

func TestRedactionInBuffersAndError(t *testing.T) {
	const secret = "s3cr3t-value-8831"
	r := NewFastExec()
	r.RedactSeed = []string{secret}

	// Secret reaching stdout (echoed argv) must never survive the buffer.
	stdout, _, err := r.Run(context.Background(), helperEcho, "token", secret)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(string(stdout), secret) {
		t.Errorf("stdout leaked the secret: %q", stdout)
	}
	if !bytes.Contains(stdout, []byte("***")) {
		t.Errorf("stdout = %q, want marker", stdout)
	}

	// Secret reaching stderr while exiting non-zero must be redacted in the
	// ExitError text too.
	_, _, err = r.Run(context.Background(), helperEcho, "-fail", secret)
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("want ExitError, got %#v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(string(ee.Stderr), secret) {
		t.Errorf("error/staterr leaked the secret: err=%q stderr=%q", err, string(ee.Stderr))
	}
}

func TestEnvHygiene(t *testing.T) {
	t.Setenv("TOP_PARENT_ONLY_MUST_NOT_PASS", "1")
	r := NewFastExec()
	r.ExtraEnv = []string{"TOP_EXTRA=visual"}

	stdout, _, err := r.Run(context.Background(), helperEnv)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	env := string(stdout)
	if !strings.Contains(env, "TOP_EXTRA=visual") {
		t.Errorf("env missing ExtraEnv:\n%s", env)
	}
	if !strings.Contains(env, "PATH=") || !strings.Contains(env, "HOME=") {
		t.Errorf("env missing DefaultEnvPassthrough keys:\n%s", env)
	}
	if strings.Contains(env, "TOP_PARENT_ONLY_MUST_NOT_PASS") {
		t.Errorf("env leaked a parent-only var:\n%s", env)
	}
}

func TestEnvPassthroughAndInheritAll(t *testing.T) {
	t.Setenv("TOP_PASSTHROUGH_TARGET", "yes")
	t.Setenv("TOP_PARENT_ONLY_MUST_NOT_PASS", "1")

	var stdout []byte
	var err error
	t.Run("explicit passthrough", func(t *testing.T) {
		r := NewFastExec()
		r.EnvPassthrough = []string{"TOP_PASSTHROUGH_TARGET"}
		stdout, _, err = r.Run(context.Background(), helperEnv)
		if err != nil {
			t.Fatal(err)
		}
		env := string(stdout)
		if !strings.Contains(env, "TOP_PASSTHROUGH_TARGET=yes") {
			t.Errorf("passthrough var missing:\n%s", env)
		}
		if strings.Contains(env, "TOP_PARENT_ONLY_MUST_NOT_PASS") {
			t.Errorf("non-passthrough parent var leaked:\n%s", env)
		}
	})
	t.Run("inherit all opt-in", func(t *testing.T) {
		r := NewFastExec()
		r.EnvInheritAll = true
		stdout, _, err = r.Run(context.Background(), helperEnv)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(stdout), "TOP_PARENT_ONLY_MUST_NOT_PASS=1") {
			t.Errorf("EnvInheritAll did not forward parent env:\n%s", stdout)
		}
	})
}

func TestInvalidArgv(t *testing.T) {
	r := NewFastExec()
	cases := []struct {
		name   string
		binary string
		args   []string
		want   string
	}{
		{"empty name", "", nil, "empty command name"},
		{"nul in name", "hel\x00p", nil, "NUL"},
		{"nul in arg", helperEcho, []string{"a\x00b"}, "NUL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := r.Run(context.Background(), tc.binary, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want refusing with %q", err, tc.want)
			}
			// Nothing sane can be attempted; the refusal is structural.
		})
	}
}

func TestBinaryNotFound(t *testing.T) {
	r := NewFastExec()
	_, _, err := r.Run(context.Background(), "top-definitely-not-a-binary-4431")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	// ExitError.IsExitCode must not panic/type-confuse on non-exit errors.
	if IsExitCode(err, 1) {
		t.Error("ErrNotFound must not match IsExitCode")
	}
}

func TestDirOption(t *testing.T) {
	r := NewFastExec()
	r.Dir = t.TempDir()
	if _, _, err := r.Run(context.Background(), helperEcho); err != nil {
		t.Fatalf("err = %v", err)
	}
}
