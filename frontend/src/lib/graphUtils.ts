import type { Edge, Node } from '@xyflow/react'
import { CANVAS_ONLY_TYPES } from '../config/graph'

// 画布几何与图结构工具（纯函数，便于单独推理与测试）

/** 剔除只用于画布的节点（批注 / 分组），保存与运行时用 */
export function stripCanvasOnlyNodes(nodes: Node[]): Node[] {
  return nodes
    .filter((n) => !CANVAS_ONLY_TYPES.has(n.type ?? ''))
    .map(({ width, height, selected, dragging, ...rest }) => rest)
}

/** 只保留两端都是可执行节点的连线（UI 节点的连接不能提交给引擎） */
export function stripCanvasOnlyEdges(nodes: Node[], edges: Edge[]): Edge[] {
  const executable = new Set(nodes.filter((n) => !CANVAS_ONLY_TYPES.has(n.type ?? '')).map((n) => n.id))
  return edges
    .filter((e) => executable.has(e.source) && executable.has(e.target))
    .map(({ selected, ...rest }) => rest)
}

export interface LayoutOptions {
  layerGap?: number // 层间距（横向）
  nodeGap?: number // 同层节点间距（纵向）
  originX?: number
  originY?: number
}

/**
 * 分层自动布局（Kahn 拓扑分层）。
 *
 * 为什么不用力导向：工作流是 DAG，用户关心的是「第几步」，不是能量最小。
 * 分层布局能让数据流向一眼看穿，且同一张图反复布局结果稳定（可复现）。
 * 孤立节点与环内节点排在最后一行，不会被静默丢弃。
 */
export function autoLayout(nodes: Node[], edges: Edge[], opts: LayoutOptions = {}): Node[] {
  const layerGap = opts.layerGap ?? 300
  const nodeGap = opts.nodeGap ?? 150
  const originX = opts.originX ?? 80
  const originY = opts.originY ?? 120

  const ids = nodes.filter((n) => !CANVAS_ONLY_TYPES.has(n.type ?? '')).map((n) => n.id)
  const idSet = new Set(ids)
  const indeg = new Map<string, number>()
  const succ = new Map<string, string[]>()
  ids.forEach((id) => {
    indeg.set(id, 0)
    succ.set(id, [])
  })
  for (const e of edges) {
    if (!idSet.has(e.source) || !idSet.has(e.target)) continue
    succ.get(e.source)!.push(e.target)
    indeg.set(e.target, (indeg.get(e.target) ?? 0) + 1)
  }

  const layer = new Map<string, number>()
  let frontier = ids.filter((id) => (indeg.get(id) ?? 0) === 0)
  frontier.forEach((id) => layer.set(id, 0))
  let guard = 0
  while (frontier.length > 0 && guard++ < ids.length + 5) {
    const next: string[] = []
    for (const id of frontier) {
      for (const t of succ.get(id) ?? []) {
        const cand = (layer.get(id) ?? 0) + 1
        const cur = layer.get(t)
        if (cur === undefined || cand > cur) layer.set(t, cand)
        indeg.set(t, (indeg.get(t) ?? 0) - 1)
        if ((indeg.get(t) ?? 0) === 0) next.push(t)
      }
    }
    frontier = next
  }

  // 环内 / 孤立节点：没有层号的统一放到最后一层（不让它们堆在原点）
  const maxLayer = Math.max(0, ...Array.from(layer.values()))
  ids.forEach((id) => {
    if (!layer.has(id)) layer.set(id, maxLayer + 1)
  })

  const byLayer = new Map<number, string[]>()
  ids.forEach((id) => {
    const l = layer.get(id) ?? 0
    if (!byLayer.has(l)) byLayer.set(l, [])
    byLayer.get(l)!.push(id)
  })

  // 同层内按原有 y 排序，保持用户手工调过的相对顺序
  const yOf = new Map(nodes.map((n) => [n.id, n.position.y]))
  const positions = new Map<string, { x: number; y: number }>()
  Array.from(byLayer.keys())
    .sort((a, b) => a - b)
    .forEach((l) => {
      const group = byLayer.get(l)!.sort((a, b) => (yOf.get(a) ?? 0) - (yOf.get(b) ?? 0))
      group.forEach((id, idx) => {
        positions.set(id, { x: originX + l * layerGap, y: originY + idx * nodeGap })
      })
    })

  return nodes.map((n) => {
    const p = positions.get(n.id)
    if (!p) return n // 批注 / 分组保持原位（用户摆的位置就是语义）
    return { ...n, position: p }
  })
}

/** 计算一组节点的包围盒，用于生成分组框 */
export function boundingBox(nodes: Node[], ids: string[], pad = 32) {
  const picked = nodes.filter((n) => ids.includes(n.id))
  if (picked.length === 0) return null
  let minX = Infinity
  let minY = Infinity
  let maxX = -Infinity
  let maxY = -Infinity
  for (const n of picked) {
    const x = n.position.x
    const y = n.position.y
    const w = n.width ?? n.measured?.width ?? 200
    const h = n.height ?? n.measured?.height ?? 80
    minX = Math.min(minX, x)
    minY = Math.min(minY, y)
    maxX = Math.max(maxX, x + w)
    maxY = Math.max(maxY, y + h)
  }
  return {
    x: minX - pad,
    y: minY - pad - 28, // 给分组标题留一条
    width: maxX - minX + pad * 2,
    height: maxY - minY + pad * 2 + 28,
  }
}

/** 生成一个不重复的节点 ID */
export function nextId(prefix: string, existing: Set<string>): string {
  let i = 1
  while (existing.has(`${prefix}-${i}`)) i++
  return `${prefix}-${i}`
}
