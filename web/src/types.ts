// Типы JSON API демона (internal/monitor, internal/plan, internal/core) и
// ramplus-web (internal/web). Имена полей — контракт API.

export type Level = 'ok' | 'info' | 'warn' | 'fail'

export interface Check {
  level: Level
  code: string
  title: string
  message: string
}

export interface Alternative {
  value: string
  selected: boolean
  summary: string
}

export interface Choice {
  title: string
  flag: string
  value: string
  defaulted: boolean
  note?: string
  alternatives?: Alternative[]
}

export interface StepView {
  kind: string
  path?: string
  size?: number
  description: string
}

export interface SwapRow {
  path: string
  type: string
  size_bytes: number
  used_bytes: number
  priority: number
  tag?: string
}

export interface ProfileRow {
  key: string
  title: string
  current: string
  new: string
  source: string
  change: boolean
  skip?: string
  from_set?: boolean
}

export interface ProfileView {
  name: string
  title?: string
  status?: string
  experimental: boolean
  reset: boolean
  rows: ProfileRow[]
  current?: string
}

export interface RollbackView {
  operation_id: string
  action: string
  domain: string
  command: string
  started_at: string
  age_seconds: number
}

export interface Plan {
  schema_version: number
  id?: string
  fingerprint: string
  action: string
  command: string
  blocked: boolean
  choices: Choice[]
  checks: Check[]
  steps: StepView[]
  summary: {
    swap_before_bytes: number
    swap_after_bytes: number
    created: string[]
    disabled: string[]
    deleted: string[]
  }
  swap?: {
    swaps: SwapRow[]
    mem_total_bytes: number
    mem_available_bytes?: number
    managed?: { path: string; size_bytes: number; priority: number; unit: string }
  }
  profile?: ProfileView
  rollback?: RollbackView
}

export interface SwapDevice {
  path: string
  type: string
  size_bytes: number
  used_bytes: number
  priority: number
  managed: boolean
  disk_kind?: string
}

export interface PSILine {
  avg10: number
  avg60: number
  avg300: number
}

export interface Status {
  schema_version: number
  collected_at: string
  system: {
    hostname: string
    kernel: string
    arch: string
    os: { id: string; name: string; pretty_name: string; version_id: string } | null
  }
  memory: {
    total_bytes: number | null
    available_bytes: number | null
    used_bytes: number | null
    swap_total_bytes: number | null
    swap_used_bytes: number | null
  }
  swaps: { devices: SwapDevice[]; warnings: { code: string; path: string }[] }
  zswap: {
    present: boolean
    params: { enabled: boolean | null; compressor: string | null; max_pool_percent: number | null }
    pool_bytes: number | null
    stored_bytes: number | null
  }
  psi: { some: PSILine | null; full: PSILine | null; pressure_level: string }
  profile: { name: string; status: string; applied_at: string } | null
  drift: { key: string; expected: string; actual: string }[]
}

export interface StatusFrame {
  status: Status
  peaks: { pressure_level: string; psi_some_avg10: number } | null
}

export interface RecTarget {
  path: string
  mount_point: string
  fs_type: string
  disk_kind: string
  transport?: string
  total_bytes: number | null
  available_bytes: number | null
  max_bytes: number
  usable: boolean
  problem?: string
  selected: boolean
}

export interface Recommendation {
  schema_version: number
  ram_bytes: number
  ram_rounded_bytes: number
  swap_size_bytes: number | null
  swap_reason: string
  swap_reason_code: string
  swap_max_bytes: number
  swap_blocked: Check[]
  target: RecTarget | null
  targets: RecTarget[]
  managed_bytes: number | null
  existing: 'replace' | 'keep' | null
  existing_reason?: string
  profile: string
  profile_status: string
  profile_reason: string
  notes: { code: string; message: string }[]
}

export interface ChangeView {
  swap_before_bytes: number | null
  swap_after_bytes: number | null
  profile_before: string
  profile_after: string
  target?: ChangeView
}

export interface HistoryEntry {
  id: string
  action: string
  command: string
  status: string
  target?: string
  started_at: string
  steps: number
  error?: string
  undoable: boolean
  rollback_next: boolean
  title: string
  change?: ChangeView
  source: 'cli' | 'daemon' | 'unknown'
  requested_by?: { uid: number; user?: string; via?: string }
}

export interface History {
  operations: HistoryEntry[]
}

export interface ProfileEntry {
  name: string
  title: string
  friendly: string
  description: string
  status: string
  status_text: string
  evidence: string
  params: Record<string, string>
  recommended: boolean
  active: boolean
}

export interface ParamEntry {
  key: string
  title: string
  path: string
  group: string
  type: string
  min?: number
  max?: number
  words?: string[]
  current: string | null
}

export interface ProfileCatalog {
  profiles: ProfileEntry[]
  params: ParamEntry[]
  active: string
  zswap_present: boolean
}

export interface EventView {
  undo: boolean
  index: number
  total: number
  step: StepView
  error?: string
}

export interface Result {
  status: string
  operation_id?: string
  error?: string
  events: EventView[]
}

export interface SessionInfo {
  password_set: boolean
  authenticated: boolean
  csrf_token?: string
  expires_at?: string
  version: string
}

export interface ApiErrorBody {
  code: string
  message: string
  retry_after_seconds?: number
  attempts_left?: number
  plan?: Plan
}

export interface SetupRequest {
  swap: { size: number; existing?: 'replace' | 'keep'; path?: string } | null
  profile: { name: string; set?: Record<string, string> } | null
}
