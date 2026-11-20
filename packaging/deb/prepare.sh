#!/bin/sh
# Prepares generated files for the .deb (called by GoReleaser before the
# build, see .goreleaser.yaml): systemd units with @BINDIR@ → /usr/bin and a
# Debian changelog for the given version. Output: build/deb/.
#
#   packaging/deb/prepare.sh <version>     e.g. 0.2.0-alpha.3
set -eu

version=${1:?usage: prepare.sh <version>}
out=build/deb
rm -rf "$out"
mkdir -p "$out"

for u in ramplusd ramplus-web; do
	sed 's|@BINDIR@|/usr/bin|g' "packaging/systemd/$u.service" >"$out/$u.service"
done

# Debian orders "~" before anything, so 0.2.0~alpha.3 < 0.2.0 (nfpm does the same).
debver=$(printf '%s' "$version" | sed 's/-/~/')
# Commit date, not "now": the same commit gives the same package.
when=$(date -u -R -d "@$(git log -1 --format=%ct 2>/dev/null || date +%s)")
cat >"$out/changelog" <<CHANGELOG
ramplus ($debver) unstable; urgency=medium

  * Upstream release $version.
    Release notes: https://github.com/ashot0907/ramplus/releases

 -- RamPlus maintainers <ashot0907@users.noreply.github.com>  $when
CHANGELOG
gzip -n -9 -f "$out/changelog"
