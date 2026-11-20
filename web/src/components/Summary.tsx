import { useI18n } from '../i18n'
import { profileName } from '../i18n/checks'
import { gbNum } from '../format'
import { isRecommended, projectedSwap, type Settings } from '../logic'
import type { ProfileCatalog, Recommendation, Status } from '../types'

interface Props {
  settings: Settings
  rec: Recommendation
  status: Status | null
  catalog: ProfileCatalog | null
  onApply: () => void
  busy: boolean
}

export function Summary({ settings, rec, status, catalog, onApply, busy }: Props) {
  const i = useI18n()
  const { t } = i
  const recOK = isRecommended(settings, rec, status)
  const before = status?.memory.swap_total_bytes ?? 0
  const after = projectedSwap(settings, status)
  const ram = status?.memory.total_bytes ?? rec.ram_bytes
  const p = catalog?.profiles.find((x) => x.name === settings.profile)
  return (
    <section aria-labelledby="apply-title" className="summary">
      <div className="summary-text">
        <div className="summary-head">
          <h2 id="apply-title" className="summary-title">
            {t('summary.title')}
          </h2>
          <span className={recOK ? 'chip chip-rec' : 'chip chip-changed'}>{recOK ? t('summary.recommended') : t('summary.changed')}</span>
        </div>
        <span className="summary-line">
          {t('summary.line', { a: gbNum(before), b: gbNum(after), mode: profileName(i, settings.profile, p?.friendly) })}
        </span>
        <span className="summary-sub">{t('summary.total', { v: gbNum(ram + after) })}</span>
      </div>
      <button type="button" className="btn btn-primary btn-lg" onClick={onApply} disabled={busy}>
        {busy ? t('summary.building') : t('summary.apply')}
      </button>
    </section>
  )
}
