// 节点类型 → 人话名（来自后端 /api/modules 的 Modules 契约）。
//
// 为什么要从接口拿，而不是在前端手抄一份：
// 手抄清单一定会漂——加了节点忘改前端，画布上就出现 "sub_workflow" 这种字面量。
// 后端 Modules 是唯一事实源，前端只做显示。

export interface ModuleInfo {
  type: string
  title: string
  role?: string
  side?: string
  heavy?: boolean
  inputs?: { name: string; kind: string }[]
  outputs?: { name: string; kind: string }[]
}

let labels: Record<string, string> = {}
let sides: Record<string, { side?: string; heavy?: boolean }> = {}

export function setModules(mods: ModuleInfo[] | null | undefined) {
  const m: Record<string, string> = {}
  const sd: Record<string, { side?: string; heavy?: boolean }> = {}
  for (const mod of mods || []) {
    if (!mod?.type) continue
    m[mod.type] = mod.title || mod.type
    sd[mod.type] = { side: mod.side, heavy: mod.heavy }
  }
  labels = m
  sides = sd
}

/** 显存侧的**调度口径**（可能与该节点实际用到的侧别不同）。 */
export function sideOf(type?: string): { side?: string; heavy?: boolean } {
  if (!type) return {}
  return sides[type] || {}
}

const SIDE_LABEL: Record<string, string> = {
  language: '语言侧',
  diffusion: '扩散侧',
  audio: '语音侧',
}

/** 一句人话：这个节点会占哪一侧显存、是不是重任务。 */
export function sideHint(type?: string): string {
  const { side, heavy } = sideOf(type)
  if (!side) return '不占显存（可与任意节点并行）'
  const name = SIDE_LABEL[side] || side
  return heavy ? `${name} · 重任务（与其他重任务互斥，切侧约 180 秒）` : `${name} · 轻任务`
}

/** 类型名兜底：接口没拿到时显示原始 type，总比空白好。 */
export function typeLabel(type?: string): string {
  if (!type) return ''
  return labels[type] || type
}

export async function fetchModules(): Promise<ModuleInfo[]> {
  try {
    const r = await fetch('/api/modules')
    if (!r.ok) return []
    const d = await r.json()
    const list = Array.isArray(d) ? d : d?.modules
    return Array.isArray(list) ? (list as ModuleInfo[]) : []
  } catch {
    return []
  }
}
