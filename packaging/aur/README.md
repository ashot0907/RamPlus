# AUR packages

Two packages, one install script (`ramplus.install`, identical copies):

| package | what | builds |
|---|---|---|
| `ramplus-bin` | static binaries from the GitHub release tar.gz | nothing (fast) |
| `ramplus` | from the tagged source | `npm ci && npm run build`, `go build`, `go test` |

They conflict with each other. Both install to `/usr/bin`,
`/usr/lib/systemd/system`, `/usr/lib/sysusers.d`, `/usr/lib/tmpfiles.d`.
pacman's hooks create the users/groups and `/run/ramplus` and reload systemd;
the package never enables or starts the services.

Removal: pacman cannot abort a removal from a script, so unlike the .deb the
package is removed even while RamPlus manages swap — `pre_remove` prints a
warning with the undo commands. `/etc/ramplus` and `/var/lib/ramplus` are kept
(pacman has no purge); the journal is needed if the user reinstalls to roll
back.

## First publication

1. Create an account on <https://aur.archlinux.org> and add your SSH public
   key in *My Account → SSH Public Key*.
2. `~/.ssh/config`:

   ```
   Host aur.archlinux.org
     IdentityFile ~/.ssh/aur
     User aur
   ```

3. For each package (`ramplus-bin`, `ramplus`):

   ```sh
   git clone ssh://aur@aur.archlinux.org/ramplus-bin.git aur-ramplus-bin   # empty repo = name is free
   cp packaging/aur/ramplus-bin/{PKGBUILD,ramplus.install} aur-ramplus-bin/
   cd aur-ramplus-bin
   # set _ver and the real sha256 sums (see "Update" below), then:
   makepkg -f && namcap PKGBUILD *.pkg.tar.zst
   makepkg --printsrcinfo > .SRCINFO
   git add PKGBUILD ramplus.install .SRCINFO
   git commit -m "ramplus-bin 0.2.0alpha.3-1"
   git push origin master
   ```

The AUR accepts only the `master` branch and requires `.SRCINFO` in every
commit.

## Update for a new release

1. Wait for the GitHub release (tag `vX.Y.Z[-pre]`) to finish.
2. `ramplus-bin`: set `_ver` (without `v`), `pkgrel=1`, and copy the sums of
   `ramplus_<ver>_linux_amd64.tar.gz` / `..._arm64.tar.gz` from the release's
   `SHA256SUMS` into `sha256sums_x86_64` / `sha256sums_aarch64`.
3. `ramplus`: set `_ver`, `pkgrel=1`, then `updpkgsums` (from
   `pacman-contrib`) fills in the source tarball sum.
4. `makepkg -f`, `namcap`, `makepkg --printsrcinfo > .SRCINFO`, commit, push.

Packaging-only fixes (same upstream version): bump `pkgrel` instead.

Never publish with `SKIP` sums. The source tarball unpacks to `RamPlus-<ver>/`
(GitHub uses the repository name), hence `_srcdir` in `ramplus/PKGBUILD`.

## Checks (namcap)

Built from the real v0.2.0-alpha.3 downloads (checksums verified, `go test`
passed in `check()`) and checked in an `archlinux:latest` container
(2026-11-20). Remaining namcap warnings, all expected:

- `Dependency included, but may not be needed ('systemd')` — no binary links
  against libsystemd, but the package needs `systemctl`, sysusers.d and
  tmpfiles.d at runtime.
- `ramplus-bin`: `lacks FULL RELRO` / `lacks PIE` — the release binaries are
  static `CGO_ENABLED=0` builds (they run on any distribution). The source
  package `ramplus` follows the Arch Go guidelines (PIE, external linker,
  full RELRO) and has no such warnings.

Without namcap, check by hand: `pacman -Qlp *.pkg.tar.zst` lists only
`/usr/bin/{ramplus,ramplusd,ramplus-web}`, the two units, the sysusers.d and
tmpfiles.d files, README and LICENSE; the units contain `ExecStart=/usr/bin/…`
(no `@BINDIR@`); `.SRCINFO` matches the PKGBUILD.
