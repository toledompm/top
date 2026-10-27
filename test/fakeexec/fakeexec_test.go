package fakeexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignatureDeterministicAndDistinct(t *testing.T) {
	a := Signature("dokku", "apps:list", "--format", "json")
	b := Signature("dokku", "apps:list", "--format", "json")
	if a != b {
		t.Fatalf("signature not deterministic: %q != %q", a, b)
	}
	if len(a) != 40 {
		t.Fatalf("signature %q not 40-hex", a)
	}
	for _, r := range a {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("signature %q not hex", a)
		}
	}
	if Signature("dokku", "apps:list") == Signature("dokku", "apps:inspect") {
		t.Fatal("distinct argv must not collide on one signature")
	}
}

func TestEqualArgv(t *testing.T) {
	got := Command{Name: "dokku", Args: []string{"apps:list", "--format", "json"}}
	if got.String() != "dokku apps:list --format json" {
		t.Errorf("String() = %q", got.String())
	}
	if !EqualArgv("dokku", []string{"apps:list", "--format", "json"}, got) {
		t.Error("equal argv rejected")
	}
	for name, c := range map[string]Command{
		"different name":      {Name: "git", Args: []string{"apps:list"}},
		"missing arg":         {Name: "dokku", Args: []string{"apps:list"}},
		"extra arg":           {Name: "dokku", Args: []string{"apps:list", "--format", "json", "x"}},
		"different arg value": {Name: "dokku", Args: []string{"apps:list", "--format", "table"}},
	} {
		if EqualArgv("dokku", []string{"apps:list", "--format", "json"}, c) {
			t.Errorf("%s: EqualArgv accepted a mismatch", name)
		}
	}
}

func TestParseLogRoundTrip(t *testing.T) {
	in := strings.Join([]string{
		`{"name":"dokku","args":["apps:list","--format","json"]}`,
		``,
		`{"name":"dokku","args":["version"]}`,
	}, "\n") + "\n"
	got, err := ParseLog(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d commands, want 2", len(got))
	}
	if !got[0].Equal(Command{Name: "dokku", Args: []string{"apps:list", "--format", "json"}}) {
		t.Errorf("first record = %+v", got[0])
	}
}

func TestLoadFixturesFromFilesystem(t *testing.T) {
	// cwd of this test is test/fakeexec; fixtures live over at test/fixtures.
	fxs, err := LoadFixtures("../fixtures/dokku")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		sig  string
		exit int
	}{
		{Signature("dokku", "apps:list", "--format", "json"), 0},
		{Signature("dokku", "ps:report", "top-test-app", "--format", "json"), 0},
		{Signature("dokku", "config:set", "--no-restart", "top-test-app", "EXAMPLE_TOKEN=example-token-value"), 0},
		{Signature("dokku", "version"), 0},
	}
	if len(fxs) != len(want) {
		t.Fatalf("loaded %d fixtures, want %d: %+v", len(fxs), len(want), fxs)
	}
	for i, w := range want {
		if fxs[i].Signature() != w.sig {
			t.Errorf("fixture %d signature = %q, want %q", i, fxs[i].Signature(), w.sig)
		}
		if fxs[i].Exit != w.exit {
			t.Errorf("fixture %d exit = %d, want %d", i, fxs[i].Exit, w.exit)
		}
	}
}

func TestWriteShimRegistry(t *testing.T) {
	dir := t.TempDir()
	err := WriteShimFixtures(dir, []Fixture{{
		Name: "dokku", Args: []string{"apps:exists", "top-test-app"},
		Stderr: " !     App not found\n", Exit: 10,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteShimSubcommand(dir, "config:set", Fixture{
		Name: "dokku", Stdout: "set\n",
	}); err != nil {
		t.Fatal(err)
	}
	if err := WriteShimFamily(dir, "apps", Fixture{
		Name: "dokku", Stderr: "family fallback\n", Exit: 3,
	}); err != nil {
		t.Fatal(err)
	}

	for _, f := range []string{
		Signature("dokku", "apps:exists", "top-test-app") + ".exit",
		"subcmd.config:set.exit",
		"prefix.apps.exit",
	} {
		if _, ok := fileContent(dir, f); !ok {
			t.Errorf("registry file %s missing", f)
		}
	}
	// The exact-match .exit exists but .stderr must contain the not-found line.
	if s, _ := fileContent(dir, Signature("dokku", "apps:exists", "top-test-app")+".stderr"); s != " !     App not found\n" {
		t.Errorf("exact stderr = %q", s)
	}
}

func fileContent(dir, name string) (string, bool) {
	if bs, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
		return string(bs), true
	}
	return "", false
}
