import { useI18n, type I18n } from '../i18n'
import { profileName } from '../i18n/checks'
import { gb, when } from '../format'
import type { ChangeView, HistoryEntry } from '../types'

// historyTitle — название операции: по-русски — с сервера, иначе — из
// данных (change).
export function historyTitle(i: I18n, e: HistoryEntry, all: HistoryEntry[] = []): string {
  if (i.lang === 'ru' && e.title) return e.title
  const part = (c: ChangeView | undefined, action: string): string => {
    if (!c) return i.t('history.op.' + action)
    const out: string[] = []
    const swapChanged = c.swap_before_bytes !== c.swap_after_bytes
    const swapAction = ['apply', 'resize', 'remove', 'setup'].includes(action)
    if (swapAction && swapChanged) {
      if (c.swap_after_bytes == null) out.push(i.t('history.swap_removed'))
      else if (c.swap_before_bytes == null) out.push(i.t('history.swap_added', { v: gb(c.swap_after_bytes) }))
      else out.push(i.t('history.swap_resized', { a: gb(c.swap_before_bytes), b: gb(c.swap_after_bytes) }))
    }
    const profAction = ['profile-apply', 'profile-reset', 'setup'].includes(action)
    if (profAction && (action !== 'setup' || c.profile_after !== c.profile_before || c.profile_after)) {
      if (c.profile_after) out.push(i.t('history.mode', { mode: profileName(i, c.profile_after) }))
      else if (c.profile_before) out.push(i.t('history.mode_reset'))
    }
    return out.length ? out.join(' + ') : i.t('history.op.' + action)
  }
  if (e.action === 'rollback') {
    const target = all.find((x) => x.id === e.target)
    return target ? i.t('history.undo', { what: historyTitle(i, target, all) }) : i.t('history.op.rollback')
  }
  return part(e.change, e.action)
}

// historySource — откуда запрошена операция; старый журнал — "" (не показываем).
export function historySource(i: I18n, e: HistoryEntry): string {
  if (e.source === 'daemon' && e.requested_by?.via === 'web') return i.t('history.source.web')
  if (e.source === 'cli') return i.t('history.source.cli')
  if (e.source === 'daemon') return i.t('history.source.daemon')
  return ''
}

export function History({ entries, onUndo, busy }: { entries: HistoryEntry[]; onUndo: () => void; busy: boolean }) {
  const i = useI18n()
  const { t } = i
  const list = [...entries].reverse()
  return (
    <section id="history" aria-labelledby="history-title" className="card section-card">
      <div className="section-head">
        <h2 id="history-title" className="h2 h2-sm">
          {t('history.title')}
        </h2>
        <span className="muted">{t('history.hint')}</span>
      </div>
      {list.length === 0 && <p className="muted">{t('history.empty')}</p>}
      <ul className="history">
        {list.map((e) => {
          const w = when(e.started_at, i.lang)
          const at = w.key === 'date' ? w.date : t('history.' + w.key, { time: w.time })
          return (
            <li key={e.id} className="history-row">
              <span className="history-text">
                <span className="history-title">{historyTitle(i, e, entries)}</span>
                <span className="muted">
                  {at} · {t('history.status.' + e.status)}
                  {historySource(i, e) && <> · {historySource(i, e)}</>}
                </span>
              </span>
              {e.rollback_next && e.undoable && (
                <button type="button" className="btn" onClick={onUndo} disabled={busy}>
                  {t('history.undo_btn')}
                </button>
              )}
            </li>
          )
        })}
      </ul>
    </section>
  )
}
