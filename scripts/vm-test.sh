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
# Изоляция: если в журнале есть операции прошлых прогонов (следов в системе
# нет — check-clean проходит), журнал откладывается в state.json.vmtest-<время>
# и возвращается в конце успешного прогона.
#
# Сценарии:
#   A. apply 4G → status → юнит поднимает swap → resize 6G → remove →
#      rollback (6G) → rollback (4G) → rollback (ничего) = исходное состояние
#   B. replace существующего swap (если swap нет — создаётся /swap.img 2G
#      со строкой в fstab, как в Ubuntu desktop) → rollback: старый swap
#      снова активен, fstab восстановлен байт в байт
#   C. replace --delete-old → rollback пересоздаёт старый swapfile
#   D. профиль zswap-lz4: plan → apply --experimental → status → «перезагрузка»
#      (ручная порча значений + restart ramplus-zswap.service) → дрейф →
#      rollback → apply + default --set → reset = значения как в начале
set -euo pipefail

BIN=${1:-./ramplus}
BIN=$(readlink -f "$BIN")
SWAPFILE=/ramplus.swapfile
UNIT=ramplus.swapfile.swap
STATE=/var/lib/ramplus/state.json
OLD=""            # существующий swap для сценариев B/C
CREATED_OLD=0     # /swap.img создан этим скриптом
SAVED_STATE=""    # журнал прошлых прогонов, отложенный на время теста
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

# assert_swap PATH SIZE_GIB — swap активен, размер в /proc/swaps и размер
# файла на диске соответствуют SIZE_GIB
assert_swap() {
	local path=${1:-} gib=${2:-0} size="" want=0 fsize=""
	size=$(swap_size "$path")
	[[ -n $size ]] || die "$path не активен; swapon --show:\n$(swaps)"
	[[ $size =~ ^[0-9]+$ ]] || die "$path: swapon вернул не число: '$size'"
	want=$((gib * 1024 * 1024 * 1024))
	# swapon показывает размер без заголовка (одна страница)
	((size > want - 1048576 && size <= want)) || die "$path: размер в swapon $size, ожидалось ~$want"
	fsize=$(stat -c %s "$path")
	[[ $fsize == "$want" ]] || die "$path: размер файла $fsize, ожидалось $want"
	echo "ok: $path активен, $(numfmt --to=iec "$size") (файл $(numfmt --to=iec "$fsize"))"
}

assert_no_swap() {
	if is_active "${1:-}"; then
		die "${1:-} всё ещё активен"
	fi
	echo "ok: ${1:-} не активен"
}

# names_sizes — активный swap без приоритетов (приоритет восстановленного
# swap ядро может назначить заново).
names_sizes() { swaps | awk '{print $1, $2}'; }

# assert_state PATH GIB — swap RamPlus PATH активен с размером GIB (0 —
# выключен и файла нет), весь остальной swap — как BASE (имена и размеры).
assert_state() {
	local path=${1:-} gib=${2:-0} base=${3:-}
	if ((gib > 0)); then
		assert_swap "$path" "$gib"
	else
		assert_no_swap "$path"
		[[ ! -e $path ]] || die "$path остался на диске"
	fi
	assert_eq "$(names_sizes | awk -v p="$path" '$1 != p')" "$base" "остальной swap не изменился"
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
	if [[ -n $SAVED_STATE ]]; then
		if ((rc == 0)); then
			mv -f "$SAVED_STATE" "$STATE"
			echo "журнал прошлых операций возвращён на место: $STATE"
		else
			# Журнал теста описывает то, что осталось в системе после падения, —
			# он нужен для разбора и rollback; старый не подменяем.
			echo "журнал прошлых операций остался в $SAVED_STATE (верните его вручную после разбора)"
		fi
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

# Изоляция от прошлых прогонов: незавершённых следов нет, но в журнале
# могут остаться неотменённые операции (например, profile reset), и
# «лишний rollback» дошёл бы до них. Журнал откладывается на время теста.
if "$BIN" history --json | grep -q '"id":'; then
	SAVED_STATE="$STATE.vmtest-$(date +%Y%m%dT%H%M%S)"
	mv "$STATE" "$SAVED_STATE"
	echo "в журнале есть операции прошлых прогонов: $STATE перемещён в $SAVED_STATE,"
	echo "тест начинается с пустого журнала (в конце журнал вернётся на место)"
fi
cp -p /etc/fstab "$FSTAB_ORIG"
INITIAL_SWAPS=$(swaps)
INITIAL_NS=$(names_sizes)

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
assert_state "$SWAPFILE" 4 "$INITIAL_NS"
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
assert_state "$SWAPFILE" 4 "$INITIAL_NS"

step "A5. resize 6G"
rp swap resize --size 6G --yes
assert_state "$SWAPFILE" 6 "$INITIAL_NS"

step "A6. remove"
rp swap remove --yes
assert_state "$SWAPFILE" 0 "$INITIAL_NS"
[[ ! -e $SWAPFILE && ! -e /etc/systemd/system/$UNIT ]] || die "remove оставил файлы"

step "A7. rollback (отмена remove → 6G)"
rp rollback --yes
assert_state "$SWAPFILE" 6 "$INITIAL_NS"
systemctl is-enabled "$UNIT" >/dev/null || die "$UNIT не восстановлен"

step "A8. rollback (отмена resize → 4G), rollback (отмена apply)"
rp rollback --yes
assert_state "$SWAPFILE" 4 "$INITIAL_NS"
rp rollback --yes
assert_state "$SWAPFILE" 0 "$INITIAL_NS"
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
BASE_NS=$(names_sizes)
OLD_NS=$(names_sizes | awk -v p="$OLD" '$1 != p')
cp -p /etc/fstab "$WORK/fstab.before-replace"

step "B1. plan без --existing: по умолчанию keep, сказано явно"
rp swap plan --size 4G | grep -q 'по умолчанию — флаг не указан' || die "plan не сообщил о выборе по умолчанию"

step "B2. apply --existing=replace"
rp swap apply --size 4G --existing=replace --yes
assert_no_swap "$OLD"
assert_state "$SWAPFILE" 4 "$OLD_NS"
if grep -q "$OLD" "$WORK/fstab.before-replace"; then
	grep -q "^# disabled by ramplus: $OLD" /etc/fstab || die "строка $OLD не закомментирована"
	compgen -G '/var/lib/ramplus/backups/*etc_fstab' >/dev/null || die "нет бэкапа fstab"
fi
[[ -e $OLD ]] || die "$OLD удалён без --delete-old"

step "B3. rollback: старый swap активен, fstab байт в байт"
rp rollback --yes
assert_state "$SWAPFILE" 0 "$BASE_NS"
assert_same_file "$WORK/fstab.before-replace" /etc/fstab "fstab как до replace"

########################################################################
if [[ -f $OLD ]]; then
	OLD_SIZE=$(stat -c %s "$OLD")

	step "C1. apply --existing=replace --delete-old"
	rp swap apply --size 4G --existing=replace --delete-old --yes
	[[ ! -e $OLD ]] || die "$OLD не удалён"
	assert_state "$SWAPFILE" 4 "$OLD_NS"

	step "C2. rollback пересоздаёт $OLD"
	rp rollback --yes
	[[ $(stat -c %s "$OLD") == "$OLD_SIZE" ]] || die "$OLD пересоздан с другим размером"
	assert_state "$SWAPFILE" 0 "$BASE_NS"
	assert_same_file "$WORK/fstab.before-replace" /etc/fstab "fstab как до replace"
else
	step "C. пропущен: $OLD — раздел, а не файл"
fi

########################################################################
ZP=/sys/module/zswap/parameters
PROFILE_FILES=(/etc/sysctl.d/99-ramplus.conf /etc/systemd/system/ramplus-zswap.service)

# params — текущие значения параметров профиля (путь=значение).
params() {
	local f
	for f in "$ZP"/enabled "$ZP"/compressor "$ZP"/zpool "$ZP"/max_pool_percent "$ZP"/shrinker_enabled \
		/proc/sys/vm/swappiness /proc/sys/vm/page-cluster /proc/sys/vm/watermark_scale_factor /proc/sys/vm/vfs_cache_pressure; do
		if [[ -r $f ]]; then
			printf '%s=%s\n' "$f" "$(<"$f")"
		fi
	done
}

assert_param() {
	local f=${1:-} want=${2:-} got=""
	got=$(<"$f")
	[[ $got == "$want" ]] || die "$f = $got, ожидалось $want"
	echo "ok: $f = $want"
}

assert_no_profile_files() {
	local f
	for f in "${PROFILE_FILES[@]}"; do
		[[ ! -e $f ]] || die "остался $f"
	done
	echo "ok: файлов профиля нет"
}

step "D0. исходные значения"
INITIAL_PARAMS=$(params)
echo "$INITIAL_PARAMS"
assert_no_profile_files

step "D1. plan без root ничего не меняет"
rp profile plan zswap-lz4 | grep -q 'zswap.compressor' || die "plan не показал zswap.compressor"
assert_eq "$(params)" "$INITIAL_PARAMS" "plan не изменил значения"

step "D2. apply без подтверждения экспериментального профиля — отказ"
if rp profile apply zswap-lz4 --yes; then
	die "экспериментальный профиль применился без --experimental"
fi
assert_eq "$(params)" "$INITIAL_PARAMS" "значения не изменились"

step "D3. apply --experimental"
rp profile apply zswap-lz4 --experimental --yes
assert_param "$ZP/enabled" Y
assert_param "$ZP/compressor" lz4
assert_param "$ZP/max_pool_percent" 20
if [[ -e $ZP/zpool ]]; then
	assert_param "$ZP/zpool" zsmalloc
fi
grep -q '# Managed by RamPlus' /etc/sysctl.d/99-ramplus.conf || die "нет маркера в 99-ramplus.conf"
systemctl is-enabled ramplus-zswap.service >/dev/null || die "ramplus-zswap.service не enabled"

step "D4. status: профиль активен"
rp status --json | grep -q '"name": "zswap-lz4"' || die "status --json: нет профиля"
rp status | sed -n '/Профиль RamPlus/,/^$/p'

step "D5. «перезагрузка»: значения испорчены вручную, юнит их возвращает"
echo N >"$ZP/enabled"
echo lzo >"$ZP/compressor"
systemctl restart ramplus-zswap.service
assert_param "$ZP/enabled" Y
assert_param "$ZP/compressor" lz4

step "D6. дрейф: ручная запись swappiness → status предупреждает"
SWAPPINESS=$(</proc/sys/vm/swappiness)
echo 133 >/proc/sys/vm/swappiness
rp status --json | grep -q '"key": "vm.swappiness"' || die "status --json: нет дрейфа vm.swappiness"
rp status | grep -q 'значения изменены вручную' || die "status не предупредил о дрейфе"
echo "$SWAPPINESS" >/proc/sys/vm/swappiness

step "D7. rollback: значения как в начале, файлов нет"
rp rollback --yes
assert_eq "$(params)" "$INITIAL_PARAMS" "значения как в начале"
assert_no_profile_files

step "D8. apply + default --set → reset: значения как в начале, файлов нет"
rp profile apply zswap-lz4 --experimental --yes
rp profile apply default --set vm.swappiness=80 --yes
assert_param /proc/sys/vm/swappiness 80
rp profile reset --yes
assert_eq "$(params)" "$INITIAL_PARAMS" "значения как в начале"
assert_no_profile_files
rp profile reset --yes | grep -q 'Нечего делать' || die "повторный reset должен быть пустым"
"$BIN" swap check-clean || die "после reset остались следы"

printf '\nВСЕ СЦЕНАРИИ ПРОЙДЕНЫ\n'
