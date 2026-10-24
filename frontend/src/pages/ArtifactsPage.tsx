import { useCallback, useEffect, useMemo, useState } from 'react'

import { artifactURL, deleteArtifact, errorText, fetchArtifacts } from '../api/client'
import type { ArtifactItem } from '../api/types'
import { confirmDialog, toast } from '../components/Toast'
import {
  ARTIFACT_KINDS,
  countByKind,
  filterArtifacts,
  joinArtifactPath,
  kindOf,
  type ArtifactFilter,
} from '../lib/artifacts'
import { formatBytes, formatTime } from '../lib/format'

// 产物页：data/generated 下的图片 / 视频 / 音频。
// 图片给缩略图，视频音频给原生控件；点开是全屏预览，另有复制路径与删除（二次确认）。

const KIND_LABEL: Record<string, string> = { image: '图片', video: '视频', audio: '音频' }
const KIND_ICON: Record<string, string> = { image: '🖼', video: '🎬', audio: '🔊' }

function modTimeText(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '—'
  return formatTime(new Date(seconds * 1000).toISOString())
}

export default function ArtifactsPage() {
  const [dir, setDir] = useState('')
  const [items, setItems] = useState<ArtifactItem[]>([])
  const [filter, setFilter] = useState<ArtifactFilter>('all')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [preview, setPreview] = useState<ArtifactItem | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const data = await fetchArtifacts()
      setDir(data.dir)
      setItems(data.items)
      setError('')
    } catch (err) {
      setError(errorText(err))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  useEffect(() => {
    if (!preview) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setPreview(null)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [preview])

  const counts = useMemo(() => countByKind(items), [items])
  const shown = useMemo(() => filterArtifacts(items, filter), [items, filter])

  const copyPath = async (name: string) => {
    const full = joinArtifactPath(dir, name)
    try {
      await navigator.clipboard.writeText(full)
      toast(`已复制：${full}`, 'success')
    } catch {
      toast(`复制失败，路径是 ${full}`, 'error')
    }
  }

  const remove = async (name: string) => {
    const ok = await confirmDialog(`删除产物 ${name}？此操作不可撤销。`, {
      title: '删除产物',
      okText: '删除',
    })
    if (!ok) return
    try {
      await deleteArtifact(name)
      toast(`已删除 ${name}`, 'success')
      if (preview?.name === name) setPreview(null)
      await load()
    } catch (err) {
      toast(errorText(err), 'error')
    }
  }

  return (
    <div className="page page--artifacts">
      <div className="artifacts-bar">
        <div className="filters">
          <button
            className={`chip chip--filter ${filter === 'all' ? 'is-on' : ''}`}
            onClick={() => setFilter('all')}
          >
            全部 {counts.all}
          </button>
          {ARTIFACT_KINDS.map((kind) => (
            <button
              key={kind}
              className={`chip chip--filter ${filter === kind ? 'is-on' : ''}`}
              onClick={() => setFilter(kind)}
            >
              <span aria-hidden="true">{KIND_ICON[kind]}</span> {KIND_LABEL[kind]} {counts[kind]}
            </button>
          ))}
        </div>
        <div className="artifacts-bar__right">
          <span className="mono muted2" title="产物目录">
            {dir || 'data/generated'}
          </span>
          <button className="btn btn--ghost btn--sm" onClick={() => void load()} disabled={loading}>
            {loading ? '刷新中…' : '刷新'}
          </button>
        </div>
      </div>

      {error && (
        <div className="notice notice--error" role="alert">
          <p className="notice__title">产物目录读取失败</p>
          <pre className="notice__raw mono">{error}</pre>
        </div>
      )}

      <div className="page__scroll scroll">
        {!error && shown.length === 0 && (
          <p className="empty">
            {items.length === 0
              ? '还没有产物。出片成功后，文件会出现在这里。'
              : '这个分类下没有产物。'}
          </p>
        )}
        <div className="art-grid">
          {shown.map((item) => {
            const kind = kindOf(item.name, item.kind) ?? 'image'
            return (
              <article key={item.name} className="art-card">
                <div
                  className="art-card__media"
                  role="button"
                  tabIndex={0}
                  aria-label={`预览 ${item.name}`}
                  onClick={() => setPreview(item)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault()
                      setPreview(item)
                    }
                  }}
                >
                  {kind === 'image' && (
                    <img
                      src={artifactURL(item.name)}
                      alt={item.name}
                      loading="lazy"
                      className="art-card__img"
                    />
                  )}
                  {kind === 'video' && (
                    <video
                      src={artifactURL(item.name)}
                      className="art-card__video"
                      controls
                      preload="metadata"
                      onClick={(e) => e.stopPropagation()}
                    />
                  )}
                  {kind === 'audio' && (
                    <div className="art-card__audio" onClick={(e) => e.stopPropagation()}>
                      <audio src={artifactURL(item.name)} controls preload="metadata" />
                    </div>
                  )}
                </div>
                <div className="art-card__meta">
                  <p className="art-card__name mono" title={item.name}>
                    {item.name}
                  </p>
                  <p className="muted2">
                    {formatBytes(item.size)} · {modTimeText(item.mod_time)}
                  </p>
                </div>
                <div className="art-card__actions">
                  <button className="btn btn--ghost btn--sm" onClick={() => void copyPath(item.name)}>
                    复制路径
                  </button>
                  <button className="btn btn--danger btn--sm" onClick={() => void remove(item.name)}>
                    删除
                  </button>
                </div>
              </article>
            )
          })}
        </div>
      </div>

      {preview && (
        <div
          className="overlay"
          role="dialog"
          aria-modal="true"
          aria-label={preview.name}
          onClick={() => setPreview(null)}
        >
          <div className="overlay__body" onClick={(e) => e.stopPropagation()}>
            {(kindOf(preview.name, preview.kind) ?? 'image') === 'image' && (
              <img src={artifactURL(preview.name)} alt={preview.name} className="overlay__media" />
            )}
            {(kindOf(preview.name, preview.kind) ?? 'image') === 'video' && (
              <video src={artifactURL(preview.name)} className="overlay__media" controls autoPlay />
            )}
            {(kindOf(preview.name, preview.kind) ?? 'image') === 'audio' && (
              <audio src={artifactURL(preview.name)} controls autoPlay className="overlay__media" />
            )}
            <div className="overlay__bar">
              <span className="mono">{preview.name}</span>
              <span className="muted2">{formatBytes(preview.size)}</span>
              <button className="btn btn--ghost btn--sm" onClick={() => void copyPath(preview.name)}>
                复制路径
              </button>
              <button className="btn btn--danger btn--sm" onClick={() => void remove(preview.name)}>
                删除
              </button>
              <button className="btn btn--ghost btn--sm" onClick={() => setPreview(null)}>
                关闭
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
