import { useCallback, useEffect, useState } from 'react'

import {
  errorText,
  fetchDeps,
  fetchMcpConfig,
  reloadDeps,
  startAndWaitDep,
  stopDep,
  unloadDep,
} from '../api/client'
import { depSupportsUnload } from '../api/types'
import type { DepStatus, McpConfig } from '../api/types'
import { toast } from './Toast'

// 依赖抽屉：服务可不可用、端口、一键启停，外加给 DSH 用的 MCP 接入配置。
//
// 字段全部来自后端真实结构（backend/service/deps.go DepStatus、
// backend/handler/integration.go MCPConfig），没有猜的字段。

interface Props {
  onClose: () => void
}

export default function DepsDrawer({ onClose }: Props) {
  const [items, setItems] = useState<DepStatus[]>([])
  const [depsError, setDepsError] = useState('')
  const [loading, setLoading] = useState(false)
  const [pending, setPending] = useState('')
  const [mcp, setMcp] = useState<McpConfig | null>(null)
  const [mcpError, setMcpError] = useState('')

  const load = useCallback(async () => {
    try {
      const data = await fetchDeps()
      setItems(data.items)
      setDepsError('')
    } catch (err) {
      setDepsError(errorText(err))
    }
  }, [])

  useEffect(() => {
    setLoading(true)
    void load().finally(() => setLoading(false))
    // MCP 配置是静态的：开抽屉取一次即可
    void fetchMcpConfig()
      .then((cfg) => {
        setMcp(cfg)
        setMcpError('')
      })
      .catch((err) => setMcpError(errorText(err)))
  }, [load])

  // 抽屉开着时每 3 秒对一次账：别的终端起了服务，这里要能看见
  useEffect(() => {
    const timer = setInterval(() => void load(), 3000)
    return () => clearInterval(timer)
  }, [load])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const handleStart = async (dep: DepStatus) => {
    setPending(dep.name)
    try {
      await startAndWaitDep(dep.name)
      toast(`${dep.label} 已就绪`, 'success')
    } catch (err) {
      toast(errorText(err), 'error')
    } finally {
      setPending('')
      await load()
    }
  }

  const handleStop = async (dep: DepStatus) => {
    setPending(dep.name)
    try {
      await stopDep(dep.name)
      toast(`${dep.label} 已停止`, 'success')
    } catch (err) {
      toast(errorText(err), 'error')
    } finally {
      setPending('')
      await load()
    }
  }

  // 卸载模型但保留进程：省显存又不付冷启动（和「停止」是两件事）
  const handleUnload = async (dep: DepStatus) => {
    setPending(dep.name)
    try {
      const res = await unloadDep(dep.name)
      if (!res.supported) {
        toast(res.note || `${dep.label} 没有卸载接口`, 'error')
      } else if (res.freed_mb > 0) {
        toast(`${dep.label} 已释放约 ${(res.freed_mb / 1024).toFixed(1)}GB 显存（进程保留）`, 'success')
      } else {
        toast(`${dep.label} 已请求卸载；拿不到读数，无法确认回收量`, 'success')
      }
    } catch (err) {
      toast(errorText(err), 'error')
    } finally {
      setPending('')
      await load()
    }
  }

  const handleReload = async () => {
    try {
      await reloadDeps()
      toast('已重新读取 data/deps.json', 'success')
      await load()
    } catch (err) {
      toast(errorText(err), 'error')
    }
  }

  const copyMcp = async () => {
    if (!mcp) return
    try {
      await navigator.clipboard.writeText(JSON.stringify(mcp, null, 2))
      toast('已复制 MCP 配置', 'success')
    } catch {
      toast('复制失败，请手动选中文本', 'error')
    }
  }

  return (
    <div className="drawer-backdrop" onClick={onClose}>
      <aside
        className="drawer"
        role="dialog"
        aria-modal="true"
        aria-label="依赖"
        onClick={(e) => e.stopPropagation()}
      >
        <header className="drawer__hd">
          <h2 className="drawer__title">依赖</h2>
          <button className="drawer__x" onClick={onClose} aria-label="关闭">
            ×
          </button>
        </header>

        <div className="drawer__body scroll">
          <div className="drawer__row">
            <span className="muted2">
              配置在 <code className="mono">data/deps.json</code>（引擎工作目录下），改完点「重读配置」即时生效；端点变更需重启引擎。
            </span>
            <button className="btn btn--ghost btn--sm" onClick={() => void handleReload()}>
              重读配置
            </button>
          </div>

          {depsError && (
            <div className="notice notice--error" role="alert">
              <p className="notice__title">依赖状态读取失败</p>
              <pre className="notice__raw mono">{depsError}</pre>
            </div>
          )}

          {!depsError && items.length === 0 && (
            <p className="empty">{loading ? '读取中…' : '后端没有托管任何依赖（/api/deps 返回空）'}</p>
          )}

          <div className="deps">
            {items.map((dep) => (
              <div key={dep.name} className="dep-row">
                <span className={`dot ${dep.running ? 'dot--ok' : 'dot--off'}`} aria-hidden="true" />
                <div className="dep-row__main">
                  <div className="dep-row__head">
                    <span className="dep-row__label">{dep.label}</span>
                    <span className="mono muted2">{dep.name}</span>
                    <span className="muted2 mono">{dep.endpoint}</span>
                    {dep.pid ? <span className="muted2">PID {dep.pid}</span> : null}
                    {dep.autostart ? <span className="chip chip--quiet">自启</span> : null}
                    <span className={`chip ${dep.running ? 'chip--success' : 'chip--quiet'}`}>
                      {dep.busy ? '忙（无响应）' : dep.running ? '就绪' : '未启动'}
                    </span>
                  </div>
                  {dep.hint && (!dep.running || dep.busy) && <p className="dep-row__hint">{dep.hint}</p>}
                  {dep.last_error && <pre className="dep-row__err mono">{dep.last_error}</pre>}
                </div>
                <div className="dep-row__actions">
                  {dep.running ? (
                    <>
                      {depSupportsUnload(dep.name) && (
                        <button
                          className="btn btn--ghost btn--sm"
                          disabled={pending !== ''}
                          title="只释放模型显存，进程保留：下次出片不用等重启"
                          onClick={() => void handleUnload(dep)}
                        >
                          {pending === dep.name ? '处理中…' : '卸载显存'}
                        </button>
                      )}
                      {dep.managed ? (
                        <button
                          className="btn btn--ghost btn--sm"
                          disabled={pending !== ''}
                          onClick={() => void handleStop(dep)}
                        >
                          {pending === dep.name ? '停止中…' : '停止'}
                        </button>
                      ) : (
                        <span className="muted2">外部进程</span>
                      )}
                    </>
                  ) : (
                    <button
                      className="btn btn--primary btn--sm"
                      disabled={pending !== ''}
                      onClick={() => void handleStart(dep)}
                    >
                      {pending === dep.name ? '启动中…' : '启动'}
                    </button>
                  )}
                </div>
              </div>
            ))}
          </div>

          <section className="mcp">
            <div className="mcp__hd">
              <h3 className="drawer__subtitle">MCP 接入</h3>
              <button className="btn btn--ghost btn--sm" onClick={() => void copyMcp()} disabled={!mcp}>
                复制配置
              </button>
            </div>
            {mcpError && (
              <div className="notice notice--error" role="alert">
                <p className="notice__title">MCP 配置读取失败</p>
                <pre className="notice__raw mono">{mcpError}</pre>
              </div>
            )}
            {mcp && (
              <>
                <p className="muted2">
                  给 DSH / Claude Desktop 用。OpenAI 兼容 Base URL：
                  <span className="mono"> {mcp.openai_base_url}</span>
                </p>
                <pre className="mcp__pre mono scroll">{JSON.stringify(mcp, null, 2)}</pre>
                {mcp.notes.length > 0 && (
                  <ul className="mcp__notes">
                    {mcp.notes.map((note) => (
                      <li key={note}>{note}</li>
                    ))}
                  </ul>
                )}
              </>
            )}
          </section>
        </div>
      </aside>
    </div>
  )
}
