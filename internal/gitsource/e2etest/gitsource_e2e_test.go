//go:build e2e

// SPDX-License-Identifier: MIT

// Tier-3 e2e for internal/gitsource (ADR-0012 tier 3): REAL git over REAL
// ssh. Run with:
//
//	TOP_E2E=1 go test -tags e2e ./internal/gitsource/e2etest/... -race -count=1
//
// The suite NEVER touches a host git checkout or authorizes anything on a
// real provider. It provisions a disposable container (alpine + git +
// sshd) via testcontainers-go serving a bare repo, and drives the
// PRODUCTION Source (exec.FastRunner) from the local host against it:
// real argv, real GIT_SSH_COMMAND, real strict host-key checking.
//
// Cost: pulls alpine:3.20 (~3 MB) plus in-container `apk add git
// openssh-server` (~25 MB of packages, cached by Docker). Everything is
// disposable (Ryuk teardown); nothing named "top-" outside this run's
// temp dirs. Needs local Docker (macOS-runnable via Docker Desktop, like
// the backend e2e); the TOP_E2E skip guard below fails politely without it.
package e2etest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcwait "github.com/testcontainers/testcontainers-go/wait"

	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/gitsource"
)

const envE2E = "TOP_E2E"

// imageData is the base container image (small alpine; packages installed
// in-container, see package doc).
const imageData = "docker.io/library/alpine:3.20"

func skipWithoutE2E(t *testing.T) {
	t.Helper()
	if os.Getenv(envE2E) != "1" {
		t.Skipf("tier-3 git e2e: set %s=1 to run (needs local Docker)", envE2E)
	}
}

// TestGitSourceE2E is the single top-level test: one disposable git-server
// container, several subtests against it in the documented order.
func TestGitSourceE2E(t *testing.T) {
	skipWithoutE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	srv := startGitServer(t, ctx)

	t.Run("resolve+fetch via deploy key", func(t *testing.T) {
		s := srv.newSource(t, svcKeyGood, svcHostsPinned)
		sha, err := s.ResolveLatest(ctx)
		if err != nil {
			t.Fatalf("ResolveLatest: %v", err)
		}
		if len(sha) != 40 {
			t.Fatalf("sha %q not hex-40", sha)
		}
		snap, err := s.FetchRevision(ctx, sha)
		if err != nil {
			t.Fatalf("FetchRevision: %v", err)
		}
		defer snap.Close()
		body, err := os.ReadFile(filepath.Join(snap.Dir, "specs", "core.yaml"))
		if err != nil || !strings.Contains(string(body), "name: core") {
			t.Errorf("extracted spec wrong: %q err %v", body, err)
		}
		if st, err := os.Stat(snap.Dir); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("snapshot dir perms/err: %v %v", st, err)
		}
	})

	t.Run("host-key mismatch fails loudly", func(t *testing.T) {
		s := srv.newSource(t, svcKeyGood, svcHostsWrong)
		_, err := s.ResolveLatest(ctx)
		var ge *gitsource.Error
		if !errors.As(err, &ge) || ge.Kind != gitsource.KindHostKey {
			t.Fatalf("want typed host-key failure; got %#v", err)
		}
	})

	t.Run("unauthorized key fails loudly", func(t *testing.T) {
		s := srv.newSource(t, svcKeyBad, svcHostsPinned)
		_, err := s.ResolveLatest(ctx)
		var ge *gitsource.Error
		if !errors.As(err, &ge) || ge.Kind != gitsource.KindAuth {
			t.Fatalf("want typed auth failure; got %#v", err)
		}
	})

	t.Run("sha gone after branch rewrite", func(t *testing.T) {
		s := srv.newSource(t, svcKeyGood, svcHostsPinned)
		sha, err := s.ResolveLatest(ctx)
		if err != nil {
			t.Fatalf("ResolveLatest: %v", err)
		}
		// Simulate the M3 force-push: delete the branch remote-side
		// (container exec; the host never mutates anything real).
		if err := srv.exec(ctx,
			"git -C /srv/repo.git update-ref -d refs/heads/main"); err != nil {
			t.Fatalf("remote branch rewrite: %v", err)
		}
		// A FRESH mirror must fail to fetch the vanished sha.
		fresh := srv.newSource(t, svcKeyGood, svcHostsPinned)
		_, err = fresh.FetchRevision(ctx, sha)
		var ge *gitsource.Error
		if !errors.As(err, &ge) || ge.Kind != gitsource.KindRevisionGone {
			t.Fatalf("want typed revision-gone; got %#v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// the disposable git server
// ---------------------------------------------------------------------------

type keySlot int

const (
	svcKeyGood keySlot = iota // authorized on the container
	svcKeyBad                 // generated but never authorized
)

const (
	svcHostsPinned = iota // real, container-provided host key
	svcHostsWrong         // deliberately mismatched pin
)

type gitServer struct {
	ctx        context.Context
	dockerExec *exec.FastExec // host docker CLI for provisioning
	container  string
	port       string // mapped host port for ssh
	url        string
	keyPEM     string // private deploy key authorized on the container
	otherPEM   string // private key NOT authorized (auth-failure probe)
	realHosts  string // the container's real ssh-ed25519 host key line
}

// startGitServer provisions one disposable container serving a bare repo
// over ssh from the mapped port.
func startGitServer(t *testing.T, ctx context.Context) *gitServer {
	t.Helper()

	_, priv, authorized, err := makeKey()
	if err != nil {
		t.Fatalf("ssh-keygen deploy key: %v", err)
	}
	script := fmt.Sprintf(`set -e
apk add --no-cache git openssh-server
adduser -D -s /bin/sh git
mkdir -p /home/git/.ssh
printf '%%s\n' '%s' > /home/git/.ssh/authorized_keys
chmod 700 /home/git/.ssh
chmod 600 /home/git/.ssh/authorized_keys
chown -R git:git /home/git/.ssh
ssh-keygen -A
/usr/sbin/sshd
mkdir -p /srv
git init --bare /srv/repo.git
git init -q -b main /tmp/work
cd /tmp/work
git config user.email top@example.invalid
git config user.name "TOP e2e"
mkdir specs
printf 'namespace: core\nname: core\n' > specs/core.yaml
git add -A
git commit -qm "specs"
git push -q /srv/repo.git HEAD:refs/heads/main
`, authorized)

	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        imageData,
			Cmd:          []string{"sh", "-ec", script},
			ExposedPorts: []string{"22/tcp"},
			WaitingFor:   tcwait.ForListeningPort("22/tcp"),
		},
		Started: true,
	}
	ctr, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		t.Fatalf("start git server container: %v", err)
	}
	t.Cleanup(func() {
		_ = ctr.Terminate(context.Background())
	})
	port, err := ctr.MappedPort(ctx, "22/tcp")
	if err != nil {
		t.Fatalf("mapped ssh port: %v", err)
	}

	srv := &gitServer{
		ctx:        ctx,
		dockerExec: &exec.FastExec{},
		container:  ctr.GetContainerID(),
		port:       port.Port(),
		url:        fmt.Sprintf("ssh://git@127.0.0.1:%s/srv/repo.git", port.Port()),
		keyPEM:     priv,
	}
	if _, wrongPEM, _, err := makeKey(); err != nil {
		t.Fatalf("second (unauthorized) key: %v", err)
	} else {
		srv.otherPEM = wrongPEM
	}
	srv.realHosts = srv.fetchHostKey(t)
	waitRepoReady(t, srv)
	return srv
}

// waitRepoReady polls ls-remote through the PRODUCTION source path until
// the seeded commit is reachable over ssh (script ordering: sshd comes up
// after apk; the initial push lands last).
func waitRepoReady(t *testing.T, srv *gitServer) {
	t.Helper()
	probe := &gitsource.Source{
		URL:        srv.url,
		Branch:     "main",
		KeyPath:    "",
		KnownHosts: "",
		WorkDir:    filepath.Join(t.TempDir(), "git"),
		Runner:     gitsource.FastRunner{Base: &exec.FastExec{}},
	}
	probe.KeyPath, probe.KnownHosts = probeWriteCreds(t, srv)
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := probe.ResolveLatest(srv.ctx); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("git server never became reachable")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// probeWriteCreds writes deploy-key material for the readiness probe.
func probeWriteCreds(t *testing.T, srv *gitServer) (keyPath, knownHosts string) {
	t.Helper()
	base := t.TempDir()
	keyPath = filepath.Join(base, "id_ed25519")
	knownHosts = filepath.Join(base, "known_hosts")
	if err := os.WriteFile(keyPath, []byte(srv.keyPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(knownHosts, []byte(srv.pinLine()), 0o600); err != nil {
		t.Fatal(err)
	}
	return keyPath, knownHosts
}

func (s *gitServer) fetchHostKey(t *testing.T) string {
	t.Helper()
	out, _, err := s.dockerExec.Run(s.ctx, "docker", "exec", s.container,
		"sh", "-c", "cat /etc/ssh/ssh_host_ed25519_key.pub")
	if err != nil {
		t.Fatalf("read container host key: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// exec runs one container-side command (provisioning only: the container
// is disposable and dedicated; nothing on the host is touched).
func (s *gitServer) exec(ctx context.Context, sh string) error {
	_, stderr, err := s.dockerExec.Run(ctx, "docker", "exec", s.container, "sh", "-ec", sh)
	if err != nil {
		return fmt.Errorf("%v (exec stderr: %s)", err, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// newSource builds the production Source against the server with the
// requested key/host-pin slots, all material in a private temp dir.
func (srv *gitServer) newSource(t *testing.T, key keySlot, hosts int) *gitsource.Source {
	t.Helper()
	base := t.TempDir()
	keyPath := filepath.Join(base, "id_ed25519")
	knownHosts := filepath.Join(base, "known_hosts")

	switch key {
	case svcKeyGood:
		if err := os.WriteFile(keyPath, []byte(srv.keyPEM), 0o600); err != nil {
			t.Fatal(err)
		}
	case svcKeyBad:
		if err := os.WriteFile(keyPath, []byte(srv.otherPEM), 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown key slot %v", key)
	}

	switch hosts {
	case svcHostsPinned:
		if err := os.WriteFile(knownHosts, []byte(srv.pinLine()), 0o600); err != nil {
			t.Fatal(err)
		}
	case svcHostsWrong:
		if err := os.WriteFile(knownHosts, []byte(srv.wrongPinLine()), 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown hosts slot %d", hosts)
	}

	return &gitsource.Source{
		URL:        srv.url,
		Branch:     "main",
		KeyPath:    keyPath,
		KnownHosts: knownHosts,
		WorkDir:    filepath.Join(base, "git"),
		Runner:     gitsource.FastRunner{Base: &exec.FastExec{}},
	}
}

// pinLine renders the known_hosts entry for the mapped ssh endpoint
// (bracketed [host]:port form required for non-22 ports).
func (s *gitServer) pinLine() string {
	return fmt.Sprintf("[127.0.0.1]:%s %s\n", s.port, s.realHosts)
}

func (s *gitServer) wrongPinLine() string {
	return fmt.Sprintf("[127.0.0.1]:%s %s\n", s.port, otherHostKey())
}

// -- helpers ---------------------------------------------------------------

// makeKey generates an ed25519 keypair using ssh-keygen (the same tool a
// real operator would run) and returns (pubLine, privPEM, authorizedLine).
func makeKey() (pubLine, privPEM, authorizedLine string, err error) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		return "", "", "", fmt.Errorf("ssh-keygen required for tier-3 git e2e: %w", err)
	}
	dir, err := os.MkdirTemp("", "top-e2e-key-*")
	if err != nil {
		return "", "", "", err
	}
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-q",
		"-C", "top-e2e-deploy-key", "-f", filepath.Join(dir, "id_ed25519"))
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", "", "", fmt.Errorf("ssh-keygen: %v: %s", err, out)
	}
	priv, err := os.ReadFile(filepath.Join(dir, "id_ed25519"))
	if err != nil {
		return "", "", "", err
	}
	pubB, err := os.ReadFile(filepath.Join(dir, "id_ed25519.pub"))
	if err != nil {
		return "", "", "", err
	}
	return strings.TrimSpace(string(pubB)), string(priv), strings.TrimSpace(string(pubB)), nil
}

// otherKeyLines renders syntactically valid but DIFFERENT ed25519 public
// key lines (random bodies nobody holds) for the mismatch scenarios.
func otherKeyLines(comment string) string {
	_, pub, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "ssh-ed25519 AAAA" + comment // never reached in practice
	}
	// ssh ed25519 wire format: 4-byte big-endian key length (32),
	// the literal "ssh-ed25519", then 32 bytes of raw key.
	body := make([]byte, 0, 51)
	body = append(body, 0, 0, 0, 11)
	body = append(body, "ssh-ed25519"...)
	body = append(body, 0, 0, 0, 32)
	body = append(body, pub...)
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(body) + " " + comment
}

// otherHostKey builds a wrong-but-plausible host-key pin line.
func otherHostKey() string { return otherKeyLines("top-e2e-wrong-host-key") }
