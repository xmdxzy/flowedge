import type {
  CatalogNode,
  GraphEdge,
  GraphEstimate,
  GraphNode,
  GraphReport,
  GraphTemplate,
  PortDef,
  PortKind,
} from '../api/graphTypes'

// 画布的纯逻辑层。
//
// 刻意把「该不该连」「估算怎么念」「问题挂在哪个节点上」抽成纯函数并单测：
// React Flow 在 jsdom 里跑不起来（要 ResizeObserver），
// 但真正会出错的是这些判断，不是画布渲染本身。

export function catalogMap(catalog: CatalogNode[]): Map<string, CatalogNode> {
  return new Map(catalog.map((c) => [c.type, c]))
}

export function portKinds(ports: PortDef[] | null | undefined): PortKind[] {
  if (!ports) return []
  return ports.map((p) => p.kind)
}

/**
 * 两个端口类型集合是否可连。
 * 与后端 kindsCompatible 逐条对齐：any 兼容一切；任一侧没有端口声明视为兼容。
 * 前端先拦一次只是为了让用户立刻看到反馈，**最终判定仍在后端**。
 */
export function kindsCompatible(src: PortKind[], dst: PortKind[]): boolean {
  if (src.length === 0 || dst.length === 0) return true
  if (src.includes('any') || dst.includes('any')) return true
  return src.some((k) => dst.includes(k))
}

export function canConnect(
  catalog: Map<string, CatalogNode>,
  srcType: string,
  dstType: string,
): boolean {
  const src = catalog.get(srcType)
  const dst = catalog.get(dstType)
  if (!src || !dst) return false
  return kindsCompatible(portKinds(src.out), portKinds(dst.in))
}

/** 连线被拒绝时可以照抄给用户的理由（含两端各有什么端口）。 */
export function connectRejection(catalog: Map<string, CatalogNode>, srcType: string, dstType: string): string {
  const src = catalog.get(srcType)
  const dst = catalog.get(dstType)
  if (!src || !dst) return `本版本没有 ${!src ? srcType : dstType} 这种节点`
  const s = portKinds(src.out).join('/') || '（无输出）'
  const d = portKinds(dst.in).join('/') || '（无输入）'
  return `连不上：${srcType} 产出 ${s}，而 ${dstType} 只接受 ${d}。本版本不做隐式转换`
}

/** 秒 → 人话。≤60 秒按秒，≤60 分按分，其余按小时。 */
export function humanDuration(sec: number): string {
  const s = Math.max(0, Math.round(sec))
  if (s < 60) return `${s} 秒`
  const m = s / 60
  if (m < 60) return m < 10 ? `${m.toFixed(1)} 分钟` : `${Math.round(m)} 分钟`
  return `${(m / 60).toFixed(1)} 小时`
}

/**
 * 估算的念法：**永远给区间**（AGENTS §0.3 不报单值）。
 * 主行是「乐观–P90」，副行是冷加载与硬上限。
 */
export function formatEstimate(est: GraphEstimate): { main: string; detail: string } {
  const main = `${humanDuration(est.optimistic_sec)} – ${humanDuration(est.p90_sec)}`
  const bits: string[] = [`P50 ${humanDuration(est.p50_sec)}`, `上限 ${humanDuration(est.worst_sec)}`]
  if (est.cold_load_sec > 0) bits.push(`含冷加载 ${humanDuration(est.cold_load_sec)}`)
  if (est.vram_peak_mb > 0) bits.push(`峰值显存 ${(est.vram_peak_mb / 1024).toFixed(1)} GB`)
  return { main, detail: bits.join(' · ') }
}

export interface NodeIssues {
  errors: string[]
  warns: string[]
}

/** 把一次体检报告摊到节点/连线上：画布据此染色，用户不用去读一整段 JSON。 */
export function issueIndex(report: GraphReport | null | undefined): {
  byNode: Map<string, NodeIssues>
  byEdge: Map<string, NodeIssues>
} {
  const byNode = new Map<string, NodeIssues>()
  const byEdge = new Map<string, NodeIssues>()
  const slot = (m: Map<string, NodeIssues>, key: string): NodeIssues => {
    const found = m.get(key)
    if (found) return found
    const fresh: NodeIssues = { errors: [], warns: [] }
    m.set(key, fresh)
    return fresh
  }
  const push = (level: 'error' | 'warn', bucket: NodeIssues, msg: string) => {
    if (level === 'error') bucket.errors.push(msg)
    else bucket.warns.push(msg)
  }
  if (!report) return { byNode, byEdge }

  for (const issue of report.dag ?? []) {
    for (const id of issue.nodes ?? []) push(issue.level, slot(byNode, id), issue.message)
  }
  for (const issue of report.ports ?? []) {
    for (const id of issue.nodes ?? []) push(issue.level, slot(byNode, id), issue.message)
    if (issue.edge_id) push(issue.level, slot(byEdge, issue.edge_id), issue.message)
  }
  for (const p of report.params ?? []) {
    push('error', slot(byNode, p.node_id), p.message)
  }
  return { byNode, byEdge }
}

export function summarizeReport(report: GraphReport | null | undefined): string {
  if (!report) return '还没体检'
  if (report.ok && report.warnings === 0) return `可以保存：${report.node_count} 个节点 / ${report.edge_count} 条连线`
  const parts: string[] = []
  if (report.blocking > 0) parts.push(`${report.blocking} 个错误`)
  if (report.warnings > 0) parts.push(`${report.warnings} 个提醒`)
  return parts.join(' · ') || '可以保存'
}

/** 模板 → 可编辑草稿。坐标缺失时给一个可用的初始网格，避免全堆在原点。 */
export function draftFromTemplate(tpl: GraphTemplate): { nodes: GraphNode[]; edges: GraphEdge[] } {
  const nodes = (tpl.nodes ?? []).map((n, i) => ({
    ...n,
    position: n.position && (n.position.x !== 0 || n.position.y !== 0)
      ? n.position
      : { x: 80 + (i % 4) * 260, y: 80 + Math.floor(i / 4) * 150 },
  }))
  return { nodes, edges: (tpl.edges ?? []).map((e) => ({ ...e })) }
}

/** 估算明细按耗时降序（"谁最贵"）。 */
export function costliestLines(est: GraphEstimate | null | undefined) {
  if (!est?.lines) return []
  return [...est.lines].sort((a, b) => b.seconds - a.seconds)
}

/** 新增节点时的默认 data：全部来自 catalog 的字段默认值，前端不硬编码。 */
export function defaultDataFor(node: CatalogNode): Record<string, unknown> {
  const data: Record<string, unknown> = {}
  for (const f of node.fields ?? []) {
    if (f.default !== undefined && f.default !== null) data[f.key] = f.default
  }
  return data
}

/** 生成不冲突的节点 id（类型-序号），用于"拖一个新节点上来"。 */
export function nextNodeId(type: string, existing: { id: string }[]): string {
  const used = new Set(existing.map((n) => n.id))
  for (let i = 1; i < 1000; i += 1) {
    const id = `${type}-${i}`
    if (!used.has(id)) return id
  }
  return `${type}-${Date.now()}`
}
