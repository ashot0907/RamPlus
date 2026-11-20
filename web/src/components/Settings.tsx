import { useId, useState } from 'react'
import { useI18n } from '../i18n'
import { diskKind, profileDesc, profileName } from '../i18n/checks'
import { gb, GiB } from '../format'
import {
  managedSwap,
  maxSizeGB,
  orderProfiles,
  otherSwaps,
  profileBadge,
  recSizeGB,
  showExisting,
  sizeIsRecommended,
  type Settings as S,
} from '../logic'
import type { ProfileCatalog, Recommendation, Status } from '../types'

interface Props {
  settings: S
  onChange: (s: S) => void
  rec: Recommendation
  status: Status | null
  catalog: ProfileCatalog | null
  onPath: (path: string) => void
}

export function Badge({ kind, children }: { kind: 'rec' | 'std' | 'exp' | 'warn'; children: string }) {
  return <span className={`badge badge-${kind}`}>{children}</span>
}

function SwapCard({ settings: s, onChange, rec, status, onPath }: Omit<Props, 'catalog'>) {
  const i = useI18n()
  const { t } = i
  const [pickDisk, setPickDisk] = useState(false)
  const sliderId = useId()
  const recGB = recSizeGB(rec)
  const max = Math.max(maxSizeGB(rec), 1)
  const managed = managedSwap(status)
  const others = otherSwaps(status)
  const os = status?.system.os?.name || t('mem.system')
  const oldTotal = others.reduce((a, d) => a + d.size_bytes, 0)
  const target = rec.target

  if (!s.swapEnabled) {
    return (
      <section aria-labelledby="swap-title" className="card setting">
        <h3 id="swap-title" className="h3">
          {t('swap.title')}
        </h3>
        <div className="note note-warn" role="note">
          <strong>{t('swap.unavailable')}</strong> {i.has('rec.' + rec.swap_reason_code) ? t('rec.' + rec.swap_reason_code) : rec.swap_reason}
        </div>
        {rec.swap_reason_code !== 'blocked.usb' && rec.swap_reason_code !== 'blocked' && maxSizeGB(rec) >= 1 && (
          <button type="button" className="btn" onClick={() => onChange({ ...s, swapEnabled: true })}>
            {t('swap.enable_anyway')}
          </button>
        )}
        <DiskPicker rec={rec} open={pickDisk} setOpen={setPickDisk} onPath={onPath} />
      </section>
    )
  }

  return (
    <section aria-labelledby="swap-title" className="card setting">
      <div className="setting-head">
        <h3 id="swap-title" className="h3">
          {t('swap.title')}
        </h3>
        <p className="muted">{t('swap.desc')}</p>
      </div>
      <div className="big-row">
        <span className="big-ssd">+{s.sizeGB} GB</span>
        {sizeIsRecommended(s, rec) ? (
          <Badge kind="rec">{t('badge.recommended')}</Badge>
        ) : (
          recGB != null && (
            <button type="button" className="btn" onClick={() => onChange({ ...s, sizeGB: recGB })}>
              {t('swap.back_to_rec', { n: recGB })}
            </button>
          )
        )}
      </div>
      {managed && <p className="muted">{t('swap.managed_now', { v: gb(managed.size_bytes) })}</p>}
      <div className="slider">
        <label htmlFor={sliderId} className="label">
          {t('swap.size')}
        </label>
        <input
          id={sliderId}
          type="range"
          min={1}
          max={max}
          step={1}
          value={Math.min(s.sizeGB, max)}
          onChange={(e) => onChange({ ...s, sizeGB: Number(e.target.value) })}
          aria-valuetext={`${s.sizeGB} GB`}
        />
        <div className="slider-scale" aria-hidden="true">
          <span>1 GB</span>
          {recGB != null && <span>{t('swap.rec_n', { n: recGB })}</span>}
          <span>{max} GB</span>
        </div>
      </div>
      {target && (
        <div className="disk-row">
          <span>
            {t('swap.on_disk')} <strong>{diskKind(i, target.disk_kind)}</strong>
            {target.available_bytes != null && <> · {t('swap.free', { v: gb(target.available_bytes) })}</>}
          </span>
          {rec.targets.length > 1 && (
            <button type="button" className="btn btn-link" aria-expanded={pickDisk} onClick={() => setPickDisk(!pickDisk)}>
              {t('swap.other_disk')}
            </button>
          )}
        </div>
      )}
      <DiskPicker rec={rec} open={pickDisk} setOpen={setPickDisk} onPath={onPath} />
      {showExisting(status) && (
        <fieldset className="choice">
          <legend className="legend">{t('swap.existing_q', { os, v: gb(oldTotal) })}</legend>
          <button
            type="button"
            className={s.existing === 'replace' ? 'option selected' : 'option'}
            aria-pressed={s.existing === 'replace'}
            onClick={() => onChange({ ...s, existing: 'replace' })}
          >
            <span className="option-head">
              <span className="option-title">{t('swap.replace')}</span>
              {rec.existing === 'replace' && <Badge kind="rec">{t('badge.recommended')}</Badge>}
            </span>
            <span className="option-text">{t('swap.replace_text', { n: s.sizeGB })}</span>
          </button>
          <button
            type="button"
            className={s.existing === 'keep' ? 'option selected' : 'option'}
            aria-pressed={s.existing === 'keep'}
            onClick={() => onChange({ ...s, existing: 'keep' })}
          >
            <span className="option-head">
              <span className="option-title">{t('swap.keep')}</span>
              {rec.existing === 'keep' && <Badge kind="rec">{t('badge.recommended')}</Badge>}
            </span>
            <span className="option-text">
              {t('swap.keep_text', { total: Math.round((oldTotal / GiB + s.sizeGB) * 10) / 10, old: gb(oldTotal), os, n: s.sizeGB })}
            </span>
          </button>
        </fieldset>
      )}
    </section>
  )
}

function DiskPicker({ rec, open, setOpen, onPath }: { rec: Recommendation; open: boolean; setOpen: (b: boolean) => void; onPath: (p: string) => void }) {
  const i = useI18n()
  const name = useId()
  if (!open) return null
  return (
    <fieldset className="disks">
      <legend className="legend">{i.t('swap.pick_disk')}</legend>
      {rec.targets.map((tg) => (
        <label key={tg.mount_point} className={tg.usable ? 'disk' : 'disk disk-off'}>
          <input
            type="radio"
            name={name}
            checked={tg.selected}
            disabled={!tg.usable && !tg.selected}
            onChange={() => {
              onPath(tg.mount_point === '/' ? '' : tg.path)
              setOpen(false)
            }}
          />
          <span>
            <strong>{tg.mount_point}</strong> · {diskKind(i, tg.disk_kind)} · {tg.fs_type}
            {tg.available_bytes != null && <> · {i.t('swap.free', { v: gb(tg.available_bytes) })}</>}
            {tg.usable ? <span className="muted"> · {i.t('swap.up_to', { v: gb(tg.max_bytes) })}</span> : <span className="warn-text"> · {tg.problem}</span>}
          </span>
        </label>
      ))}
    </fieldset>
  )
}

function ModeCard({ settings: s, onChange, rec, catalog }: Omit<Props, 'status' | 'onPath'>) {
  const i = useI18n()
  const { t } = i
  const names = orderProfiles(catalog, rec)
  return (
    <section aria-labelledby="mode-title" className="card setting">
      <div className="setting-head">
        <h3 id="mode-title" className="h3">
          {t('mode.title')}
        </h3>
        <p className="muted">{t('mode.desc')}</p>
      </div>
      {names.map((n) => {
        const p = catalog?.profiles.find((x) => x.name === n)
        const badge = profileBadge(n, rec)
        const zswapProfile = p?.params['zswap.enabled'] === 'Y'
        const unavailable = zswapProfile && catalog?.zswap_present === false
        const preliminary = badge === 'recommended' && p?.status === 'experimental'
        return (
          <button
            key={n}
            type="button"
            className={s.profile === n ? 'option selected' : 'option'}
            aria-pressed={s.profile === n}
            disabled={unavailable}
            onClick={() => onChange({ ...s, profile: n })}
          >
            <span className="option-head">
              <span className="option-title">{profileName(i, n, p?.friendly)}</span>
              {badge === 'recommended' && <Badge kind="rec">{t('badge.recommended')}</Badge>}
              {badge === 'unchanged' && <Badge kind="std">{t('badge.unchanged')}</Badge>}
              {badge === 'expert' && <Badge kind="exp">{t('badge.expert')}</Badge>}
            </span>
            <span className="option-text">{profileDesc(i, n, p?.description)}</span>
            {preliminary && <span className="option-note">{t('mode.preliminary')}</span>}
            {unavailable && <span className="option-note">{t('mode.no_zswap')}</span>}
            {p?.active && <span className="option-active">{t('mode.active')}</span>}
          </button>
        )
      })}
      <a href="#/profiles" className="link">
        {t('mode.expert_link')}
      </a>
    </section>
  )
}

export function Settings(props: Props) {
  const { t } = useI18n()
  return (
    <section id="settings" aria-labelledby="settings-title" className="section">
      <div className="section-head-col">
        <h2 id="settings-title" className="h2">
          {t('settings.title')}
        </h2>
        <p className="muted">{t('settings.desc')}</p>
      </div>
      <div className="settings-grid">
        <SwapCard {...props} />
        <ModeCard {...props} />
      </div>
    </section>
  )
}
