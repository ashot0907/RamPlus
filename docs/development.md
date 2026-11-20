# Development

## Requirements

- Linux only.
- Go — the version in `go.mod` (current stable).
- Node.js with npm — only to build and test the web UI (`web/`).
- `shellcheck` for `make lint` (skipped with a note if not installed; CI runs it).
- For the integration test: a disposable VM with systemd, `curl`, root.

Dependencies are kept minimal: Go standard library, `golang.org/x/sys`, `golang.org/x/crypto`
(argon2id only), `cobra` for the CLI; in the frontend `react` and `react-dom`, no UI or CSS frameworks.

## Build

```sh
make build        # web UI (npm ci && npm run build) + ./ramplus, ./ramplusd, ./ramplus-web
make web          # web UI only: web/ → internal/webui/dist
make build-probe  # ./ramplus-probe for scripts/bench.sh
make clean
```

The built frontend is embedded into `ramplus-web` with `go:embed` from `internal/webui/dist`. That
directory is git-ignored except `.keep`, so the Go code builds and tests without Node.js; such a
`ramplus-web` serves a page saying "frontend is not built".

Frontend development with hot reload, against a running `ramplus-web` on `127.0.0.1:7781`:

```sh
cd web && npm run dev
```

## Tests

```sh
make test       # go test ./...
make web-test   # vitest: ru/en dictionaries complete, UI logic, plan dialog rendering
make lint       # gofmt, go vet, shellcheck (scripts/*.sh, packaging/deb/*)
make all        # lint + test + web-test + build + build-probe
```

Before a pull request run `make test web-test lint`. The race detector is worth running on the
packages with goroutines: `go test -race ./internal/api ./internal/core ./internal/web`.

- **Unit tests never change the system.** Every system action goes through the interfaces `FS`,
  `Executor` and `Kernel` (`internal/sys`); tests use the fake system in `internal/fakesys`, which
  also injects failures at any step (`FailAt`) to prove automatic rollback.
- **Parsers** of `/proc` and `/sys` are pure functions (`io.Reader`/`[]byte` → struct) tested on
  fixture files in `testdata/`.
- **Golden tests** (`cmd/ramplus/testdata/golden`) pin the CLI's text output in Russian
  (`--lang ru`), and `testdata/golden/en/` the same commands in English; both are checked. Update
  them only when the output change is intended: `go test ./cmd/ramplus -run Golden -update`, then
  review the diff.
- **Messages** live in one catalog (`internal/i18n/catalog_*.go`, key → English and Russian).
  `TestCatalogComplete` requires both languages and matching `%` verbs; `TestKeysUsedInCode` fails
  on a key used in code but missing from the catalog.
- **API and web tests** run the real HTTP servers on temporary Unix sockets over the fake system.

### Integration test (VM only)

`scripts/vm-test.sh` creates and removes swapfiles, edits `/etc/fstab`, installs and removes the
services. **Run it only in a disposable VM**:

```sh
multipass launch 24.04 --disk 30G --memory 4G   # for example
make build
sudo RAMPLUS_VM_TEST=1 scripts/vm-test.sh ./ramplus

# with scenario G (the .deb package):
make deb
sudo RAMPLUS_VM_TEST=1 scripts/vm-test.sh ./ramplus dist/ramplus_*_amd64.deb
```

Scenarios:

- **A.** apply 4G → status → the unit brings swap back → resize 6G → remove → three rollbacks → the
  original state.
- **B.** replace existing swap → rollback: old swap active again, fstab restored byte for byte.
- **C.** replace with `--delete-old` → rollback recreates the old swapfile.
- **D.** profile `zswap-lz4`: apply, simulated reboot, drift, rollback, reset — values as at the start.
- **E.** daemon: install, API status/plan/apply/rollback, permissions (no group — cannot reach the
  socket; `ramplus` member — can plan but apply is `403`), uninstall.
- **F.** web: install, `ramplus init`, login, wrong password, rate limit, `Host` check, recommendation
  → `setup` → apply with CSRF → one journal operation with `via: web` → rollback → clean, logout,
  `uninstall PURGE=1` leaves nothing.
- **G.** (only with a `.deb` argument) `apt install ./ramplus_*.deb` → services neither enabled nor
  running → `init --password-stdin`, `enable --now`, `daemon status` → `setup` from the
  recommendation → `apt remove` is refused while RamPlus manages swap → `rollback` → `apt remove`
  passes, services stopped and disabled → `apt purge` removes `/etc/ramplus` and the clean journal.

On a failure the script saves the test journal (`/var/lib/ramplus/state.json.vmtest-failed-<time>`)
and, in scenarios E and F, the last 50 lines of the service logs
(`/var/lib/ramplus/vmtest-failed-<time>.journal.txt`), then rolls the VM back to its initial state.

### Packages and releases

```sh
make deb            # GoReleaser snapshot: dist/*.tar.gz, dist/*.deb (amd64, arm64), SHA256SUMS
make lintian        # lintian on dist/*.deb in an ubuntu:24.04 container (docker)
make release-check  # goreleaser check
```

`make deb` needs [GoReleaser](https://goreleaser.com) v2 (`GORELEASER=/path/to/goreleaser` to use a
binary outside `PATH`). The config is `.goreleaser.yaml`: the web UI is built first, then static
binaries (`CGO_ENABLED=0`, `-trimpath`, version via `-X main.version`), archives with `LICENSE`,
`README.md` and `packaging/`, and the `.deb` through nfpm. Maintainer scripts and the lintian
overrides are in `packaging/deb/`; `packaging/deb/prepare.sh` generates the units with
`/usr/bin` and the Debian changelog into `build/deb/`. AUR PKGBUILDs and how to publish them:
[packaging/aur/README.md](../packaging/aur/README.md).

A release is a tag: `git tag v0.3.0 && git push origin v0.3.0` runs `.github/workflows/release.yml`
(tests, GoReleaser, GitHub Release with the changelog from commits). Tags with `-alpha`, `-beta` or
`-rc` are published as prereleases.

### Benchmark

`scripts/bench.sh` compares no-swap, swap and zswap configurations under several workloads. See
[scripts/BENCH.md](../scripts/BENCH.md). Numbers from a VM are never used to choose defaults.

## Package layout

```
cmd/ramplus        CLI — thin shell over internal/core
cmd/ramplusd       daemon — thin shell over internal/core (via internal/api)
cmd/ramplus-web    web server (never root) — thin shell over internal/web
cmd/ramplus-probe  load generator, latency probe, sampler and report for bench.sh
internal/core      service layer: status, plans (swap, profile, setup, rollback), recommendation,
                   execution, history, daemon operations, web password
internal/plan      plan model — data (JSON): checks, choices, steps, summary, fingerprint
internal/journal   change engine: state.json, steps and their undo, write-ahead, rollback stack,
                   state verification, migrations, lock; knows nothing about swap or profiles
internal/swap      swap: facts, planner, steps, orphans
internal/profile   profiles: catalogue, parameters, sysctl.d, planner, steps
internal/render    text output for the CLI
internal/api       daemon API: Unix socket, SO_PEERCRED, handlers, SSE, client
internal/web       ramplus-web: login, sessions, CSRF, rate limit, Host check, security headers,
                   proxy /api → daemon, TLS
internal/webauth   argon2id, password file format (pure functions)
internal/webui     embedded frontend (go:embed)
internal/sys       FS/Executor/Kernel interfaces and real implementations
internal/fakesys   fake system for tests
internal/detect    system, disks, filesystems
internal/monitor   meminfo, swaps, zswap, zram, PSI, vm sysctl
web/               frontend: React + TypeScript (Vite), ru/en dictionaries, vitest
packaging/         systemd units, sysusers.d, tmpfiles.d; deb/ (maintainer scripts), aur/ (PKGBUILDs)
scripts/           VM integration test, benchmark
docs/              user and developer documentation
```

Domains register their step kinds and actions with `journal.Register` and `plan.RegisterAction`, so
the journal stays generic.

## Rules for contributors

These come from the project's `CLAUDE.md` and apply to every change:

1. **Any code that changes the system must have a dry-run and a rollback.** Changes go through a plan
   and the journal; no direct writes from command handlers.
2. **Logic lives only in `internal/core` and below.** The CLI, the daemon and the web server are thin
   shells over `internal/core`; they parse input and render output.
3. **Parsers of `/proc` and `/sys` are pure functions**, covered by tests on fixture files in
   `testdata/`.
4. **Never crash when a file is unavailable** (no permission, old kernel): report `n/a` with the
   reason.
5. **No system changes unless the task explicitly asks for them** — including in tests and local
   experiments. Use the fake system; real changes only in the VM test.
6. **Every default value in a profile** (zswap parameters, swappiness, …) must be backed by
   `scripts/bench.sh` results on a real SSD, linked from the profile (`evidence`). Until then the
   profile stays `experimental`.
7. **Performance numbers from virtual machines are not used to choose defaults.**

Also:

- The HTTP server never runs as root; the web UI listens on `127.0.0.1` by default; remote access is
  through an SSH tunnel or an explicit `--listen` with mandatory TLS.
- Check codes (`disk.usb`, `rollback.old`, …) are part of the API: keep them stable, and add a
  translation for new codes to `web/src/i18n/ru.ts` and `en.ts` (the dictionaries must have the same
  keys — a test checks it).
- Log records of the daemon and web server carry a stable `event=` field; tests and the VM test match
  on it, not on message text.
- In shell scripts under `set -o pipefail`, do not write `cmd | grep -q`: `grep` exits on the first
  match, `cmd` gets `SIGPIPE` and the check fails spuriously. Capture the output first.
