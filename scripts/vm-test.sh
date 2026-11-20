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
# Падение в сценариях E и F: перед очисткой последние 50 строк журнала
# ramplusd и ramplus-web печатаются и сохраняются в
# /var/lib/ramplus/vmtest-failed-<время>.journal.txt.
#
# Записи служб ищутся по машинному полю event= (journalctl -o cat --since
# <время перед запросом>) с повтором до 5 с: journald принимает вывод
# асинхронно. Вывод команд проверяется целиком (out_has), а не через
# `cmd | grep -q`: под pipefail это ложно падает с SIGPIPE.
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
#   F. веб: make install → ramplus init --password-stdin → ramplusd и
#      ramplus-web → страница входа; неверный пароль → отказ; 6 неверных →
#      пауза; Host: evil.example → 421; вход → status → recommendation →
#      plan/setup (рекомендация) → без CSRF отказ → apply с CSRF и
#      confirm_experimental → swap и профиль применены, одна операция setup →
#      plan rollback → apply → чисто; выход → status 401; make uninstall
#      PURGE=1 → следов нет (пользователь ramplus-web, пароль, юниты).
#      Нужен собранный ./ramplus-web (make build).
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
INSTALLED=0       # make install выполнен этим скриптом (сценарии E, F)
WEB_INIT=0        # пароль веба задан этим скриптом (сценарий F)
DAEMON_SCENARIO=0 # начались сценарии E/F (при падении сохранить журнал служб)
FAIL_TS=""
WEB=http://127.0.0.1:7781
JAR=""            # cookie веб-сессии (сценарий F)
CSRF=""
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
[[ -x $REPO/ramplus-web ]] || die "нет $REPO/ramplus-web (make build)"
command -v curl >/dev/null || die "нужен curl (сценарии E, F)"

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

# out_has PATTERN CMD... — вывод CMD (stdout, целиком) содержит PATTERN.
# Не `cmd | grep -q`: под pipefail grep выходит на первом совпадении, cmd
# получает SIGPIPE (код 141), и проверка ложно падает.
out_has() {
	local pattern=$1 out
	shift
	out=$("$@") || return 1
	grep -q -- "$pattern" <<<"$out"
}

# now_ts — метка времени для journalctl --since (снимается до запроса).
now_ts() { date '+%Y-%m-%d %H:%M:%S'; }

# journal_wait SINCE UNIT EVENT [KEY=VALUE...] — в журнале UNIT с момента
# SINCE есть запись с машинным полем event=EVENT и всеми полями KEY=VALUE
# (одна строка). journald принимает вывод службы асинхронно, поэтому повтор
# до 5 с. Ищется по полям (-o cat — строка как её пишет служба), не по тексту.
journal_wait() {
	local since=$1 unit=$2 event=$3 i out lines f
	shift 3
	for ((i = 0; i < 25; i++)); do
		out=$(journalctl -u "$unit" --since "$since" -o cat --no-pager 2>/dev/null || true)
		lines=$(grep -Fw -- "event=$event" <<<"$out" || true)
		for f in "$@"; do
			lines=$(grep -Fw -- "$f" <<<"$lines" || true)
		done
		if [[ -n $lines ]]; then
			echo "ok: журнал $unit: $(head -1 <<<"$lines")"
			return 0
		fi
		sleep 0.2
	done
	die "в журнале $unit с $since нет записи event=$event $*:\n$(journalctl -u "$unit" --since "$since" -o cat --no-pager 2>/dev/null | tail -20)"
}

# save_journal — при падении в сценариях E и F: последние 50 строк журнала
# ramplusd и ramplus-web на экран и в файл рядом с журналом теста.
save_journal() {
	local file=/var/lib/ramplus/vmtest-failed-$FAIL_TS.journal.txt out
	out=$(journalctl -u ramplusd -u ramplus-web --no-pager -n 50 2>&1 || true)
	echo
	echo "Журнал ramplusd и ramplus-web (последние 50 строк):"
	echo "$out"
	mkdir -p /var/lib/ramplus
	printf '%s\n' "$out" >"$file"
	echo "журнал служб сохранён: $file"
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
		systemctl stop ramplus-web ramplusd 2>/dev/null || true
	fi
	if ((WEB_INIT)) && ((GROUPS_PREEXIST)); then
		"$BIN" passwd --disable || true # без PURGE uninstall пароль не удаляет
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
		if ! out_has '"rollback_next": true' "$BIN" history --json; then
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
		FAIL_TS=$(date +%Y%m%dT%H%M%S)
		if ((DAEMON_SCENARIO)); then
			save_journal
		fi
		if [[ -f $STATE ]]; then
			FAILED_STATE="$STATE.vmtest-failed-$FAIL_TS"
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
if out_has '"id":' "$BIN" history --json; then
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
out_has '"managed": true' rp status --json || die "status --json: нет managed: true"
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
out_has 'по умолчанию — флаг не указан' rp swap plan --size 4G || die "plan не сообщил о выборе по умолчанию"

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
out_has 'zswap.compressor' rp profile plan zswap-lz4 || die "plan не показал zswap.compressor"
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
out_has '"name": "zswap-lz4"' rp status --json || die "status --json: нет профиля"
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
out_has '"key": "vm.swappiness"' rp status --json || die "status --json: нет дрейфа vm.swappiness"
out_has 'значения изменены вручную' rp status || die "status не предупредил о дрейфе"
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
out_has 'Нечего делать' rp profile reset --yes || die "повторный reset должен быть пустым"
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

# field NAME — первое значение "NAME": "..." в $BODY (с пробелом после
# двоеточия — ответы демона, без — компактный JSON ramplus-web).
field() { sed -n "s/.*\"$1\": *\"\([^\"]*\)\".*/\1/p" <<<"$BODY" | head -1; }

# numfield NAME — первое числовое значение "NAME": 123 ("" — null или нет).
numfield() { sed -n "s/.*\"$1\": *\([0-9][0-9]*\).*/\1/p" <<<"$BODY" | head -1; }

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
DAEMON_SCENARIO=1
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
since=$(now_ts)
api "${reader[@]}" -- POST /v1/apply "{\"plan_id\": \"$id\"}"
expect_code 403 "apply от члена ramplus (не ramplus-admin)"
[[ $(field code) == auth.forbidden ]] || die "код отказа: $BODY"
journal_wait "$since" ramplusd auth.forbidden "uid=$(id -u nobody)" need=admin method=POST path=/v1/apply
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

########################################################################
# web METHOD PATH [BODY] [HOST]: запрос к ramplus-web с cookie-сессией $JAR
# и токеном $CSRF; тело — в $BODY, код — в $CODE.
web() {
	local method=$1 path=$2 data=${3:-} host=${4:-} out rc=0
	local args=(-sS -b "$JAR" -c "$JAR" -X "$method" -w '\n%{http_code}')
	if [[ -n $data ]]; then
		args+=(-H 'Content-Type: application/json' -d "$data")
	fi
	if [[ -n $CSRF ]]; then
		args+=(-H "X-CSRF-Token: $CSRF")
	fi
	if [[ -n $host ]]; then
		args+=(-H "Host: $host")
	fi
	out=$(curl "${args[@]}" "$WEB$path") || rc=$?
	((rc == 0)) || die "curl $method $path (веб): код $rc"
	CODE=${out##*$'\n'}
	BODY=${out%$'\n'*}
}

# web_wait_op ID — ждать завершения операции через веб, status=done.
web_wait_op() {
	local i
	for ((i = 0; i < 600; i++)); do
		web GET "/api/operations/$1"
		expect_code 200 "операция $1 (веб)"
		if grep -q '"running": false' <<<"$BODY"; then
			[[ $(field status) == "done" ]] || die "операция $1 не выполнена:\n$BODY"
			return
		fi
		sleep 0.5
	done
	die "операция $1 не завершилась за 5 минут"
}

WEBPW="vm-test-$(date +%s)-пароль"
WRONGPW="wrong-password-$$"
JAR="$WORK/cookies"
: >"$JAR"

step "F1. make install, ramplus init --password-stdin, ramplusd и ramplus-web"
DAEMON_SCENARIO=1
F_START=$(now_ts)
make -s -C "$REPO" install
INSTALLED=1
out_has ramplus-admin id ramplus-web || die "ramplus-web не в группе ramplus-admin: $(id ramplus-web)"
printf '%s\n' "$WEBPW" | "$BIN" init --password-stdin
WEB_INIT=1
[[ $(stat -c '%a %U %G' /etc/ramplus/web-auth) == "640 root ramplus-web" ]] ||
	die "права /etc/ramplus/web-auth: $(stat -c '%a %U %G' /etc/ramplus/web-auth)"
if grep -qF "$WEBPW" /etc/ramplus/web-auth; then
	die "пароль лежит в файле открытым текстом"
fi
systemctl start ramplusd ramplus-web
for ((i = 0; i < 50; i++)); do
	curl -s -o /dev/null "$WEB/api/session" && break
	sleep 0.2
done
[[ $(ps -o user:32= -C ramplus-web | head -1) == ramplus-web ]] || die "ramplus-web работает не от ramplus-web: $(ps -o user:32= -C ramplus-web)"

step "F2. страница входа отдаётся, заголовки безопасности"
web GET /
expect_code 200 "главная страница"
grep -q '<div id="root">' <<<"$BODY" || die "нет фронтенда:\n$BODY"
headers=$(curl -sS -D - -o /dev/null "$WEB/")
grep -qi "^content-security-policy: default-src 'none'" <<<"$headers" || die "нет CSP:\n$headers"
grep -qi '^x-frame-options: DENY' <<<"$headers" || die "нет X-Frame-Options:\n$headers"
web GET /api/session
grep -q '"password_set":true' <<<"$BODY" || die "сессия: $BODY"

step "F3. неверный пароль → отказ"
F3_START=$(now_ts)
web POST /api/login "{\"password\": \"$WRONGPW\"}"
expect_code 401 "неверный пароль"
[[ $(field code) == auth.invalid ]] || die "код: $BODY"

step "F4. 6 неверных попыток → пауза"
for ((i = 2; i <= 5; i++)); do
	web POST /api/login "{\"password\": \"$WRONGPW\"}"
	expect_code 401 "неверный пароль #$i"
done
web POST /api/login "{\"password\": \"$WEBPW\"}"
expect_code 429 "6-я попытка — пауза (даже с верным паролем)"
[[ $(field code) == auth.rate_limited ]] || die "код: $BODY"
wait_s=$(numfield retry_after_seconds)
[[ -n $wait_s ]] || die "нет retry_after_seconds: $BODY"
journal_wait "$F3_START" ramplus-web login.failed ip=127.0.0.1
journal_wait "$F3_START" ramplus-web login.rate_limited ip=127.0.0.1
web_log=$(journalctl -u ramplus-web --since "$F_START" -o cat --no-pager)
if grep -qF -e "$WRONGPW" -e "$WEBPW" <<<"$web_log"; then
	die "пароль попал в журнал ramplus-web"
fi
echo "пауза $wait_s с"
sleep "$((wait_s + 1))"

step "F5. Host: evil.example → 421"
web GET / "" evil.example
expect_code 421 "чужой Host"
web GET /api/session "" evil.example:7781
expect_code 421 "чужой Host с портом"

step "F6. вход"
web POST /api/login "{\"password\": \"$WEBPW\"}"
expect_code 200 "вход"
CSRF=$(field csrf_token)
[[ -n $CSRF ]] || die "нет csrf_token: $BODY"
grep -qi 'HttpOnly' "$JAR" || grep -q '#HttpOnly_' "$JAR" || die "cookie без HttpOnly:\n$(cat "$JAR")"

step "F7. status и recommendation через веб"
web GET /api/status
expect_code 200 "status (веб)"
grep -q '"memory"' <<<"$BODY" || die "status без memory"
web GET /api/recommendation
expect_code 200 "recommendation"
REC_SIZE=$(numfield swap_size_bytes)
REC_EXISTING=$(field existing)
REC_PROFILE=$(field profile)
[[ -n $REC_SIZE ]] || die "рекомендации swap нет (нужно место на диске):\n$BODY"
REC_GIB=$((REC_SIZE / 1024 / 1024 / 1024))
echo "рекомендация: ${REC_GIB}G, existing=${REC_EXISTING:-null}, профиль $REC_PROFILE"
[[ $REC_PROFILE == zswap-lz4 ]] || die "ожидался профиль zswap-lz4 (zswap в ядре VM?):\n$BODY"

step "F8. plan/setup по рекомендации; без CSRF — отказ"
before
setup_body="{\"swap\": {\"size\": $REC_SIZE"
if [[ -n $REC_EXISTING ]]; then
	setup_body+=", \"existing\": \"$REC_EXISTING\""
fi
setup_body+="}, \"profile\": {\"name\": \"$REC_PROFILE\"}}"
saved_csrf=$CSRF
CSRF=""
web POST /api/plan/setup "$setup_body"
expect_code 403 "plan/setup без CSRF"
[[ $(field code) == csrf.invalid ]] || die "код: $BODY"
CSRF=$saved_csrf
web POST /api/plan/setup "$setup_body"
expect_code 200 "plan/setup"
grep -q '"blocked": false' <<<"$BODY" || die "план setup заблокирован:\n$BODY"
grep -q '"action": "setup"' <<<"$BODY" || die "не setup:\n$BODY"
plan_id=$(field id)

# История через веб: в журнале — операции всего прогона (A–E тоже), поэтому
# проверяется только операция с operation_id из /api/apply и прирост журнала.
# Ответ демона — JSON с отступом 2: операции — объекты на уровне 4 пробелов,
# их поля — 6.

# history_ids — ID операций из $BODY (GET /api/history), по одному в строке.
history_ids() { sed -n 's/^      "id": "\([^"]*\)".*/\1/p' <<<"$BODY"; }

# history_op ID — JSON-блок операции ID из $BODY.
history_op() {
	awk -v want="      \"id\": \"$1\"," '
		/^    \{/ { blk = ""; inop = 1 }
		inop { blk = blk $0 "\n" }
		/^    \}/ && inop { inop = 0; if (index(blk, want)) printf "%s", blk }
	' <<<"$BODY"
}

# history_snapshot — запомнить ID операций журнала (до apply).
history_snapshot() {
	web GET /api/history
	expect_code 200 "history (веб)"
	history_ids >"$WORK/history.before"
}

# history_expect_one OPID KEY=VALUE... — после apply в журнале ровно одна
# новая операция, это OPID, и в её JSON есть все "KEY": "VALUE". При
# падении печатаются только эта операция и новые записи.
history_expect_one() {
	local opid=$1 kv key val block new n_before n_after
	shift
	web GET /api/history
	expect_code 200 "history (веб)"
	block=$(history_op "$opid")
	new=$(history_ids | grep -vxF -f "$WORK/history.before" || true)
	n_before=$(wc -l <"$WORK/history.before")
	n_after=$(history_ids | wc -l)
	[[ -n $block ]] || die "операции $opid нет в журнале; новые операции:\n${new:-(нет)}"
	if ((n_after != n_before + 1)) || [[ $new != "$opid" ]]; then
		die "журнал должен вырасти ровно на одну операцию $opid: было $n_before, стало $n_after; новые:\n${new:-(нет)}\nоперация $opid:\n$block"
	fi
	for kv in "$@"; do
		key=${kv%%=*} val=${kv#*=}
		grep -qF "\"$key\": \"$val\"" <<<"$block" || die "операция $opid: нет \"$key\": \"$val\":\n$block"
	done
	echo "ok: в журнале одна новая операция $opid ($*)"
}

# history_expect_field OPID KEY=VALUE — у операции OPID в последнем ответе
# истории есть "KEY": "VALUE".
history_expect_field() {
	local block key=${2%%=*} val=${2#*=}
	block=$(history_op "$1")
	grep -qF "\"$key\": \"$val\"" <<<"$block" || die "операция $1: нет \"$key\": \"$val\":\n${block:-(нет в журнале)}"
	echo "ok: операция $1: $key=$val"
}

step "F9. apply с CSRF и confirm_experimental → swap и профиль применены, одна операция setup"
history_snapshot
web POST /api/apply "{\"plan_id\": \"$plan_id\", \"confirm_experimental\": true}"
expect_code 202 "apply (веб)"
SETUP_OP=$(field operation_id)
[[ -n $SETUP_OP ]] || die "нет operation_id: $BODY"
web_wait_op "$SETUP_OP"
expected=$BEFORE
if [[ $REC_EXISTING == replace ]]; then
	expected=$(awk '$1 ~ /^\/dev\/zram/' <<<"$BEFORE") # заменён весь swap, кроме zram
fi
assert_state "$SWAPFILE" "$REC_GIB" "$expected"
assert_param /sys/module/zswap/parameters/enabled Y
assert_param /sys/module/zswap/parameters/compressor lz4
history_expect_one "$SETUP_OP" action=setup status=committed source=daemon via=web \
	user=ramplus-web "title=Запас $REC_GIB GB из SSD + режим «Сжатие — быстрое»"

step "F10. plan rollback → apply → чисто; setup отменён одной операцией"
web POST /api/plan/rollback "{}"
expect_code 200 "plan rollback (веб)"
grep -q '"domain": "setup"' <<<"$BODY" || die "rollback отменяет не setup:\n$BODY"
grep -q "\"operation_id\": \"$SETUP_OP\"" <<<"$BODY" || die "rollback отменяет не $SETUP_OP:\n$(grep -A8 '"rollback"' <<<"$BODY")"
plan_id=$(field id)
history_snapshot
web POST /api/apply "{\"plan_id\": \"$plan_id\"}"
expect_code 202 "apply rollback (веб)"
ROLLBACK_OP=$(field operation_id)
[[ -n $ROLLBACK_OP ]] || die "нет operation_id: $BODY"
web_wait_op "$ROLLBACK_OP"
assert_state "$SWAPFILE" 0 "$BEFORE"
assert_eq "$(params)" "$INITIAL_PARAMS" "параметры zswap/vm.* как в начале"
"$BIN" swap check-clean || die "после rollback через веб остались следы"
history_expect_one "$ROLLBACK_OP" action=rollback status=committed "target=$SETUP_OP" source=daemon via=web
history_expect_field "$SETUP_OP" status=rolled_back

step "F11. выход → status 401"
web POST /api/logout "{}"
expect_code 204 "выход"
web GET /api/status
expect_code 401 "status после выхода"

step "F12. make uninstall PURGE=1 — следов нет"
if ((GROUPS_PREEXIST)); then
	"$BIN" passwd --disable # группы были до теста: PURGE нельзя, пароль убираем сами
fi
make -s -C "$REPO" uninstall "${PURGE[@]}"
INSTALLED=0
WEB_INIT=0
for f in /usr/local/bin/ramplus-web /usr/local/lib/systemd/system/ramplus-web.service /etc/ramplus/web-auth /run/ramplus; do
	[[ ! -e $f ]] || die "после uninstall остался $f"
done
if ((!GROUPS_PREEXIST)); then
	if getent passwd ramplus-web >/dev/null; then
		die "после uninstall PURGE=1 остался пользователь ramplus-web"
	fi
	for g in ramplus ramplus-admin ramplus-web; do
		if getent group "$g" >/dev/null; then
			die "после uninstall PURGE=1 осталась группа $g"
		fi
	done
fi
if pgrep -x ramplus-web >/dev/null || pgrep -x ramplusd >/dev/null; then
	die "после uninstall работает ramplus-web или ramplusd"
fi
if curl -s -o /dev/null "$WEB/"; then
	die "после uninstall порт 7781 отвечает"
fi
"$BIN" swap check-clean || die "после uninstall остались следы"

printf '\nВСЕ СЦЕНАРИИ ПРОЙДЕНЫ\n'
