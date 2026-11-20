import { useI18n, type Lang } from '../i18n'
import type { Status } from '../types'
import { Logo } from './Logo'

interface Props {
  status: Status | null
  daemonOK: boolean
  onLogout?: () => void
  showNav?: boolean
}

export function LangSwitch() {
  const { lang, setLang, t } = useI18n()
  const next: Lang = lang === 'ru' ? 'en' : 'ru'
  return (
    <button type="button" className="btn btn-ghost" onClick={() => setLang(next)} aria-label={t('header.language')}>
      {next.toUpperCase()}
    </button>
  )
}

export function Header({ status, daemonOK, onLogout, showNav = true }: Props) {
  const { t } = useI18n()
  const sys = status?.system
  const subtitle = sys
    ? [sys.hostname, sys.os?.pretty_name, t('header.kernel', { v: sys.kernel.split('-')[0] ?? sys.kernel })].filter(Boolean).join(' · ')
    : ''
  return (
    <header className="header">
      <div className="header-inner">
        <a href="#/" className="brand">
          <Logo />
          <span className="brand-text">
            <span className="brand-name">RamPlus</span>
            {subtitle && <span className="brand-sub">{subtitle}</span>}
          </span>
        </a>
        <div className="header-actions">
          {showNav && (
            <span className={daemonOK ? 'pill pill-ok' : 'pill pill-bad'} role="status">
              <span className="dot" aria-hidden="true" />
              {daemonOK ? t('header.running') : t('header.daemon_down')}
            </span>
          )}
          {showNav && (
            <a href="#history" className="btn btn-ghost">
              {t('header.history')}
            </a>
          )}
          <LangSwitch />
          {onLogout && (
            <button type="button" className="btn btn-ghost" onClick={onLogout}>
              {t('header.logout')}
            </button>
          )}
        </div>
      </div>
    </header>
  )
}
