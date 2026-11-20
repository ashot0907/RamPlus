export const GiB = 1024 ** 3
const MiB = 1024 ** 2

// gb — размер в «GB» интерфейса (двоичные гигабайты, как в CLI — GiB):
// 16 GB, 10.2 GB, 512 MB.
export function gb(bytes: number | null | undefined): string {
  if (bytes == null) return 'n/a'
  if (bytes === 0) return '0 GB'
  if (bytes < 0.1 * GiB) return `${Math.max(1, Math.round(bytes / MiB))} MB`
  const v = bytes / GiB
  return Number.isInteger(v) ? `${v} GB` : `${v.toFixed(1)} GB`
}

// gbNum — число GB (с одним знаком) без единиц.
export function gbNum(bytes: number): string {
  const v = bytes / GiB
  return Number.isInteger(v) ? String(v) : v.toFixed(1)
}

export function pct(part: number | null, whole: number | null): number {
  if (part == null || !whole) return 0
  return Math.min(100, Math.max(0, (part / whole) * 100))
}

// when — «сегодня, 10:12», «вчера, 18:40», «17 ноября» / «Nov 17».
export function when(iso: string, lang: string, now = new Date()): { key: 'today' | 'yesterday' | 'date'; time: string; date: string } {
  const d = new Date(iso)
  const time = d.toLocaleTimeString(lang, { hour: '2-digit', minute: '2-digit' })
  const date = d.toLocaleDateString(lang, { day: 'numeric', month: 'long' })
  const day = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime()
  const diff = Math.round((day(now) - day(d)) / 86400000)
  return { key: diff === 0 ? 'today' : diff === 1 ? 'yesterday' : 'date', time, date }
}
