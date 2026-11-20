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
#   # + scenario G (the .deb package; Debian/Ubuntu with apt):
#   make deb
#   sudo RAMPLUS_VM_TEST=1 scripts/vm-test.sh ./ramplus dist/ramplus_*_amd64.deb
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
#   G. пакет .deb (только если передан второй аргумент): apt install ./…deb →
#      службы не запущены и не включены → ramplus init --password-stdin →
#      systemctl enable --now → ramplus daemon status → setup из рекомендации
#      (ramplus recommend --json) → apt remove отказывает (RamPlus управляет
#      swap), пакет и службы на месте → rollback → apt remove проходит,
#      службы остановлены и отключены → apt purge: нет /etc/ramplus и
#      state.json (журнал чистый); пользователи и группы остаются
#      (политика Debian) — тест удаляет их сам, если создал.
set -euo pipefail
# Вывод CLI — на английском (--lang en в rp и локаль C для прямых вызовов):
# скрипт ищет английские строки, а где можно — проверяет JSON и коды.
export LC_ALL=C.UTF-8

BIN=${1:-./ramplus}
BIN=$(readlink -f "$BIN")
DEB=${2:-}        # .deb для сценария G ("" — G пропускается)
if [[ -n $DEB ]]; then
	DEB=$(readlink -f "$DEB")
fi
DEB_INSTALLED=0   # пакет ramplus установлен этим скриптом (сценарий G)
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

[[ ${RAMPLUS_VM_TEST:-} == 1 ]] || die "this test is destructive: run it in a VM with RAMPLUS_VM_TEST=1"
[[ $EUID -eq 0 ]] || die "root is required"
if command -v systemd-detect-virt >/dev/null; then
	virt=$(systemd-detect-virt || true)
	[[ $virt != none ]] || die "this does not look like a VM (systemd-detect-virt: none)"
fi
[[ -x $BIN ]] || die "no binary $BIN (make build)"
[[ -x $REPO/ramplusd ]] || die "no $REPO/ramplusd (make build)"
[[ -x $REPO/ramplus-web ]] || die "no $REPO/ramplus-web (make build)"
command -v curl >/dev/null || die "curl is required (scenarios E, F)"
if [[ -n $DEB ]]; then
	[[ -f $DEB ]] || die "no package $DEB (make deb)"
	command -v apt-get >/dev/null || die "scenario G needs apt (Debian/Ubuntu)"
	if dpkg-query -W -f='${Status}' ramplus 2>/dev/null | grep -q installed; then
		die "the ramplus package is already installed: remove it before the test"
	fi
fi

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
	[[ -n $size ]] || die "$path is not active; swapon --show:\n$(swaps)"
	[[ $size =~ ^[0-9]+$ ]] || die "$path: swapon returned a non-number: '$size'"
	want=$((gib * 1024 * 1024 * 1024))
	# swapon показывает размер без заголовка (одна страница)
	((size > want - 1048576 && size <= want)) || die "$path: size in swapon $size, expected ~$want"
	fsize=$(stat -c %s "$path")
	[[ $fsize == "$want" ]] || die "$path: file size $fsize, expected $want"
	echo "ok: $path active, $(numfmt --to=iec "$size") (file $(numfmt --to=iec "$fsize"))"
}

assert_no_swap() {
	if is_active "${1:-}"; then
		die "${1:-} is still active"
	fi
	echo "ok: ${1:-} is not active"
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
		[[ ! -e $path ]] || die "$path is still on disk"
	fi
	assert_eq "$(names_sizes | awk -v p="$path" '$1 != p')" "$base" "the other swap did not change"
}

assert_eq() {
	[[ ${1:-} == "${2:-}" ]] || die "${3:-}:\nexpected:\n${2:-}\ngot:\n${1:-}"
	echo "ok: ${3:-}"
}

# assert_same_file A B ЧТО — файлы совпадают байт в байт
assert_same_file() {
	cmp -s "${1:-}" "${2:-}" || die "${3:-}: files differ:\n$(diff -u "${1:-}" "${2:-}" || true)"
	echo "ok: ${3:-} (byte for byte)"
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
	"$BIN" --lang en "$@"
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
			echo "ok: journal $unit: $(head -1 <<<"$lines")"
			return 0
		fi
		sleep 0.2
	done
	die "journal $unit has no record event=$event $* since $since:\n$(journalctl -u "$unit" --since "$since" -o cat --no-pager 2>/dev/null | tail -20)"
}

# save_journal — при падении в сценариях E и F: последние 50 строк журнала
# ramplusd и ramplus-web на экран и в файл рядом с журналом теста.
save_journal() {
	local file=/var/lib/ramplus/vmtest-failed-$FAIL_TS.journal.txt out
	out=$(journalctl -u ramplusd -u ramplus-web --no-pager -n 50 2>&1 || true)
	echo
	echo "ramplusd and ramplus-web journal (last 50 lines):"
	echo "$out"
	mkdir -p /var/lib/ramplus
	printf '%s\n' "$out" >"$file"
	echo "service journal saved: $file"
}

# system_clean — RamPlus ничего не держит (check-clean: нет managed swap,
# активного профиля, незавершённых операций, файлов и юнитов RamPlus) и
# параметры zswap/vm.* равны снятым в начале теста.
system_clean() {
	"$BIN" swap check-clean >/dev/null 2>&1 || return 1
	[[ -z $INITIAL_PARAMS || $(params) == "$INITIAL_PARAMS" ]]
}

# remove_deb_accounts — после apt purge: пакет оставляет пользователей и
# группы (политика Debian); тест удаляет их, если до теста их не было.
remove_deb_accounts() {
	((GROUPS_PREEXIST)) && return 0
	if getent passwd ramplus-web >/dev/null; then
		userdel ramplus-web || true
	fi
	local g
	for g in ramplus-web ramplus-admin ramplus; do
		if getent group "$g" >/dev/null; then
			groupdel "$g" || true
		fi
	done
}

# restore_after_failure — вернуть VM в исходное состояние после падения в
# любом сценарии: демон остановить, ручные изменения вернуть, операции теста
# откатывать через CLI по одной, пока система не чистая (журнал теста
# содержит только их; перед каждым откатом — system_clean), демон удалить.
restore_after_failure() {
	echo
	echo "=== Restoring the VM to its initial state"
	if ((INSTALLED || DEB_INSTALLED)); then
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
			echo "the system is in its initial state — no rollbacks needed"
			break
		fi
		if ! out_has '"rollback_next": true' "$BIN" history --json; then
			echo "nothing to roll back, but the system is not in its initial state — investigate manually"
			break
		fi
		if ! "$BIN" rollback --yes; then
			echo "rollback refused — investigate the rest manually"
			break
		fi
	done
	if ((DEB_INSTALLED)); then
		if ! apt-get purge -y -qq ramplus; then
			echo "apt purge refused (RamPlus still manages the system) — forcing, leaving traces for investigation"
			RAMPLUS_FORCE_REMOVE=1 apt-get purge -y -qq ramplus || true
		fi
		remove_deb_accounts
		DEB_INSTALLED=0
	fi
	if ((INSTALLED)); then
		if ! make -s -C "$REPO" uninstall "${PURGE[@]}"; then
			echo "uninstall refused (RamPlus still manages the system) — removing the daemon, leaving traces for investigation"
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
		echo "The test failed. State for investigation:"
		swapon --show || true
		FAIL_TS=$(date +%Y%m%dT%H%M%S)
		if ((DAEMON_SCENARIO)); then
			save_journal
		fi
		if [[ -f $STATE ]]; then
			FAILED_STATE="$STATE.vmtest-failed-$FAIL_TS"
			cp -p "$STATE" "$FAILED_STATE"
			echo "test journal saved: $FAILED_STATE"
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
			echo "the VM is in its initial state (check-clean)"
		else
			echo "WARNING: the VM did not return to its initial state:"
			"$BIN" swap check-clean || true
		fi
	fi
	if [[ -n $SAVED_STATE ]]; then
		if ((clean)) || ((!STARTED)); then
			mkdir -p "$(dirname "$STATE")" # apt purge (G) удаляет чистый /var/lib/ramplus
			mv -f "$SAVED_STATE" "$STATE"
			echo "the journal of previous operations is back in place: $STATE"
		else
			# Журнал теста описывает то, что осталось в системе, — он нужен для
			# разбора и rollback; старый не подменяем.
			echo "the journal of previous operations stays in $SAVED_STATE (restore it manually after investigation)"
		fi
	fi
	rm -rf "$WORK"
	exit "$rc"
}
trap cleanup EXIT

# Чистый старт по фактическому состоянию: нет управляемого swap, активного
# swap RamPlus, юнитов, незавершённых операций. Журнал с историей — не помеха.
if ! clean_report=$("$BIN" swap check-clean 2>&1); then
	die "the VM is not clean:\n$clean_report"
fi
echo "$clean_report"

# Изоляция от прошлых прогонов: незавершённых следов нет, но в журнале
# могут остаться неотменённые операции (например, profile reset), и
# «лишний rollback» дошёл бы до них. Журнал откладывается на время теста.
if out_has '"id":' "$BIN" history --json; then
	SAVED_STATE="$STATE.vmtest-$(date +%Y%m%dT%H%M%S)"
	mv "$STATE" "$SAVED_STATE"
	echo "the journal has operations from previous runs: $STATE moved to $SAVED_STATE,"
	echo "the test starts with an empty journal (it is restored at the end)"
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
step "A1. plan without root changes nothing"
if id nobody >/dev/null 2>&1; then
	# Копия в /tmp: у nobody может не быть доступа к каталогу сборки.
	install -m 755 "$BIN" "$WORK/ramplus"
	chmod 755 "$WORK"
	setpriv --reuid=nobody --regid="$(id -g nobody)" --clear-groups "$WORK/ramplus" swap plan --size 4G >/dev/null
fi
assert_eq "$(swaps)" "$A_START_SWAPS" "plan did not change swap"

step "A2. apply 4G"
before
rp swap apply --size 4G --existing=keep --yes
assert_state "$SWAPFILE" 4 "$BEFORE"
[[ $(stat -c %a "$SWAPFILE") == 600 ]] || die "$SWAPFILE mode is not 600"
systemctl is-enabled "$UNIT" >/dev/null || die "$UNIT is not enabled"
if grep -q ramplus /etc/fstab; then
	die "fstab must not change for our swap"
fi

step "A3. status shows managed"
out_has '"managed": true' rp status --json || die "status --json: no managed: true"
rp status | sed -n '/Swap devices/,/^$/p'

step "A4. the systemd unit brings swap up by itself (as after a reboot)"
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
[[ ! -e $SWAPFILE && ! -e /etc/systemd/system/$UNIT ]] || die "remove left files"

step "A7. rollback (undo remove → 6G)"
before
rp rollback --yes
assert_state "$SWAPFILE" 6 "$BEFORE"
systemctl is-enabled "$UNIT" >/dev/null || die "$UNIT was not restored"

step "A8. rollback (undo resize → 4G), rollback (undo apply)"
before
rp rollback --yes
assert_state "$SWAPFILE" 4 "$BEFORE"
before
rp rollback --yes
assert_state "$SWAPFILE" 0 "$BEFORE"
assert_eq "$(swaps)" "$A_START_SWAPS" "swap as at the start of scenario A"
assert_same_file "$FSTAB_ORIG" /etc/fstab "fstab as at the start"
"$BIN" swap check-clean || die "traces remain after a full rollback"
if rp rollback --yes; then
	die "an extra rollback must be refused"
fi

########################################################################
step "B0. existing swap"
OLD=$(swaps | awk '$1 !~ /^\/dev\/zram/ {print $1; exit}')
if [[ -z $OLD ]]; then
	echo "no swap — creating /swap.img 2G and an fstab line, as on Ubuntu desktop"
	[[ ! -e /swap.img ]] || die "/swap.img exists but is not active — investigate manually"
	CREATED_OLD=1
	fallocate -l 2G /swap.img
	chmod 600 /swap.img
	mkswap /swap.img >/dev/null
	swapon /swap.img
	printf '/swap.img\tnone\tswap\tsw\t0\t0\n' >>/etc/fstab
	OLD=/swap.img
fi
echo "existing swap: $OLD"
cp -p /etc/fstab "$WORK/fstab.before-replace"

step "B1. plan without --existing: keep by default, stated explicitly"
out_has 'default — flag not given' rp swap plan --size 4G || die "plan did not report the default choice"

step "B2. apply --existing=replace"
before
REPLACE_BEFORE=$BEFORE # rollback replace возвращает ровно это
rp swap apply --size 4G --existing=replace --yes
assert_no_swap "$OLD"
assert_state "$SWAPFILE" 4 "$(without "$BEFORE" "$OLD")"
if grep -q "$OLD" "$WORK/fstab.before-replace"; then
	grep -q "^# disabled by ramplus: $OLD" /etc/fstab || die "line $OLD is not commented out"
	compgen -G '/var/lib/ramplus/backups/*etc_fstab' >/dev/null || die "no fstab backup"
fi
[[ -e $OLD ]] || die "$OLD was deleted without --delete-old"

step "B3. rollback: the old swap is active, fstab byte for byte"
rp rollback --yes
assert_state "$SWAPFILE" 0 "$REPLACE_BEFORE"
assert_same_file "$WORK/fstab.before-replace" /etc/fstab "fstab as before replace"

########################################################################
if [[ -f $OLD ]]; then
	OLD_SIZE=$(stat -c %s "$OLD")

	step "C1. apply --existing=replace --delete-old"
	before
	REPLACE_BEFORE=$BEFORE
	rp swap apply --size 4G --existing=replace --delete-old --yes
	[[ ! -e $OLD ]] || die "$OLD was not deleted"
	assert_state "$SWAPFILE" 4 "$(without "$BEFORE" "$OLD")"

	step "C2. rollback recreates $OLD"
	rp rollback --yes
	[[ $(stat -c %s "$OLD") == "$OLD_SIZE" ]] || die "$OLD was recreated with a different size"
	assert_state "$SWAPFILE" 0 "$REPLACE_BEFORE"
	assert_same_file "$WORK/fstab.before-replace" /etc/fstab "fstab as before replace"
else
	step "C. skipped: $OLD is a partition, not a file"
fi

########################################################################
assert_param() {
	local f=${1:-} want=${2:-} got=""
	got=$(<"$f")
	[[ $got == "$want" ]] || die "$f = $got, expected $want"
	echo "ok: $f = $want"
}

assert_no_profile_files() {
	local f
	for f in "${PROFILE_FILES[@]}"; do
		[[ ! -e $f ]] || die "$f remains"
	done
	echo "ok: no profile files"
}

step "D0. initial values (captured at the start of the test)"
echo "$INITIAL_PARAMS"
assert_eq "$(params)" "$INITIAL_PARAMS" "scenarios A–C did not change the parameters"
assert_no_profile_files

step "D1. plan without root changes nothing"
out_has 'zswap.compressor' rp profile plan zswap-lz4 || die "plan did not show zswap.compressor"
assert_eq "$(params)" "$INITIAL_PARAMS" "plan did not change the values"

step "D2. apply without confirming the experimental profile — refused"
if rp profile apply zswap-lz4 --yes; then
	die "the experimental profile was applied without --experimental"
fi
assert_eq "$(params)" "$INITIAL_PARAMS" "the values did not change"

step "D3. apply --experimental"
rp profile apply zswap-lz4 --experimental --yes
assert_param "$ZP/enabled" Y
assert_param "$ZP/compressor" lz4
assert_param "$ZP/max_pool_percent" 20
if [[ -e $ZP/zpool ]]; then
	assert_param "$ZP/zpool" zsmalloc
fi
grep -q '# Managed by RamPlus' /etc/sysctl.d/99-ramplus.conf || die "no marker in 99-ramplus.conf"
systemctl is-enabled ramplus-zswap.service >/dev/null || die "ramplus-zswap.service is not enabled"

step "D4. status: the profile is active"
out_has '"name": "zswap-lz4"' rp status --json || die "status --json: no profile"
rp status | sed -n '/RamPlus profile/,/^$/p'

step "D5. «reboot»: values spoiled by hand, the unit restores them"
DIRTY_ZSWAP=1
echo N >"$ZP/enabled"
echo lzo >"$ZP/compressor"
systemctl restart ramplus-zswap.service
DIRTY_ZSWAP=0
assert_param "$ZP/enabled" Y
assert_param "$ZP/compressor" lz4

step "D6. drift: a manual swappiness write → status warns"
SWAPPINESS=$(</proc/sys/vm/swappiness)
DIRTY_SWAPPINESS=$SWAPPINESS
echo 133 >/proc/sys/vm/swappiness
out_has '"key": "vm.swappiness"' rp status --json || die "status --json: no vm.swappiness drift"
out_has 'values were changed by hand' rp status || die "status did not warn about the drift"
echo "$SWAPPINESS" >/proc/sys/vm/swappiness
DIRTY_SWAPPINESS=""

step "D7. rollback: values as at the start, no files"
rp rollback --yes
assert_eq "$(params)" "$INITIAL_PARAMS" "values as at the start"
assert_no_profile_files

step "D8. apply + default --set → reset: values as at the start, no files"
rp profile apply zswap-lz4 --experimental --yes
rp profile apply default --set vm.swappiness=80 --yes
assert_param /proc/sys/vm/swappiness 80
rp profile reset --yes
assert_eq "$(params)" "$INITIAL_PARAMS" "values as at the start"
assert_no_profile_files
out_has 'Nothing to do' rp profile reset --yes || die "a repeated reset must be empty"
"$BIN" swap check-clean || die "traces remain after reset"

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
	((rc == 0)) || die "curl $method $path: code $rc (could not connect or send the request)"
	CODE=${out##*$'\n'}
	BODY=${out%$'\n'*}
}

# field NAME — первое значение "NAME": "..." в $BODY (с пробелом после
# двоеточия — ответы демона, без — компактный JSON ramplus-web).
field() { sed -n "s/.*\"$1\": *\"\([^\"]*\)\".*/\1/p" <<<"$BODY" | head -1; }

# numfield NAME — первое числовое значение "NAME": 123 ("" — null или нет).
numfield() { sed -n "s/.*\"$1\": *\([0-9][0-9]*\).*/\1/p" <<<"$BODY" | head -1; }

expect_code() {
	[[ $CODE == "$1" ]] || die "$2: expected HTTP $1, got $CODE:\n$BODY"
}

# wait_op ID — ждать завершения операции демона, проверить status=done.
wait_op() {
	local i
	for ((i = 0; i < 600; i++)); do
		api -- GET "/v1/operations/$1"
		expect_code 200 "operation $1"
		if grep -q '"running": false' <<<"$BODY"; then
			[[ $(field status) == "done" ]] || die "operation $1 did not complete:\n$BODY"
			return
		fi
		sleep 0.5
	done
	die "operation $1 did not finish within 5 minutes"
}

# apply_plan — POST /v1/apply по plan_id из $BODY, ждать завершения.
apply_plan() {
	local id
	id=$(field id)
	[[ -n $id ]] || die "no plan id in the response:\n$BODY"
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
[[ -S $SOCK ]] || die "the daemon did not create $SOCK:\n$(journalctl -u ramplusd -n 30 --no-pager)"
[[ $(stat -c '%a %U %G' "$SOCK") == "660 root ramplus" ]] || die "socket permissions: $(stat -c '%a %U %G' "$SOCK")"
rp daemon status

step "E2. status via the API"
api -- GET /v1/status
expect_code 200 "status"
grep -q '"memory"' <<<"$BODY" || die "status without memory:\n$BODY"

step "E3. plan swap 4G → apply → swap 4G"
before
api -- POST /v1/plan/swap '{"action": "apply", "size": "4G", "existing": "keep"}'
expect_code 200 "plan swap"
apply_plan
assert_state "$SWAPFILE" 4 "$BEFORE"

step "E4. plan rollback → apply → clean"
before
api -- POST /v1/plan/rollback
expect_code 200 "plan rollback"
grep -q '"domain": "swap"' <<<"$BODY" || die "rollback does not undo swap:\n$BODY"
apply_plan
assert_state "$SWAPFILE" 0 "$BEFORE"
"$BIN" swap check-clean || die "traces remain after a rollback via the daemon"

step "E5. permissions"
before
NOGROUP=$(id -gn nobody)
nobody=(setpriv --reuid=nobody --regid="$NOGROUP" --clear-groups)
reader=(setpriv --reuid=nobody --regid="$NOGROUP" --groups=ramplus)
# Без групп nobody не проходит права каталога/сокета (0750/0660 root:ramplus):
# до HTTP дело не доходит (403 auth.forbidden за SO_PEERCRED — в go test).
# curl любую ошибку подключения к unix-сокету сообщает кодом 7 и текстом не
# различает причину, поэтому причина доказывается отдельно: сокет есть,
# права каталога те, nobody не может в него войти, а член ramplus — может.
[[ -S $SOCK ]] || die "no socket $SOCK"
[[ $(stat -c '%a %U %G' "$(dirname "$SOCK")") == "750 root ramplus" ]] ||
	die "permissions of $(dirname "$SOCK"): $(stat -c '%a %U %G' "$(dirname "$SOCK")")"
rc=0
"${nobody[@]}" curl -sS --unix-socket "$SOCK" http://localhost/v1/status >/dev/null 2>&1 || rc=$?
[[ $rc == 7 ]] || die "nobody: curl must fail to connect (code 7), got code $rc"
if "${nobody[@]}" test -x "$(dirname "$SOCK")"; then
	die "nobody without groups can enter $(dirname "$SOCK") — the refusal is not caused by permissions"
fi
if "${nobody[@]}" stat "$SOCK" >/dev/null 2>&1; then
	die "nobody without groups can see $SOCK — the refusal is not caused by permissions"
fi
"${reader[@]}" test -x "$(dirname "$SOCK")" || die "a ramplus member cannot enter $(dirname "$SOCK")"
"${reader[@]}" test -w "$SOCK" || die "a ramplus member cannot write to $SOCK"
echo "ok: nobody without groups cannot reach the socket because of permissions (curl code 7, directory closed), a ramplus member can"
api "${reader[@]}" -- GET /v1/status
expect_code 200 "status from a ramplus member"
api "${reader[@]}" -- POST /v1/plan/swap '{"action": "apply", "size": "4G", "existing": "keep"}'
expect_code 200 "plan from a ramplus member"
id=$(field id)
since=$(now_ts)
api "${reader[@]}" -- POST /v1/apply "{\"plan_id\": \"$id\"}"
expect_code 403 "apply from a ramplus member (not ramplus-admin)"
[[ $(field code) == auth.forbidden ]] || die "refusal code: $BODY"
journal_wait "$since" ramplusd auth.forbidden "uid=$(id -u nobody)" need=admin method=POST path=/v1/apply
assert_state "$SWAPFILE" 0 "$BEFORE"

step "E6. make uninstall — no traces"
make -s -C "$REPO" uninstall "${PURGE[@]}"
INSTALLED=0
for f in /usr/local/bin/ramplusd /usr/local/bin/ramplus /usr/local/lib/systemd/system/ramplusd.service \
	/usr/local/lib/sysusers.d/ramplus.conf /usr/local/lib/tmpfiles.d/ramplus.conf /run/ramplus; do
	[[ ! -e $f ]] || die "$f remains after uninstall"
done
if ((!GROUPS_PREEXIST)); then
	for g in ramplus ramplus-admin; do
		if getent group "$g" >/dev/null; then
			die "group $g remains after uninstall PURGE=1"
		fi
	done
fi
if pgrep -x ramplusd >/dev/null; then
	die "ramplusd runs after uninstall"
fi
"$BIN" swap check-clean || die "traces remain after uninstall"

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
	((rc == 0)) || die "curl $method $path (web): code $rc"
	CODE=${out##*$'\n'}
	BODY=${out%$'\n'*}
}

# web_wait_op ID — ждать завершения операции через веб, status=done.
web_wait_op() {
	local i
	for ((i = 0; i < 600; i++)); do
		web GET "/api/operations/$1"
		expect_code 200 "operation $1 (web)"
		if grep -q '"running": false' <<<"$BODY"; then
			[[ $(field status) == "done" ]] || die "operation $1 did not complete:\n$BODY"
			return
		fi
		sleep 0.5
	done
	die "operation $1 did not finish within 5 minutes"
}

WEBPW="vm-test-$(date +%s)-пароль"
WRONGPW="wrong-password-$$"
JAR="$WORK/cookies"
: >"$JAR"

step "F1. make install, ramplus init --password-stdin, ramplusd and ramplus-web"
DAEMON_SCENARIO=1
F_START=$(now_ts)
make -s -C "$REPO" install
INSTALLED=1
out_has ramplus-admin id ramplus-web || die "ramplus-web is not in the ramplus-admin group: $(id ramplus-web)"
printf '%s\n' "$WEBPW" | "$BIN" init --password-stdin
WEB_INIT=1
[[ $(stat -c '%a %U %G' /etc/ramplus/web-auth) == "640 root ramplus-web" ]] ||
	die "permissions of /etc/ramplus/web-auth: $(stat -c '%a %U %G' /etc/ramplus/web-auth)"
if grep -qF "$WEBPW" /etc/ramplus/web-auth; then
	die "the password is stored in plain text"
fi
systemctl start ramplusd ramplus-web
for ((i = 0; i < 50; i++)); do
	curl -s -o /dev/null "$WEB/api/session" && break
	sleep 0.2
done
[[ $(ps -o user:32= -C ramplus-web | head -1) == ramplus-web ]] || die "ramplus-web does not run as ramplus-web: $(ps -o user:32= -C ramplus-web)"

step "F2. the login page is served, security headers"
web GET /
expect_code 200 "main page"
grep -q '<div id="root">' <<<"$BODY" || die "no frontend:\n$BODY"
headers=$(curl -sS -D - -o /dev/null "$WEB/")
grep -qi "^content-security-policy: default-src 'none'" <<<"$headers" || die "no CSP:\n$headers"
grep -qi '^x-frame-options: DENY' <<<"$headers" || die "no X-Frame-Options:\n$headers"
web GET /api/session
grep -q '"password_set":true' <<<"$BODY" || die "session: $BODY"

step "F3. wrong password → refused"
F3_START=$(now_ts)
web POST /api/login "{\"password\": \"$WRONGPW\"}"
expect_code 401 "wrong password"
[[ $(field code) == auth.invalid ]] || die "code: $BODY"

step "F4. 6 wrong attempts → pause"
for ((i = 2; i <= 5; i++)); do
	web POST /api/login "{\"password\": \"$WRONGPW\"}"
	expect_code 401 "wrong password #$i"
done
web POST /api/login "{\"password\": \"$WEBPW\"}"
expect_code 429 "6th attempt — pause (even with the right password)"
[[ $(field code) == auth.rate_limited ]] || die "code: $BODY"
wait_s=$(numfield retry_after_seconds)
[[ -n $wait_s ]] || die "no retry_after_seconds: $BODY"
journal_wait "$F3_START" ramplus-web login.failed ip=127.0.0.1
journal_wait "$F3_START" ramplus-web login.rate_limited ip=127.0.0.1
web_log=$(journalctl -u ramplus-web --since "$F_START" -o cat --no-pager)
if grep -qF -e "$WRONGPW" -e "$WEBPW" <<<"$web_log"; then
	die "the password ended up in the ramplus-web journal"
fi
echo "pause $wait_s s"
sleep "$((wait_s + 1))"

step "F5. Host: evil.example → 421"
web GET / "" evil.example
expect_code 421 "foreign Host"
web GET /api/session "" evil.example:7781
expect_code 421 "foreign Host with a port"

step "F6. login"
web POST /api/login "{\"password\": \"$WEBPW\"}"
expect_code 200 "login"
CSRF=$(field csrf_token)
[[ -n $CSRF ]] || die "no csrf_token: $BODY"
grep -qi 'HttpOnly' "$JAR" || grep -q '#HttpOnly_' "$JAR" || die "cookie without HttpOnly:\n$(cat "$JAR")"

step "F7. status and recommendation via the web"
web GET /api/status
expect_code 200 "status (web)"
grep -q '"memory"' <<<"$BODY" || die "status without memory"
web GET /api/recommendation
expect_code 200 "recommendation"
REC_SIZE=$(numfield swap_size_bytes)
REC_EXISTING=$(field existing)
REC_PROFILE=$(field profile)
[[ -n $REC_SIZE ]] || die "no swap recommendation (disk space needed):\n$BODY"
REC_GIB=$((REC_SIZE / 1024 / 1024 / 1024))
echo "recommendation: ${REC_GIB}G, existing=${REC_EXISTING:-null}, profile $REC_PROFILE"
[[ $REC_PROFILE == zswap-lz4 ]] || die "expected profile zswap-lz4 (zswap in the VM kernel?):\n$BODY"

step "F8. plan/setup from the recommendation; without CSRF — refused"
before
setup_body="{\"swap\": {\"size\": $REC_SIZE"
if [[ -n $REC_EXISTING ]]; then
	setup_body+=", \"existing\": \"$REC_EXISTING\""
fi
setup_body+="}, \"profile\": {\"name\": \"$REC_PROFILE\"}}"
saved_csrf=$CSRF
CSRF=""
web POST /api/plan/setup "$setup_body"
expect_code 403 "plan/setup without CSRF"
[[ $(field code) == csrf.invalid ]] || die "code: $BODY"
CSRF=$saved_csrf
web POST /api/plan/setup "$setup_body"
expect_code 200 "plan/setup"
grep -q '"blocked": false' <<<"$BODY" || die "the setup plan is blocked:\n$BODY"
grep -q '"action": "setup"' <<<"$BODY" || die "not setup:\n$BODY"
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
	expect_code 200 "history (web)"
	history_ids >"$WORK/history.before"
}

# history_expect_one OPID KEY=VALUE... — после apply в журнале ровно одна
# новая операция, это OPID, и в её JSON есть все "KEY": "VALUE". При
# падении печатаются только эта операция и новые записи.
history_expect_one() {
	local opid=$1 kv key val block new n_before n_after
	shift
	web GET /api/history
	expect_code 200 "history (web)"
	block=$(history_op "$opid")
	new=$(history_ids | grep -vxF -f "$WORK/history.before" || true)
	n_before=$(wc -l <"$WORK/history.before")
	n_after=$(history_ids | wc -l)
	[[ -n $block ]] || die "operation $opid is not in the journal; new operations:\n${new:-(none)}"
	if ((n_after != n_before + 1)) || [[ $new != "$opid" ]]; then
		die "the journal must grow by exactly one operation $opid: was $n_before, now $n_after; new:\n${new:-(none)}\noperation $opid:\n$block"
	fi
	for kv in "$@"; do
		key=${kv%%=*} val=${kv#*=}
		grep -qF "\"$key\": \"$val\"" <<<"$block" || die "operation $opid: no \"$key\": \"$val\":\n$block"
	done
	echo "ok: one new operation in the journal $opid ($*)"
}

# history_expect_field OPID KEY=VALUE — у операции OPID в последнем ответе
# истории есть "KEY": "VALUE".
history_expect_field() {
	local block key=${2%%=*} val=${2#*=}
	block=$(history_op "$1")
	grep -qF "\"$key\": \"$val\"" <<<"$block" || die "operation $1: no \"$key\": \"$val\":\n${block:-(not in the journal)}"
	echo "ok: operation $1: $key=$val"
}

step "F9. apply with CSRF and confirm_experimental → swap and profile applied, one setup operation"
history_snapshot
web POST /api/apply "{\"plan_id\": \"$plan_id\", \"confirm_experimental\": true}"
expect_code 202 "apply (web)"
SETUP_OP=$(field operation_id)
[[ -n $SETUP_OP ]] || die "no operation_id: $BODY"
web_wait_op "$SETUP_OP"
expected=$BEFORE
if [[ $REC_EXISTING == replace ]]; then
	expected=$(awk '$1 ~ /^\/dev\/zram/' <<<"$BEFORE") # заменён весь swap, кроме zram
fi
assert_state "$SWAPFILE" "$REC_GIB" "$expected"
assert_param /sys/module/zswap/parameters/enabled Y
assert_param /sys/module/zswap/parameters/compressor lz4
history_expect_one "$SETUP_OP" action=setup status=committed source=daemon via=web \
	user=ramplus-web profile_after=zswap-lz4
grep -q "\"swap_after_bytes\": $REC_SIZE," <<<"$(history_op "$SETUP_OP")" ||
	die "operation $SETUP_OP: swap_after_bytes is not $REC_SIZE:\n$(history_op "$SETUP_OP")"

step "F10. plan rollback → apply → clean; setup undone by one operation"
web POST /api/plan/rollback "{}"
expect_code 200 "plan rollback (web)"
grep -q '"domain": "setup"' <<<"$BODY" || die "rollback does not undo setup:\n$BODY"
grep -q "\"operation_id\": \"$SETUP_OP\"" <<<"$BODY" || die "rollback does not undo $SETUP_OP:\n$(grep -A8 '"rollback"' <<<"$BODY")"
plan_id=$(field id)
history_snapshot
web POST /api/apply "{\"plan_id\": \"$plan_id\"}"
expect_code 202 "apply rollback (web)"
ROLLBACK_OP=$(field operation_id)
[[ -n $ROLLBACK_OP ]] || die "no operation_id: $BODY"
web_wait_op "$ROLLBACK_OP"
assert_state "$SWAPFILE" 0 "$BEFORE"
assert_eq "$(params)" "$INITIAL_PARAMS" "zswap/vm.* parameters as at the start"
"$BIN" swap check-clean || die "traces remain after a rollback via the web"
history_expect_one "$ROLLBACK_OP" action=rollback status=committed "target=$SETUP_OP" source=daemon via=web
history_expect_field "$SETUP_OP" status=rolled_back

step "F11. logout → status 401"
web POST /api/logout "{}"
expect_code 204 "logout"
web GET /api/status
expect_code 401 "status after logout"

step "F12. make uninstall PURGE=1 — no traces"
if ((GROUPS_PREEXIST)); then
	"$BIN" passwd --disable # группы были до теста: PURGE нельзя, пароль убираем сами
fi
make -s -C "$REPO" uninstall "${PURGE[@]}"
INSTALLED=0
WEB_INIT=0
for f in /usr/local/bin/ramplus-web /usr/local/lib/systemd/system/ramplus-web.service /etc/ramplus/web-auth /run/ramplus; do
	[[ ! -e $f ]] || die "$f remains after uninstall"
done
if ((!GROUPS_PREEXIST)); then
	if getent passwd ramplus-web >/dev/null; then
		die "user ramplus-web remains after uninstall PURGE=1"
	fi
	for g in ramplus ramplus-admin ramplus-web; do
		if getent group "$g" >/dev/null; then
			die "group $g remains after uninstall PURGE=1"
		fi
	done
fi
if pgrep -x ramplus-web >/dev/null || pgrep -x ramplusd >/dev/null; then
	die "ramplus-web or ramplusd runs after uninstall"
fi
if curl -s -o /dev/null "$WEB/"; then
	die "port 7781 answers after uninstall"
fi
"$BIN" swap check-clean || die "traces remain after uninstall"

########################################################################
if [[ -z $DEB ]]; then
	printf '\n=== G. .deb package: skipped (pass the .deb as the second argument)\n'
	printf '\nALL SCENARIOS PASSED (A–F)\n'
	exit 0
fi

PKG=/usr/bin/ramplus
pkg_status() { dpkg-query -W -f='${Status}' ramplus 2>/dev/null || true; }

step "G1. apt install ./ramplus_*.deb — services do not start on their own"
DAEMON_SCENARIO=1
apt-get install -y -qq "$DEB"
DEB_INSTALLED=1
[[ $(pkg_status) == "install ok installed" ]] || die "package status: $(pkg_status)"
for f in /usr/bin/ramplus /usr/bin/ramplusd /usr/bin/ramplus-web /usr/lib/systemd/system/ramplusd.service \
	/usr/lib/systemd/system/ramplus-web.service /usr/lib/sysusers.d/ramplus.conf /usr/lib/tmpfiles.d/ramplus.conf; do
	[[ -e $f ]] || die "$f is not installed"
done
grep -q '^ExecStart=/usr/bin/ramplusd$' /usr/lib/systemd/system/ramplusd.service ||
	die "ramplusd.service: ExecStart is not /usr/bin/ramplusd"
for u in ramplusd ramplus-web; do
	if systemctl is-active --quiet "$u"; then
		die "$u started by itself after apt install"
	fi
	if systemctl is-enabled --quiet "$u" 2>/dev/null; then
		die "$u enabled by itself after apt install"
	fi
done
echo "ok: services are installed but neither enabled nor running"
out_has ramplus-admin id ramplus-web || die "ramplus-web is not in ramplus-admin: $(id ramplus-web)"
[[ $(stat -c '%a %U %G' /run/ramplus) == "750 root ramplus" ]] || die "/run/ramplus: $(stat -c '%a %U %G' /run/ramplus)"

step "G2. ramplus init --password-stdin, systemctl enable --now, daemon status"
printf '%s\n' "$WEBPW" | "$PKG" init --password-stdin
systemctl enable --now ramplusd ramplus-web
for ((i = 0; i < 50; i++)); do
	[[ -S $SOCK ]] && curl -s -o /dev/null "$WEB/api/session" && break
	sleep 0.2
done
out_has 'matches the CLI' "$PKG" --lang en daemon status || die "daemon status:\n$("$PKG" --lang en daemon status 2>&1)"
"$PKG" --lang en daemon status
BODY=$(curl -sS "$WEB/api/session")
grep -q '"password_set":true' <<<"$BODY" || die "web session: $BODY"

step "G3. setup from the recommendation (CLI of the package)"
BODY=$("$PKG" --lang en recommend --json)
REC_SIZE=$(numfield swap_size_bytes)
REC_EXISTING=$(field existing)
REC_PROFILE=$(field profile)
[[ -n $REC_SIZE ]] || die "no swap recommendation:\n$BODY"
REC_GIB=$((REC_SIZE / 1024 / 1024 / 1024))
echo "recommendation: ${REC_GIB}G, existing=${REC_EXISTING:-null}, profile $REC_PROFILE"
before
setup_args=(setup --size "${REC_GIB}G" --profile "$REC_PROFILE" --experimental --yes)
if [[ -n $REC_EXISTING ]]; then
	setup_args+=(--existing="$REC_EXISTING")
fi
echo "+ ramplus ${setup_args[*]}"
"$PKG" --lang en "${setup_args[@]}"
expected=$BEFORE
if [[ $REC_EXISTING == replace ]]; then
	expected=$(awk '$1 ~ /^\/dev\/zram/' <<<"$BEFORE")
fi
assert_state "$SWAPFILE" "$REC_GIB" "$expected"

step "G4. apt remove while RamPlus manages swap → refused"
rc=0
out=$(apt-get remove -y -qq ramplus 2>&1) || rc=$?
((rc != 0)) || die "apt remove succeeded although RamPlus manages swap:\n$out"
grep -q 'refusing to remove the package' <<<"$out" || die "no refusal message:\n$out"
grep -q 'RAMPLUS_FORCE_REMOVE=1' <<<"$out" || die "no override hint:\n$out"
echo "ok: apt remove refused"
[[ $(pkg_status) == "install ok installed" ]] || die "package status after the refused remove: $(pkg_status)"
systemctl is-active --quiet ramplusd || die "ramplusd stopped by the refused remove"
assert_state "$SWAPFILE" "$REC_GIB" "$expected"

step "G5. rollback → apt remove passes, services stopped and disabled"
"$PKG" --lang en rollback --yes
assert_state "$SWAPFILE" 0 "$BEFORE"
assert_eq "$(params)" "$INITIAL_PARAMS" "zswap/vm.* parameters as at the start"
"$BIN" swap check-clean || die "traces remain after rollback"
apt-get remove -y -qq ramplus
[[ $(pkg_status) == "deinstall ok config-files" ]] || die "package status after remove: $(pkg_status)"
for u in ramplusd ramplus-web; do
	if systemctl is-active --quiet "$u"; then
		die "$u runs after apt remove"
	fi
	[[ ! -e /etc/systemd/system/multi-user.target.wants/$u.service ]] || die "$u is still enabled after apt remove"
done
[[ ! -e /usr/bin/ramplus ]] || die "/usr/bin/ramplus remains after apt remove"
[[ -f /etc/ramplus/web-auth ]] || die "apt remove (not purge) must keep /etc/ramplus/web-auth"
echo "ok: removed; configuration kept until purge"

step "G6. apt purge — no /etc/ramplus, clean journal removed, users kept"
if [[ -n $SAVED_STATE ]]; then
	# purge removes /var/lib/ramplus when it holds only RamPlus files: keep the
	# journal of previous runs out of its way (cleanup puts it back).
	mv "$SAVED_STATE" "$WORK/state.json.saved"
	SAVED_STATE=$WORK/state.json.saved
fi
apt-get purge -y -qq ramplus
DEB_INSTALLED=0
[[ -z $(pkg_status) || $(pkg_status) == "unknown ok not-installed" ]] || die "package status after purge: $(pkg_status)"
[[ ! -e /etc/ramplus ]] || die "/etc/ramplus remains after purge"
[[ ! -e $STATE ]] || die "$STATE remains after purge (the journal is clean)"
if [[ -d /var/lib/ramplus ]]; then
	# Allowed only with leftovers of earlier failed test runs (not RamPlus files).
	extra=$(find /var/lib/ramplus -mindepth 1 -maxdepth 1 ! -name '*vmtest-failed*')
	[[ -z $extra ]] || die "/var/lib/ramplus remains after purge:
$extra"
fi
getent passwd ramplus-web >/dev/null || die "purge must keep the ramplus-web user (Debian policy)"
echo "ok: purged; users and groups kept as Debian policy recommends"
remove_deb_accounts
"$BIN" swap check-clean || die "traces remain after purge"

printf '\nALL SCENARIOS PASSED (A–G)\n'

