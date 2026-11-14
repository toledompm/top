# Homelab live e2e playbook — the full operator acceptance run

A structured live checklist for a real-host TOP install. Every step names
the command, the evidence to assert, and the failure class it catches
(the classes below were all found by live runs; the e2e exists because
hermetic tests cannot see sudo/root identities, GitHub propagation
delays, non-TTY config readability, or daemon-boot ordering).

Run it as the operator (not as an agent) — this playbook IS the
ease-of-use assessment. Note every stumble; each one is either a bug or
a UX line for the backlog.

## 0. Preconditions

- Dokku ≥ 0.38 installed and healthy (`dokku version`); ports 80/443 free
  or owned by Dokku's nginx.
- GitHub account; `gh auth status` works on any machine (optional but
  recommended on the HOST — the bootstrap deep-links are always printed).
- No leftover TOP state: `sudo ls /var/lib/top` fails, `dokku apps:list`
  empty.
- `topctl` current: `curl -LO
  https://github.com/toledompm/top/releases/latest/download/topctl-linux-$(dpkg --print-architecture)`
  → checksum-verify → install to /usr/local/bin; `topctl version -o json`
  shows the release tag.

## 1. Install (one command)

```sh
sudo topctl install git@github.com:<owner>/top-homelab.git --init
```

Interactive on a TTY; it fills its own preconditions:
- deploy key generated at `~/.ssh/top_deploy_key` (+ pinned known_hosts),
- repo + deploy key registered (gh when available, else two deep-links —
  `https://github.com/new?name=<repo>&visibility=…` is the correct
  create-form; the owner-scoped `/settings/keys/new` is the key page),
- a single ENTER checkpoint for the browser steps,
- **bounded retry (~15s)** after the checkpoint — a freshly-registered
  key does not authenticate instantly (GitHub propagation; live finding:
  the first run "failed" on pure delay).

**Assert after:**
- [ ] envelope green; `created:` lists the runtime config, the deploy key
      as `id_ed25519`, the socket mount, the bootstrap deploy at the
      CURRENT release tag (not a stale pinned default).
- [ ] `adopted:` records `bootstrap-owned → managed` (op id + revision).
- [ ] the **generated secret note** exists (grafana admin password) and
      names only a file path — never a value.
- [ ] no manual steps were needed beyond the two browser actions.

**Catches:** stale pinned image refs, config/secret manual steps,
missing key-name alignment, gh/gh-less paths, propagation delays.

## 2. Post-install state (the part that failed live)

- [ ] `sudo cat /var/lib/top/top-config.yaml` — git.url set, NOT empty.
- [ ] **`topctl diff` WITHOUT sudo works** (config is 0644 + datadir 0711
      traverse-only; the live failure was a 0600-root config making every
      non-sudo verb resolve an empty git url). Expected: drift view shows
      the controller's own field set (`drifted(self)`, report-only) and
      observability `update` rows until the first daemon sweep.
- [ ] `topctl doctor` WITHOUT sudo: **no `names/top-controller` FAIL** —
      the names check consults the ledger when the store is readable and
      knows TOP's own app (live failure: doctor-after-install failed on
      TOP's own controller; as non-sudo with an unreadable store it is
      allowed to fail — run `sudo topctl doctor` for the ledger-aware
      view, which must be finding-free except resource warnings).
- [ ] controller polls clean: `sudo dokku logs core-top-controller
      --num 5` shows NO "poll failed" warnings within one poll interval
      (60s). Catches: config written after daemon boot (ordering), empty
      git url, identity-file-not-accessible.
- [ ] datadir posture: `/var/lib/top` 0711, `secrets/` + `git/` 0700,
      files 0600; runtime config 0644.

## 3. Convergence + GitOps loop

- [ ] within `reconcileIntervalSeconds` (default 300s) the drift view
      clears EXCEPT the controller's `drifted(self)` rows (human-gated,
      ADR-0005) — or run `sudo topctl reconcile --now`.
- [ ] deploy a first app: add `services/default/httpbin.yaml` (image
      `docker.io/kennethreitz/httpbin`, tag `latest`, port 80, proxy
      routing with a domain) → push → converge →
      `curl http://<domain>/get` returns JSON.
- [ ] UPDATE the app (env change) → push → converge → the env shows in
      the response; `topctl history default/httpbin` shows two ops.

## 4. Self-update (human-gated, ADR-0005)

- [ ] bump the controller spec's image tag to the next release tag,
      push.
- [ ] `topctl deploy core/top-controller` WITHOUT `--confirm` exits 2
      non-interactively.
- [ ] with `--confirm`: applied; on any failure the OLD release keeps
      serving (the envelope's echo says so) — assert the controller is
      still healthy either way.

## 5. Survival (ADR-0010 independence)

- [ ] `sudo dokku ps:stop core-top-controller` → workloads + monitoring
      keep serving.
- [ ] restart it → poll resumes, no redeploys in `topctl history`.

## 6. Teardown (repeatable lifecycle)

- [ ] `sudo topctl uninstall --purge-data --purge-secrets --confirm` —
      all apps removed with provenance; Dokku untouched; state gone.
- [ ] delete the GitHub repo; remove the deploy key.
- [ ] the host is back to step 0's state — the playbook loops.

## Failure triage quick-reference (from live runs)

| Symptom | Root cause class | Where it was fixed |
| --- | --- | --- |
| `git ls-remote ""` (no path) | config not read (perms/order) | runtime config 0644 + written before daemon boot |
| `Identity file ... not accessible` | key provisioning name mismatch | installer → gitsource's `id_ed25519` layout |
| `manifest unknown` on the bootstrap image | stale/un-prefixed image tag | version-derived tag + `BootstrapTagFor` |
| first probe after ENTER fails | GitHub propagation delay | bounded retry in the interactive bootstrap |
| doctor fails on TOP's own app | standalone doctor had no ledger view | `LedgeredApps` wiring (sudo for the ledger view) |
| non-sudo verbs see an empty git url | 0600-root config + 0700 datadir | 0644 config + 0711 datadir |
