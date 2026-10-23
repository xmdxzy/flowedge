// 外部智能体名册（客户端）。
//
// 两件事必须分开显示，否则面板会骗人：
//   enabled    —— 人配了没有
//   available  —— 真的能用没有（命令在不在 PATH、端点通不通）
// 只显示前者，用户会以为"配好了就能跑"，然后在节点上撞 command not found。

export type AgentKind = 'cli' | 'http'

export interface AgentStatus {
  id: string
  label: string
  kind: AgentKind
  enabled?: boolean
  command?: string
  args?: string[]
  stdin?: string
  workdir?: string
  base_url?: string
  model?: string
  credential?: string
  timeout_sec?: number
  capabilities?: string[]
  note?: string
  available: boolean
  detail?: string
}

function isEnabled(a: AgentStatus): boolean {
  return a.enabled === undefined || a.enabled === true
}

/** 下拉里可选的（已启用）；停用的仍列出来但标灰，避免"我明明配了怎么没有" */
export function selectableAgents(list: AgentStatus[]): AgentStatus[] {
  return list.filter(isEnabled)
}

export function agentStateLabel(a: AgentStatus): string {
  if (!isEnabled(a)) return '已停用'
  return a.available ? '可用' : '不可用'
}

export function agentStateClass(a: AgentStatus): string {
  if (!isEnabled(a)) return 'text-gray-500'
  return a.available ? 'text-green-400' : 'text-amber-400'
}

/** 拉名册。probe=false 跳过体检（快），true 时每个 agent 都做 LookPath / TCP dial */
export async function fetchAgents(probe = true): Promise<AgentStatus[]> {
  try {
    const r = await fetch(`/api/agents${probe ? '' : '?probe=0'}`)
    if (!r.ok) return []
    const d = await r.json()
    return Array.isArray(d?.agents) ? d.agents : []
  } catch {
    return []
  }
}

/** 单个体检（面板上「重新探测」按钮用） */
export async function probeAgent(id: string): Promise<AgentStatus | null> {
  try {
    const r = await fetch(`/api/agents/${encodeURIComponent(id)}/probe`, { method: 'POST' })
    if (!r.ok) return null
    const d = await r.json()
    return d?.agent || null
  } catch {
    return null
  }
}

/** 保存（增/改）。命令只存在服务端配置里，节点数据只放 agent_id。 */
export async function upsertAgent(def: Partial<AgentStatus> & { id: string }): Promise<string | null> {
  try {
    const r = await fetch('/api/agents', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(def),
    })
    if (!r.ok) {
      const d = await r.json().catch(() => null)
      return (d && d.error) || `保存失败（${r.status}）`
    }
    return null
  } catch (e) {
    return String(e)
  }
}

export async function deleteAgent(id: string): Promise<string | null> {
  try {
    const r = await fetch(`/api/agents/${encodeURIComponent(id)}`, { method: 'DELETE' })
    if (!r.ok) return `删除失败（${r.status}）`
    return null
  } catch (e) {
    return String(e)
  }
}
