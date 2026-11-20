import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, api, setCSRF, stream } from './api'
import { Drift } from './components/Drift'
import { DaemonDown, InitNeeded, Login } from './components/Gate'
import { Header } from './components/Header'
import { History } from './components/History'
import { MemoryNow } from './components/MemoryNow'
import { PlanDialog } from './components/PlanDialog'
import { Profiles } from './components/Profiles'
import { Settings } from './components/Settings'
import { Summary } from './components/Summary'
import { useI18n } from './i18n'
import { defaultSettings, isRecommended, setupRequest, type Settings as S } from './logic'
import type { History as Hist, Plan, ProfileCatalog, Recommendation, SessionInfo, Status, StatusFrame } from './types'

type Gate = 'loading' | 'init' | 'login' | 'main'

interface Dialog {
  plan: Plan
  subtitle: string
  rebuild: () => Promise<Plan | null>
}

function useHashRoute(): string {
  const [hash, setHash] = useState(() => window.location.hash)
  useEffect(() => {
    const on = () => setHash(window.location.hash)
    window.addEventListener('hashchange', on)
    return () => window.removeEventListener('hashchange', on)
  }, [])
  return hash
}

export function App() {
  const { t } = useI18n()
  const [gate, setGate] = useState<Gate>('loading')
  const [gateError, setGateError] = useState('')

  const checkSession = useCallback(async () => {
    try {
      const s = await api.session()
      setCSRF(s.csrf_token)
      setGate(!s.password_set ? 'init' : s.authenticated ? 'main' : 'login')
      setGateError('')
    } catch (e) {
      setGateError(e instanceof Error ? e.message : String(e))
    }
  }, [])

  useEffect(() => {
    void checkSession()
  }, [checkSession])

  if (gate === 'init') return <InitNeeded onRetry={checkSession} />
  if (gate === 'login')
    return (
      <Login
        onLogin={(s: SessionInfo) => {
          setCSRF(s.csrf_token)
          setGate('main')
        }}
      />
    )
  if (gate === 'loading')
    return (
      <div className="app">
        <main className="gate">
          <p className="muted">{gateError || t('common.loading')}</p>
          {gateError && (
            <button type="button" className="btn" onClick={checkSession}>
              {t('common.retry')}
            </button>
          )}
        </main>
      </div>
    )
  return (
    <Main
      onAuthLost={() => {
        setCSRF('')
        setGate('login')
      }}
    />
  )
}

function Main({ onAuthLost }: { onAuthLost: () => void }) {
  const i18n = useI18n()
  const { t } = i18n
  const route = useHashRoute()
  const [status, setStatus] = useState<Status | null>(null)
  const [live, setLive] = useState(false)
  const [psi, setPsi] = useState<number[]>([])
  const [rec, setRec] = useState<Recommendation | null>(null)
  const [hist, setHist] = useState<Hist | null>(null)
  const [catalog, setCatalog] = useState<ProfileCatalog | null>(null)
  const [settings, setSettings] = useState<S | null>(null)
  const [down, setDown] = useState('')
  const [dialog, setDialog] = useState<Dialog | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [path, setPath] = useState('')
  const pathRef = useRef('')

  // Ошибка API: сессия истекла → вход; демон недоступен → экран ошибки.
  const handle = useCallback(
    (e: unknown): void => {
      if (e instanceof ApiError && (e.code === 'auth.required' || e.code === 'auth.not_configured')) {
        onAuthLost()
        return
      }
      if (e instanceof ApiError && (e.code === 'daemon.unavailable' || e.status === 0)) {
        setDown(e.message)
        return
      }
      setError(e instanceof Error ? e.message : String(e))
    },
    [onAuthLost],
  )

  const loadRec = useCallback(
    async (p: string, reset: boolean) => {
      const r = await api.get<Recommendation>('/api/recommendation' + (p ? '?path=' + encodeURIComponent(p) : ''))
      setRec(r)
      setSettings((cur) => {
        const d = { ...defaultSettings(r), path: p }
        if (!cur || reset) return d
        // Другой диск: размер ограничен новым максимумом.
        const max = Math.max(1, Math.floor(r.swap_max_bytes / 1024 ** 3))
        return { ...cur, path: p, sizeGB: Math.min(cur.sizeGB, max), swapEnabled: d.swapEnabled && cur.swapEnabled }
      })
    },
    [],
  )

  const loadAll = useCallback(
    async (reset: boolean) => {
      try {
        const [st, h, cat] = await Promise.all([
          api.get<Status>('/api/status'),
          api.get<Hist>('/api/history'),
          api.get<ProfileCatalog>('/api/profiles'),
        ])
        setStatus(st)
        setHist(h)
        setCatalog(cat)
        await loadRec(pathRef.current, reset)
        setDown('')
      } catch (e) {
        handle(e)
      }
    },
    [handle, loadRec],
  )

  useEffect(() => {
    void loadAll(true)
  }, [loadAll])

  // Память сейчас: SSE раз в секунду; разрыв — проверить, что случилось.
  useEffect(() => {
    if (down) return
    const stop = stream(
      '/api/status/stream',
      {
        status: (d) => {
          const f = d as StatusFrame
          setStatus(f.status)
          setLive(true)
          setPsi((h) => [...h.slice(-29), f.status.psi.some?.avg10 ?? 0])
        },
      },
      () => {
        setLive(false)
        api.get('/api/health').catch(handle)
      },
    )
    return stop
  }, [down, handle])

  const openPlan = async (build: () => Promise<Plan>, subtitle: string) => {
    setBusy(true)
    setError('')
    try {
      const plan = await build()
      const rebuild = async () => {
        try {
          return await build()
        } catch (e) {
          handle(e)
          return null
        }
      }
      setDialog({ plan, subtitle, rebuild })
    } catch (e) {
      handle(e)
    } finally {
      setBusy(false)
    }
  }

  const onApply = () => {
    if (!settings) return
    const body = setupRequest(settings, status)
    const sub = isRecommended(settings, rec, status) ? t('plan.sub.recommended') : t('plan.sub.custom')
    void openPlan(() => api.post<Plan>('/api/plan/setup', body), sub)
  }

  const onUndo = () => void openPlan(() => api.post<Plan>('/api/plan/rollback', {}), t('plan.sub.undo'))

  const onRestore = () => {
    const name = status?.profile?.name
    if (name) void openPlan(() => api.post<Plan>('/api/plan/profile', { name }), t('plan.sub.restore'))
  }

  const onPath = (p: string) => {
    pathRef.current = p
    setPath(p)
    loadRec(p, false).catch(handle)
  }

  if (down) {
    return (
      <div className="app">
        <Header status={status} daemonOK={false} onLogout={() => api.logout().finally(onAuthLost)} />
        <main className="main">
          <DaemonDown detail={down} onRetry={() => void loadAll(false)} />
        </main>
      </div>
    )
  }

  const page = route === '#/profiles' ? 'profiles' : 'main'
  return (
    <div className="app">
      <Header status={status} daemonOK={!down} onLogout={() => api.logout().catch(() => {}).finally(onAuthLost)} />
      <main className="main">
        {error && (
          <p className="note note-bad" role="alert">
            {error}{' '}
            <button type="button" className="btn btn-link" onClick={() => setError('')}>
              {t('common.close')}
            </button>
          </p>
        )}
        {page === 'profiles' ? (
          <Profiles catalog={catalog} />
        ) : (
          <>
            {status && <Drift status={status} onRestore={onRestore} busy={busy} />}
            <MemoryNow status={status} psiHistory={psi} live={live} />
            {rec && settings ? (
              <>
                <Settings settings={settings} onChange={setSettings} rec={rec} status={status} catalog={catalog} onPath={onPath} key={path} />
                <Summary settings={settings} rec={rec} status={status} catalog={catalog} onApply={onApply} busy={busy} />
              </>
            ) : (
              <p className="muted">{t('common.loading')}</p>
            )}
            <History entries={hist?.operations ?? []} onUndo={onUndo} busy={busy} />
            <p className="footnote">{t('footer.local')}</p>
          </>
        )}
      </main>
      {dialog && (
        <PlanDialog
          plan={dialog.plan}
          subtitle={dialog.subtitle}
          rebuild={dialog.rebuild}
          onClose={(changed) => {
            setDialog(null)
            if (changed) void loadAll(true)
          }}
        />
      )}
    </div>
  )
}
