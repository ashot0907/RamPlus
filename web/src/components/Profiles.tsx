import { useI18n } from '../i18n'
import { profileName } from '../i18n/checks'
import type { ProfileCatalog } from '../types'

export function Profiles({ catalog }: { catalog: ProfileCatalog | null }) {
  const i = useI18n()
  const { t } = i
  if (!catalog) return <p className="muted">{t('common.loading')}</p>
  return (
    <section className="section" aria-labelledby="prof-title">
      <a href="#/" className="link">
        ← {t('profiles.back')}
      </a>
      <h1 id="prof-title" className="h2">
        {t('profiles.title')}
      </h1>
      <p className="muted">{t('profiles.desc')}</p>
      <div className="card table-wrap">
        <table className="table">
          <caption className="sr-only">{t('profiles.title')}</caption>
          <thead>
            <tr>
              <th scope="col">{t('profiles.param')}</th>
              <th scope="col">{t('profiles.now')}</th>
              {catalog.profiles.map((p) => (
                <th scope="col" key={p.name}>
                  {profileName(i, p.name, p.friendly)}
                  <span className="th-sub">
                    {p.name} · {t('status.' + p.status)}
                  </span>
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {catalog.params.map((pr) => (
              <tr key={pr.key}>
                <th scope="row">
                  <code>{pr.key}</code>
                  <span className="th-sub">{i.has('param.' + pr.key) ? t('param.' + pr.key) : pr.title}</span>
                </th>
                <td>{pr.current ?? t('profiles.absent')}</td>
                {catalog.profiles.map((p) => (
                  <td key={p.name} className={p.params[pr.key] && p.params[pr.key] !== pr.current ? 'strong' : ''}>
                    {p.params[pr.key] ?? <span className="muted">{t('profiles.untouched')}</span>}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="card">
        <h2 className="h3">{t('profiles.set_title')}</h2>
        <p>{t('profiles.set_text')}</p>
        <pre className="cmd">sudo ramplus profile apply zswap-lz4 --set vm.swappiness=100</pre>
        <p className="muted small">{t('profiles.evidence')}</p>
      </div>
    </section>
  )
}
