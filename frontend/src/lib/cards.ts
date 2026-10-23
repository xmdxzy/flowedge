// 能力卡片：类型、索引与请求。
//
// 三张卡长什么样不一样，不是装饰差异——**形状即语义**：
//   agent    圆形（一个人）
//   skill    六边形（一个工具/刃口）
//   workflow 三层堆叠（一条流水线）
// 用户扫一眼画布就知道这个节点是"有人在里面干活"还是"被一条流水线接管了"。

export type CardKind = 'agent' | 'skill' | 'workflow'

export interface AgentCard {
  id: string
  name: string
  role: string
  system: string
  tools?: string[]
  icon?: string
  category: string
  tags?: string[]
}

export interface SkillCard {
  id: string
  name: string
  desc: string
  system: string
  icon?: string
  output_hint?: string
  out_kind?: string
  /** 吃哪种媒体：空=纯文本卡；"image"=看图说话（审片/反推），只能贴在图节点上 */
  media?: string
  category: string
  tags?: string[]
}

export interface WorkflowCard {
  id: string
  name: string
  nodes: number
  desc: string
}

export interface CardCatalog {
  agents: AgentCard[]
  skills: SkillCard[]
  workflows: WorkflowCard[]
  categories?: Record<string, string[]>
  effects?: Record<string, string>
}

export interface Binding {
  kind: CardKind
  id: string
  stage?: Stage
  order?: number
}

export type Stage = 'input' | 'body' | 'output' | 'replace'

export const STAGE_LABEL: Record<Stage, string> = {
  input: '输入处理',
  body: '本体角色',
  output: '输出处理',
  replace: '整节点替换',
}

export const STAGE_HINT: Record<Stage, string> = {
  input: '上游文本进来先过它：翻译 / 清洗 / 抽字段',
  body: '换掉这个节点的角色设定（只能 1 张，两张会打架）',
  output: '本体产出再过一遍：整形 / 抽 JSON / 合规检查',
  replace: '整个节点换成一条工作流（改执行结构，只能 1 张）',
}

export interface Pipeline {
  input: Binding[]
  body: Binding | null
  output: Binding[]
  replace: Binding | null
}

export const EMPTY_PIPELINE: Pipeline = { input: [], body: null, output: [], replace: null }

/** 读管线：新格式 bindings[] 优先，旧格式 binding 自动迁移。 */
export function readPipeline(data: Record<string, unknown> | undefined): Pipeline {
  const out: Pipeline = { input: [], body: null, output: [], replace: null }
  if (!data) return out

  const push = (b: Binding) => {
    const stage: Stage = b.stage || (b.kind === 'workflow' ? 'replace' : 'body')
    if (stage === 'input') out.input.push({ ...b, stage })
    else if (stage === 'output') out.output.push({ ...b, stage })
    else if (stage === 'body') out.body = { ...b, stage }
    else out.replace = { ...b, stage }
  }

  const list = data.bindings
  if (Array.isArray(list)) {
    for (const raw of list) {
      const b = raw as Binding
      if (b && b.id && b.kind) push(b)
    }
    out.input.sort((a, b) => (a.order || 0) - (b.order || 0))
    out.output.sort((a, b) => (a.order || 0) - (b.order || 0))
    return out
  }

  const legacy = data.binding as Binding | undefined
  if (legacy && legacy.id && legacy.kind) push(legacy)
  return out
}

/** 写回：始终写成数组，order 由数组顺序决定。 */
export function writePipeline(p: Pipeline): Binding[] {
  return [
    ...p.input.map((b, i) => ({ ...b, stage: 'input' as Stage, order: i })),
    ...(p.body ? [{ ...p.body, stage: 'body' as Stage, order: 0 }] : []),
    ...p.output.map((b, i) => ({ ...b, stage: 'output' as Stage, order: i })),
    ...(p.replace ? [{ ...p.replace, stage: 'replace' as Stage, order: 0 }] : []),
  ]
}

export const CARD_KIND_LABEL: Record<CardKind, string> = {
  agent: '智能体',
  skill: '技能',
  workflow: '工作流',
}

export const CARD_KIND_EFFECT: Record<CardKind, string> = {
  agent: '该节点改用这张卡的角色设定与默认工具',
  skill: '该节点执行一次确定性变换（同输入必同输出）',
  workflow: '该节点被替换成一次子工作流调用（会改执行结构）',
}

// 卡片名索引：节点卡片上的徽章需要显示名字，但它拿不到 App 的 state。
// 用一个模块级快照（App 拉取后写入），避免为了一个标签把 catalog 透传 20 层。
let index: Record<string, string> = {}

export function setCardIndex(cat: CardCatalog | null) {
  const m: Record<string, string> = {}
  cat?.agents?.forEach((c) => (m[`agent:${c.id}`] = c.name))
  cat?.skills?.forEach((c) => (m[`skill:${c.id}`] = c.name))
  cat?.workflows?.forEach((c) => (m[`workflow:${c.id}`] = c.name))
  index = m
}

export function cardName(kind?: string, id?: string): string {
  if (!kind || !id) return ''
  return index[`${kind}:${id}`] || id
}

export function readBinding(data: Record<string, unknown> | undefined): Binding | null {
  const b = data?.binding as Binding | undefined
  if (!b || !b.kind || !b.id) return null
  if (b.kind !== 'agent' && b.kind !== 'skill' && b.kind !== 'workflow') return null
  return b
}

export async function fetchCards(): Promise<CardCatalog | null> {
  try {
    const r = await fetch('/api/cards')
    if (!r.ok) return null
    return (await r.json()) as CardCatalog
  } catch {
    return null
  }
}

export interface LLMProfile {
  name: string
  base_url: string
  model: string
  backend?: string
  credential?: string
  note?: string
}

export async function fetchProfiles(): Promise<{ profiles: LLMProfile[]; default: string } | null> {
  try {
    const r = await fetch('/api/llm/profiles')
    if (!r.ok) return null
    return await r.json()
  } catch {
    return null
  }
}

export async function saveProfiles(profiles: LLMProfile[], def = ''): Promise<string | null> {
  try {
    const r = await fetch('/api/llm/profiles', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ profiles, default: def }),
    })
    if (!r.ok) {
      const body = await r.json().catch(() => ({}))
      return (body as { error?: string }).error || `保存失败（${r.status}）`
    }
    return null
  } catch (e) {
    return String(e)
  }
}
