import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import { I18nProvider, detectLang } from './i18n'
import './styles.css'

const lang = detectLang()
document.documentElement.lang = lang

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <I18nProvider initial={lang}>
      <App />
    </I18nProvider>
  </StrictMode>,
)
