// Данные для тестов (как отдаёт демон).
import type { Plan, ProfileCatalog, Recommendation, Status } from './types'

const G = 1024 ** 3

export function recommendation(over: Partial<Recommendation> = {}): Recommendation {
  return {
    schema_version: 1,
    ram_bytes: 31.1 * G,
    ram_rounded_bytes: 32 * G,
    swap_size_bytes: 16 * G,
    swap_reason: 'RAM 32 GB: запас на SSD — половина RAM',
    swap_reason_code: 'ram.medium',
    swap_max_bytes: 48 * G,
    swap_blocked: [],
    target: { path: '/ramplus.swapfile', mount_point: '/', fs_type: 'ext4', disk_kind: 'nvme', total_bytes: 500 * G, available_bytes: 210 * G, max_bytes: 48 * G, usable: true, selected: true },
    targets: [],
    managed_bytes: null,
    existing: 'replace',
    profile: 'zswap-lz4',
    profile_status: 'experimental',
    profile_reason: '',
    notes: [{ code: 'profile.preliminary', message: '' }],
    ...over,
  }
}

export function status(over: Partial<Status> = {}): Status {
  return {
    schema_version: 1,
    collected_at: '2026-11-20T10:00:00Z',
    system: { hostname: 'host', kernel: '7.0.0-1', arch: 'x86_64', os: { id: 'ubuntu', name: 'Ubuntu', pretty_name: 'Ubuntu 24.04', version_id: '24.04' } },
    memory: { total_bytes: 31.1 * G, available_bytes: 20.8 * G, used_bytes: 10.2 * G, swap_total_bytes: 8 * G, swap_used_bytes: 0 },
    swaps: { devices: [{ path: '/swap.img', type: 'file', size_bytes: 8 * G, used_bytes: 0, priority: -2, managed: false }], warnings: [] },
    zswap: { present: true, params: { enabled: false, compressor: 'lzo', max_pool_percent: 20 }, pool_bytes: null, stored_bytes: null },
    psi: { some: { avg10: 0, avg60: 0, avg300: 0 }, full: null, pressure_level: 'normal' },
    profile: null,
    drift: [],
    ...over,
  }
}

export function catalog(): ProfileCatalog {
  const p = (name: string, status: string, zswap: boolean) => ({
    name, title: name, friendly: name, description: '', status, status_text: status, evidence: '',
    params: zswap ? { 'zswap.enabled': 'Y' } : { 'zswap.enabled': 'N' }, recommended: name === 'zswap-lz4', active: false,
  })
  return { profiles: [p('default', 'standard', false), p('zswap-lz4', 'experimental', true), p('zswap-zstd', 'experimental', true)], params: [], active: '', zswap_present: true }
}

export function plan(over: Partial<Plan> = {}): Plan {
  return {
    schema_version: 1,
    id: 'abc',
    fingerprint: 'f',
    action: 'setup',
    command: 'ramplus setup --size 16G --existing=replace --profile zswap-lz4',
    blocked: false,
    choices: [],
    checks: [
      { level: 'ok', code: 'disk_space.ok', title: 'место', message: 'на / доступно 210 GiB' },
      { level: 'warn', code: 'profile.experimental', title: 'статус', message: 'профиль экспериментальный' },
    ],
    steps: [{ kind: 'allocate', path: '/ramplus.swapfile', description: 'создать /ramplus.swapfile' }],
    summary: { swap_before_bytes: 8 * G, swap_after_bytes: 16 * G, created: [], disabled: ['/swap.img'], deleted: [] },
    swap: { swaps: [], mem_total_bytes: 31.1 * G },
    profile: { name: 'zswap-lz4', experimental: true, reset: false, rows: [] },
    ...over,
  }
}
