import { useEffect, useState } from 'react'

import { fetchMCPTools, fetchToolchain, type MCPTool } from '../lib/toolchain'

// mcp_call 节点的参数区。
//
// 为什么不复用 agent_task 的下拉写法：MCP 是**两级**选择（先服务器、再工具），
// 而工具清单要现场握手才拿得到。做成一个自由填写的文本框，用户就得去翻
// mcp.json 和服务器源码——那是"能用"和"用不了"的分界线。
//
// 取值失败时全部退化为手填：探不通不代表不能先把图搭起来（运行时会给明确报错）。

interface Props {
  data: Record<string, unknown>
  onUpdate: (key: string, value: unknown) => void
}

export default function MCPCallFields({ data, onUpdate }: Props) {
  const [servers, setServers] = useState<{ name: string; enabled: boolean; usable: boolean }[]>([])
  const [tools, setTools] = useState<MCPTool[] | null>(null)
  const [loading, setLoading] = useState('')
  const [err, setErr] = useState('')

  const server = String(data.server || '')
  const tool = String(data.tool || '')

  useEffect(() => {
    let alive = true
    fetchToolchain(false)
      .then((v) => {
        if (!alive) return
        setServers(
          v.entries
            .filter((e) => e.kind === 'mcp')
            .map((e) => ({ name: e.id, enabled: e.enabled, usable: e.usable })),
        )
      })
      .catch(() => setServers([]))
    return () => {
      alive = false
    }
  }, [])

  // 选定服务器后自动取一次工具清单（用户已经在面板上明确选了这台）
  useEffect(() => {
    setTools(null)
    setErr('')
    if (!server) return
    let alive = true
    setLoading('tools')
    fetchMCPTools(server)
      .then((r) => {
        if (alive) setTools(r.tools)
      })
      .catch((e) => {
        if (alive) setErr(e instanceof Error ? e.message : String(e))
      })
      .finally(() => {
        if (alive) setLoading('')
      })
    return () => {
      alive = false
    }
  }, [server])

  return (
    <div className="flex flex-col gap-3">
      <div className="rounded-lg border border-sky-500/30 bg-sky-500/10 px-2 py-1.5">
        <p className="text-[10px] text-sky-300 leading-relaxed">
          调用的结果来自外部系统，<strong className="font-medium">不承诺可复现</strong>；
          先点「服务器」旁的重新探测确认它能通，再写工具名。
        </p>
      </div>

      <div>
        <label className="text-[10px] text-gray-500 block mb-1">MCP 服务器</label>
        <select
          value={server}
          onChange={(e) => {
            onUpdate('server', e.target.value)
            onUpdate('tool', '')
          }}
          className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
        >
          <option value="">— 选一台 —</option>
          {servers.map((s) => (
            <option key={s.name} value={s.name}>
              {s.name}
              {!s.enabled ? '（停用）' : s.usable ? '' : '（未探活）'}
            </option>
          ))}
          {/* 已配置但当前扫不到（服务器被删/配置移动）：仍显示，避免静默清空 */}
          {server && !servers.some((s) => s.name === server) && (
            <option value={server}>{server}（当前扫不到）</option>
          )}
        </select>
        {servers.length === 0 && (
          <p className="text-[10px] text-amber-400 mt-1 leading-relaxed">
            没有扫到 MCP 服务器。在「工具台 → MCP」里能看到扫过哪些配置文件，
            或直接手填服务器名（运行时按 data/mcp.json 与各客户端配置解析）。
          </p>
        )}
      </div>

      <div>
        <div className="flex items-center justify-between mb-1">
          <label className="text-[10px] text-gray-500">工具</label>
          {loading === 'tools' && <span className="text-[9px] text-gray-500">正在取工具清单…</span>}
        </div>
        {tools && tools.length > 0 ? (
          <>
            <select
              value={tool}
              onChange={(e) => onUpdate('tool', e.target.value)}
              className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
            >
              <option value="">— 选一个工具 —</option>
              {tools.map((t) => (
                <option key={t.name} value={t.name}>
                  {t.name}
                  {t.description ? ` · ${t.description.slice(0, 40)}` : ''}
                </option>
              ))}
            </select>
            {tool && (
              <pre className="mt-1 max-h-28 overflow-auto text-[9px] text-gray-500 whitespace-pre-wrap bg-canvas/60 rounded p-1.5">
                {JSON.stringify(tools.find((t) => t.name === tool)?.input_schema || {}, null, 2)}
              </pre>
            )}
          </>
        ) : (
          <input
            value={tool}
            onChange={(e) => onUpdate('tool', e.target.value)}
            placeholder="工具名（tools/list 里的 name）"
            className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs outline-none focus:border-primary"
          />
        )}
        {err && <p className="text-[10px] text-amber-400 mt-1 leading-relaxed">{err}（可手填工具名继续）</p>}
      </div>

      <div>
        <label className="text-[10px] text-gray-500 block mb-1">
          参数 JSON（可写 {'{{text}}'} 引用上游文本；留空则把上游文本包成 {'{"text": ...}'}）
        </label>
        <textarea
          value={String(data.args || '')}
          onChange={(e) => onUpdate('args', e.target.value)}
          rows={4}
          placeholder='{"query": "{{text}}", "limit": 5}'
          className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-[11px] font-mono outline-none focus:border-primary"
        />
      </div>

      <div>
        <label className="text-[10px] text-gray-500 block mb-1">超时（毫秒，默认 180000，上限 1800000）</label>
        <input
          value={String(data.timeout_ms ?? '')}
          onChange={(e) => onUpdate('timeout_ms', e.target.value ? Number(e.target.value) : undefined)}
          placeholder="180000"
          className="w-28 px-2 py-1 rounded-lg bg-canvas border border-border text-[11px] outline-none focus:border-primary"
        />
      </div>

      <label className="flex items-center gap-2 text-[11px] text-gray-400">
        <input
          type="checkbox"
          checked={data.allow_failure === true}
          onChange={(e) => onUpdate('allow_failure', e.target.checked)}
        />
        失败也往下传（is_error 不阻断，但 info 里如实记录）
      </label>
    </div>
  )
}
