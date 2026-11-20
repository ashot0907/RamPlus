import { useState } from 'react'
import { useI18n } from '../i18n'
import type { Status } from '../types'

export function Drift({ status, onRestore, busy }: { status: Status; onRestore: () => void; busy: boolean }) {
  const { t } = useI18n()
  const [open, setOpen] = useState(false)
  if (status.drift.length === 0 || !status.profile) return null
  return (
    <div className="banner banner-warn" role="status">
      <div className="banner-row">
        <span>
          <strong>{t('drift.title')}</strong> {t('drift.text')}
        </span>
        <span className="banner-actions">
          <button type="button" className="btn btn-link-warn" aria-expanded={open} onClick={() => setOpen(!open)}>
            {t('drift.what')}
          </button>
          <button type="button" className="btn btn-warn" onClick={onRestore} disabled={busy}>
            {t('drift.restore')}
          </button>
        </span>
      </div>
      {open && (
        <ul className="drift-list">
          {status.drift.map((d) => (
            <li key={d.key}>
              <code>{d.key}</code>: {t('drift.item', { expected: d.expected, actual: d.actual })}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
