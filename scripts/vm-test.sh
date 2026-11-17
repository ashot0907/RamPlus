#!/usr/bin/env bash
# Интеграционный тест управления swap. ЗАПУСКАТЬ ТОЛЬКО В ОДНОРАЗОВОЙ VM:
# скрипт создаёт и удаляет swapfile, правит /etc/fstab и systemd-юниты.
#
# Требования к VM: systemd, ext4/xfs на /, root и свободное место: после
# swap 6 GiB должно остаться max(5 GiB, min(10% диска, 20 GiB)). Для
# multipass: multipass launch 24.04 --disk 30G --memory 4G.
#
#   make build
#   sudo RAMPLUS_VM_TEST=1 scripts/vm-test.sh ./ramplus
#
# Сценарии:
#   A. apply 4G → status → юнит поднимает swap → resize 6G → remove →
#      rollback (6G) → rollback (4G) → rollback (ничего) = исходное состояние
#   B. replace существующего swap (если swap нет — создаётся /swap.img 2G
#      со строкой в fstab, как в Ubuntu desktop) → rollback: старый swap
#      снова активен, fstab восстановлен байт в байт
#   C. replace --delete-old → rollback пересоздаёт старый swapfile
set -euo pipefail

BIN=${1:-./ramplus}
BIN=$(readlink -f "$BIN")
SWAPFILE=/ramplus.swapfile
UNIT=ramplus.swapfile.swap
STATE=/var/lib/ramplus/state.json
OLD=""            # существующий swap для сценариев B/C
CREATED_OLD=0     # /swap.img создан этим скриптом
WORK=$(mktemp -d /tmp/ramplus-vm-test.XXXXXX)
FSTAB_ORIG="$WORK/fstab.orig"

die() {
	printf 'FAIL: %b\n' "$*" >&2
	exit 1
}
step() { printf '\n=== %s\n' "$*"; }

[[ ${RAMPLUS_VM_TEST:-} == 1 ]] || die "это разрушающий тест: запустите в VM с RAMPLUS_VM_TEST=1"
[[ $EUID -eq 0 ]] || die "нужен root"
if command -v systemd-detect-virt >/dev/null; then
	virt=$(systemd-detect-virt || true)
	[[ $virt != none ]] || die "похоже, это не VM (systemd-detect-virt: none)"
fi
[[ -x $BIN ]] || die "нет бинарника $BIN (make build)"

# Активный swap: NAME SIZE(байты) PRIO. Колонки задаются через --show=...
# (у swapon нет опции --output: она молча игнорируется).
swaps() { swapon --show=NAME,SIZE,PRIO --noheadings --raw --bytes | sort; }

swap_size() { swaps | awk -v p="${1:-}" '$1 == p {print $2}'; }

is_active() { [[ -n $(swap_size "${1:-}") ]]; }

# assert_swap PATH SIZE_GIB — swap активен и примерно нужного размера
assert_swap() {
	local path=${1:-} gib=${2:-0} size="" want=0
	size=$(swap_size "$path")
	[[ -n $size ]] || die "$path не активен; swapon --show:\n$(swaps)"
	[[ $size =~ ^[0-9]+$ ]] || die "$path: swapon вернул не число: '$size'"
	want=$((gib * 1024 * 1024 * 1024))
	# swapon показывает размер без заголовка (одна страница)
	((size > want - 1048576 && size <= want)) || die "$path: размер $size, ожидалось ~$want"
	echo "ok: $path активен, $(numfmt --to=iec "$size")"
}

assert_no_swap() {
	if is_active "${1:-}"; then
		die "${1:-} всё ещё активен"
	fi
	echo "ok: ${1:-} не активен"
}

assert_eq() {
	[[ ${1:-} == "${2:-}" ]] || die "${3:-}:\nожидалось:\n${2:-}\nполучено:\n${1:-}"
	echo "ok: ${3:-}"
}

# assert_same_file A B ЧТО — файлы совпадают байт в байт
assert_same_file() {
	cmp -s "${1:-}" "${2:-}" || die "${3:-}: файлы различаются:\n$(diff -u "${1:-}" "${2:-}" || true)"
	echo "ok: ${3:-} (байт в байт)"
}

rp() {
	echo "+ ramplus $*"
	"$BIN" "$@"
}

cleanup() {
	local rc=$?
	if ((rc != 0)); then
		echo
		echo "Тест упал. Состояние для разбора:"
		swapon --show || true
		head -60 "$STATE" 2>/dev/null || true
	fi
	if ((CREATED_OLD)); then
		swapoff /swap.img 2>/dev/null || true
		rm -f /swap.img
		cp -p "$FSTAB_ORIG" /etc/fstab
	fi
	rm -rf "$WORK"
	exit "$rc"
}
trap cleanup EXIT

# Чистый старт по фактическому состоянию: нет управляемого swap, активного
# swap RamPlus, юнитов, незавершённых операций. Журнал с историей — не помеха.
if ! clean_report=$("$BIN" swap check-clean 2>&1); then
	die "VM не чистая:\n$clean_report"
fi
echo "$clean_report"
cp -p /etc/fstab "$FSTAB_ORIG"
INITIAL_SWAPS=$(swaps)

########################################################################
step "A1. plan без root ничего не меняет"
if id nobody >/dev/null 2>&1; then
	# Копия в /tmp: у nobody может не быть доступа к каталогу сборки.
	install -m 755 "$BIN" "$WORK/ramplus"
	chmod 755 "$WORK"
	setpriv --reuid=nobody --regid="$(id -g nobody)" --clear-groups "$WORK/ramplus" swap plan --size 4G >/dev/null
fi
assert_eq "$(swaps)" "$INITIAL_SWAPS" "plan не изменил swap"

step "A2. apply 4G"
rp swap apply --size 4G --existing=keep --yes
assert_swap "$SWAPFILE" 4
[[ $(stat -c %a "$SWAPFILE") == 600 ]] || die "права $SWAPFILE не 600"
systemctl is-enabled "$UNIT" >/dev/null || die "$UNIT не enabled"
if grep -q ramplus /etc/fstab; then
	die "fstab не должен меняться для нашего swap"
fi

step "A3. status показывает managed"
rp status --json | grep -q '"managed": true' || die "status --json: нет managed: true"
rp status | sed -n '/Swap-устройства/,/^$/p'

step "A4. systemd-юнит сам поднимает swap (как после перезагрузки)"
swapoff "$SWAPFILE"
systemctl start "$UNIT"
assert_swap "$SWAPFILE" 4

step "A5. resize 6G"
rp swap resize --size 6G --yes
assert_swap "$SWAPFILE" 6

step "A6. remove"
rp swap remove --yes
assert_no_swap "$SWAPFILE"
[[ ! -e $SWAPFILE && ! -e /etc/systemd/system/$UNIT ]] || die "remove оставил файлы"

step "A7. rollback (отмена remove → 6G)"
rp rollback --yes
assert_swap "$SWAPFILE" 6
systemctl is-enabled "$UNIT" >/dev/null || die "$UNIT не восстановлен"

step "A8. rollback (отмена resize → 4G), rollback (отмена apply)"
rp rollback --yes
assert_swap "$SWAPFILE" 4
rp rollback --yes
assert_no_swap "$SWAPFILE"
assert_eq "$(swaps)" "$INITIAL_SWAPS" "swap как в начале"
assert_same_file "$FSTAB_ORIG" /etc/fstab "fstab как в начале"
"$BIN" swap check-clean || die "после полного rollback остались следы"
if rp rollback --yes; then
	die "лишний rollback должен отказать"
fi

########################################################################
step "B0. существующий swap"
OLD=$(swaps | awk '$1 !~ /^\/dev\/zram/ {print $1; exit}')
if [[ -z $OLD ]]; then
	echo "swap нет — создаём /swap.img 2G и строку в fstab, как в Ubuntu desktop"
	[[ ! -e /swap.img ]] || die "/swap.img существует, но не активен — разберитесь вручную"
	CREATED_OLD=1
	fallocate -l 2G /swap.img
	chmod 600 /swap.img
	mkswap /swap.img >/dev/null
	swapon /swap.img
	printf '/swap.img\tnone\tswap\tsw\t0\t0\n' >>/etc/fstab
	OLD=/swap.img
fi
echo "существующий swap: $OLD"
BASE_SWAPS=$(swaps)
cp -p /etc/fstab "$WORK/fstab.before-replace"

step "B1. plan без --existing: по умолчанию keep, сказано явно"
rp swap plan --size 4G | grep -q 'по умолчанию — флаг не указан' || die "plan не сообщил о выборе по умолчанию"

step "B2. apply --existing=replace"
rp swap apply --size 4G --existing=replace --yes
assert_swap "$SWAPFILE" 4
assert_no_swap "$OLD"
if grep -q "$OLD" "$WORK/fstab.before-replace"; then
	grep -q "^# disabled by ramplus: $OLD" /etc/fstab || die "строка $OLD не закомментирована"
	compgen -G '/var/lib/ramplus/backups/*etc_fstab' >/dev/null || die "нет бэкапа fstab"
fi
[[ -e $OLD ]] || die "$OLD удалён без --delete-old"

step "B3. rollback: старый swap активен, fstab байт в байт"
rp rollback --yes
is_active "$OLD" || die "$OLD не активен после rollback"
assert_eq "$(swaps | awk '{print $1, $2}')" "$(awk '{print $1, $2}' <<<"$BASE_SWAPS")" "swap как до replace"
assert_same_file "$WORK/fstab.before-replace" /etc/fstab "fstab как до replace"

########################################################################
if [[ -f $OLD ]]; then
	OLD_SIZE=$(stat -c %s "$OLD")

	step "C1. apply --existing=replace --delete-old"
	rp swap apply --size 4G --existing=replace --delete-old --yes
	[[ ! -e $OLD ]] || die "$OLD не удалён"

	step "C2. rollback пересоздаёт $OLD"
	rp rollback --yes
	[[ $(stat -c %s "$OLD") == "$OLD_SIZE" ]] || die "$OLD пересоздан с другим размером"
	is_active "$OLD" || die "$OLD не активен после rollback"
	assert_same_file "$WORK/fstab.before-replace" /etc/fstab "fstab как до replace"
else
	step "C. пропущен: $OLD — раздел, а не файл"
fi

printf '\nВСЕ СЦЕНАРИИ ПРОЙДЕНЫ\n'
