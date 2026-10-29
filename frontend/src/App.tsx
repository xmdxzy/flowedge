import { lazy, Suspense, useCallback, useEffect, useState } from 'react'

import { errorText, fetchPipelines } from './api/client'
import type { Pipeline } from './api/types'
import DepsDrawer from './components/DepsDrawer'
import ToastHost from './components/Toast'
import { applyTheme, getInitialPref, THEME_OPTIONS, watchSystemTheme, type ThemePref } from './lib/theme'
import ArtifactsPage from './pages/ArtifactsPage'
// 画布按需加载：@xyflow/react 约 110KB gzip，不该让只用出片台的人付这份启动成本
const CanvasPage = lazy(() => import('./pages/CanvasPage'))
import ProducePage from './pages/ProducePage'
import RunsPage from './pages/RunsPage'

// FlowEdge：四段导航（出片 / 画布 / 运行 / 产物）+ 右上角依赖抽屉。
//
// 两个视图是同一份数据的两个入口（见 docs/画布与流水线统一-20261029.md）：
//   出片台 = 7 条预置流水线，填字段即出片（新手路径）；
//   画布   = 从模板出发改任意图（专家路径），与出片台共用引擎、校验、门与队列。

type Tab = 'produce' | 'canvas' | 'runs' | 'artifacts'

const TABS: { id: Tab; label: string }[] = [
  { id: 'produce', label: '出片' },
  { id: 'canvas', label: '画布' },
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
        {tab === 'canvas' && (
          <Suspense fallback={<div className="empty">正在载入画布…</div>}>
            <CanvasPage onRan={handleProduced} />
          </Suspense>
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
