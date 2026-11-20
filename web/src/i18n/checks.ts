import type { Check } from '../types'
import type { I18n, Params } from './index'

// checkText — текст проверки по коду; нет перевода — message с сервера.
// params — подстановки для кода (rollback.old: дата и возраст операции).
export function checkText(i: I18n, c: Pick<Check, 'code' | 'message'>, params?: Params): { text: string; translated: boolean } {
  const key = 'check.' + c.code
  if (i.has(key)) return { text: i.t(key, params), translated: true }
  return { text: c.message, translated: false }
}

// humanAge — «3 дня назад».
export function humanAge(i: I18n, seconds: number): string {
  if (seconds < 60) return i.t('age.now')
  if (seconds < 3600) return i.tp('age.minutes', Math.floor(seconds / 60))
  if (seconds < 48 * 3600) return i.tp('age.hours', Math.floor(seconds / 3600))
  return i.tp('age.days', Math.floor(seconds / 86400))
}

// profileName — название режима для человека.
export function profileName(i: I18n, name: string, fallback?: string): string {
  const key = `profile.${name}.name`
  return i.has(key) ? i.t(key) : (fallback ?? name)
}

export function profileDesc(i: I18n, name: string, fallback?: string): string {
  const key = `profile.${name}.desc`
  return i.has(key) ? i.t(key) : (fallback ?? '')
}

// diskKind — тип диска: nvme → NVMe.
export function diskKind(i: I18n, kind: string): string {
  const key = 'disk.' + kind
  return i.has(key) ? i.t(key) : kind
}
