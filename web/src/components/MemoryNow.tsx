import { useI18n } from '../i18n'
import { gb, pct } from '../format'
import { pressure } from '../logic'
import type { Status } from '../types'

function Bar({ value, color }: { value: number; color: 'ram' | 'ssd' | 'zswap' }) {
  return (
    <div className="bar" aria-hidden="true">
      <div className={`bar-fill bar-${color}`} style={{ width: `${Math.max(value, value > 0 ? 1 : 0)}%` }} />
    </div>
  )
}

// Spark — мини-график PSI some avg10 за последние ~30 с.
export function Spark({ values }: { values: number[] }) {
  const n = 30
  const pts = values.slice(-n)
  const max = Math.max(10, ...pts)
  const w = 120
  const h = 28
  const step = w / (n - 1)
  const offset = n - pts.length
  const d = pts.map((v, i) => `${i === 0 ? 'M' : 'L'}${((offset + i) * step).toFixed(1)},${(h - 2 - (v / max) * (h - 4)).toFixed(1)}`).join(' ')
  return (
    <svg className="spark" viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" aria-hidden="true">
      <line x1="0" y1={h - 2} x2={w} y2={h - 2} className="spark-base" />
      {pts.length > 1 && <path d={d} className="spark-line" />}
    </svg>
  )
}

export function MemoryNow({ status, psiHistory, live }: { status: Status | null; psiHistory: number[]; live: boolean }) {
  const { t } = useI18n()
  const m = status?.memory
  const z = status?.zswap
  const os = status?.system.os?.name || t('mem.system')
  const devices = status?.swaps.devices ?? []
  const ours = devices.some((d) => d.managed)
  const theirs = devices.some((d) => !d.managed)
  const owner = devices.length === 0 ? t('mem.swap_none') : ours && theirs ? t('mem.swap_both', { os }) : ours ? t('mem.swap_ours') : t('mem.swap_theirs', { os })
  const lvl = pressure(status?.psi.pressure_level)
  const zswapOn = z?.params.enabled === true
  return (
    <section aria-labelledby="now-title" className="section">
      <div className="section-head">
        <h2 id="now-title" className="h2">
          {t('mem.title')}
        </h2>
        <span className="muted live">
          <span className={live ? 'dot dot-live' : 'dot dot-off'} aria-hidden="true" />
          {live ? t('mem.live') : t('mem.paused')}
        </span>
      </div>
      <div className="cards4">
        <div className="card stat">
          <span className="stat-label">{t('mem.ram')}</span>
          <span className="stat-value">
            {gb(m?.used_bytes)} <span className="stat-of">{t('mem.of', { total: gb(m?.total_bytes) })}</span>
          </span>
          <Bar value={pct(m?.used_bytes ?? null, m?.total_bytes ?? null)} color="ram" />
          <span className="stat-hint">{t('mem.free', { v: gb(m?.available_bytes) })}</span>
        </div>
        <div className="card stat">
          <span className="stat-label">{t('mem.zswap')}</span>
          {zswapOn ? (
            <>
              <span className="stat-value">{z?.stored_bytes != null ? gb(z.stored_bytes) : t('mem.zswap_on')}</span>
              <Bar value={pct(z?.pool_bytes ?? null, m?.total_bytes ?? null)} color="zswap" />
              <span className="stat-hint">
                {z?.pool_bytes != null ? t('mem.zswap_pool', { v: gb(z.pool_bytes), c: z.params.compressor ?? '' }) : t('mem.zswap_on_hint')}
              </span>
            </>
          ) : (
            <>
              <span className="stat-value">{z?.present === false ? t('mem.zswap_absent') : t('mem.zswap_off')}</span>
              <Bar value={0} color="zswap" />
              <span className="stat-hint">{t('mem.zswap_off_hint')}</span>
            </>
          )}
        </div>
        <div className="card stat">
          <span className="stat-label">{t('mem.swap')}</span>
          <span className="stat-value">
            {gb(m?.swap_used_bytes)} <span className="stat-of">{t('mem.of', { total: gb(m?.swap_total_bytes) })}</span>
          </span>
          <Bar value={pct(m?.swap_used_bytes ?? null, m?.swap_total_bytes ?? null)} color="ssd" />
          <span className="stat-hint">{owner}</span>
        </div>
        <div className="card stat">
          <span className="stat-label">{t('mem.pressure')}</span>
          <span className="stat-value stat-pressure">
            <span className={`pdot pdot-${lvl}`} aria-hidden="true" />
            {t('pressure.' + lvl)}
          </span>
          <Spark values={psiHistory} />
          <span className="stat-hint">{t('pressure.' + lvl + '.hint')}</span>
        </div>
      </div>
    </section>
  )
}
