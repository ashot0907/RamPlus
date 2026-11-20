#!/usr/bin/env bash
# Бенчмарк конфигураций swap/zswap. МЕНЯЕТ СИСТЕМУ: отключает весь swap,
# меняет параметры zswap, создаёт временный swapfile. В конце всё
# возвращается как было (trap EXIT). Подробности — scripts/BENCH.md.
#
#   make build-probe
#   sudo RAMPLUS_VM_TEST=1 scripts/bench.sh            # в VM
#   sudo scripts/bench.sh --i-know-this-changes-swap   # на реальном железе
#
# Время (≈ пауза + 2 × длительность + 30 с на прогон; в начале скрипт
# печатает оценку для выбранного набора): --quick ≈ 25–30 мин, полный
# прогон ≈ 2 ч, с --pool-sweep ≈ 3 ч.
set -euo pipefail

usage() {
	cat <<'EOF'
Usage: sudo scripts/bench.sh [--i-know-this-changes-swap] [options]

  --probe PATH        ramplus-probe binary (default ./ramplus-probe)
  --out DIR           results directory (default ./bench-results-<time>)
  --runs N            runs per configuration (3)
  --duration SEC      load duration (30)
  --work-size SIZE    load memory size (default 1.3 × RAM)
  --swap-size SIZE    swapfile size (4G)
  --pause SEC         pause after the state reset (10)
  --configs LIST      comma-separated: no-swap,swap,zswap-lz4,zswap-zstd,zswap-lzo-zbud
  --workloads LIST    comma-separated: hot,hot-probe,cold
  --data LIST         comma-separated: compressible,random (for hot-probe and cold)
  --pool-sweep        add zswap-lz4 with max_pool_percent 35 and 50 (zswap-lz4 = 20%)
  --quick             1 run, 15 s, pause 3 s — check that everything works
                      (≈ 25–30 min; --pool-sweep is not included)
EOF
}

die() {
	printf 'bench: %b\n' "$*" >&2
	exit 1
}
log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }

I_KNOW=0
PROBE=./ramplus-probe
OUT=""
RUNS=3
DURATION=30
WORK_SIZE=""
SWAP_SIZE=4G
PAUSE=10
CONFIGS=(no-swap swap zswap-lz4 zswap-zstd zswap-lzo-zbud)
WORKLOADS=(hot hot-probe cold)
DATAS=(compressible random)
POOL_SWEEP=0

while (($# > 0)); do
	case $1 in
	--i-know-this-changes-swap) I_KNOW=1 ;;
	--probe) PROBE=${2:?--probe: a path is required}; shift ;;
	--out) OUT=${2:?--out: a directory is required}; shift ;;
	--runs) RUNS=${2:?--runs: a number is required}; shift ;;
	--duration) DURATION=${2:?--duration: a number of seconds is required}; shift ;;
	--work-size) WORK_SIZE=${2:?--work-size: a size is required}; shift ;;
	--swap-size) SWAP_SIZE=${2:?--swap-size: a size is required}; shift ;;
	--pause) PAUSE=${2:?--pause: a number of seconds is required}; shift ;;
	--configs) IFS=, read -r -a CONFIGS <<<"${2:?--configs: a list is required}"; shift ;;
	--workloads) IFS=, read -r -a WORKLOADS <<<"${2:?--workloads: a list is required}"; shift ;;
	--data) IFS=, read -r -a DATAS <<<"${2:?--data: a list is required}"; shift ;;
	--quick) RUNS=1; DURATION=15; PAUSE=3 ;;
	--pool-sweep) POOL_SWEEP=1 ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown argument: $1" ;;
	esac
	shift
done

[[ ${RAMPLUS_VM_TEST:-} == 1 || $I_KNOW == 1 ]] ||
	die "the benchmark turns off all swap and changes zswap. Run with RAMPLUS_VM_TEST=1 (in a VM) or --i-know-this-changes-swap"
[[ $EUID -eq 0 ]] || die "root is required (swapon/swapoff, zswap parameters, drop_caches)"
[[ $RUNS =~ ^[1-9][0-9]*$ && $DURATION =~ ^[1-9][0-9]*$ && $PAUSE =~ ^[0-9]+$ ]] || die "--runs/--duration/--pause: integers are required"
PROBE=$(readlink -f "$PROBE")
[[ -x $PROBE ]] || die "no $PROBE (make build-probe)"
for w in "${WORKLOADS[@]}"; do
	if [[ $w == hot ]] && ! command -v stress-ng >/dev/null; then
		die "the hot workload needs stress-ng (apt install stress-ng), or drop it: --workloads hot-probe,cold"
	fi
done

if ((POOL_SWEEP)); then
	CONFIGS+=(zswap-lz4-p35 zswap-lz4-p50)
fi

ZSWAP=/sys/module/zswap/parameters
ZSWAP_DEBUG=/sys/kernel/debug/zswap
BENCH_SWAP=/ramplus-bench.swapfile
MEM_TOTAL_KB=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)
[[ -n $WORK_SIZE ]] || WORK_SIZE="$((MEM_TOTAL_KB * 13 / 10 / 1024))M"
[[ -n $OUT ]] || OUT="./bench-results-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUT/runs"
OUT=$(readlink -f "$OUT")
[[ ! -e $BENCH_SWAP ]] || die "$BENCH_SWAP already exists — remove it manually"

########################################################################
# Исходное состояние и его восстановление.

ORIG_SWAPS=$(swapon --show=NAME,PRIO --noheadings --raw)
declare -A ORIG_ZSWAP=()
if [[ -d $ZSWAP ]]; then
	for p in enabled compressor zpool max_pool_percent; do
		if [[ -r $ZSWAP/$p ]]; then
			ORIG_ZSWAP[$p]=$(<"$ZSWAP/$p")
		fi
	done
fi

restore() {
	local rc=$?
	set +e
	log "restoring the original state"
	jobs -p | xargs -r kill -TERM 2>/dev/null
	swapoff "$BENCH_SWAP" 2>/dev/null
	rm -f "$BENCH_SWAP"
	local p name prio
	for p in compressor zpool max_pool_percent enabled; do
		if [[ -n ${ORIG_ZSWAP[$p]:-} ]]; then
			echo "${ORIG_ZSWAP[$p]}" >"$ZSWAP/$p" 2>/dev/null
		fi
	done
	while read -r name prio; do
		[[ -n $name ]] || continue
		if is_active "$name"; then
			continue
		fi
		if [[ $prio =~ ^[0-9]+$ ]]; then
			swapon -p "$prio" "$name"
		else
			swapon "$name"
		fi
	done <<<"$ORIG_SWAPS"
	swapon --show
	exit "$rc"
}
trap restore EXIT

is_active() { swapon --show=NAME --noheadings --raw | grep -qxF "${1:-}"; }

########################################################################

create_bench_swap() {
	local fstype
	fstype=$(findmnt -n -o FSTYPE --target "$(dirname "$BENCH_SWAP")")
	if [[ $fstype == btrfs ]]; then
		btrfs filesystem mkswapfile --size "$SWAP_SIZE" "$BENCH_SWAP" >/dev/null
	else
		fallocate -l "$SWAP_SIZE" "$BENCH_SWAP"
		chmod 600 "$BENCH_SWAP"
		mkswap "$BENCH_SWAP" >/dev/null
	fi
}

zswap_set() {
	local param=${1:-} value=${2:-}
	[[ -w $ZSWAP/$param ]] || return 1
	echo "$value" >"$ZSWAP/$param" 2>/dev/null || return 1
	[[ $(<"$ZSWAP/$param") == "$value" ]]
}

# reset_state: весь swap выключен, zswap выключен, кэши сброшены.
reset_state() {
	local name
	# Только активные устройства: swapoff -a падает на неактивных строках fstab.
	while read -r name; do
		[[ -n $name ]] || continue
		swapoff "$name" || die "swapoff $name failed (not enough RAM?) — the state will be restored"
	done < <(swapon --show=NAME --noheadings --raw)
	if [[ -n ${ORIG_ZSWAP[enabled]:-} ]]; then
		zswap_set enabled N || true
	fi
	sync
	echo 3 >/proc/sys/vm/drop_caches
	sleep "$PAUSE"
}

# configure CONFIG — включить swap/zswap; ненулевой код и причина в
# SKIP_REASON, если конфигурация недоступна на этом ядре.
SKIP_REASON=""
POOL_PCT=0
configure() {
	local config=${1:-} comp="" pool="" pct=20
	SKIP_REASON=""
	POOL_PCT=0
	if [[ $config =~ ^(zswap-.+)-p([0-9]+)$ ]]; then
		config=${BASH_REMATCH[1]}
		pct=${BASH_REMATCH[2]}
	fi
	case $config in
	no-swap) return 0 ;;
	swap) ;;
	zswap-lz4) comp=lz4 pool=zsmalloc ;;
	zswap-zstd) comp=zstd pool=zsmalloc ;;
	zswap-lzo-zbud) comp=lzo pool=zbud ;;
	*) SKIP_REASON="unknown configuration"; return 1 ;;
	esac
	swapon -p 10 "$BENCH_SWAP"
	[[ -n $comp ]] || return 0

	if [[ ! -d $ZSWAP ]]; then
		SKIP_REASON="the kernel has no zswap"
		return 1
	fi
	modprobe "$comp" 2>/dev/null || true
	if ! zswap_set compressor "$comp"; then
		SKIP_REASON="compressor $comp is not available"
		return 1
	fi
	if [[ -e $ZSWAP/zpool ]]; then
		modprobe "$pool" 2>/dev/null || true
		if ! zswap_set zpool "$pool"; then
			SKIP_REASON="zpool $pool is not available"
			return 1
		fi
	elif [[ $pool != zsmalloc ]]; then
		SKIP_REASON="the kernel has no zpool choice (only zsmalloc), $pool is not available"
		return 1
	fi
	if ! zswap_set max_pool_percent "$pct"; then
		SKIP_REASON="could not set max_pool_percent=$pct"
		return 1
	fi
	POOL_PCT=$pct
	if ! zswap_set enabled Y; then
		SKIP_REASON="could not enable zswap"
		return 1
	fi
}

write_meta() {
	local dir=${1:-} config=${2:-} workload=${3:-} data=${4:-} run=${5:-0} status=${6:-} rc=${7:-0} dur=${8:-0} note=${9:-} pct=${10:-0}
	# JSON: удаляются кавычки (\042 = ") и обратные слэши (\134 = \),
	# переводы строк и табы заменяются пробелами.
	note=$(printf '%s' "$note" | tr -d '\042\134' | tr '\n\t' '  ')
	printf '{"config":"%s","workload":"%s","data":"%s","run":%d,"status":"%s","exit_code":%d,"duration_s":%s,"note":"%s","max_pool_percent":%d}\n' \
		"$config" "$workload" "$data" "$run" "$status" "$rc" "$dur" "$note" "$pct" >"$dir/meta.json"
}

# run_one CONFIG WORKLOAD DATA N
run_one() {
	local config=${1:-} workload=${2:-} data=${3:-} n=${4:-1}
	local dir="$OUT/runs/${config}__${workload}__${data}__${n}"
	local sp="" lp="" rc=0 start end dur status="" note="" limit
	mkdir -p "$dir"
	log "$config / $workload / $data / run $n"

	reset_state
	if ! configure "$config"; then
		log "  skipped: $SKIP_REASON"
		write_meta "$dir" "$config" "$workload" "$data" "$n" skipped 0 0 "$SKIP_REASON"
		return 0
	fi

	# Счётчики zswap до прогона (debugfs, root); без debugfs — available: false.
	"$PROBE" zswapstat --dir "$ZSWAP_DEBUG" --out "$dir/zswap-before.json"

	"$PROBE" sample --interval 200ms --out "$dir/samples.csv" --summary "$dir/sample.json" &
	sp=$!
	"$PROBE" latency --size 50M --interval 100ms --out "$dir/latency.json" &
	lp=$!
	sleep 1

	limit=$((DURATION * 6 + 120)) # страховка от зависания под давлением
	start=$(date +%s.%N)
	set +e
	case $workload in
	hot)
		timeout --signal=KILL "$limit" stress-ng --vm 1 --vm-bytes "$WORK_SIZE" --vm-keep \
			-t "${DURATION}s" --metrics-brief >"$dir/workload.log" 2>&1
		;;
	hot-probe | cold)
		local hot=0.15
		[[ $workload == cold ]] || hot=1.0
		timeout --signal=KILL "$limit" "$PROBE" workload --size "$WORK_SIZE" --hot "$hot" --data "$data" \
			--duration "${DURATION}s" --out "$dir/workload.json" >"$dir/workload.log" 2>&1
		;;
	esac
	rc=$?
	set -e
	end=$(date +%s.%N)
	dur=$(awk -v s="$start" -v e="$end" 'BEGIN {printf "%.2f", e - s}')

	kill -TERM "$lp" "$sp" 2>/dev/null || true
	wait "$lp" 2>/dev/null || true
	wait "$sp" 2>/dev/null || true
	"$PROBE" zswapstat --dir "$ZSWAP_DEBUG" --out "$dir/zswap-after.json"

	# ok | refused (система отказала в памяти) | oom | timeout | failed (ошибка инструмента)
	local cls
	cls=$("$PROBE" classify --workload "$workload" --rc "$rc" --log "$dir/workload.log" --duration "$dur" --limit "$limit")
	IFS=$'\t' read -r status note <<<"$cls"
	log "  $status in ${dur}s${note:+ — $note}"
	write_meta "$dir" "$config" "$workload" "$data" "$n" "$status" "$rc" "$dur" "$note" "$POOL_PCT"
}

########################################################################

VIRT=none
if command -v systemd-detect-virt >/dev/null; then
	VIRT=$(systemd-detect-virt || true)
fi
ROOT_DEV=$(findmnt -n -o SOURCE /)
ROOT_DISK=$(lsblk -no PKNAME "$ROOT_DEV" 2>/dev/null | head -n 1 || true)
ROOT_INFO=$(lsblk -dno NAME,ROTA,TRAN,MODEL "/dev/${ROOT_DISK:-x}" 2>/dev/null | tr -s ' ' || true)
DEBUGFS=false
if [[ -r $ZSWAP_DEBUG/pool_limit_hit ]]; then
	DEBUGFS=true
fi
printf '{"date":"%s","kernel":"%s","virt":"%s","mem_total_mib":%d,"swap_size":"%s","work_size":"%s","duration_s":%d,"runs":%d,"root_disk":"%s","zswap_debugfs":%s}\n' \
	"$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$(uname -r)" "$VIRT" "$((MEM_TOTAL_KB / 1024))" "$SWAP_SIZE" "$WORK_SIZE" \
	"$DURATION" "$RUNS" "${ROOT_INFO//\"/}" "$DEBUGFS" >"$OUT/env.json"
if [[ $DEBUGFS != true ]]; then
	log "zswap debugfs is not available ($ZSWAP_DEBUG): the pool limit is estimated from the pool peak"
fi

if [[ $VIRT != none && -n $VIRT ]]; then
	log "WARNING: running in a VM ($VIRT) — performance numbers are not representative (see scripts/BENCH.md)"
fi
variants=0
for w in "${WORKLOADS[@]}"; do
	if [[ $w == hot ]]; then
		variants=$((variants + 1))
	else
		variants=$((variants + ${#DATAS[@]}))
	fi
done
TOTAL_RUNS=$((${#CONFIGS[@]} * variants * RUNS))
PER_RUN=$((PAUSE + 2 * DURATION + 30)) # пауза + заполнение памяти + нагрузка + сброс swap
log "results: $OUT; load $WORK_SIZE, swapfile $SWAP_SIZE, $DURATION s × $RUNS runs"
log "runs: $TOTAL_RUNS (${#CONFIGS[@]} configurations × $variants workload variants × $RUNS), about $((TOTAL_RUNS * PER_RUN / 60)) min"

create_bench_swap

for workload in "${WORKLOADS[@]}"; do
	datas=("${DATAS[@]}")
	[[ $workload != hot ]] || datas=(stress-ng) # данные stress-ng не управляются
	for data in "${datas[@]}"; do
		for config in "${CONFIGS[@]}"; do
			for ((n = 1; n <= RUNS; n++)); do
				run_one "$config" "$workload" "$data" "$n"
			done
		done
	done
done

"$PROBE" report "$OUT"
log "done: $OUT/results.csv, $OUT/summary.md"
