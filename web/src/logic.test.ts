import { describe, expect, it } from 'vitest'
import { catalog, plan, recommendation, status } from './fixtures'
import { GiB, gb } from './format'
import {
  canApply,
  defaultSettings,
  isRecommended,
  orderProfiles,
  profileBadge,
  projectedSwap,
  setupRequest,
  showExisting,
  sizeIsRecommended,
} from './logic'

describe('рекомендации → настройки по умолчанию', () => {
  it('берёт размер, existing и профиль из /api/recommendation', () => {
    expect(defaultSettings(recommendation())).toEqual({ swapEnabled: true, sizeGB: 16, path: '', existing: 'replace', profile: 'zswap-lz4' })
  })
  it('swap не рекомендован — часть swap выключена', () => {
    const s = defaultSettings(recommendation({ swap_size_bytes: null, swap_reason_code: 'blocked.usb' }))
    expect(s.swapEnabled).toBe(false)
    expect(setupRequest(s, status()).swap).toBeNull()
  })
  it('existing: keep из рекомендации', () => {
    expect(defaultSettings(recommendation({ existing: 'keep' })).existing).toBe('keep')
  })
})

describe('значки «рекомендуем / вы изменили»', () => {
  const rec = recommendation()
  const st = status()
  const base = defaultSettings(rec)
  it('по умолчанию — рекомендуемые настройки', () => {
    expect(isRecommended(base, rec, st)).toBe(true)
    expect(sizeIsRecommended(base, rec)).toBe(true)
  })
  it('другой размер, existing, профиль или диск — «вы изменили»', () => {
    expect(isRecommended({ ...base, sizeGB: 20 }, rec, st)).toBe(false)
    expect(sizeIsRecommended({ ...base, sizeGB: 20 }, rec)).toBe(false)
    expect(isRecommended({ ...base, existing: 'keep' }, rec, st)).toBe(false)
    expect(isRecommended({ ...base, profile: 'default' }, rec, st)).toBe(false)
    expect(isRecommended({ ...base, path: '/mnt/x/ramplus.swapfile' }, rec, st)).toBe(false)
  })
  it('existing не важен, если старого swap нет', () => {
    const noSwap = status({ swaps: { devices: [], warnings: [] }, memory: { ...st.memory, swap_total_bytes: 0 } })
    expect(showExisting(noSwap)).toBe(false)
    expect(isRecommended({ ...base, existing: 'keep' }, rec, noSwap)).toBe(true)
  })
  it('значки режимов и порядок: рекомендуемый первым', () => {
    expect(profileBadge('zswap-lz4', rec)).toBe('recommended')
    expect(profileBadge('default', rec)).toBe('unchanged')
    expect(profileBadge('zswap-zstd', rec)).toBe('expert')
    expect(orderProfiles(catalog(), rec)).toEqual(['zswap-lz4', 'default', 'zswap-zstd'])
  })
})

describe('итог и запрос плана', () => {
  const st = status()
  const s = defaultSettings(recommendation())
  it('замена: ровно размер ползунка; оставить: старый + наш', () => {
    expect(projectedSwap(s, st)).toBe(16 * GiB)
    expect(projectedSwap({ ...s, existing: 'keep' }, st)).toBe(24 * GiB)
    expect(projectedSwap({ ...s, swapEnabled: false }, st)).toBe(8 * GiB)
  })
  it('POST /api/plan/setup', () => {
    expect(setupRequest(s, st)).toEqual({ swap: { size: 16 * GiB, existing: 'replace' }, profile: { name: 'zswap-lz4' } })
    // «Как в обычной Ubuntu» без применённого профиля — профиль не трогаем.
    expect(setupRequest({ ...s, profile: 'default' }, st).profile).toBeNull()
    const active = status({ profile: { name: 'zswap-lz4', status: 'experimental', applied_at: '' } })
    expect(setupRequest({ ...s, profile: 'default' }, active).profile).toEqual({ name: 'default' })
    expect(setupRequest({ ...s, path: '/mnt/d/ramplus.swapfile' }, st).swap?.path).toBe('/mnt/d/ramplus.swapfile')
  })
  it('свой swap уже есть — existing не отправляется', () => {
    const managed = status({ swaps: { devices: [{ path: '/ramplus.swapfile', type: 'file', size_bytes: 16 * GiB, used_bytes: 0, priority: 10, managed: true }], warnings: [] } })
    expect(setupRequest(s, managed).swap).toEqual({ size: 16 * GiB })
  })
})

describe('«Применить» в окне плана', () => {
  it('экспериментальный профиль — только с галочкой', () => {
    expect(canApply(plan(), false)).toBe(false)
    expect(canApply(plan(), true)).toBe(true)
  })
  it('план заблокирован — неактивна даже с галочкой', () => {
    expect(canApply(plan({ blocked: true }), true)).toBe(false)
  })
  it('без экспериментального профиля галочка не нужна; пустой план — нечего применять', () => {
    expect(canApply(plan({ profile: undefined }), false)).toBe(true)
    expect(canApply(plan({ steps: [] }), true)).toBe(false)
  })
})

describe('формат', () => {
  it('GB как в интерфейсе', () => {
    expect([0, 16 * GiB, 10.2 * GiB, 50 * 1024 ** 2, 512 * 1024 ** 2, null].map(gb)).toEqual(['0 GB', '16 GB', '10.2 GB', '50 MB', '0.5 GB', 'n/a'])
  })
})
