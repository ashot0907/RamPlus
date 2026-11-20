import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import { en } from './i18n/en'
import { makeI18n, pluralForm } from './i18n'
import { ru } from './i18n/ru'
import { checkText } from './i18n/checks'

function sourceFiles(dir: string): string[] {
  return readdirSync(dir).flatMap((f) => {
    const p = join(dir, f)
    if (statSync(p).isDirectory()) return sourceFiles(p)
    return /\.(ts|tsx)$/.test(f) && !/\.test\./.test(f) ? [p] : []
  })
}

// Коды проверок, которые отдаёт планировщик (internal/swap, profile,
// journal, core) — все переведены в обоих словарях.
const knownCodes = [
  'disk.usb', 'disk.hdd', 'disk.ok', 'disk_space.ok', 'disk_space.insufficient', 'path.not_writable_by_daemon',
  'rollback.old', 'rollback.other_domain', 'profile.experimental', 'swapoff_memory.insufficient', 'setup.nothing',
  'setup.swap_unchanged', 'existing.selected', 'zswap.missing', 'journal.interrupted', 'rollback.state_mismatch',
]

describe('словари ru/en', () => {
  it('одинаковые ключи в обоих словарях', () => {
    const r = Object.keys(ru).sort()
    const e = Object.keys(en).sort()
    expect(r.filter((k) => !(k in en))).toEqual([])
    expect(e.filter((k) => !(k in ru))).toEqual([])
    expect(r.length).toBeGreaterThan(200)
  })

  it('нет пустых переводов, плейсхолдеры совпадают', () => {
    for (const k of Object.keys(ru)) {
      expect(ru[k]?.trim(), k).not.toBe('')
      expect(en[k]?.trim(), k).not.toBe('')
      const ph = (s: string) => [...new Set([...s.matchAll(/\{(\w+)\}/g)].map((m) => m[1]))].sort().join(',')
      expect(ph(en[k] ?? ''), k).toBe(ph(ru[k] ?? ''))
    }
  })

  it('все статические ключи из кода есть в словарях', () => {
    const missing: string[] = []
    for (const f of sourceFiles(__dirname)) {
      const src = readFileSync(f, 'utf8')
      for (const m of src.matchAll(/\btp?\('([a-z_.-]+[a-z_-])'/g)) {
        const k = m[1] as string
        if (!(k in ru) || !(k in en)) missing.push(`${f}: ${k}`)
      }
    }
    expect(missing).toEqual([])
  })

  it('коды проверок переводятся; неизвестный код — message с сервера', () => {
    for (const lang of ['ru', 'en'] as const) {
      const i = makeI18n(lang)
      for (const code of knownCodes) {
        expect(checkText(i, { code, message: 'x' }).translated, `${lang} ${code}`).toBe(true)
      }
      expect(checkText(i, { code: 'brand.new', message: 'текст сервера' })).toEqual({ text: 'текст сервера', translated: false })
    }
  })

  it('русские словоформы', () => {
    const f = ['попытка', 'попытки', 'попыток']
    expect([1, 2, 5, 11, 21, 22, 25].map((n) => pluralForm('ru', n, f))).toEqual(['попытка', 'попытки', 'попыток', 'попыток', 'попытка', 'попытки', 'попыток'])
    expect(makeI18n('en').tp('login.left', 1)).toBe('1 attempt left before a pause.')
    expect(makeI18n('ru').tp('login.left', 3)).toBe('Осталось 3 попытки до паузы.')
  })
})
