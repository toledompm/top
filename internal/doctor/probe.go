// SPDX-License-Identifier: MIT

package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/toledompm/top/internal/exec"
)

// errPermission marks "the probe itself could not run for lack of
// privilege" (e.g. unprivileged bind of a port < 1024), distinct from
// "something holds the port".
var errPermission = errors.New("probe requires privileges")

// linux and macOS only (documented in README): syscall.Statfs exists on
// both with the same field names we need. Windows/statfs-free targets are
// out of scope for doctor (checkOS fails/warns them anyway, and there is
// no statfs shim for them — the config explicitly refuses).
func systemDiskProbe(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// systemMemProbe returns a probe reading total system RAM, honoring the
// platform:
//   - linux: /proc/meminfo MemTotal (kB → bytes) — no subprocess;
//   - darwin (dev-run): `sysctl -n hw.memsize` via the same Runner as the
//     rest of doctor (dev-run accuracy; the check messages say so).
func systemMemProbe(runner exec.Runner) MemProbe {
	return func() (uint64, error) {
		if runtime.GOOS == "linux" {
			return meminfoTotal("/proc/meminfo")
		}
		out, _, err := runner.Run(context.Background(), "sysctl", "-n", "hw.memsize")
		if err != nil {
			return 0, err
		}
		s := strings.TrimSpace(string(out))
		var total uint64
		if _, err := fmt.Sscanf(s, "%d", &total); err != nil {
			return 0, fmt.Errorf("sysctl hw.memsize: unexpected output %q", s)
		}
		return total, nil
	}
}

// meminfoTotal extracts MemTotal's kB total. strict about the mal-form
// file: unknown content is an error (the check turns it into a warn).
func meminfoTotal(path string) (uint64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, ln := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(ln, "MemTotal:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(ln, "MemTotal:"))
		if len(fields) >= 2 && fields[1] == "kB" {
			kb, err := strconv.ParseUint(fields[0], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("MemTotal %q: %w", fields[0], err)
			}
			return kb * 1024, nil
		}
	}
	return 0, errors.New("MemTotal not found in " + path)
}

// liveListener is the production ListenProbe: bind, hand back a function
// that immediately closes. Doing both right away keeps the observed
// "free" verdict honest; the tiny window between close and the install's
// own bind is a documented race (README).
func liveListener(network, addr string) (func(), error) {
	l, err := net.Listen(network, addr)
	if err != nil {
		if isPermission(err) {
			return nil, fmt.Errorf("%s %s: %w", network, addr, errPermission)
		}
		return nil, err
	}
	return func() { _ = l.Close() }, nil
}

// isPermission classifies bind failures that are privilege-driven
// (EACCES/EPERM) as opposed to occupancy (EADDRINUSE).
func isPermission(err error) bool {
	if ev, ok := unwrapErrno(err); ok {
		return ev == syscall.EACCES || ev == syscall.EPERM
	}
	return false
}

func unwrapErrno(err error) (syscall.Errno, bool) {
	var se syscall.Errno
	if errors.As(err, &se) {
		return se, true
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return unwrapErrno(oe.Err)
	}
	return 0, false
}
