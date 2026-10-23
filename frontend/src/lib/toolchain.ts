// 工具链客户端。
//
// 与 lib/skillpacks.ts 的分工：
//   skillpacks  技能包的**归属审计**（这个 SKILL.md 算不算数）
//   toolchain   机器上**有什么工具、怎么用、能不能用**（四类统一，可操作）
//
// 四类工具共用一个条目形状（ToolEntry），面板才能用一套卡片渲染。
// kind 的取值固定为 agent | skill | mcp | plugin，顺序即面板页签顺序。

export type ToolKind = 'agent' | 'skill' | 'mcp' | 'plugin'

export interface ToolEntry {
  kind: ToolKind
  id: string
  name: string
  source?: string
  path?: string
  enabled: boolean
  /** true = 引擎可写（能改配置/删条目）；false = 别人的资产，只读 + 覆盖层启停 */
  managed: boolean
  /** 现在真的能用（与 enabled 分开：启用了但命令不在 PATH 也算不可用） */
  usable: boolean
  detail?: string
  /** 怎么用它（一句话） */
  use?: string
  actions?: string[]
  meta?: Record<string, unknown>
  probed?: boolean
}

export interface MCPConfigSource {
  path: string
  label: string
  managed: boolean
  exists: boolean
}

export interface ToolchainView {
  generated_at: string
  base_dir: string
  counts: Partial<Record<ToolKind, number>>
  usable: Partial<Record<ToolKind, number>>
  entries: ToolEntry[]
  sources: {
    skill?: string[]
    mcp?: MCPConfigSource[]
    plugin?: string[]
    agent?: string[]
  }
  problems?: string[]
}

export interface MCPTool {
  name: string
  title?: string
  description?: string
  input_schema?: Record<string, unknown>
}

export interface MCPProbeInfo {
  ok: boolean
  detail?: string
  server?: Record<string, unknown>
  protocol?: string
  tools?: MCPTool[]
  elapsed_ms?: number
}

export interface MCPCallResult {
  text: string
  is_error: boolean
  content?: Record<string, unknown>[]
  raw?: unknown
  elapsed_ms?: number
}

export interface MCPDraft {
  name: string
  type?: string
  command?: string
  args?: string[]
  env?: Record<string, string>
  url?: string
  headers?: Record<string, string>
}

export const KIND_LABEL: Record<ToolKind, string> = {
  agent: '智能体',
  skill: '技能',
  mcp: 'MCP',
  plugin: '插件',
}

export const KIND_HINT: Record<ToolKind, string> = {
  agent: '能接活儿的执行者。可执行的（CLI / OpenAI 兼容端点）走「外部智能体」节点；插件里的定义只读，可当提示词参考。',
  skill: '方法论与领域知识包。列在这里 = 引擎找得到；引用方式见每条说明。',
  mcp: '外部 MCP 服务器。探活通过后可用「MCP 工具」节点直接调用它的工具。',
  plugin: '插件。引擎自有的可启停（热注册，不重启）；WorkBuddy 侧的只读发现，技能与定义已并入本面板。',
}

// 统一错误处理：后端的 {error} 直接当异常信息抛（面板原样显示）。
// 不返回 null——"拿不到"与"拿到空"在这里是两件事，调用方必须分开处理。
async function jfetch<T>(url: string, init?: RequestInit): Promise<T> {
  const r = await fetch(url, init)
  if (!r.ok) {
    const body = await r.json().catch(() => ({}))
    throw new Error((body as { error?: string })?.error || `HTTP ${r.status}`)
  }
  return (await r.json()) as T
}

export function fetchToolchain(probe = false): Promise<ToolchainView> {
  return jfetch<ToolchainView>(`/api/toolchain${probe ? '?probe=1' : ''}`)
}

export function reloadToolchain(): Promise<{
  plugins: number
  node_types: string[]
  agents: number
  problems: string[]
  reload_count: number
}> {
  return jfetch('/api/toolchain/reload', { method: 'POST' })
}

export function probeMCP(name: string): Promise<MCPProbeInfo> {
  return jfetch(`/api/mcp/servers/${encodeURIComponent(name)}/probe`, { method: 'POST' })
}

export function fetchMCPTools(name: string): Promise<{ count: number; tools: MCPTool[] }> {
  return jfetch(`/api/mcp/servers/${encodeURIComponent(name)}/tools`)
}

export function callMCP(name: string, tool: string, args: Record<string, unknown>): Promise<MCPCallResult> {
  return jfetch(`/api/mcp/servers/${encodeURIComponent(name)}/call`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ tool, args }),
  })
}

export function setMCPEnabled(name: string, enabled: boolean): Promise<unknown> {
  return jfetch(`/api/mcp/servers/${encodeURIComponent(name)}/enabled`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled }),
  })
}

export function deleteMCP(name: string): Promise<unknown> {
  return jfetch(`/api/mcp/servers/${encodeURIComponent(name)}`, { method: 'DELETE' })
}

export function saveMCP(draft: MCPDraft): Promise<unknown> {
  return jfetch('/api/mcp/servers', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(draft),
  })
}

export function setPluginEnabled(name: string, enabled: boolean): Promise<unknown> {
  return jfetch(`/api/plugins/${encodeURIComponent(name)}/enabled`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled }),
  })
}

export function adoptAgent(command: string): Promise<unknown> {
  return jfetch('/api/toolchain/adopt', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ command }),
  })
}

export function setAgentEnabled(id: string, enabled: boolean): Promise<unknown> {
  return jfetch(`/api/toolchain/agents/${encodeURIComponent(id)}/enabled`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled }),
  })
}

export function deleteAgent(id: string): Promise<unknown> {
  return jfetch(`/api/agents/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

export async function fetchToolDoc(kind: 'skill' | 'agent', name: string): Promise<string | null> {
  try {
    const r = await fetch(`/api/toolchain/doc?kind=${kind}&name=${encodeURIComponent(name)}`)
    if (!r.ok) return null
    const d = await r.json()
    return typeof d?.doc === 'string' ? d.doc : null
  } catch {
    return null
  }
}

/** 状态点：可用 / 启用但用不了 / 停用 */
export function statusOf(e: ToolEntry): { label: string; tone: string } {
  if (!e.enabled) return { label: '停用', tone: 'text-gray-500' }
  if (e.usable) return { label: e.probed ? '可用' : '已启用', tone: 'text-green-400' }
  return { label: '不可用', tone: 'text-amber-400' }
}

export function kindTone(kind: ToolKind): string {
  switch (kind) {
    case 'agent':
      return 'border-violet-500/40 text-violet-300'
    case 'skill':
      return 'border-teal-500/40 text-teal-300'
    case 'mcp':
      return 'border-sky-500/40 text-sky-300'
    case 'plugin':
      return 'border-amber-500/40 text-amber-300'
  }
}
