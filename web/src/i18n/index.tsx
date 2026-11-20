import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from 'react'
import { setApiLang } from '../api'
import { en } from './en'
import { ru } from './ru'

export type Lang = 'ru' | 'en'
export type Params = Record<string, string | number>

export const dicts: Record<Lang, Record<string, string>> = { ru, en }

function interpolate(s: string, params?: Params): string {
  if (!params) return s
  return s.replace(/\{(\w+)\}/g, (m, k: string) => (k in params ? String(params[k]) : m))
}

// pluralForm — «1 попытка, 2 попытки, 5 попыток» / «1 attempt, 2 attempts».
export function pluralForm(lang: Lang, n: number, forms: string[]): string {
  if (lang === 'ru') {
    const m10 = n % 10
    const m100 = n % 100
    const i = m100 >= 11 && m100 <= 14 ? 2 : m10 === 1 ? 0 : m10 >= 2 && m10 <= 4 ? 1 : 2
    return forms[Math.min(i, forms.length - 1)] ?? ''
  }
  return (n === 1 ? forms[0] : forms[forms.length - 1]) ?? ''
}

export function translate(lang: Lang, key: string, params?: Params): string {
  const s = dicts[lang][key] ?? dicts.ru[key] ?? key
  return interpolate(s, params)
}

export function hasKey(lang: Lang, key: string): boolean {
  return key in dicts[lang]
}

const storageKey = 'ramplus.lang'

export function detectLang(): Lang {
  try {
    const saved = localStorage.getItem(storageKey)
    if (saved === 'ru' || saved === 'en') return saved
  } catch {
    /* хранилище недоступно */
  }
  const nav = typeof navigator !== 'undefined' ? navigator.language : 'ru'
  return nav.toLowerCase().startsWith('ru') ? 'ru' : 'en'
}

export interface I18n {
  lang: Lang
  setLang: (l: Lang) => void
  t: (key: string, params?: Params) => string
  // tp — ключ со словоформами через «|», n подставляется как {n}.
  tp: (key: string, n: number, params?: Params) => string
  has: (key: string) => boolean
}

const Ctx = createContext<I18n | null>(null)

export function makeI18n(lang: Lang, setLang: (l: Lang) => void = () => {}): I18n {
  return {
    lang,
    setLang,
    t: (key, params) => translate(lang, key, params),
    tp: (key, n, params) => interpolate(pluralForm(lang, n, translate(lang, key).split('|')), { n, ...params }),
    has: (key) => hasKey(lang, key),
  }
}

export function I18nProvider({ initial, children }: { initial?: Lang; children: ReactNode }) {
  const [lang, setLangState] = useState<Lang>(initial ?? detectLang())
  setApiLang(lang)
  const setLang = useCallback((l: Lang) => {
    setLangState(l)
    try {
      localStorage.setItem(storageKey, l)
    } catch {
      /* не сохраняется — не страшно */
    }
    document.documentElement.lang = l
  }, [])
  const value = useMemo(() => makeI18n(lang, setLang), [lang, setLang])
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}

export function useI18n(): I18n {
  const v = useContext(Ctx)
  if (!v) throw new Error('useI18n outside I18nProvider')
  return v
}
