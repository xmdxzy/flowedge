import { useCallback, useEffect, useState } from 'react'

import { errorText, fetchPipelines } from './api/client'
import type { Pipeline } from './api/types'
import DepsDrawer from './components/DepsDrawer'
import ToastHost from './components/Toast'
import { applyTheme, getInitialPref, THEME_OPTIONS, watchSystemTheme, type ThemePref } from './lib/theme'
import ArtifactsPage from './pages/ArtifactsPage'
import ProducePage from './pages/ProducePage'
import RunsPage from './pages/RunsPage'

// FlowEdge 出片台：三段导航（出片 / 运行 / 产物）+ 右上角依赖抽屉。
// 没有画布、没有节点图——用户只选固定流水线并填字段，服务端组装图去跑。

type Tab = 'produce' | 'runs' | 'artifacts'

const TABS: { id: Tab; label: string }[] = [
  { id: 'produce', label: '出片' },
  { id: 'runs', label: '运行' },
  { id: 'artifacts', label: '产物' },
]

export default function App() {
  const [tab, setTab] = useState<Tab>('produce')
  const [pipelines, setPipelines] = useState<Pipeline[]>([])
  const [pipelinesLoading, setPipelinesLoading] = useState(true)
  const [pipelinesError, setPipelinesError] = useState('')
  const [focusRunId, setFocusRunId] = useState<number | null>(null)
  const [depsOpen, setDepsOpen] = useState(false)
  const [pref, setPref] = useState<ThemePref>(() => getInitialPref())

  const loadPipelines = useCallback(async () => {
    setPipelinesLoading(true)
    try {
      const list = await fetchPipelines()
      setPipelines(list)
      setPipelinesError('')
    } catch (err) {
      setPipelines([])
      setPipelinesError(errorText(err))
    } finally {
      setPipelinesLoading(false)
    }
  }, [])

  useEffect(() => {
    void loadPipelines()
  }, [loadPipelines])

  // 主题：偏好落 localStorage，system 时跟随系统外观变化
  useEffect(() => {
    applyTheme(pref)
    if (pref !== 'system') return
    return watchSystemTheme(() => {
      applyTheme('system')
    })
  }, [pref])

  // 出片提交成功 → 立刻切到运行页并聚焦这条
  const handleProduced = useCallback((runId: number) => {
    setFocusRunId(runId)
    setTab('runs')
  }, [])

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <span className="brand__mark" aria-hidden="true">
            ▚
          </span>
          <span className="brand__name">FlowEdge</span>
          <span className="brand__sub">出片台</span>
        </div>

        <nav className="tabs" aria-label="主导航">
          {TABS.map((t) => (
            <button
              key={t.id}
              className={`tab ${tab === t.id ? 'is-active' : ''}`}
              aria-current={tab === t.id ? 'page' : undefined}
              onClick={() => setTab(t.id)}
            >
              {t.label}
            </button>
          ))}
        </nav>

        <div className="topbar__right">
          <div className="segmented segmented--sm" role="group" aria-label="主题">
            {THEME_OPTIONS.map((opt) => (
              <button
                key={opt.value}
                className={`seg ${pref === opt.value ? 'is-on' : ''}`}
                title={opt.hint}
                aria-pressed={pref === opt.value}
                onClick={() => setPref(opt.value)}
              >
                {opt.icon}
              </button>
            ))}
          </div>
          <button className="btn btn--ghost btn--sm" onClick={() => setDepsOpen(true)}>
            依赖
          </button>
        </div>
      </header>

      <main className="main">
        {tab === 'produce' && (
          <ProducePage
            pipelines={pipelines}
            loading={pipelinesLoading}
            loadError={pipelinesError}
            onReload={() => void loadPipelines()}
            onProduced={handleProduced}
          />
        )}
        {tab === 'runs' && (
          <RunsPage pipelines={pipelines} focusRunId={focusRunId} onFocusRun={setFocusRunId} />
        )}
        {tab === 'artifacts' && <ArtifactsPage />}
      </main>

      {depsOpen && <DepsDrawer onClose={() => setDepsOpen(false)} />}
      <ToastHost />
    </div>
  )
}
