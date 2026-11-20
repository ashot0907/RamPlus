import { useEffect, useId, useRef, useState } from 'react'
import { ApiError, api, stream } from '../api'
import { useI18n, type I18n } from '../i18n'
import { checkText, humanAge, profileName } from '../i18n/checks'
import { gb } from '../format'
import { canApply, needsAck, needsReplan } from '../logic'
import type { Check, EventView, Plan, Result } from '../types'

export type Phase = 'idle' | 'running' | 'done' | 'failed'

interface Props {
  plan: Plan
  subtitle: string
  // rebuild — построить план заново (stale/used/not_found).
  rebuild: () => Promise<Plan | null>
  onClose: (changed: boolean) => void
  // Для тестов отрисовки: начальное состояние.
  initialPhase?: Phase
}

const levelOrder: Record<string, number> = { fail: 0, warn: 1, info: 2, ok: 3 }

export function CheckList({ checks, rollback }: { checks: Check[]; rollback?: Plan['rollback'] }) {
  const i = useI18n()
  const params = rollback
    ? { date: new Date(rollback.started_at).toLocaleString(i.lang, { day: 'numeric', month: 'long', hour: '2-digit', minute: '2-digit' }), age: humanAge(i, rollback.age_seconds) }
    : undefined
  const shown = checks.filter((c) => c.code !== 'root').sort((a, b) => (levelOrder[a.level] ?? 9) - (levelOrder[b.level] ?? 9))
  return (
    <ul className="checks">
      {shown.map((c, n) => {
        const { text, translated } = checkText(i, c, params)
        return (
          <li key={n} className={`check check-${c.level}`}>
            <span className="check-dot" aria-hidden="true" />
            <span className="check-body">
              <span className="check-label">{i.t('level.' + c.level)}</span>
              <span className="check-text">{text}</span>
              {translated && c.message && (
                <details className="check-details">
                  <summary>{i.t('plan.details')}</summary>
                  <span className="check-msg">{c.message}</span>
                </details>
              )}
            </span>
          </li>
        )
      })}
    </ul>
  )
}

// changeRows — таблица «Параметр / Сейчас / Станет» из плана.
export function changeRows(i: I18n, p: Plan): { name: string; now: string; next: string }[] {
  const { t } = i
  const rows: { name: string; now: string; next: string }[] = []
  const s = p.summary
  if (p.action === 'rollback' || s.swap_before_bytes !== s.swap_after_bytes || p.steps.some((x) => x.kind.includes('swap'))) {
    rows.push({ name: t('plan.row.swap'), now: gb(s.swap_before_bytes), next: gb(s.swap_after_bytes) })
  }
  for (const d of s.disabled) {
    if (!d.startsWith('/')) continue
    rows.push({ name: d, now: t('plan.row.works'), next: s.deleted.includes(d) ? t('plan.row.deleted') : t('plan.row.disabled_kept') })
  }
  if (p.profile && !p.profile.reset && p.action !== 'rollback') {
    const cur = p.profile.current ? profileName(i, p.profile.current) : t('plan.row.no_profile')
    rows.push({ name: t('plan.row.mode'), now: cur, next: profileName(i, p.profile.name) })
  }
  if (p.profile?.reset) rows.push({ name: t('plan.row.mode'), now: p.profile.current ? profileName(i, p.profile.current) : '—', next: t('plan.row.baseline') })
  const ram = p.swap?.mem_total_bytes
  if (ram) rows.push({ name: t('plan.row.total'), now: gb(ram + s.swap_before_bytes), next: gb(ram + s.swap_after_bytes) })
  return rows
}

export function PlanDialog({ plan: initial, subtitle, rebuild, onClose, initialPhase = 'idle' }: Props) {
  const i = useI18n()
  const { t } = i
  const titleId = useId()
  const ackId = useId()
  const [plan, setPlan] = useState(initial)
  const [ack, setAck] = useState(false)
  const [phase, setPhase] = useState<Phase>(initialPhase)
  const [events, setEvents] = useState<EventView[]>([])
  const [result, setResult] = useState<Result | null>(null)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const dialogRef = useRef<HTMLDivElement>(null)
  const stopRef = useRef<(() => void) | null>(null)

  useEffect(() => {
    dialogRef.current?.querySelector<HTMLElement>('button, [href], input')?.focus()
    return () => stopRef.current?.()
  }, [])

  const close = () => {
    if (phase === 'running') return
    onClose(phase === 'done' || phase === 'failed')
  }

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') close()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  })

  const replan = async (msg: string) => {
    setNotice(msg)
    const p = await rebuild()
    if (p) {
      setPlan(p)
      setAck(false)
    }
  }

  const follow = (opID: string) => {
    stopRef.current = stream(
      `/api/operations/${encodeURIComponent(opID)}/events`,
      {
        step: (d) => setEvents((evs) => [...evs, d as EventView]),
        result: (d) => {
          const r = d as Result
          stopRef.current?.()
          setResult(r)
          setPhase(r.status === 'done' || r.status === 'nothing' ? 'done' : 'failed')
        },
      },
      () => {
        // Поток оборвался: узнать итог запросом.
        api
          .get<{ running: boolean; result?: Result; events: EventView[] }>(`/api/operations/${encodeURIComponent(opID)}`)
          .then((v) => {
            if (!v.running && v.result) {
              stopRef.current?.()
              setEvents(v.events)
              setResult(v.result)
              setPhase(v.result.status === 'done' ? 'done' : 'failed')
            }
          })
          .catch(() => {})
      },
    )
  }

  const apply = async () => {
    if (!canApply(plan, ack) || busy) return
    setBusy(true)
    setError('')
    setNotice('')
    try {
      const r = await api.post<{ operation_id?: string; status: string } & Partial<Result>>('/api/apply', {
        plan_id: plan.id,
        confirm_experimental: ack,
      })
      if (r.operation_id && r.status === 'running') {
        setPhase('running')
        follow(r.operation_id)
      } else {
        setResult(r as Result)
        setPhase('done')
      }
    } catch (x) {
      if (x instanceof ApiError && needsReplan(x.code)) await replan(t('plan.stale'))
      else if (x instanceof ApiError && x.code === 'plan.blocked' && x.body.plan) setPlan(x.body.plan)
      else if (x instanceof ApiError && x.code === 'op.busy') setError(t('plan.busy'))
      else if (x instanceof ApiError && x.code === 'op.locked') setError(t('plan.locked'))
      else if (x instanceof ApiError && x.code === 'profile.experimental_not_confirmed') setError(t('plan.need_ack'))
      else if (x instanceof ApiError && x.code === 'auth.forbidden') setError(t('plan.forbidden'))
      else setError(x instanceof Error ? x.message : String(x))
    } finally {
      setBusy(false)
    }
  }

  const fails = plan.checks.filter((c) => c.level === 'fail')
  const rows = changeRows(i, plan)
  const doneSteps = events.filter((e) => !e.undo && !e.error).length
  const undo = events.filter((e) => e.undo)
  const failed = events.find((e) => !e.undo && e.error)
  const heading = phase === 'running' ? t('plan.running_title') : phase === 'done' ? t('plan.done_title') : t('plan.title')
  const profTitle = plan.profile ? profileName(i, plan.profile.name) : ''

  return (
    <div className="overlay" role="presentation" onMouseDown={(e) => e.target === e.currentTarget && close()}>
      <div className="dialog" role="dialog" aria-modal="true" aria-labelledby={titleId} ref={dialogRef}>
        <div className="dialog-head">
          <div>
            <h2 id={titleId} className="h2">
              {heading}
            </h2>
            <span className="muted">{subtitle}</span>
          </div>
          <button type="button" className="btn btn-icon" aria-label={t('common.close')} onClick={close} disabled={phase === 'running'}>
            <svg width="20" height="20" viewBox="0 0 20 20" aria-hidden="true">
              <path d="M5 5l10 10M15 5L5 15" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
            </svg>
          </button>
        </div>
        <div className="dialog-body">
          {notice && (
            <p className="note note-info" role="status">
              {notice}
            </p>
          )}
          {plan.blocked && (
            <div className="note note-bad" role="alert">
              <strong>{t('plan.blocked')}</strong>
              <ul className="plain">
                {fails.map((c, n) => (
                  <li key={n}>{checkText(i, c).text}</li>
                ))}
              </ul>
            </div>
          )}
          {rows.length > 0 && (
            <section aria-labelledby={titleId + 'c'} className="dialog-section">
              <h3 id={titleId + 'c'} className="caps">
                {t('plan.changes')}
              </h3>
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th scope="col">{t('plan.col.param')}</th>
                      <th scope="col">{t('plan.col.now')}</th>
                      <th scope="col">{t('plan.col.next')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((r) => (
                      <tr key={r.name}>
                        <td>{r.name}</td>
                        <td className="muted">{r.now}</td>
                        <td className="strong">{r.next}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </section>
          )}
          <section aria-labelledby={titleId + 'k'} className="dialog-section">
            <h3 id={titleId + 'k'} className="caps">
              {t('plan.checks')}
            </h3>
            <CheckList checks={plan.checks} rollback={plan.rollback} />
          </section>
          <details className="tech">
            <summary>{t('plan.tech', { n: plan.steps.length })}</summary>
            <p className="small">
              {t('plan.command')} <code>sudo {plan.command}</code>
            </p>
            <ol className="steps">
              {plan.steps.map((s, n) => (
                <li key={n}>{s.description}</li>
              ))}
            </ol>
            {plan.profile && plan.profile.rows.some((r) => r.change) && (
              <div className="table-wrap">
                <table className="table small">
                  <tbody>
                    {plan.profile.rows
                      .filter((r) => r.change)
                      .map((r) => (
                        <tr key={r.key}>
                          <td>
                            <code>{r.key}</code>
                          </td>
                          <td className="muted">{r.current || '—'}</td>
                          <td className="strong">{r.new}</td>
                        </tr>
                      ))}
                  </tbody>
                </table>
              </div>
            )}
          </details>

          {phase === 'idle' && (
            <div className="dialog-foot-wrap">
              {needsAck(plan) && !plan.blocked && (
                <label className="ack" htmlFor={ackId}>
                  <input id={ackId} type="checkbox" checked={ack} onChange={(e) => setAck(e.target.checked)} />
                  <span>{t('plan.ack', { mode: profTitle })}</span>
                </label>
              )}
              {!plan.blocked && plan.steps.length === 0 && <p className="note note-info">{t('plan.nothing')}</p>}
              {error && (
                <p className="note note-bad" role="alert">
                  {error}
                </p>
              )}
              <div className="dialog-foot">
                <span className="muted small">{t('plan.undo_hint')}</span>
                <span className="foot-buttons">
                  <button type="button" className="btn" onClick={close}>
                    {plan.steps.length === 0 ? t('common.close') : t('common.cancel')}
                  </button>
                  <button type="button" className="btn btn-primary" onClick={apply} disabled={!canApply(plan, ack) || busy}>
                    {t('plan.apply')}
                  </button>
                </span>
              </div>
            </div>
          )}

          {phase !== 'idle' && (
            <section aria-live="polite" className="dialog-section">
              <h3 className="caps">{t('plan.progress', { i: Math.min(doneSteps + (phase === 'running' ? 1 : 0), plan.steps.length), n: plan.steps.length })}</h3>
              <ul className="progress">
                {plan.steps.map((s, n) => {
                  const ev = events.find((e) => !e.undo && e.index === n + 1)
                  const state = ev ? (ev.error ? 'error' : 'done') : phase === 'running' && n === doneSteps ? 'running' : 'waiting'
                  return (
                    <li key={n} className={`pstep pstep-${state}`}>
                      <span className="pstep-icon" aria-hidden="true">
                        {state === 'done' ? '✓' : state === 'running' ? '•' : state === 'error' ? '!' : ''}
                      </span>
                      <span>{s.description}</span>
                      <span className="pstep-state">{t('step.' + state)}</span>
                    </li>
                  )
                })}
              </ul>
            </section>
          )}
          {phase === 'done' && (
            <div className="note note-ok done-row" role="status">
              <span>
                <strong>{t('plan.done')}</strong> {result?.status === 'nothing' ? t('plan.nothing') : t('plan.done_text')}
              </span>
              <button type="button" className="btn btn-ok" onClick={() => onClose(true)}>
                {t('plan.to_main')}
              </button>
            </div>
          )}
          {phase === 'failed' && (
            <div className="note note-bad" role="alert">
              <p>
                <strong>{t('plan.failed')}</strong> {failed ? `${failed.step.description}: ${failed.error}` : (result?.error ?? '')}
              </p>
              {result?.status === 'rollback_failed' ? (
                <p>{t('plan.rollback_failed')}</p>
              ) : (
                <>
                  <p>{undo.length > 0 ? t('plan.rolled_back') : t('plan.nothing_changed')}</p>
                  {undo.length > 0 && (
                    <ul className="plain">
                      {undo.map((e, n) => (
                        <li key={n}>
                          {e.error ? '✗' : '✓'} {e.step.description}
                        </li>
                      ))}
                    </ul>
                  )}
                </>
              )}
              <button type="button" className="btn" onClick={() => onClose(true)}>
                {t('plan.to_main')}
              </button>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
