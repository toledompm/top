# test/fakeexec — tier-2 helpers (ADR-0012)

Support package for tests that exercise `exec.Runner` (ADR-0006) through real
child processes — primarily the fake `dokku` on PATH (`test/shim/bin/dokku`).
Not imported by production code.

| Tier | Need | Use |
| --- | --- | --- |
| 1 | fake Runner, no processes: fast unit tests of argv comparison | `internal/exec/exectest.FakeRunner` |
| 2 | real child via PATH: argv logs, run fixtures, shim registry | this package |
| 3–4 | real Dokku | `TOP_E2E` suites (Milestone 3+) |

## API

```go
// argv records
fakeexec.Command{Name: "dokku", Args: []string{"apps:list"}} // String() → "dokku apps:list"
fakeexec.EqualArgv("dokku", []string{"apps:list", "--format", "json"}, got)
fakeexec.ParseLog(r)             // newline-JSON {"name","args"} lines → []Command
fakeexec.ParseLogFile(path)

// fixtures
fxs, err := fakeexec.LoadFixtures("../fixtures/dokku") // YAML, format below
fakeexec.Signature("dokku", "apps:list")               // hex sha1 of "dokku apps:list"
fakeexec.WriteShimFixtures(dir, fxs)                   // exact-argv responses
fakeexec.WriteShimSubcommand(dir, "config:set", resp)  // any args, one subcommand
fakeexec.WriteShimFamily(dir, "apps", resp)            // per-family fallback
```

## Run-fixture format (files under `test/fixtures/dokku/`)

Each YAML file holds one fixture mapping or a sequence of them:

```yaml
- name: dokku
  args: [apps:list, --format, json]
  stdout: |
    ["top-test-app"]
  stderr: ""
  exit: 0
```

`exit` defaults to 0. Only fictional values belong in fixtures
(`top-test-app`, `ghcr.io/example/...`) — ADR-0014 hygiene applies.

## Shim registry mapping

`WriteShim*` materialize `<sha1(name+" "+args joined with spaces)>.stdout`,
`.stderr`, `.exit` files (plus `subcmd.<sub>.*` / `prefix.<family>.*`
fallbacks) — the exact layout `test/shim/bin/dokku` reads; precedence is
documented in `../shim/README.md`.

One deliberate imprecision: the key is a *space-joined string*, so fixture
args containing spaces would be ambiguous. Dokku arguments in this repo never
contain spaces; a fixture arg type that did would need a separator change.
