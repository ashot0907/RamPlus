# CLI reference

`ramplus` is the command-line front end. It talks to the system directly; it does not need the
daemon. The CLI and the daemon can run at the same time: the journal and the lock
`/run/lock/ramplus.lock` keep their operations from overlapping (the second one is refused, the
journal stays consistent).

- Read-only commands (`status`, `recommend`, `history`, every `plan` / `--dry-run`) do not need root.
- Commands that change the system need root (`sudo`). They always print the plan first and ask for
  confirmation.
- Messages are in English. Russian: `--lang ru`, or a locale whose `LC_ALL` / `LC_MESSAGES` /
  `LANG` starts with `ru` (e.g. `LANG=ru_RU.UTF-8`); `--lang en|ru` wins over the environment.
  JSON output (`--json`) keeps the same keys and codes in both languages; only human-readable
  messages are translated. The history stores codes, so old entries are shown in the current
  language.

Contents: [status](#status) · [recommend](#recommend) · [setup](#setup) · [swap](#swap) ·
[profile](#profile) · [rollback](#rollback) · [history](#history) · [init, passwd](#init-and-passwd) ·
[daemon status](#daemon-status) · [JSON output](#json-output) · [exit codes](#exit-codes)

## Common flags

Every command that changes swap or memory settings (`setup`, `swap apply|resize|remove`,
`profile apply|reset`, `rollback`) has:

| flag | meaning |
|---|---|
| `--dry-run` | print the plan, change nothing |
| `-y`, `--yes` | do not ask for confirmation (scripts) |
| `--json` | JSON output (same schema as the daemon API); when executing, requires `--yes` |
| `-v`, `--verbose` | show the full content of files that will be written (systemd units, sysctl file) |

Sizes use binary units: `512M`, `4G`, `1.5G` (1G = 1 GiB).

## status

```sh
ramplus status                          # table
ramplus status --json                   # JSON (same schema as GET /v1/status)
ramplus status --watch [--interval 1s]  # live view with peaks; Ctrl+C prints a summary
```

| flag | default | |
|---|---|---|
| `--json` | | JSON output |
| `--watch` | | refresh in place: RAM, swap, zswap, PSI and peaks |
| `--interval` | `1s` | refresh period for `--watch` |

No root needed. A source that cannot be read is shown as `n/a (reason)` and the command still
succeeds (for example, zswap statistics from debugfs need root).

| section | source |
|---|---|
| System | `uname`, `/etc/os-release` |
| Memory | `/proc/meminfo` |
| Swap devices | `/proc/swaps` |
| zswap | `/sys/module/zswap/parameters/`, `/proc/meminfo`, debugfs (root) |
| VM | `/proc/sys/vm/{swappiness,vfs_cache_pressure,watermark_scale_factor}` |
| PSI | `/proc/pressure/memory` |
| Root disk | `/proc/self/mountinfo`, `/sys/block/*/queue/rotational`, `statfs` |
| zram | `/sys/block/zram*` |

Memory pressure is interpreted from PSI `some avg10`: below 5 — `normal`, 5–20 — `elevated`,
20–50 — `high`, above 50 — `severe` (`psi.pressure_level` in JSON). The thresholds are constants in
`internal/monitor/psi.go` and are starting values.

Virtual machine disks (virtio, xen, Hyper-V, VMware, `vd*`/`xvd*`, QEMU) are reported as `virtual`:
their `rotational` flag is unreliable.

`status` also shows the active RamPlus profile and **drift** — kernel values that were changed by
hand after the profile was applied (`profile` and `drift` in JSON).

## recommend

```sh
ramplus recommend [--json] [--path /mnt/data/ramplus.swapfile]
```

What RamPlus suggests for this machine. Read-only.

```text
$ ramplus recommend
RamPlus recommendation

  RAM: 16.0 GiB (MemTotal 16.0 GiB)
  location: /ramplus.swapfile (ext4 on /, nvme), 300.0 GiB free, up to 280.0 GiB
  RamPlus swap: 8.0 GiB — RAM 16 GB: SSD swap of half the RAM
  existing swap: --existing=replace — the existing swap is a file: replace it with ours, the old file is kept for undo
  profile: zswap-lz4 (experimental) — won our tests: fewer freezes when memory runs short
  [profile.preliminary] preliminary: profile zswap-lz4 is still being confirmed on a real SSD (scripts/bench.sh)

Plan: ramplus setup --size 8G --existing=replace --profile zswap-lz4 --experimental --dry-run
```

- **Swap size:** RAM ≤ 8 GB → same as RAM; 8–32 GB → half of RAM; more → 16 GB. RAM is taken from
  `MemTotal` rounded **up** to a whole GB (the kernel reserves part of the installed memory, so a
  32 GB machine reports ~31.1). The size is then limited by free space on the target filesystem
  (keeping the same free-space reserve as the plan, see [safety](safety.md#checks)) and rounded down.
  Below 1 GB no swap is recommended, with a reason.
- **Existing swap:** `replace` if it is a file and its data fits into RAM plus the new swap; `keep`
  for partitions, zram, or when turning it off would not be safe; `null` if there is no other swap or
  RamPlus swap already exists.
- **Profile:** `zswap-lz4` with its real status (`experimental` — shown as "preliminary"). Without
  zswap in the kernel, or with zram active, `default`.
- The recommendation is checked by the same planner as `setup`. If that plan would be blocked (system
  on a USB disk, not enough space, interrupted operation), `swap_size_bytes` is `null` and
  `swap_reason` / `swap_blocked` explain why.

| flag | |
|---|---|
| `--path` | swapfile location to evaluate (default: as for `swap apply`) |
| `--json` | JSON (same as `GET /v1/recommendation`) |

## setup

```sh
ramplus setup --size 16G --existing=replace --profile zswap-lz4 --dry-run
sudo ramplus setup --size 16G --existing=replace --profile zswap-lz4 --experimental
sudo ramplus setup --profile default             # profile only
sudo ramplus setup --size 8G                     # swap only
```

Swap and profile **in one operation**: checks of both parts, steps in a row (swap first, then the
profile), one plan fingerprint, one journal entry, one `rollback`. Either part may be omitted. A
failed check in either part blocks the whole plan.

Swap is a *desired state*: if RamPlus swap does not exist, it is created; if it has a different size,
it is resized; if the size is the same, that part does nothing. Moving existing RamPlus swap to
another disk is refused (`setup.swap_path_change`): remove it first.

| flag | |
|---|---|
| `--size` | desired size of the RamPlus swapfile |
| `--path` | swapfile path (default `/ramplus.swapfile`, on btrfs `/ramplus-swap/ramplus.swapfile`) |
| `--existing` | existing (non-RamPlus) swap: `keep` (default) or `replace` |
| `--delete-old` | with `replace`, delete old swap files |
| `--profile` | `default`, `zswap-lz4` or `zswap-zstd` |
| `--set key=value` | override a profile parameter (repeatable) |
| `--experimental` | confirm an experimental profile (scripts) |
| `--force` | swap on a USB disk; zswap while zram is installed |

Plus the [common flags](#common-flags).

## swap

```sh
ramplus swap plan --size 12G [--verbose]           # plan only, no root needed
ramplus swap plan --size 16G --path /mnt/nvme/ramplus.swapfile
sudo ramplus swap apply --size 12G                 # prints the same plan and asks
sudo ramplus swap apply --size 12G --existing=replace [--delete-old]
sudo ramplus swap resize --size 16G
sudo ramplus swap remove
ramplus swap check-clean                           # any RamPlus traces? exit code 1 if yes
sudo ramplus swap remove --orphan                  # remove RamPlus traces that have no journal entry
```

| command | flags |
|---|---|
| `swap plan` | `--size`, `--path`, `--existing`, `--delete-old`, `--force`, `--json`, `-v` |
| `swap apply` | same as `plan` + `--dry-run`, `-y` |
| `swap resize` | `--size`, `--force`, `--dry-run`, `-y`, `--json`, `-v` |
| `swap remove` | `--orphan`, `--force`, `--dry-run`, `-y`, `--json`, `-v` |
| `swap check-clean` | — |

The plan always shows choices explicitly:

- size and the maximum allowed on the target filesystem;
- location (`--path`): every mounted filesystem is listed as an alternative, marked USB, network,
  virtual disk, "not suitable", "not enough space";
- `keep` or `replace` for existing swap, with the resulting total and which files are created,
  disabled or deleted. If `--existing` is not given and there is other swap, the safe `keep` is used
  and the plan says so;
- `--delete-old` (old swap files are kept by default so the change can be undone).

**What `swap apply` creates:** `/ramplus.swapfile` (mode 600) and the systemd unit
`ramplus.swapfile.swap` (`--verbose` prints it). RamPlus does not add its own swap to `/etc/fstab`.
With `replace`, the old swap line in fstab is commented out with `# disabled by ramplus:` and a backup
of fstab is kept. See [safety](safety.md) for all checks.

**Orphans.** If the journal was deleted or lost but a swapfile or unit is left, `check-clean` reports
"RamPlus traces without a journal entry". `swap remove --orphan` removes them with a plan and
confirmation, only after checking markers (see [safety](safety.md#orphans)).

## profile

```sh
ramplus profile list                               # profiles and their status
ramplus profile show zswap-lz4                     # parameters and evidence
ramplus profile plan zswap-lz4 [--set vm.swappiness=80] [--verbose]   # no root needed
sudo ramplus profile apply zswap-lz4 --experimental
ramplus profile current                            # what is applied, differences from original values
sudo ramplus profile reset                         # restore values from before the first apply
```

| command | flags |
|---|---|
| `profile list`, `profile show <name>`, `profile current` | — |
| `profile plan <name>` | `--set`, `--force`, `--json`, `-v` |
| `profile apply <name>` | `--set`, `--force`, `--experimental`, `--dry-run`, `-y`, `--json`, `-v` |
| `profile reset` | `--dry-run`, `-y`, `--json`, `-v` |

A profile sets zswap parameters (`enabled`, `compressor`, `zpool`, `max_pool_percent`,
`shrinker_enabled`) and sysctl values (`vm.swappiness`, `vm.page-cluster`,
`vm.watermark_scale_factor`, `vm.vfs_cache_pressure`). A parameter not in the profile is not
touched.

| profile | UI name | status | values |
|---|---|---|---|
| `default` | Like stock Ubuntu | standard | zswap off, swappiness 60, page-cluster 3, watermark_scale_factor 10, vfs_cache_pressure 100 |
| `zswap-lz4` | Fast compression | experimental | zswap on, lz4, zsmalloc, pool 20 %, the rest as in `default` |
| `zswap-zstd` | Dense compression | experimental | the same with zstd |

- **Status.** An experimental profile is applied only with explicit confirmation: a question in the
  terminal, `--experimental` in scripts, a checkbox in the web UI. A profile becomes **verified**
  only with *evidence* — results of `scripts/bench.sh` on a real SSD; this is enforced when the
  catalogue is loaded. `--set` marks the profile as modified by the user.
- **How it is applied.** Values are written right away to `/sys/module/zswap/parameters` and
  `/proc/sys/vm`. After a reboot they are restored by `/etc/sysctl.d/99-ramplus.conf` and the oneshot
  unit `ramplus-zswap.service` (values are embedded in the unit). The kernel command line and GRUB are
  never changed.
- **Original values** are captured once, on the first apply, and restored by `profile reset`.

## rollback

```sh
sudo ramplus rollback [--dry-run] [--yes] [--json] [--force]
```

Undoes the last operation (swap, profile or setup share one stack). Calling it again goes further
back. Before undoing, the current state is compared with what the journal recorded after that
operation; on any difference the plan is blocked (`rollback.state_mismatch`) and nothing changes.

The plan says exactly what is undone. If the operation is more than a day old — `[warn] rollback.old`
with the date and age; if it is of another kind than the user's last operation (for example, swap was
already undone and now a profile would be) — `[warn] rollback.other_domain`. In JSON the plan has a
`rollback` object with `operation_id`, `action`, `domain`, `started_at` and `age_seconds`.

`--force`: continue when there is not enough free space to recreate deleted swap files.

## history

```sh
ramplus history [--json]
```

Operations from the journal: ID, time, status, whether `rollback` will undo it, a human-readable
title ("16 GB swap from SSD + mode «Fast compression»"), where it came from, and the command.
No root needed.

The **source** column comes from the journal fields `source` and `requested_by`:

| shown as | journal |
|---|---|
| "from the terminal (alice)" | `source: "cli"`, `requested_by` = the user who ran `sudo` (`SUDO_UID`/`SUDO_USER`) |
| "via the web interface" | `source: "daemon"`, `requested_by.via: "web"` (request from the `ramplus-web` user) |
| "via the daemon API (bob)" | `source: "daemon"`, `requested_by` = uid from `SO_PEERCRED` |
| "unknown" | journals written before these fields existed |

## init and passwd

```sh
sudo ramplus init                           # set the web UI password (asked twice, no echo)
printf '%s\n' "$PW" | sudo ramplus init --password-stdin
sudo ramplus passwd                         # change it; open web sessions end
sudo ramplus passwd --disable               # remove it (undo of init): the web UI shows "run sudo ramplus init"
```

| flag | |
|---|---|
| `--password-stdin` | read the password from the first line of stdin |
| `--dry-run` | show what would be written |
| `--disable` | (`passwd` only) delete the password file |

The password (at least 8 characters) is stored as an argon2id hash in `/etc/ramplus/web-auth`,
owned `root:ramplus-web`, mode 0640. It is never written to the journal (the journal is world
readable), so there is no `rollback` for it; `passwd --disable` is the way back. `init` requires the
`ramplus-web` group, created by `make install`. See [web.md](web.md).

## daemon status

```sh
ramplus daemon status [--json] [--socket /run/ramplus/ramplusd.sock]
```

Is the daemon reachable, which version is it, does it match the CLI.

## JSON output

`--json` exists on `status`, `recommend`, `history`, `daemon status`, `swap plan|apply|resize|remove`,
`setup`, `profile plan|apply|reset` and `rollback`. The schemas are the same as in the
[daemon API](api.md).

General rules:

- `schema_version` grows only on incompatible changes; adding fields is compatible.
- All sizes are in bytes (`*_bytes` fields).
- `null` means "not available". The reason is in the neighbouring `error` / `*_error` field:
  `{"reason": "permission_denied" | "not_present" | "unsupported" | "parse_error" | "unavailable", "detail": "..."}`.
- Lists (`swaps.devices`, `zram.devices`, `root_disk.disks`, `checks`, `steps`, …) are always arrays,
  never `null`.

**Plan** (`plan`, `--dry-run --json`):

| field | |
|---|---|
| `schema_version`, `id`, `fingerprint` | `id` is set when the plan is stored by the daemon |
| `action`, `command` | `apply`, `resize`, `remove`, `setup`, `profile-apply`, `profile-reset`, `rollback`; the equivalent command line |
| `blocked` | a check failed — the plan cannot be applied |
| `choices` | explicit choices and their alternatives |
| `checks` | `level` (`ok`/`info`/`warn`/`fail`), stable machine `code` (`fs.unsupported`, `disk.usb`, `rollback.nothing`, …), human `title`/`message` |
| `steps` | `kind` and step parameters, plus a human `description` |
| `summary` | `swap_before_bytes`, `swap_after_bytes`, `created`, `disabled`, `deleted` |
| `swap`, `profile`, `rollback` | domain details |

**Result** (`--json --yes`; there is no confirmation in JSON mode): `status` (`done`, `nothing`,
`blocked`, `cancelled`, `need_root`, `stale`, `unknown_plan`, `plan_used`, `locked`, `busy`,
`experimental_not_confirmed`, `failed`, `rollback_failed`, `error`), `operation_id`, `error`,
`events` (steps and their rollback) and `plan`.

**Fingerprint.** A hash of the facts the plan was built on: active swap, filesystem and free space
(to the GiB), fstab, zswap/sysctl values, the journal (version, operations and their status), plus the
plan's decision (steps and check verdicts). Executing a stored plan rebuilds it under the lock and
refuses with `stale` if the fingerprint changed: "the system changed since the plan was built, build
the plan again". The CLI builds the plan and executes it through the same path.

## Exit codes

- `0` — success, or nothing to do.
- `1` — error, blocked plan, cancelled, or (with `--json`) any result other than `done`/`nothing`.
- `swap check-clean`: `1` if RamPlus traces were found.
