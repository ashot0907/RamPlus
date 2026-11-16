#!/usr/bin/env bash
# Интеграционный тест управления swap. ЗАПУСКАТЬ ТОЛЬКО В ОДНОРАЗОВОЙ VM:
# скрипт создаёт и удаляет swapfile, правит /etc/fstab и systemd-юниты.
#
# Требования к VM: systemd, ext4/xfs на /, ≥ 30 GiB диска (после 6 GiB swap
# должно остаться ≥ 10% и ≥ 5 GiB), root.
#
#   make build
#   sudo RAMPLUS_VM_TEST=1 scripts/vm-test.sh ./ramplus
#
# Сценарии:
#   A. apply 4G → status → unit поднимает swap → resize 6G → remove →
#      rollback (6G) → rollback (4G) → rollback (ничего) = исходное состояние
#   B. replace существующего /swap.img → rollback = исходное состояние
#   C. replace --delete-old → rollback пересоздаёт /swap.img
set -euo pipefail

BIN=${1:-./ramplus}
BIN=$(readlink -f "$BIN")
UNIT=ramplus.swap.swap
OLD=/swap.img
CREATED_OLD=0

die() { echo "FAIL: $*" >&2; exit 1; }
step() { echo; echo "=== $*"; }

[[ ${RAMPLUS_VM_TEST:-} == 1 ]] || die "это разрушающий тест: запустите в VM с RAMPLUS_VM_TEST=1"
[[ $EUID -eq 0 ]] || die "нужен root"
if command -v systemd-detect-virt >/dev/null && [[ $(systemd-detect-virt || true) == none ]]; then
	die "похоже, это не VM (systemd-detect-virt: none)"
fi
[[ -x $BIN ]] || die "нет бинарника $BIN (make build)"

# swapon --show без заголовка: NAME SIZE(байты) PRIO
swaps() { swapon --show --noheadings --raw --bytes --output NAME,SIZE,PRIO | sort; }

# assert_swap PATH SIZE_GIB — swap активен и примерно нужного размера
assert_swap() {
	local path=$1 gib=$2 line size
	line=$(swaps | awk -v p="$path" '$1 == p') || true
	[[ -n $line ]] || die "$path не активен; swapon --show:\n$(swaps)"
	size=$(awk '{print $2}' <<<"$line")
	local want=$((gib * 1024 * 1024 * 1024))
	# swapon показывает размер без заголовка (одна страница)
	(( size > want - 1048576 && size <= want )) || die "$path: размер $size, ожидалось ~$want"
	echo "ok: $path активен, $(numfmt --to=iec "$size")"
}

assert_no_swap() {
	swaps | awk -v p="$1" '$1 == p {found=1} END {exit !found}' && die "$1 всё ещё активен"
	echo "ok: $1 не активен"
}

assert_eq() { [[ $1 == "$2" ]] || die "$3:\nожидалось:\n$2\nполучено:\n$1"; echo "ok: $3"; }

rp() { echo "+ ramplus $*"; "$BIN" "$@"; }

cleanup() {
	local rc=$?
	if (( rc != 0 )); then
		echo; echo "Тест упал. Состояние для разбора:"
		swapon --show || true
		head -50 /var/lib/ramplus/state.json 2>/dev/null || true
	fi
	if (( CREATED_OLD )); then
		swapoff "$OLD" 2>/dev/null || true
		sed -i "\|^$OLD\s|d" /etc/fstab
		rm -f "$OLD"
	fi
	exit $rc
}
trap cleanup EXIT

# Чистый старт: нашего swap и журнала быть не должно.
[[ ! -e /ramplus.swap && ! -e /var/lib/ramplus/state.json ]] || die "VM не чистая: есть /ramplus.swap или state.json"

INITIAL_SWAPS=$(swaps)
INITIAL_FSTAB=$(cat /etc/fstab)

########################################################################
step "A1. plan без root ничего не меняет"
if id nobody >/dev/null 2>&1; then
	# Копия в /tmp: у nobody может не быть доступа к каталогу сборки.
	TMPBIN=$(mktemp /tmp/ramplus.XXXXXX)
	install -m 755 "$BIN" "$TMPBIN"
	setpriv --reuid=nobody --regid="$(id -g nobody)" --clear-groups "$TMPBIN" swap plan --size 4G >/dev/null
	rm -f "$TMPBIN"
fi
assert_eq "$(swaps)" "$INITIAL_SWAPS" "plan не изменил swap"

step "A2. apply 4G"
rp swap apply --size 4G --existing=keep --yes
assert_swap /ramplus.swap 4
[[ $(stat -c %a /ramplus.swap) == 600 ]] || die "права /ramplus.swap не 600"
systemctl is-enabled "$UNIT" >/dev/null || die "$UNIT не enabled"
grep -q ramplus /etc/fstab && die "fstab не должен меняться для нашего swap"

step "A3. status показывает managed"
rp status --json | grep -q '"managed": true' || die "status --json: нет managed: true"
rp status | grep -A5 'Swap-устройства'

step "A4. systemd-юнит сам поднимает swap (как после перезагрузки)"
swapoff /ramplus.swap
systemctl start "$UNIT"
assert_swap /ramplus.swap 4

step "A5. resize 6G"
rp swap resize --size 6G --yes
assert_swap /ramplus.swap 6

step "A6. remove"
rp swap remove --yes
assert_no_swap /ramplus.swap
[[ ! -e /ramplus.swap && ! -e /etc/systemd/system/$UNIT ]] || die "remove оставил файлы"

step "A7. rollback (отмена remove → 6G)"
rp rollback --yes
assert_swap /ramplus.swap 6
systemctl is-enabled "$UNIT" >/dev/null || die "$UNIT не восстановлен"

step "A8. rollback (отмена resize → 4G), rollback (отмена apply)"
rp rollback --yes
assert_swap /ramplus.swap 4
rp rollback --yes
assert_no_swap /ramplus.swap
assert_eq "$(swaps)" "$INITIAL_SWAPS" "swap как в начале"
assert_eq "$(cat /etc/fstab)" "$INITIAL_FSTAB" "fstab как в начале"
if rp rollback --yes; then die "лишний rollback должен отказать"; fi

########################################################################
step "B0. подготовка: существующий $OLD в fstab"
if ! swaps | awk -v p="$OLD" '$1 == p {f=1} END {exit !f}'; then
	[[ ! -e $OLD ]] || die "$OLD существует, но не активен — разберитесь вручную"
	fallocate -l 1G "$OLD"; chmod 600 "$OLD"; mkswap "$OLD" >/dev/null; swapon "$OLD"
	printf '%s\tnone\tswap\tsw\t0\t0\n' "$OLD" >>/etc/fstab
	CREATED_OLD=1
fi
BASE_SWAPS=$(swaps)
BASE_FSTAB=$(cat /etc/fstab)
OLD_SIZE=$(stat -c %s "$OLD")

step "B1. plan без --existing: по умолчанию keep, сказано явно"
rp swap plan --size 4G | grep -q 'по умолчанию — флаг не указан' || die "plan не сообщил о выборе по умолчанию"

step "B2. apply --existing=replace"
rp swap apply --size 4G --existing=replace --yes
assert_swap /ramplus.swap 4
assert_no_swap "$OLD"
grep -q "^# disabled by ramplus: $OLD" /etc/fstab || die "строка $OLD не закомментирована"
[[ -e $OLD ]] || die "$OLD удалён без --delete-old"
ls /var/lib/ramplus/backups/*etc_fstab >/dev/null || die "нет бэкапа fstab"

step "B3. rollback"
rp rollback --yes
assert_eq "$(swaps)" "$BASE_SWAPS" "swap как до replace"
assert_eq "$(cat /etc/fstab)" "$BASE_FSTAB" "fstab как до replace"

########################################################################
step "C1. apply --existing=replace --delete-old"
rp swap apply --size 4G --existing=replace --delete-old --yes
[[ ! -e $OLD ]] || die "$OLD не удалён"

step "C2. rollback пересоздаёт $OLD"
rp rollback --yes
[[ $(stat -c %s "$OLD") == "$OLD_SIZE" ]] || die "$OLD пересоздан с другим размером"
assert_eq "$(swaps)" "$BASE_SWAPS" "swap как до replace"
assert_eq "$(cat /etc/fstab)" "$BASE_FSTAB" "fstab как до replace"

echo; echo "ВСЕ СЦЕНАРИИ ПРОЙДЕНЫ"
