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
# Падение в любом сценарии: журнал теста копируется в state.json.vmtest-failed-
# <время>, демон останавливается (дождавшись операции), ручные изменения
# возвращаются, операции теста откатываются через CLI по одной — только пока
# система не в исходном состоянии (check-clean и параметры как в начале), демон удаляется,
# /swap.img и fstab — как были. Если check-clean после этого чистый, журнал
# прошлых прогонов возвращается на место.
#
# Ожидаемый «остальной swap» снимается непосредственно перед каждым
# изменяющим шагом (before), а не один раз в начале.
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
#   E. демон: make install → systemctl start ramplusd → API status → plan swap
#      4G → apply → ждать /operations/{id} → swap 4G → plan rollback → apply →
#      чисто; права: nobody без групп не доходит до сокета, член ramplus
#      строит план, но apply → 403; make uninstall — следов нет.
#      Нужны curl и собранный ./ramplusd (make build).
set -euo pipefail

BIN=${1:-./ramplus}
BIN=$(readlink -f "$BIN")
SWAPFILE=/ramplus.swapfile
UNIT=ramplus.swapfile.swap
STATE=/var/lib/ramplus/state.json
OLD=""            # существующий swap для сценариев B/C
CREATED_OLD=0     # /swap.img создан этим скриптом
SAVED_STATE=""    # журнал прошлых прогонов, отложенный на время теста
REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
INSTALLED=0       # make install выполнен этим скриптом (сценарий E)
GROUPS_PREEXIST=0 # группы ramplus/ramplus-admin были до теста (не удалять)
# Ручные изменения вне журнала, которые надо вернуть при падении:
DIRTY_UNIT=0         # A4: swap выключен вручную, юнит ещё не поднял его
DIRTY_ZSWAP=0        # D5: параметры zswap испорчены вручную
DIRTY_SWAPPINESS=""  # D6: исходное значение swappiness до ручной записи
FAILED_STATE=""
INITIAL_PARAMS=""
# STARTED — тест начал менять систему: журнал с этого момента содержит только
# операции теста, и при падении их можно откатывать. До этого (например,
# «VM не чистая») trap ничего не откатывает — это чужие операции.
STARTED=0
SOCK=/run/ramplus/ramplusd.sock
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
[[ -x $REPO/ramplusd ]] || die "нет $REPO/ramplusd (make build)"
command -v curl >/dev/null || die "нужен curl (сценарий E)"

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

# others — активный swap, кроме swap RamPlus (имена и размеры).
others() { names_sizes | awk -v p="$SWAPFILE" '$1 != p'; }

# before — снимок «остального swap» непосредственно перед изменяющим шагом
# (а не один раз в начале: сценарии B/C создают и возвращают /swap.img).
before() { BEFORE=$(others); }

# without SNAPSHOT PATH — снимок без PATH (replace выключает старый swap).
without() { awk -v p="${2:-}" '$1 != p' <<<"${1:-}"; }

# assert_state PATH GIB EXPECTED — swap RamPlus PATH активен с размером GIB
# (0 — выключен и файла нет), весь остальной swap — EXPECTED (имена и размеры).
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

rp() {
	echo "+ ramplus $*"
	"$BIN" "$@"
}

# system_clean — RamPlus ничего не держит (check-clean: нет managed swap,
# активного профиля, незавершённых операций, файлов и юнитов RamPlus) и
# параметры zswap/vm.* равны снятым в начале теста.
system_clean() {
	"$BIN" swap check-clean >/dev/null 2>&1 || return 1
	[[ -z $INITIAL_PARAMS || $(params) == "$INITIAL_PARAMS" ]]
}

# restore_after_failure — вернуть VM в исходное состояние после падения в
# любом сценарии: демон остановить, ручные изменения вернуть, операции теста
# откатывать через CLI по одной, пока система не чистая (журнал теста
# содержит только их; перед каждым откатом — system_clean), демон удалить.
restore_after_failure() {
	echo
	echo "=== Возврат VM в исходное состояние"
	if ((INSTALLED)); then
		# systemd дожидается текущей операции демона (graceful stop).
		systemctl stop ramplusd 2>/dev/null || true
	fi
	if ((DIRTY_UNIT)); then
		systemctl start "$UNIT" || true
	fi
	if ((DIRTY_ZSWAP)); then
		systemctl restart ramplus-zswap.service || true
	fi
	if [[ -n $DIRTY_SWAPPINESS ]]; then
		echo "$DIRTY_SWAPPINESS" >/proc/sys/vm/swappiness || true
	fi
	local i
	for ((i = 0; i < 30; i++)); do
		# Система уже в исходном состоянии (например, упали после D8 reset) —
		# откаты не нужны: они заново применяли бы и снимали профили.
		if system_clean; then
			echo "система в исходном состоянии — откаты не нужны"
			break
		fi
		if ! "$BIN" history --json 2>/dev/null | grep -q '"rollback_next": true'; then
			echo "откатывать нечего, но система не в исходном состоянии — разбирать вручную"
			break
		fi
		if ! "$BIN" rollback --yes; then
			echo "rollback отказал — остаток разбирать вручную"
			break
		fi
	done
	if ((INSTALLED)); then
		if ! make -s -C "$REPO" uninstall "${PURGE[@]}"; then
			echo "uninstall отказал (RamPlus ещё управляет системой) — удаляем демон, следы оставляем для разбора"
			make -s -C "$REPO" uninstall FORCE=1 "${PURGE[@]}" || true
		fi
		INSTALLED=0
	fi
}

cleanup() {
	local rc=$?
	trap - EXIT
	if ((rc != 0 && STARTED)); then
		echo
		echo "Тест упал. Состояние для разбора:"
		swapon --show || true
		if [[ -f $STATE ]]; then
			FAILED_STATE="$STATE.vmtest-failed-$(date +%Y%m%dT%H%M%S)"
			cp -p "$STATE" "$FAILED_STATE"
			echo "журнал теста сохранён: $FAILED_STATE"
		fi
		restore_after_failure
	fi
	if ((CREATED_OLD)); then
		swapoff /swap.img 2>/dev/null || true
		rm -f /swap.img
		cp -p "$FSTAB_ORIG" /etc/fstab
	fi
	local clean=0
	if "$BIN" swap check-clean >/dev/null 2>&1; then
		clean=1
	fi
	if ((rc != 0 && STARTED)); then
		if ((clean)); then
			echo "VM в исходном состоянии (check-clean)"
		else
			echo "ВНИМАНИЕ: VM не вернулась в исходное состояние:"
			"$BIN" swap check-clean || true
		fi
	fi
	if [[ -n $SAVED_STATE ]]; then
		if ((clean)) || ((!STARTED)); then
			mv -f "$SAVED_STATE" "$STATE"
			echo "журнал прошлых операций возвращён на место: $STATE"
		else
			# Журнал теста описывает то, что осталось в системе, — он нужен для
			# разбора и rollback; старый не подменяем.
			echo "журнал прошлых операций остался в $SAVED_STATE (верните его вручную после разбора)"
		fi
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
if getent group ramplus >/dev/null || getent group ramplus-admin >/dev/null; then
	GROUPS_PREEXIST=1
fi
PURGE=()
if ((!GROUPS_PREEXIST)); then
	PURGE=(PURGE=1) # группы создаст make install этого теста — он их и удалит
fi
A_START_SWAPS=$(swaps)
INITIAL_PARAMS=$(params) # значения zswap/vm.* в начале теста (D и очистка)
STARTED=1

########################################################################
step "A1. plan без root ничего не меняет"
if id nobody >/dev/null 2>&1; then
	# Копия в /tmp: у nobody может не быть доступа к каталогу сборки.
	install -m 755 "$BIN" "$WORK/ramplus"
	chmod 755 "$WORK"
	setpriv --reuid=nobody --regid="$(id -g nobody)" --clear-groups "$WORK/ramplus" swap plan --size 4G >/dev/null
fi
assert_eq "$(swaps)" "$A_START_SWAPS" "plan не изменил swap"

step "A2. apply 4G"
before
rp swap apply --size 4G --existing=keep --yes
assert_state "$SWAPFILE" 4 "$BEFORE"
[[ $(stat -c %a "$SWAPFILE") == 600 ]] || die "права $SWAPFILE не 600"
systemctl is-enabled "$UNIT" >/dev/null || die "$UNIT не enabled"
if grep -q ramplus /etc/fstab; then
	die "fstab не должен меняться для нашего swap"
fi

step "A3. status показывает managed"
rp status --json | grep -q '"managed": true' || die "status --json: нет managed: true"
rp status | sed -n '/Swap-устройства/,/^$/p'

step "A4. systemd-юнит сам поднимает swap (как после перезагрузки)"
before
DIRTY_UNIT=1
swapoff "$SWAPFILE"
systemctl start "$UNIT"
DIRTY_UNIT=0
assert_state "$SWAPFILE" 4 "$BEFORE"

step "A5. resize 6G"
before
rp swap resize --size 6G --yes
assert_state "$SWAPFILE" 6 "$BEFORE"

step "A6. remove"
before
rp swap remove --yes
assert_state "$SWAPFILE" 0 "$BEFORE"
[[ ! -e $SWAPFILE && ! -e /etc/systemd/system/$UNIT ]] || die "remove оставил файлы"

step "A7. rollback (отмена remove → 6G)"
before
rp rollback --yes
assert_state "$SWAPFILE" 6 "$BEFORE"
systemctl is-enabled "$UNIT" >/dev/null || die "$UNIT не восстановлен"

step "A8. rollback (отмена resize → 4G), rollback (отмена apply)"
before
rp rollback --yes
assert_state "$SWAPFILE" 4 "$BEFORE"
before
rp rollback --yes
assert_state "$SWAPFILE" 0 "$BEFORE"
assert_eq "$(swaps)" "$A_START_SWAPS" "swap как в начале сценария A"
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
cp -p /etc/fstab "$WORK/fstab.before-replace"

step "B1. plan без --existing: по умолчанию keep, сказано явно"
rp swap plan --size 4G | grep -q 'по умолчанию — флаг не указан' || die "plan не сообщил о выборе по умолчанию"

step "B2. apply --existing=replace"
before
REPLACE_BEFORE=$BEFORE # rollback replace возвращает ровно это
rp swap apply --size 4G --existing=replace --yes
assert_no_swap "$OLD"
assert_state "$SWAPFILE" 4 "$(without "$BEFORE" "$OLD")"
if grep -q "$OLD" "$WORK/fstab.before-replace"; then
	grep -q "^# disabled by ramplus: $OLD" /etc/fstab || die "строка $OLD не закомментирована"
	compgen -G '/var/lib/ramplus/backups/*etc_fstab' >/dev/null || die "нет бэкапа fstab"
fi
[[ -e $OLD ]] || die "$OLD удалён без --delete-old"

step "B3. rollback: старый swap активен, fstab байт в байт"
rp rollback --yes
assert_state "$SWAPFILE" 0 "$REPLACE_BEFORE"
assert_same_file "$WORK/fstab.before-replace" /etc/fstab "fstab как до replace"

########################################################################
if [[ -f $OLD ]]; then
	OLD_SIZE=$(stat -c %s "$OLD")

	step "C1. apply --existing=replace --delete-old"
	before
	REPLACE_BEFORE=$BEFORE
	rp swap apply --size 4G --existing=replace --delete-old --yes
	[[ ! -e $OLD ]] || die "$OLD не удалён"
	assert_state "$SWAPFILE" 4 "$(without "$BEFORE" "$OLD")"

	step "C2. rollback пересоздаёт $OLD"
	rp rollback --yes
	[[ $(stat -c %s "$OLD") == "$OLD_SIZE" ]] || die "$OLD пересоздан с другим размером"
	assert_state "$SWAPFILE" 0 "$REPLACE_BEFORE"
	assert_same_file "$WORK/fstab.before-replace" /etc/fstab "fstab как до replace"
else
	step "C. пропущен: $OLD — раздел, а не файл"
fi

########################################################################
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

step "D0. исходные значения (сняты в начале теста)"
echo "$INITIAL_PARAMS"
assert_eq "$(params)" "$INITIAL_PARAMS" "сценарии A–C не изменили параметры"
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
DIRTY_ZSWAP=1
echo N >"$ZP/enabled"
echo lzo >"$ZP/compressor"
systemctl restart ramplus-zswap.service
DIRTY_ZSWAP=0
assert_param "$ZP/enabled" Y
assert_param "$ZP/compressor" lz4

step "D6. дрейф: ручная запись swappiness → status предупреждает"
SWAPPINESS=$(</proc/sys/vm/swappiness)
DIRTY_SWAPPINESS=$SWAPPINESS
echo 133 >/proc/sys/vm/swappiness
rp status --json | grep -q '"key": "vm.swappiness"' || die "status --json: нет дрейфа vm.swappiness"
rp status | grep -q 'значения изменены вручную' || die "status не предупредил о дрейфе"
echo "$SWAPPINESS" >/proc/sys/vm/swappiness
DIRTY_SWAPPINESS=""

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

########################################################################
# api [AS...] -- METHOD PATH [BODY]: запрос к демону; тело ответа — в $BODY,
# код — в $CODE. AS — префикс запуска от другого пользователя (setpriv ...).
api() {
	local as=()
	while [[ $1 != -- ]]; do
		as+=("$1")
		shift
	done
	shift
	local method=$1 path=$2 data=${3:-} out
	local args=(-sS --unix-socket "$SOCK" -X "$method" -w '\n%{http_code}')
	if [[ -n $data ]]; then
		args+=(-H 'Content-Type: application/json' -d "$data")
	fi
	local rc=0
	out=$("${as[@]}" curl "${args[@]}" "http://localhost$path") || rc=$?
	((rc == 0)) || die "curl $method $path: код $rc (не удалось подключиться или передать запрос)"
	CODE=${out##*$'\n'}
	BODY=${out%$'\n'*}
}

# field NAME — первое значение "NAME": "..." в $BODY.
field() { sed -n "s/.*\"$1\": \"\([^\"]*\)\".*/\1/p" <<<"$BODY" | head -1; }

expect_code() {
	[[ $CODE == "$1" ]] || die "$2: ожидался HTTP $1, получен $CODE:\n$BODY"
}

# wait_op ID — ждать завершения операции демона, проверить status=done.
wait_op() {
	local i
	for ((i = 0; i < 600; i++)); do
		api -- GET "/v1/operations/$1"
		expect_code 200 "операция $1"
		if grep -q '"running": false' <<<"$BODY"; then
			[[ $(field status) == "done" ]] || die "операция $1 не выполнена:\n$BODY"
			return
		fi
		sleep 0.5
	done
	die "операция $1 не завершилась за 5 минут"
}

# apply_plan — POST /v1/apply по plan_id из $BODY, ждать завершения.
apply_plan() {
	local id
	id=$(field id)
	[[ -n $id ]] || die "в ответе нет id плана:\n$BODY"
	api -- POST /v1/apply "{\"plan_id\": \"$id\"}"
	expect_code 202 "apply $id"
	wait_op "$(field operation_id)"
}

step "E1. make install, systemctl start ramplusd"
make -s -C "$REPO" install
INSTALLED=1
systemctl start ramplusd
for ((i = 0; i < 50; i++)); do
	[[ -S $SOCK ]] && break
	sleep 0.2
done
[[ -S $SOCK ]] || die "демон не создал $SOCK:\n$(journalctl -u ramplusd -n 30 --no-pager)"
[[ $(stat -c '%a %U %G' "$SOCK") == "660 root ramplus" ]] || die "права сокета: $(stat -c '%a %U %G' "$SOCK")"
rp daemon status

step "E2. status через API"
api -- GET /v1/status
expect_code 200 "status"
grep -q '"memory"' <<<"$BODY" || die "status без memory:\n$BODY"

step "E3. plan swap 4G → apply → swap 4G"
before
api -- POST /v1/plan/swap '{"action": "apply", "size": "4G", "existing": "keep"}'
expect_code 200 "plan swap"
apply_plan
assert_state "$SWAPFILE" 4 "$BEFORE"

step "E4. plan rollback → apply → чисто"
before
api -- POST /v1/plan/rollback
expect_code 200 "plan rollback"
grep -q '"domain": "swap"' <<<"$BODY" || die "rollback отменяет не swap:\n$BODY"
apply_plan
assert_state "$SWAPFILE" 0 "$BEFORE"
"$BIN" swap check-clean || die "после rollback через демон остались следы"

step "E5. права"
before
NOGROUP=$(id -gn nobody)
nobody=(setpriv --reuid=nobody --regid="$NOGROUP" --clear-groups)
reader=(setpriv --reuid=nobody --regid="$NOGROUP" --groups=ramplus)
# Без групп nobody не проходит права каталога/сокета (0750/0660 root:ramplus):
# до HTTP дело не доходит (403 auth.forbidden за SO_PEERCRED — в go test).
# curl любую ошибку подключения к unix-сокету сообщает кодом 7 и текстом не
# различает причину, поэтому причина доказывается отдельно: сокет есть,
# права каталога те, nobody не может в него войти, а член ramplus — может.
[[ -S $SOCK ]] || die "нет сокета $SOCK"
[[ $(stat -c '%a %U %G' "$(dirname "$SOCK")") == "750 root ramplus" ]] ||
	die "права $(dirname "$SOCK"): $(stat -c '%a %U %G' "$(dirname "$SOCK")")"
rc=0
"${nobody[@]}" curl -sS --unix-socket "$SOCK" http://localhost/v1/status >/dev/null 2>&1 || rc=$?
[[ $rc == 7 ]] || die "nobody: curl должен не подключиться (код 7), получен код $rc"
if "${nobody[@]}" test -x "$(dirname "$SOCK")"; then
	die "nobody без групп может войти в $(dirname "$SOCK") — причина отказа не права"
fi
if "${nobody[@]}" stat "$SOCK" >/dev/null 2>&1; then
	die "nobody без групп видит $SOCK — причина отказа не права"
fi
"${reader[@]}" test -x "$(dirname "$SOCK")" || die "член ramplus не может войти в $(dirname "$SOCK")"
"${reader[@]}" test -w "$SOCK" || die "член ramplus не может писать в $SOCK"
echo "ok: nobody без групп не доходит до сокета из-за прав (curl код 7, каталог закрыт), член ramplus доходит"
api "${reader[@]}" -- GET /v1/status
expect_code 200 "status от члена ramplus"
api "${reader[@]}" -- POST /v1/plan/swap '{"action": "apply", "size": "4G", "existing": "keep"}'
expect_code 200 "plan от члена ramplus"
id=$(field id)
api "${reader[@]}" -- POST /v1/apply "{\"plan_id\": \"$id\"}"
expect_code 403 "apply от члена ramplus (не ramplus-admin)"
[[ $(field code) == auth.forbidden ]] || die "код отказа: $BODY"
journalctl -u ramplusd --no-pager -n 50 | grep -q 'auth.forbidden' || die "отказ не записан в журнал демона"
assert_state "$SWAPFILE" 0 "$BEFORE"

step "E6. make uninstall — следов нет"
make -s -C "$REPO" uninstall "${PURGE[@]}"
INSTALLED=0
for f in /usr/local/bin/ramplusd /usr/local/bin/ramplus /usr/local/lib/systemd/system/ramplusd.service \
	/usr/local/lib/sysusers.d/ramplus.conf /usr/local/lib/tmpfiles.d/ramplus.conf /run/ramplus; do
	[[ ! -e $f ]] || die "после uninstall остался $f"
done
if ((!GROUPS_PREEXIST)); then
	for g in ramplus ramplus-admin; do
		if getent group "$g" >/dev/null; then
			die "после uninstall PURGE=1 осталась группа $g"
		fi
	done
fi
if pgrep -x ramplusd >/dev/null; then
	die "после uninstall работает ramplusd"
fi
"$BIN" swap check-clean || die "после uninstall остались следы"

printf '\nВСЕ СЦЕНАРИИ ПРОЙДЕНЫ\n'
