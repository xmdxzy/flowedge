// 节点使用频次：给左栏「常用」排序用。
// 与模板的 usage 分开存，两者键空间不同（节点是 type，模板是 id）。

const KEY = 'flowedge.node.usage.v1'

export interface NodeUsage {
  count: number
  lastUsed: number
}

export type NodeUsageMap = Record<string, NodeUsage>

export function readNodeUsage(): NodeUsageMap {
  try {
    const raw = localStorage.getItem(KEY)
    if (!raw) return {}
    const parsed = JSON.parse(raw)
    return parsed && typeof parsed === 'object' ? (parsed as NodeUsageMap) : {}
  } catch {
    return {}
  }
}

export function bumpNodeUsage(type: string): NodeUsageMap {
  const usage = readNodeUsage()
  const prev = usage[type]
  usage[type] = { count: (prev?.count || 0) + 1, lastUsed: Date.now() }
  try {
    localStorage.setItem(KEY, JSON.stringify(usage))
  } catch {
    /* 隐私模式写不进去，不影响本次会话 */
  }
  return usage
}

/** 取使用次数最多的 n 个节点类型 */
export function topNodeTypes(usage: NodeUsageMap, n = 4): string[] {
  return Object.entries(usage)
    .sort((a, b) => b[1].count - a[1].count || b[1].lastUsed - a[1].lastUsed)
    .slice(0, n)
    .map(([type]) => type)
}

// 折叠状态也跟着记忆，避免每次打开都要重新折叠
const COLLAPSE_KEY = 'flowedge.palette.collapsed.v1'

export function readCollapsed(): Record<string, boolean> | null {
  try {
    const raw = localStorage.getItem(COLLAPSE_KEY)
    if (!raw) return null
    const parsed = JSON.parse(raw)
    return parsed && typeof parsed === 'object' ? (parsed as Record<string, boolean>) : null
  } catch {
    return null
  }
}

export function writeCollapsed(map: Record<string, boolean>): void {
  try {
    localStorage.setItem(COLLAPSE_KEY, JSON.stringify(map))
  } catch {
    /* 忽略 */
  }
}
