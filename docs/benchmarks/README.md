# Benchmark results

Raw results of `scripts/bench.sh` runs that the RamPlus documentation refers to. Each directory is one
run: `results.csv` (one row per configuration × workload × data) and `summary.md` (medians; the
summary of this run was generated in Russian by the older script version).

How to run the benchmark yourself and what every column means: [scripts/BENCH.md](../../scripts/BENCH.md).
Please share real-hardware results with the **Benchmark result** issue template.

## 2026-11-17-vm

[results.csv](2026-11-17-vm/results.csv) · [summary.md](2026-11-17-vm/summary.md)

The run behind the table "Our test results" in the main README.

| | |
|---|---|
| Machine | KVM virtual machine, 3909 MiB RAM, root disk `QEMU HARDDISK` |
| Kernel | 6.8.0-147-generic (Ubuntu 24.04) |
| Swap | 4 GiB swapfile |
| Load | 5082 MiB (more than RAM), 15 s per run |
| Runs | **1 per configuration** |
| Configurations | `no-swap`, `swap`, `zswap-lz4`, `zswap-zstd` (both zsmalloc, pool 20 %), `zswap-lzo-zbud` (kernel defaults) |
| Workloads | `hot` (stress-ng, all pages constantly in use), `hot-probe`, `cold`; data `compressible`, `random` |

The README table uses the `hot` / `stress-ng` rows: the latency probe's p99 (`lat_p99_ms`), the PSI full
peak (`psi_full_peak`) and the duration (`duration_s`).

### Caveats

- **This is a virtual machine.** Its disk is cached in the host's RAM, so swap looks much faster than on
  a real SSD. The project rule is that VM numbers are **never** used to choose default values; they only
  show the direction of the effect. That is why the recommended mode is marked *preliminary*.
- **One run per configuration** — no medians, no spread. Individual numbers can be noise.
- **Older script version.** In this run `no-swap` is recorded as `failed` (exit codes 2 and 3) instead
  of `refused`, which newer versions of `bench.sh` report when the system denies memory. In both cases
  the meaning is the same: without swap the load does not fit and fails.
- The `hot-probe` and `cold` workloads show smaller and less consistent differences; read the full
  table before drawing conclusions. In `hot-probe / compressible`, for example, `zswap-zstd` was the
  slowest configuration.
- `max` latency was about 0.16 s for all zswap configurations under `hot`; the difference is in the
  99th percentile, i.e. how often long waits happen.
