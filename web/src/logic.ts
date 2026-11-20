// Чистая логика интерфейса (без React): что выбрано по умолчанию, значки
// «рекомендуем / вы изменили», итог, запрос плана, можно ли применить.
import { GiB } from './format'
import type { Plan, ProfileCatalog, Recommendation, SetupRequest, Status, SwapDevice } from './types'

export type Existing = 'replace' | 'keep'

// Settings — выбор пользователя на главном экране.
export interface Settings {
  swapEnabled: boolean // swap — часть плана
  sizeGB: number
  path: string // '' — по умолчанию
  existing: Existing
  profile: string
}

const isZram = (d: SwapDevice) => d.path.startsWith('/dev/zram')

// otherSwaps — swap не RamPlus и не zram (его заменяют или оставляют).
export function otherSwaps(status: Status | null): SwapDevice[] {
  return (status?.swaps.devices ?? []).filter((d) => !d.managed && !isZram(d))
}

export function managedSwap(status: Status | null): SwapDevice | undefined {
  return status?.swaps.devices.find((d) => d.managed)
}

export function recSizeGB(rec: Recommendation | null): number | null {
  return rec?.swap_size_bytes != null ? Math.floor(rec.swap_size_bytes / GiB) : null
}

export function maxSizeGB(rec: Recommendation | null): number {
  return rec ? Math.floor(rec.swap_max_bytes / GiB) : 0
}

// defaultSettings — то, что вернул /api/recommendation.
export function defaultSettings(rec: Recommendation): Settings {
  const size = recSizeGB(rec)
  return {
    swapEnabled: size != null,
    sizeGB: size ?? Math.max(1, Math.min(maxSizeGB(rec), Math.floor((rec.managed_bytes ?? GiB) / GiB))),
    path: '',
    existing: rec.existing ?? 'replace',
    profile: rec.profile,
  }
}

// showExisting — выбор «заменить / оставить» нужен, только если есть
// чужой swap, а своего ещё нет.
export function showExisting(status: Status | null): boolean {
  return otherSwaps(status).length > 0 && !managedSwap(status)
}

export function sizeIsRecommended(s: Settings, rec: Recommendation | null): boolean {
  const r = recSizeGB(rec)
  return r != null && s.swapEnabled && s.sizeGB === r
}

// isRecommended — все настройки как в рекомендации.
export function isRecommended(s: Settings, rec: Recommendation | null, status: Status | null): boolean {
  if (!rec) return false
  const swapOK = recSizeGB(rec) == null ? !s.swapEnabled : sizeIsRecommended(s, rec) && s.path === ''
  const existingOK = !showExisting(status) || rec.existing == null || s.existing === rec.existing
  return swapOK && existingOK && s.profile === rec.profile
}

// profileBadge — значок карточки режима.
export type Badge = 'recommended' | 'unchanged' | 'expert' | null
export function profileBadge(name: string, rec: Recommendation | null): Badge {
  if (rec && name === rec.profile) return 'recommended'
  if (name === 'default') return 'unchanged'
  return 'expert'
}

// orderProfiles — рекомендуемый первым, потом «как в обычной Ubuntu».
export function orderProfiles(cat: ProfileCatalog | null, rec: Recommendation | null): string[] {
  const names = (cat?.profiles ?? []).map((p) => p.name)
  const rank = (n: string) => (rec && n === rec.profile ? 0 : n === 'default' ? 1 : 2)
  return [...names].sort((a, b) => rank(a) - rank(b) || a.localeCompare(b))
}

// projectedSwap — сколько swap будет после применения (байты).
export function projectedSwap(s: Settings, status: Status | null): number {
  const current = status?.memory.swap_total_bytes ?? 0
  if (!s.swapEnabled) return current
  const zram = (status?.swaps.devices ?? []).filter(isZram).reduce((a, d) => a + d.size_bytes, 0)
  const others = otherSwaps(status).reduce((a, d) => a + d.size_bytes, 0)
  const kept = managedSwap(status) ? others : s.existing === 'keep' ? others : 0
  return zram + kept + s.sizeGB * GiB
}

// setupRequest — тело POST /api/plan/setup.
export function setupRequest(s: Settings, status: Status | null): SetupRequest {
  const active = status?.profile?.name ?? ''
  // «Как в обычной Ubuntu» без применённого профиля — менять нечего.
  const profile = s.profile === 'default' && active === '' ? null : { name: s.profile }
  const swap = s.swapEnabled
    ? {
        size: s.sizeGB * GiB,
        ...(showExisting(status) ? { existing: s.existing } : {}),
        ...(s.path ? { path: s.path } : {}),
      }
    : null
  return { swap, profile }
}

// canApply — кнопка «Применить» в окне плана.
export function canApply(plan: Plan | null, ack: boolean): boolean {
  if (!plan || plan.blocked || plan.steps.length === 0) return false
  return !needsAck(plan) || ack
}

// needsAck — экспериментальный профиль: нужна галочка «Понимаю…».
export function needsAck(plan: Plan | null): boolean {
  return !!plan?.profile?.experimental
}

// pressure — уровень давления на память для карточки «Тормозит ли компьютер».
export type Pressure = 'normal' | 'elevated' | 'high' | 'severe' | 'na'
export function pressure(level: string | undefined): Pressure {
  return level === 'normal' || level === 'elevated' || level === 'high' || level === 'severe' ? level : 'na'
}

// Ошибки apply, после которых план строится заново.
export function needsReplan(code: string): boolean {
  return code === 'plan.stale' || code === 'plan.used' || code === 'plan.not_found'
}
