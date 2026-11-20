// Экраны до главного: вход, «задайте пароль», «демон недоступен».
import { useEffect, useId, useState, type FormEvent } from 'react'
import { ApiError, api } from '../api'
import { useI18n } from '../i18n'
import type { SessionInfo } from '../types'
import { Header } from './Header'

function Shell({ children }: { children: React.ReactNode }) {
  return (
    <div className="app">
      <Header status={null} daemonOK={false} showNav={false} />
      <main className="gate">{children}</main>
    </div>
  )
}

export function InitNeeded({ onRetry }: { onRetry: () => void }) {
  const { t } = useI18n()
  return (
    <Shell>
      <section className="card gate-card" aria-labelledby="init-title">
        <h1 id="init-title" className="h2">
          {t('init.title')}
        </h1>
        <p>{t('init.text')}</p>
        <pre className="cmd">sudo ramplus init</pre>
        <p className="muted">{t('init.then')}</p>
        <button type="button" className="btn btn-primary" onClick={onRetry}>
          {t('common.refresh')}
        </button>
      </section>
    </Shell>
  )
}

export function DaemonDown({ detail, onRetry }: { detail: string; onRetry: () => void }) {
  const { t } = useI18n()
  return (
    <section className="card gate-card" aria-labelledby="down-title" role="alert">
      <h1 id="down-title" className="h2">
        {t('down.title')}
      </h1>
      <p>{t('down.text')}</p>
      <p className="label">{t('down.what')}</p>
      <pre className="cmd">sudo systemctl start ramplusd{'\n'}systemctl status ramplusd</pre>
      {detail && <p className="muted small">{detail}</p>}
      <button type="button" className="btn btn-primary" onClick={onRetry}>
        {t('common.retry')}
      </button>
    </section>
  )
}

export function Login({ onLogin }: { onLogin: (s: SessionInfo) => void }) {
  const { t, tp } = useI18n()
  const id = useId()
  const [pw, setPw] = useState('')
  const [err, setErr] = useState('')
  const [wait, setWait] = useState(0)
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (wait <= 0) return
    const timer = setTimeout(() => setWait(wait - 1), 1000)
    return () => clearTimeout(timer)
  }, [wait])

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy || wait > 0 || !pw) return
    setBusy(true)
    setErr('')
    try {
      onLogin(await api.login(pw))
    } catch (x) {
      setPw('')
      if (x instanceof ApiError && x.code === 'auth.rate_limited') {
        setWait(x.body.retry_after_seconds ?? 30)
      } else if (x instanceof ApiError && x.code === 'auth.invalid') {
        if (x.body.retry_after_seconds) setWait(x.body.retry_after_seconds)
        const left = x.body.attempts_left ?? 0
        setErr(left > 0 ? `${t('login.invalid')} ${tp('login.left', left)}` : t('login.invalid'))
      } else {
        setErr(x instanceof Error ? x.message : String(x))
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <Shell>
      <form className="card gate-card" onSubmit={submit} aria-labelledby="login-title">
        <h1 id="login-title" className="h2">
          {t('login.title')}
        </h1>
        <label htmlFor={id} className="label">
          {t('login.password')}
        </label>
        <input
          id={id}
          type="password"
          className="input"
          autoComplete="current-password"
          autoFocus
          value={pw}
          onChange={(e) => setPw(e.target.value)}
          aria-invalid={err !== ''}
          aria-describedby={`${id}-msg`}
        />
        <div id={`${id}-msg`} aria-live="polite">
          {wait > 0 && <p className="note note-warn">{t('login.wait', { s: wait })}</p>}
          {err && wait === 0 && <p className="note note-bad">{err}</p>}
        </div>
        <button type="submit" className="btn btn-primary btn-lg" disabled={busy || wait > 0 || pw === ''}>
          {t('login.submit')}
        </button>
        <p className="muted small">{t('login.hint')}</p>
      </form>
    </Shell>
  )
}
