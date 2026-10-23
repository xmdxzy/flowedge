import { useCallback, useEffect, useMemo, useState } from 'react'

import {
  KIND_HINT,
  KIND_LABEL,
  adoptAgent,
  callMCP,
  deleteAgent,
  deleteMCP,
  fetchMCPTools,
  fetchToolDoc,
  fetchToolchain,
  kindTone,
  probeMCP,
  reloadToolchain,
  saveMCP,
  setAgentEnabled,
  setMCPEnabled,
  setPluginEnabled,
  statusOf,
  type MCPTool,
  type ToolEntry,
  type ToolKind,
  type ToolchainView,
} from '../lib/toolchain'

// 工具台：这台机器上有哪些工具、怎么用、能不能用。
//
// 为什么四类放一个面板而不是四个入口：
// 用户的心智是「我要找个能干这件事的东西」，不是「我要找一个插件」。
// 分四个入口的结果是他只记得其中一个，其余三类永远找不到。
//
// 为什么每次都重扫（而不是按钮才刷新）：
// 「我刚装的东西不出现」是这个面板唯一会挨骂的地方。扫描几十毫秒，
// 换的是打开即真、装完即见。真正慢的只有 MCP 探活（要起子进程），
// 那一步必须由用户显式点。

interface Props {
  onClose: () => void
  onNotify?: (msg: string) => void
}

const TAB_ORDER: ToolKind[] = ['agent', 'skill', 'mcp', 'plugin']

export default function ToolchainPanel({ onClose, onNotify }: Props) {
  const [view, setView] = useState<ToolchainView | null>(null)
  const [tab, setTab] = useState<ToolKind>('mcp')
  const [q, setQ] = useState('')
  const [openId, setOpenId] = useState('')
  const [doc, setDoc] = useState<string | null>(null)
  const [tools, setTools] = useState<MCPTool[] | null>(null)
  const [busy, setBusy] = useState('')
  const [err, setErr] = useState('')
  const [callTool, setCallTool] = useState('')
  const [callArgs, setCallArgs] = useState('{}')
  const [callOut, setCallOut] = useState('')
  const [adding, setAdding] = useState(false)
  const [draft, setDraft] = useState({ name: '', type: 'stdio', command: '', args: '', url: '' })

  const load = useCallback(
    async (probe = false) => {
      try {
        setErr('')
        const v = await fetchToolchain(probe)
        setView(v)
      } catch (e) {
        setErr(e instanceof Error ? e.message : String(e))
      }
    },
    [],
  )

  useEffect(() => {
    load(false)
  }, [load])

  const notify = (m: string) => onNotify?.(m)

  const entries = useMemo(() => {
    const list = (view?.entries || []).filter((e) => e.kind === tab)
    const kw = q.trim().toLowerCase()
    if (!kw) return list
    return list.filter((e) =>
      `${e.name} ${e.id} ${e.source || ''} ${e.detail || ''}`.toLowerCase().includes(kw),
    )
  }, [view, tab, q])

  const toggleOpen = async (e: ToolEntry) => {
    if (openId === e.id) {
      setOpenId('')
      setDoc(null)
      setTools(null)
      setCallOut('')
      return
    }
    setOpenId(e.id)
    setDoc(null)
    setTools(null)
    setCallOut('')
    setCallTool('')
    if (e.kind === 'skill' || (e.kind === 'agent' && e.id.startsWith('def:'))) {
      setDoc('读取中…')
      const text = await fetchToolDoc(e.kind === 'skill' ? 'skill' : 'agent', e.id)
      setDoc(text ?? '（读不到正文）')
    }
    if (e.kind === 'mcp' && e.enabled) {
      setBusy(`tools:${e.id}`)
      try {
        const r = await fetchMCPTools(e.id)
        setTools(r.tools)
      } catch (ex) {
        setErr(ex instanceof Error ? ex.message : String(ex))
      } finally {
        setBusy('')
      }
    }
  }

  const doProbe = async (name: string) => {
    setBusy(`probe:${name}`)
    setErr('')
    try {
      const info = await probeMCP(name)
      notify(info.ok ? `${name}：${info.detail || '可用'}` : `${name}：${info.detail || '不可用'}`)
      await load(false)
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy('')
    }
  }

  const doReload = async () => {
    setBusy('reload')
    try {
      const r = await reloadToolchain()
      notify(
        `已重扫：插件 ${r.plugins} 个${r.node_types.length ? `（节点 ${r.node_types.join('/')}）` : ''}，智能体 ${r.agents} 个`,
      )
      await load(false)
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy('')
    }
  }

  const doProbeAll = async () => {
    setBusy('probe-all')
    try {
      await load(true)
      notify('全部 MCP 服务器已探活')
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy('')
    }
  }

  const act = async (key: string, fn: () => Promise<unknown>) => {
    setBusy(key)
    setErr('')
    try {
      await fn()
      await load(false)
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy('')
    }
  }

  const runTool = async (server: string) => {
    if (!callTool) {
      setErr('先选一个工具')
      return
    }
    let args: Record<string, unknown> = {}
    try {
      args = callArgs.trim() ? JSON.parse(callArgs) : {}
    } catch {
      setErr('参数不是合法 JSON')
      return
    }
    setBusy(`call:${server}`)
    setErr('')
    setCallOut('')
    try {
      const r = await callMCP(server, callTool, args)
      setCallOut(
        `${r.is_error ? '⚠ 返回错误' : '✓ 成功'} · ${r.elapsed_ms ?? 0}ms\n\n${r.text || '（无文本内容）'}`,
      )
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy('')
    }
  }

  const submitDraft = async () => {
    if (!draft.name.trim()) {
      setErr('服务器名不能为空')
      return
    }
    setBusy('add-mcp')
    try {
      await saveMCP({
        name: draft.name.trim(),
        type: draft.type,
        command: draft.type === 'stdio' ? draft.command.trim() : undefined,
        args: draft.type === 'stdio' ? draft.args.split(/\s+/).filter(Boolean) : undefined,
        url: draft.type !== 'stdio' ? draft.url.trim() : undefined,
      })
      setAdding(false)
      setDraft({ name: '', type: 'stdio', command: '', args: '', url: '' })
      await load(false)
      notify('已写入 data/mcp.json')
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy('')
    }
  }

  const counts = view?.counts || {}
  const usable = view?.usable || {}

  return (
    <div className="fixed inset-0 z-50 flex justify-end" onClick={onClose}>
      <div
        className="w-[600px] h-full fe-drawer fe-scroll-thin"
        onClick={(e) => e.stopPropagation()}
      >
        {/* 头部 */}
        <div className="fe-drawer__hd">
          <div className="flex items-center gap-2">
            <span className="text-sm text-white font-medium">工具台</span>
            <span className="text-[10px] text-gray-500">
              智能体 / 技能 / MCP / 插件 · 全盘发现，装完即见
            </span>
            <button onClick={onClose} className="fe-drawer__x">
              ✕
            </button>
          </div>
          <div className="mt-2 flex items-center gap-1.5 flex-wrap">
            <button
              onClick={doReload}
              disabled={busy === 'reload'}
              className="px-2 py-0.5 rounded border border-border text-[10px] text-gray-300 hover:text-white"
              title="重扫插件目录并热注册节点 + 重读智能体名册 + 清 MCP 探活缓存（不重启引擎）"
            >
              {busy === 'reload' ? '重扫中…' : '重新扫描'}
            </button>
            <button
              onClick={doProbeAll}
              disabled={busy === 'probe-all'}
              className="px-2 py-0.5 rounded border border-border text-[10px] text-gray-300 hover:text-white"
              title="对本轮未探活过的 MCP 服务器并行握手并取工具清单（会起子进程，几秒）"
            >
              {busy === 'probe-all' ? '探活中…' : '全部探活'}
            </button>
            <span className="text-[10px] text-gray-600">
              {view ? `扫描于 ${new Date(view.generated_at).toLocaleTimeString()}` : '加载中…'}
            </span>
          </div>
        </div>

        {/* 页签 + 搜索 */}
        <div className="px-4 py-2 flex items-center gap-2 border-b border-border">
          <div className="flex gap-1">
            {TAB_ORDER.map((k) => (
              <button
                key={k}
                onClick={() => {
                  setTab(k)
                  setOpenId('')
                }}
                className={`px-2 py-0.5 rounded-full border text-[10px] transition-all ${
                  tab === k
                    ? 'bg-primary/20 border-primary/50 text-indigo-200'
                    : 'border-border text-gray-500 hover:text-gray-300'
                }`}
              >
                {KIND_LABEL[k]}
                <span className="ml-1 text-gray-500">{counts[k] ?? 0}</span>
              </button>
            ))}
          </div>
          <input
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="搜索名称 / 来源…"
            className="ml-auto w-36 px-2 py-0.5 rounded-lg bg-canvas border border-border text-[10px] outline-none focus:border-primary"
          />
        </div>

        <p className="px-4 py-1.5 text-[10px] text-gray-500 border-b border-border/60 leading-relaxed">
          {KIND_HINT[tab]}
          <span className="ml-1 text-gray-600">
            可用 {usable[tab] ?? 0} / {counts[tab] ?? 0}
          </span>
        </p>

        {err && (
          <div className="px-4 py-1.5 text-[10px] text-red-300 bg-red-500/10 border-b border-red-500/30">
            {err}
            <button onClick={() => setErr('')} className="ml-2 text-gray-400 hover:text-white">
              收起
            </button>
          </div>
        )}

        {tab === 'mcp' && (
          <div className="px-4 py-1.5 border-b border-border flex items-center gap-2">
            <button
              onClick={() => setAdding((v) => !v)}
              className="text-[10px] text-gray-400 hover:text-white px-1.5 py-0.5 rounded border border-border"
            >
              {adding ? '取消' : '＋ 添加 MCP 服务器'}
            </button>
            <span className="text-[10px] text-gray-600">
              写入引擎自有 data/mcp.json；外部配置只读，启停走覆盖层
            </span>
          </div>
        )}

        {adding && tab === 'mcp' && (
          <div className="px-4 py-2 border-b border-border space-y-1.5 bg-canvas/40">
            <div className="flex gap-1.5">
              <input
                value={draft.name}
                onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                placeholder="服务器名（画布上引用它）"
                className="flex-1 px-2 py-1 rounded bg-canvas border border-border text-[10px] outline-none focus:border-primary"
              />
              <select
                value={draft.type}
                onChange={(e) => setDraft({ ...draft, type: e.target.value })}
                className="px-2 py-1 rounded bg-canvas border border-border text-[10px]"
              >
                <option value="stdio">stdio</option>
                <option value="http">http</option>
              </select>
            </div>
            {draft.type === 'stdio' ? (
              <>
                <input
                  value={draft.command}
                  onChange={(e) => setDraft({ ...draft, command: e.target.value })}
                  placeholder="命令（如 node 或 npx）"
                  className="w-full px-2 py-1 rounded bg-canvas border border-border text-[10px] outline-none focus:border-primary"
                />
                <input
                  value={draft.args}
                  onChange={(e) => setDraft({ ...draft, args: e.target.value })}
                  placeholder="参数（空格分隔，如 C:\\path\\server.mjs）"
                  className="w-full px-2 py-1 rounded bg-canvas border border-border text-[10px] outline-none focus:border-primary"
                />
              </>
            ) : (
              <input
                value={draft.url}
                onChange={(e) => setDraft({ ...draft, url: e.target.value })}
                placeholder="URL（如 http://127.0.0.1:9000/mcp）"
                className="w-full px-2 py-1 rounded bg-canvas border border-border text-[10px] outline-none focus:border-primary"
              />
            )}
            <button
              onClick={submitDraft}
              disabled={busy === 'add-mcp'}
              className="fe-btn-primary px-2 py-1 rounded text-[10px]"
            >
              {busy === 'add-mcp' ? '保存中…' : '保存'}
            </button>
          </div>
        )}

        {/* 列表 */}
        <div className="flex-1 overflow-y-auto p-3 space-y-1.5">
          {entries.map((e) => {
            const st = statusOf(e)
            const key = `${e.kind}:${e.id}`
            const open = openId === e.id
            const actions = e.actions || []
            return (
              <div
                key={key}
                className={`rounded-lg border bg-canvas/40 ${e.enabled ? 'border-border' : 'border-border/40 opacity-70'}`}
              >
                <button onClick={() => toggleOpen(e)} className="w-full text-left px-2.5 py-2">
                  <div className="flex items-start gap-2">
                    <span className={`shrink-0 mt-1 w-1.5 h-1.5 rounded-full ${
                      !e.enabled ? 'bg-gray-600' : e.usable ? 'bg-green-400' : 'bg-amber-400'
                    }`} />
                    <div className="flex-1 min-w-0">
                      <div className="text-[11px] text-gray-200 font-medium truncate">
                        {e.name}
                        <span className={`ml-1.5 px-1 py-px rounded border text-[9px] ${kindTone(e.kind)}`}>
                          {KIND_LABEL[e.kind]}
                        </span>
                        {e.source && (
                          <span className="ml-1.5 px-1 py-px rounded border border-border text-[9px] text-gray-500">
                            {e.source}
                          </span>
                        )}
                        {!e.managed && (
                          <span className="ml-1 px-1 py-px rounded border border-border text-[9px] text-gray-600">
                            只读
                          </span>
                        )}
                      </div>
                      <div className="text-[10px] text-gray-500 leading-snug truncate">{e.detail || '—'}</div>
                    </div>
                    <span className={`shrink-0 text-[9px] ${st.tone}`}>{st.label}</span>
                  </div>
                </button>

                {open && (
                  <div className="px-2.5 pb-2 border-t border-border/60">
                    {e.use && <p className="mt-1.5 text-[10px] text-gray-400 leading-relaxed">{e.use}</p>}
                    {e.path && (
                      <p className="mt-1 text-[9px] text-gray-600 font-mono truncate" title={e.path}>
                        {e.path}
                      </p>
                    )}

                    {/* 动作条 */}
                    <div className="mt-1.5 flex flex-wrap gap-1">
                      {actions.includes('probe') && (
                        <Btn onClick={() => doProbe(e.id)} busy={busy === `probe:${e.id}`}>
                          探活
                        </Btn>
                      )}
                      {actions.includes('enable') && (
                        <Btn
                          onClick={() =>
                            act(`en:${key}`, () =>
                              e.kind === 'mcp'
                                ? setMCPEnabled(e.id, true)
                                : e.kind === 'plugin'
                                  ? setPluginEnabled(e.id, true)
                                  : setAgentEnabled(e.id, true),
                            )
                          }
                          busy={busy === `en:${key}`}
                        >
                          启用
                        </Btn>
                      )}
                      {actions.includes('disable') && (
                        <Btn
                          onClick={() =>
                            act(`dis:${key}`, () =>
                              e.kind === 'mcp'
                                ? setMCPEnabled(e.id, false)
                                : e.kind === 'plugin'
                                  ? setPluginEnabled(e.id, false)
                                  : setAgentEnabled(e.id, false),
                            )
                          }
                          busy={busy === `dis:${key}`}
                        >
                          停用
                        </Btn>
                      )}
                      {actions.includes('adopt') && (
                        <Btn
                          onClick={() =>
                            act(`adopt:${key}`, () => adoptAgent(String(e.meta?.command || e.id.replace('cli:', ''))))
                          }
                          busy={busy === `adopt:${key}`}
                        >
                          纳管到名册
                        </Btn>
                      )}
                      {actions.includes('delete') && e.kind === 'mcp' && (
                        <Btn
                          danger
                          onClick={() => {
                            if (confirm(`确定删除 MCP 服务器 ${e.id}？（只删 data/mcp.json 里的条目）`)) {
                              act(`del:${key}`, () => deleteMCP(e.id))
                            }
                          }}
                          busy={busy === `del:${key}`}
                        >
                          删除
                        </Btn>
                      )}
                      {actions.includes('delete') && e.kind === 'agent' && (
                        <Btn
                          danger
                          onClick={() => {
                            if (confirm(`从名册删除智能体 ${e.id}？`)) {
                              act(`del:${key}`, () => deleteAgent(e.id))
                            }
                          }}
                          busy={busy === `del:${key}`}
                        >
                          删除
                        </Btn>
                      )}
                    </div>

                    {/* 正文（技能包 / 插件里的智能体定义） */}
                    {(e.kind === 'skill' || (e.kind === 'agent' && e.id.startsWith('def:'))) && (
                      <pre className="mt-1.5 max-h-56 overflow-auto text-[9px] leading-relaxed text-gray-400 whitespace-pre-wrap">
                        {doc === null ? '展开读取中…' : doc.slice(0, 8000)}
                      </pre>
                    )}

                    {/* MCP 工具清单 + 试跑 */}
                    {e.kind === 'mcp' && e.enabled && (
                      <div className="mt-1.5">
                        {busy === `tools:${e.id}` && (
                          <p className="text-[10px] text-gray-500">正在握手取工具清单…</p>
                        )}
                        {tools && (
                          <>
                            <p className="text-[9px] text-gray-500 mb-1">
                              {tools.length} 个工具 · 点一个填进试跑目标
                            </p>
                            <div className="max-h-32 overflow-y-auto space-y-0.5">
                              {tools.map((t) => (
                                <button
                                  key={t.name}
                                  onClick={() => setCallTool(t.name)}
                                  className={`w-full text-left px-1.5 py-0.5 rounded text-[10px] ${
                                    callTool === t.name
                                      ? 'bg-primary/20 text-indigo-200'
                                      : 'text-gray-400 hover:bg-canvas'
                                  }`}
                                  title={t.description}
                                >
                                  <span className="font-mono">{t.name}</span>
                                  {t.description && (
                                    <span className="ml-1 text-gray-600">{t.description.slice(0, 40)}</span>
                                  )}
                                </button>
                              ))}
                            </div>
                          </>
                        )}
                        {(tools?.length || 0) > 0 && (
                          <div className="mt-1.5 space-y-1">
                            <div className="flex items-center gap-1">
                              <span className="text-[9px] text-gray-500">试跑</span>
                              <span className="text-[10px] text-gray-300 font-mono">{callTool || '（未选）'}</span>
                              <Btn
                                onClick={() => runTool(e.id)}
                                busy={busy === `call:${e.id}`}
                                disabled={!callTool}
                              >
                                调用
                              </Btn>
                            </div>
                            <textarea
                              value={callArgs}
                              onChange={(ev) => setCallArgs(ev.target.value)}
                              rows={2}
                              placeholder='参数 JSON，如 {"text": "你好"}'
                              className="w-full px-2 py-1 rounded bg-canvas border border-border text-[10px] font-mono outline-none focus:border-primary"
                            />
                            {callOut && (
                              <pre className="max-h-40 overflow-auto text-[9px] leading-relaxed text-gray-300 whitespace-pre-wrap bg-black/20 rounded p-1.5">
                                {callOut}
                              </pre>
                            )}
                          </div>
                        )}
                      </div>
                    )}

                    {/* 其它元信息 */}
                    {e.kind === 'mcp' && e.meta?.shadowed != null && (
                      <p className="mt-1 text-[9px] text-amber-400/80">
                        同名还被这些位置定义（已忽略）：{(e.meta.shadowed as string[]).join('；')}
                      </p>
                    )}
                    {e.kind === 'plugin' && e.meta?.node_type != null && (
                      <p className="mt-1 text-[9px] text-sky-300/80">
                        画布节点类型：<span className="font-mono">{String(e.meta.node_type)}</span>
                      </p>
                    )}
                    {e.kind === 'agent' && e.meta?.command != null && (
                      <p className="mt-1 text-[9px] text-gray-600 font-mono">
                        {String(e.meta.command)}
                        {e.meta?.base_url ? ` · ${String(e.meta.base_url)}` : ''}
                      </p>
                    )}
                  </div>
                )}
              </div>
            )
          })}
          {entries.length === 0 && !busy && (
            <p className="text-[11px] text-gray-500 leading-relaxed">
              {q ? '没有匹配的条目，换个关键词。' : `这个页签下暂时没有条目。`}
              {tab === 'mcp' && '扫过的位置见下方来源列表——把服务器的 mcp.json 放到其中之一即可。'}
            </p>
          )}
        </div>

        {/* 脚注：扫了哪些位置 */}
        <div className="px-4 py-2 border-t border-border text-[9px] text-gray-600 space-y-0.5 max-h-28 overflow-y-auto">
          {(view?.sources?.mcp || []).length > 0 && tab === 'mcp' && (
            <p>
              MCP 来源：
              {(view?.sources.mcp || [])
                .map((s) => `${s.label}${s.exists ? '' : '（无）'}`)
                .join(' · ')}
            </p>
          )}
          {tab === 'plugin' && <p>插件来源：{(view?.sources?.plugin || []).join(' ; ')}</p>}
          {tab === 'skill' && <p>技能来源：{(view?.sources?.skill || []).join(' ; ')}</p>}
          {tab === 'agent' && <p>智能体来源：{(view?.sources?.agent || []).join(' ; ')}</p>}
          {(view?.problems || []).length > 0 && (
            <p className="text-amber-500/80">{(view?.problems || [])[0]}</p>
          )}
          <p>
            新增工具后无需重启：skills/MCP/插件/智能体每次打开本面板都会重扫；插件节点由「重新扫描」热注册。
          </p>
        </div>
      </div>
    </div>
  )
}

function Btn({
  children,
  onClick,
  busy,
  danger,
  disabled,
}: {
  children: React.ReactNode
  onClick: () => void
  busy?: boolean
  danger?: boolean
  disabled?: boolean
}) {
  return (
    <button
      onClick={onClick}
      disabled={busy || disabled}
      className={`px-1.5 py-0.5 rounded border text-[9px] transition-all ${
        danger
          ? 'border-red-500/40 text-red-300 hover:bg-red-500/10'
          : 'border-border text-gray-300 hover:text-white hover:bg-canvas'
      } ${busy || disabled ? 'opacity-50' : ''}`}
    >
      {busy ? '…' : children}
    </button>
  )
}
