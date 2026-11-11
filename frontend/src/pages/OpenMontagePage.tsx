import { useCallback, useEffect, useState } from 'react'

import {
  autoProduceOm,
  createOmProject,
  fetchOmHealth,
  fetchOmProject,
  fetchOmProjects,
  generateOmAudio,
  generateOmScript,
  generateOmStoryboard,
  generateOmVideo,
  saveOmEdit,
  errorText,
} from '../api/client'
import { toast } from '../components/Toast'
import type {
  OmCreateProjectBody,
  OmProject,
  OmProjectDetail,
} from '../api/types'

const STAGE_LABEL: Record<string, string> = {
  script: '剧本',
  storyboard: '分镜',
  generate: '视频生成',
  audio: '配音',
  edits: '剪辑导出',
}

export default function OpenMontagePage() {
  const [health, setHealth] = useState<{ ok: boolean; detail?: string }>({
    ok: false,
    detail: '未检查',
  })
  const [projects, setProjects] = useState<OmProject[]>([])
  const [projectsLoading, setProjectsLoading] = useState(false)
  const [detail, setDetail] = useState<OmProjectDetail | null>(null)
  const [creating, setCreating] = useState(false)
  const [newProjectName, setNewProjectName] = useState('')
  const [stageBusy, setStageBusy] = useState<Record<string, boolean>>({})
  const [autoProduceBusy, setAutoProduceBusy] = useState(false)
  const [autoProduceIdea, setAutoProduceIdea] = useState('')

  const checkHealth = useCallback(async () => {
    try {
      const body = await fetchOmHealth()
      setHealth({ ok: true, detail: JSON.stringify(body).slice(0, 200) })
    } catch (err) {
      setHealth({ ok: false, detail: errorText(err) })
    }
  }, [])

  const loadProjects = useCallback(async () => {
    setProjectsLoading(true)
    try {
      const list = await fetchOmProjects()
      setProjects(list)
    } catch (err) {
      toast('拉取项目失败：' + errorText(err), 'error')
    } finally {
      setProjectsLoading(false)
    }
  }, [])

  useEffect(() => {
    void checkHealth()
    void loadProjects()
  }, [checkHealth, loadProjects])

  const handleCreateProject = async () => {
    const name = newProjectName.trim()
    if (!name) return
    setCreating(true)
    try {
      const body: OmCreateProjectBody = { name }
      const created = await createOmProject(body)
      setProjects([created, ...projects])
      setNewProjectName('')
      setDetail(created)
    } catch (err) {
      toast('创建项目失败：' + errorText(err), 'error')
    } finally {
      setCreating(false)
    }
  }

  const handleSelectProject = async (id: number) => {
    try {
      const d = await fetchOmProject(id)
      setDetail(d)
    } catch (err) {
      toast('打开项目失败：' + errorText(err), 'error')
    }
  }

  const runStage = async (
    stage: string,
    fn: () => Promise<unknown>,
  ) => {
    setStageBusy({ ...stageBusy, [stage]: true })
    try {
      await fn()
      toast(`${STAGE_LABEL[stage] || stage} 完成`, 'success')
      if (detail) {
        void handleSelectProject(detail.id)
      }
    } catch (err) {
      toast(`${STAGE_LABEL[stage] || stage} 失败：${errorText(err)}`, 'error')
    } finally {
      setStageBusy({ ...stageBusy, [stage]: false })
    }
  }

  const handleAutoProduce = async () => {
    const idea = autoProduceIdea.trim()
    if (!idea) return
    setAutoProduceBusy(true)
    try {
      const body = { idea }
      await autoProduceOm(body)
      toast('一键出片已提交', 'success')
      void loadProjects()
    } catch (err) {
      toast('一键出片失败：' + errorText(err), 'error')
    } finally {
      setAutoProduceBusy(false)
    }
  }

  if (!health.ok) {
    return (
      <div className="empty om-standalone">
        <h2>OpenMontage Drama Studio 代理</h2>
        <p className="muted">OpenMontage 不可达：{health.detail}</p>
        <p className="hint">
          先按 <code>docs/OpenMontage-集成指南.md</code> 部署 OpenMontage（默认 http://127.0.0.1:8787），
          然后在 FlowEdge 侧设置 <code>OPENMONTAGE_BASE_URL</code> 环境变量并重启。
        </p>
      </div>
    )
  }

  return (
    <div className="om-page">
      <header className="om-header">
        <h1>OpenMontage Drama Studio</h1>
        <span className="badge badge--ok">已连接：{health.detail ? '可用' : '未知'}</span>
        <button className="btn btn--ghost btn--sm" onClick={checkHealth}>
          刷新状态
        </button>
      </header>

      <section className="om-section om-projects">
        <div className="om-section__head">
          <h2>项目</h2>
          <div className="om-create">
            <input
              className="input input--sm"
              placeholder="新建项目名称"
              value={newProjectName}
              onChange={(e) => setNewProjectName(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && handleCreateProject()}
            />
            <button className="btn btn--sm" disabled={creating} onClick={handleCreateProject}>
              {creating ? '创建中…' : '新建'}
            </button>
          </div>
        </div>
        {projectsLoading && <div className="empty">加载中…</div>}
        {projects.length === 0 && !projectsLoading && (
          <p className="empty">暂无项目。点「新建」创建第一个短剧/短视频项目。</p>
        )}
        <div className="om-project-list">
          {projects.map((p) => (
            <div
              key={p.id}
              className={`om-project-card ${detail?.id === p.id ? 'is-active' : ''}`}
              onClick={() => handleSelectProject(p.id)}
            >
              <div className="om-project-card__name">{p.name}</div>
              <div className="om-project-card__meta">
                {p.status} · {p.genre ?? '—'} · {p.aspect_ratio ?? '—'}
              </div>
            </div>
          ))}
        </div>
      </section>

      {detail && (
        <section className="om-section om-detail">
          <div className="om-section__head">
            <h2>{detail.name}</h2>
            <div className="om-meta">
              <span>{detail.status}</span>
              <span>{detail.genre ?? '—'}</span>
              <span>{detail.aspect_ratio ?? '—'}</span>
            </div>
          </div>

          <div className="om-stages">
            <div className="om-stage">
              <button
                className={`om-stage-btn ${stageBusy.script ? 'is-busy' : ''}`}
                disabled={stageBusy.script}
                onClick={() =>
                  runStage(
                    'script',
                    () =>
                      generateOmScript(detail.id, {
                        idea: detail.description ?? '',
                        genre: detail.genre ?? undefined,
                        aspect_ratio: detail.aspect_ratio ?? undefined,
                      }),
                  )
                }
              >
                {stageBusy.script ? '生成中…' : '生成剧本'}
              </button>
            </div>
            <div className="om-stage">
              <button
                className={`om-stage-btn ${stageBusy.storyboard ? 'is-busy' : ''}`}
                disabled={stageBusy.storyboard}
                onClick={() =>
                  runStage('storyboard', () => generateOmStoryboard(detail.id, {}))
                }
              >
                {stageBusy.storyboard ? '生成中…' : '生成分镜'}
              </button>
            </div>
            <div className="om-stage">
              <button
                className={`om-stage-btn ${stageBusy.generate ? 'is-busy' : ''}`}
                disabled={stageBusy.generate}
                onClick={() => runStage('generate', () => generateOmVideo(detail.id, {}))}
              >
                {stageBusy.generate ? '生成中…' : '生成视频'}
              </button>
            </div>
            <div className="om-stage">
              <button
                className={`om-stage-btn ${stageBusy.audio ? 'is-busy' : ''}`}
                disabled={stageBusy.audio}
                onClick={() => runStage('audio', () => generateOmAudio(detail.id, {}))}
              >
                {stageBusy.audio ? '合成中…' : '生成配音'}
              </button>
            </div>
            <div className="om-stage">
              <button
                className={`om-stage-btn ${stageBusy.edits ? 'is-busy' : ''}`}
                disabled={stageBusy.edits}
                onClick={() => runStage('edits', () => saveOmEdit(detail.id, { timeline: null }))}
              >
                {stageBusy.edits ? '导出中…' : '保存剪辑'}
              </button>
            </div>
          </div>

          <div className="om-autoproduce">
            <h3>一键出片（脚本 → 分镜 → 视频 → 配音 → 剪辑，一键完成）</h3>
            <div className="om-input-row">
              <input
                className="input"
                placeholder="一句话灵感（例如：10集复仇短剧，9:16，前3秒钩住观众）"
                value={autoProduceIdea}
                onChange={(e) => setAutoProduceIdea(e.target.value)}
              />
              <button
                className="btn"
                disabled={autoProduceBusy}
                onClick={handleAutoProduce}
              >
                {autoProduceBusy ? '出片中…' : '一键出片'}
              </button>
            </div>
          </div>

          <div className="om-json om-json--detail">
            <h3>项目详情（原始 JSON）</h3>
            <pre>{JSON.stringify(detail, null, 2)}</pre>
          </div>
        </section>
      )}
    </div>
  )
}
