# How safety works

RamPlus runs as root and changes swap and kernel settings, so every change goes through the same
path: **plan → checks → confirmation → journalled steps → automatic rollback on failure**. The CLI,
the daemon and the web UI all use this one path (`internal/core` → `internal/journal`).

## Plan first

Every operation starts as a plan built from facts gathered read-only: active swap, mounted
filesystems and free space, `/etc/fstab`, `/proc/cmdline`, current zswap and sysctl values, the
journal. The plan lists:

- the explicit **choices** (size, location, keep/replace for existing swap, deleting old files) and
  their consequences;
- every **check**, with a level (`ok`, `info`, `warn`, `fail`) and a stable code;
- every **step** that will run;
- a **summary**: swap before → after, files created, disabled, deleted.

If any check is `fail`, the plan is **blocked** and nothing is touched. Plans are shown by
`--dry-run` / `plan` without root.

**Fingerprint.** A plan carries a hash of the facts it was built on and of its own decision. When a
stored plan is applied (daemon, web UI), it is rebuilt under the lock; if anything relevant changed in
between, apply refuses with `stale` ("the system changed since the plan was built, build it again")
and nothing changes.

## Checks

Swap (before any change):

- **free space** on the target filesystem: after creating the swapfile at least
  max(5 GiB, min(10 % of the disk, 20 GiB)) must remain;
- **filesystem:** ext4 and xfs — `fallocate`; btrfs — a NOCOW file, by default in a separate subvolume
  `/ramplus-swap`; NTFS, vfat, exFAT, fuseblk, ZFS, tmpfs, overlayfs, network filesystems and read-only
  mounts are refused with an explanation;
- **disk:** a USB disk is refused unless `--force` (unplugging it would crash every process with pages
  in swap); HDD — warning; virtual disk — neutral note;
- **path:** must be absolute, not in `/proc`, `/sys`, `/dev`, `/run`, `/tmp`, `/var/tmp`, `/boot`; an
  existing file that RamPlus did not create is never overwritten;
- **hibernation:** `resume=` in the kernel command line → warning;
- **memory before swapoff:** before turning off any swap, its used data must fit into available RAM
  plus free space in the remaining swap (with a 256 MiB reserve);
- **priority:** new swap goes above existing disk swap but below zram.

Profiles:

- the compressor and zpool are available (`/proc/crypto`, built-in or as a module);
- zswap on top of zram is refused (`--force` to override);
- `zswap.*` in the kernel command line, or the same sysctl key in a file applied after ours → warning,
  with the order of application;
- `--set` values are range-checked;
- an experimental profile needs explicit confirmation.

Common: root, systemd present, the journal readable, no interrupted operation.

When run through the daemon, paths the daemon cannot write (it runs with `ProtectHome=read-only`) are
blocked at plan time (`path.not_writable_by_daemon`) instead of failing halfway.

## The journal

`/var/lib/ramplus/state.json` (format version 3) holds:

- `managed` — the RamPlus swap (path, size, priority, unit);
- `profile` — the applied profile and its expected values;
- `baseline` — the system's original values, captured once on the first profile apply;
- `operations` — every operation: id, action, command, status, time, the steps applied and how to undo
  each, the sections before/after, `source` (`cli`/`daemon`) and `requested_by` (uid, user, `via: web`).

Operation status: `in_progress`, `committed`, `failed` (a step failed, everything rolled back),
`rollback_failed` (rollback was incomplete — manual check), `rolled_back` (undone by `rollback`).

Backups of files RamPlus modifies (for example `/etc/fstab`) go to
`/var/lib/ramplus/backups/<operation-id>-<path>`.

A lock `/run/lock/ramplus.lock` (flock, non-blocking) prevents two RamPlus processes — CLI and daemon —
from running operations at the same time; the second one is refused.

### Write-ahead

1. The operation is written to the journal **before** the first change.
2. For each step: compute how to undo it and save any backup → record the step as `pending` → run it →
   record it as `done`.
3. If a step fails, the steps already done in this operation are undone in reverse order, and the
   journal is shortened after each undo. The error names the failed step.
4. If writing the journal fails after a step, that step is undone too: without a journal record there
   is no guaranteed rollback.

If the process itself dies (power loss, `SIGKILL`), the next run sees the `in_progress` operation and
blocks every other plan (`journal.interrupted`) until `ramplus rollback`. The rollback undoes the
interrupted (`pending`) step only if what is on disk matches the record (path, size, mode, content);
if it cannot tell, it refuses and asks for a manual check.

## Rollback

`ramplus rollback` (or **Undo** in the web UI) undoes the last undoable operation; repeating it goes
further back. One stack is shared by swap, profiles and `setup`.

Before undoing, RamPlus checks that the system is exactly in the state the journal recorded after that
operation: swap devices and sizes, files and their content, units, kernel values. On any difference it
refuses (`rollback.state_mismatch`) and lists the differences, instead of guessing.

The plan states what is undone. Warnings: `rollback.old` (the operation is more than a day old) and
`rollback.other_domain` (it is of another kind than your last operation).

A `setup` operation is undone as one unit: profile steps first, then swap.

## Orphans

If the journal is lost or deleted but a RamPlus swapfile or unit remains, `ramplus swap check-clean`
reports "RamPlus traces without a journal entry". `sudo ramplus swap remove --orphan` removes them —
with a plan and confirmation, as a journalled, reversible operation, and only after verifying markers:

- the unit contains `# Managed by RamPlus`;
- the file is a regular file with a `SWAPSPACE2` signature;
- the path is a RamPlus default path, or comes from `What=` of a unit with the marker.

If anything does not match, the plan is blocked (`orphan.not_ours`) and nothing is touched.

## What RamPlus creates

By operations:

| path | created by | notes |
|---|---|---|
| `/ramplus.swapfile` | swap apply/setup | mode 600; on btrfs `/ramplus-swap/ramplus.swapfile` in a NOCOW subvolume; or `--path` |
| `/etc/systemd/system/ramplus.swapfile.swap` | swap apply/setup | swap unit with the marker; name follows the path (`systemd-escape --path`) |
| `/etc/fstab` | swap apply with `replace` | the old swap line is commented out with `# disabled by ramplus:`; backup kept. RamPlus never adds its own swap to fstab |
| `/etc/sysctl.d/99-ramplus.conf` | profile apply/setup | restores `vm.*` values at boot |
| `/etc/systemd/system/ramplus-zswap.service` | profile apply/setup | oneshot unit restoring zswap parameters at boot |
| `/var/lib/ramplus/state.json`, `backups/` | any operation | kept on uninstall so rollback stays possible |
| `/run/lock/ramplus.lock` | any operation | lock file |
| `/etc/ramplus/web-auth` | `ramplus init` | `root:ramplus-web`, 0640 |

Never changed: GRUB, the kernel command line, initramfs.

By `make install` (prefix `/usr/local`): `bin/{ramplus,ramplusd,ramplus-web}`,
`lib/systemd/system/{ramplusd,ramplus-web}.service`, `lib/sysusers.d/ramplus.conf` (groups `ramplus`,
`ramplus-admin`, user `ramplus-web`), `lib/tmpfiles.d/ramplus.conf` (`/run/ramplus`).

## Removing everything

```sh
sudo ramplus rollback        # repeat until history has nothing left to undo
# or: sudo ramplus swap remove && sudo ramplus profile reset
ramplus swap check-clean     # exit code 0 — no RamPlus traces left
sudo make uninstall PURGE=1
```

`make uninstall` first runs `ramplus swap check-clean`; if RamPlus still manages swap or a profile (or
orphans exist), it refuses and says what to do (`FORCE=1` uninstalls anyway and leaves the system as
it is). It stops and disables the services. Groups, the `ramplus-web` user and the web password are
removed only with `PURGE=1`. The journal in `/var/lib/ramplus` is kept.

Note that choosing "Like stock Ubuntu" applies the `default` profile — RamPlus still manages it.
To hand settings back completely, use `profile reset` (or undo).
