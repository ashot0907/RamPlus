import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import { History } from './components/History'
import { PlanDialog } from './components/PlanDialog'
import { Settings } from './components/Settings'
import { Summary } from './components/Summary'
import { catalog, plan, recommendation, status } from './fixtures'
import { I18nProvider, type Lang } from './i18n'
import { defaultSettings } from './logic'
import type { HistoryEntry } from './types'

const wrap = (lang: Lang, node: React.ReactNode) => renderToStaticMarkup(<I18nProvider initial={lang}>{node}</I18nProvider>)

// Кнопка «Применить» окна плана (последняя кнопка в подвале).
function applyButton(html: string, label: string): string {
  const all = [...html.matchAll(/<button[^>]*>([^<]*)<\/button>/g)].filter((m) => m[1] === label)
  return all.at(-1)?.[0] ?? ''
}

describe('окно плана', () => {
  const noop = () => {}
  const rebuild = async () => null
  it('экспериментальный профиль: галочка и неактивная «Применить»', () => {
    const html = wrap('ru', <PlanDialog plan={plan()} subtitle="" rebuild={rebuild} onClose={noop} />)
    expect(html).toContain('Понимаю: режим «Сжатие — быстрое» победил в наших тестах')
    expect(applyButton(html, 'Применить')).toContain('disabled')
    expect(html).toContain('Что изменится')
    expect(html).toContain('отключён, файл сохранён для отката')
  })
  it('заблокированный план: причины сверху, «Применить» неактивна, галочки нет', () => {
    const blocked = plan({ blocked: true, checks: [{ level: 'fail', code: 'disk.usb', title: 'диск', message: 'usb' }] })
    const html = wrap('ru', <PlanDialog plan={blocked} subtitle="" rebuild={rebuild} onClose={noop} />)
    expect(html).toContain('Этот план нельзя применить')
    expect(html).toContain('Диск подключён по USB')
    expect(html).not.toContain('Понимаю:')
    expect(applyButton(html, 'Применить')).toContain('disabled')
  })
  it('обычный план без эксперимента: «Применить» активна; английский', () => {
    const html = wrap('en', <PlanDialog plan={plan({ profile: undefined })} subtitle="" rebuild={rebuild} onClose={noop} />)
    expect(html).toContain('Planned changes')
    const btn = applyButton(html, 'Apply')
    expect(btn).not.toBe('')
    expect(btn).not.toContain('disabled')
  })
})

describe('главный экран: рекомендации', () => {
  const rec = recommendation()
  const st = status()
  const s = defaultSettings(rec)
  it('рекомендуемый размер и режим отмечены, выбор существующего swap показан', () => {
    const html = wrap('ru', <Settings settings={s} onChange={() => {}} rec={rec} status={st} catalog={catalog()} onPath={() => {}} />)
    expect(html).toContain('+16 GB')
    expect(html).toContain('рекомендуем 16 GB')
    expect(html).toContain('В Ubuntu уже есть swap на 8 GB')
    expect(html).toContain('Победил в наших тестах. Предварительно: подтверждаем на реальном SSD.')
    expect(html.indexOf('Сжатие — быстрое')).toBeLessThan(html.indexOf('Как в обычной Ubuntu'))
    expect(html).not.toContain('Вернуть рекомендуемые')
  })
  it('размер изменён — кнопка «Вернуть рекомендуемые», итог «вы изменили»', () => {
    const changed = { ...s, sizeGB: 20 }
    const html = wrap('ru', <Settings settings={changed} onChange={() => {}} rec={rec} status={st} catalog={catalog()} onPath={() => {}} />)
    expect(html).toContain('Вернуть рекомендуемые 16 GB')
    const sum = wrap('ru', <Summary settings={changed} rec={rec} status={st} catalog={catalog()} onApply={() => {}} busy={false} />)
    expect(sum).toContain('вы изменили рекомендуемые')
    expect(sum).toContain('Запас на SSD: 8 → 20 GB · Сжатие — быстрое')
  })
  it('swap не рекомендуется (USB) — причина вместо ползунка', () => {
    const usb = recommendation({ swap_size_bytes: null, swap_reason_code: 'blocked.usb' })
    const html = wrap('ru', <Settings settings={defaultSettings(usb)} onChange={() => {}} rec={usb} status={st} catalog={catalog()} onPath={() => {}} />)
    expect(html).toContain('Система на USB-диске')
    expect(html).not.toContain('type="range"')
  })
})

describe('история: откуда запрошена операция', () => {
  const entry = (over: Partial<HistoryEntry>): HistoryEntry => ({
    id: '1', action: 'setup', command: 'ramplus setup', status: 'committed', started_at: '2026-11-20T10:00:00Z',
    steps: 3, undoable: true, rollback_next: true, title: 'Запас 4 GB из SSD', source: 'unknown', ...over,
  })
  it('через веб / из терминала / старый журнал без пометки', () => {
    const html = wrap('ru', (
      <History
        entries={[
          entry({ id: 'a', source: 'daemon', requested_by: { uid: 997, user: 'ramplus-web', via: 'web' } }),
          entry({ id: 'b', source: 'cli', requested_by: { uid: 1000, user: 'ashot' }, rollback_next: false }),
          entry({ id: 'c', source: 'unknown', rollback_next: false }),
        ]}
        onUndo={() => {}}
        busy={false}
      />
    ))
    expect(html).toContain('через веб-интерфейс')
    expect(html).toContain('из терминала')
    expect(html.match(/ · через| · из терминала/g)?.length).toBe(2)
    const en = wrap('en', <History entries={[entry({ source: 'daemon', requested_by: { uid: 997, via: 'web' } })]} onUndo={() => {}} busy={false} />)
    expect(en).toContain('via the web interface')
  })
})
