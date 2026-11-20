# swap / zswap benchmark

`scripts/bench.sh` compares swap and zswap configurations under memory-shortage load. Its results are
what default values in RamPlus profiles must be based on (see the rules in
[docs/development.md](../docs/development.md#rules-for-contributors)).

> **Numbers from a VM are not representative.** A virtual machine's disk is cached in the host's RAM,
> so swap in a VM looks much faster than on a real disk, and the "swap vs zswap" ratio is distorted. In
> a VM the benchmark is only good for checking that the script works. Conclusions and default values
> need runs on a real SSD (ideally on two or three different machines). The `summary.md` of a VM run
> gets this warning automatically.

Published results and their caveats: [docs/benchmarks/](../docs/benchmarks/README.md).

## What the script changes

The benchmark **changes the system**: it turns off all swap (including zram), changes zswap
parameters, and creates a temporary `/ramplus-bench.swapfile`. On exit (including Ctrl+C or an error)
the original state is restored: swap is turned back on with the previous priorities, zswap parameters
are restored, the temporary file is removed. That is why it only runs with explicit consent:

```sh
make build-probe
sudo RAMPLUS_VM_TEST=1 scripts/bench.sh                # in a disposable VM
sudo scripts/bench.sh --i-know-this-changes-swap       # on a real machine
```

Close everything important first: under load without swap, the OOM killer may kill a random process.

Dependencies: `stress-ng` (for the `hot` workload) and the `ramplus-probe` binary (`make build-probe`).

## Options

| option | default | |
|---|---|---|
| `--i-know-this-changes-swap` | | consent on a real machine (in a VM: `RAMPLUS_VM_TEST=1`) |
| `--probe PATH` | `./ramplus-probe` | the probe binary |
| `--out DIR` | `./bench-results-<time>` | results directory |
| `--runs N` | 3 | runs per combination |
| `--duration S` | 30 | load duration, seconds |
| `--work-size SIZE` | 1.3 × RAM | load size |
| `--swap-size SIZE` | 4G | swapfile size |
| `--pause S` | 10 | pause between runs |
| `--configs a,b` | all | subset of configurations |
| `--workloads a,b` | `hot,hot-probe,cold` | subset of workloads |
| `--data a,b` | `compressible,random` | subset of data types |
| `--quick` | | 1 run, 15 s, pause 3 s |
| `--pool-sweep` | | also lz4 with max_pool_percent 35 and 50 |

## Configurations

| name | what is enabled |
|---|---|
| `no-swap` | swap off (expected: refused when the load exceeds RAM) |
| `swap` | a `--swap-size` (4G) swapfile on the root filesystem |
| `zswap-lz4` | swap + zswap, lz4, zsmalloc |
| `zswap-zstd` | swap + zswap, zstd, zsmalloc |
| `zswap-lzo-zbud` | swap + zswap, lzo, zbud (the kernel's defaults; Ubuntu ships zswap disabled) |

With `--pool-sweep`, `zswap-lz4-p35` and `zswap-lz4-p50` are added: lz4 and zsmalloc with
max_pool_percent 35 and 50 (`zswap-lz4` is the 20 % point). They are not part of `--quick`.

If a configuration is not available on this kernel (newer kernels have no zbud; some lack a
compressor), the run is marked `skipped` with the reason.

Before every run the state is reset: all swap off, zswap off, `drop_caches`, then a pause (`--pause`,
10 s).

## Workloads

| name | what it does |
|---|---|
| `hot` | `stress-ng --vm 1 --vm-bytes N --vm-keep`: all pages are constantly in use. The worst case for swap and zswap. Data is stress-ng's own pattern |
| `hot-probe` | `ramplus-probe workload --hot 1.0`: the same, with a controlled data type |
| `cold` | `ramplus-probe workload --hot 0.15`: allocate N, fill it, then work with only 15 % of it. A realistic desktop: lots of background memory that is rarely needed |

Data types for `hot-probe` and `cold` (`--data`):

- `compressible` — about 3:1, like typical process memory. A third of each page is random, the rest is
  a repeating pattern. There are no identical pages: zswap stores those separately, which would distort
  the result;
- `random` — random, incompressible data. The worst case for zswap.

## What is measured

Alongside the load run:

- `ramplus-probe sample` — reads `/proc/meminfo` and `/proc/pressure/memory` every 200 ms
  (`samples.csv` in the run directory);
- `ramplus-probe latency` — a small "interactive" process. Every 100 ms it touches 50 MiB of its own
  memory and records the delay (p50, p99, max). It shows how much responsiveness suffers during the
  load.

When run as root, the counters in `/sys/kernel/debug/zswap/` are read before and after each run:
`pool_limit_hit`, `written_back_pages`, `reject_compress_poor`, `reject_reclaim_fail`. Their increase
is written to `results.csv`. Without debugfs, hitting the pool limit is estimated from the pool peak:
peak ≥ 97 % of max_pool_percent × RAM.

Every combination runs 3 times (`--runs`); the summary shows the median over successful runs.

## Run status

| status | meaning |
|---|---|
| `ok` | the load completed |
| `refused` | the system denied memory: `ramplus-probe` could not allocate (exit code 10) or stress-ng could not start its stressor for lack of resources (exit code 3). Expected for `no-swap` when the load exceeds RAM |
| `oom` | the process was killed with SIGKILL (OOM killer) |
| `timeout` | killed by the safety timeout (6 × duration + 120 s) |
| `failed` | a tool error. Such rows are unreliable and need investigation |
| `skipped` | the configuration is not available on this kernel |

## Results

```
bench-results-<time>/
  env.json            kernel, virtualization, RAM, run parameters
  results.csv         one row per run
  summary.md          summary: medians per configuration
  runs/<config>__<workload>__<data>__<N>/
    meta.json         status (ok | refused | failed | oom | timeout | skipped), duration
    samples.csv       meminfo/PSI samples
    sample.json       peaks and averages
    latency.json      latency probe p50/p99/max
    workload.json     ramplus-probe workload result
    workload.log      workload output
    zswap-before.json zswap debugfs counters before the run
    zswap-after.json  and after (available: false without root/debugfs)
```

Summary columns:

- success — how many of N runs completed;
- duration;
- peak swap used and peak zswap pool;
- PSI some and full: peak avg10 and the average over the run (from the `total` counter);
- latency p50 and p99;
- pool limit: `yes (N)` — pool_limit_hit grew by N; `no`; without debugfs — `likely: peak ≈ 20% RAM`
  or `no (by peak)`;
- hot-page walk speed. The absolute value is arbitrary; compare it between configurations.

The summary ends with an "Observations" section, generated automatically:

- the worst latency p99 and max per workload and in which configuration; a max above 1000 ms is marked
  "freeze";
- configurations where the pool limit was hit (or, judging by the peak, reached);
- duration outliers: more than 2× the median across configurations for that workload;
- refusals, OOM kills, timeouts, tool errors, skips;
- a warning if there were fewer than 3 runs.

Rebuild the report from an existing directory: `ramplus-probe report <dir>`. If the directory has no
`runs/`, the report is built from `results.csv`. This way an old run's summary can be recomputed:
`failed` statuses are re-evaluated with the current rules from the exit code and note.

## Full and quick runs

One run takes roughly pause + 2 × duration + 30 s: memory fills up under pressure, and draining swap
after the run also takes time. The script prints an estimate at the start.

| set | runs | time |
|---|---|---|
| `--quick` (1 run, 15 s, pause 3 s) | 25 | ≈ 25–30 min (27 in a VM) |
| default (3 runs, 30 s, pause 10 s) | 75 | ≈ 2 h |
| default + `--pool-sweep` | 105 | ≈ 3 h |

Narrow the set with `--configs`, `--workloads` and `--data`.

## Sharing results

Open an issue with the **Benchmark result** template and attach `results.csv` and `summary.md`, with
your CPU, RAM, SSD model, distribution and kernel.
