// 技能包索引（客户端）。
//
// 和卡片库分开：卡片是能贴到节点上的能力，技能包是"机器上那些 SKILL.md 到底算不算数"的审计。
//
// 两个正交维度（别混）：
//   verdict —— 它变成了什么（卡 / 引擎已实现 / 没变）
//   tier    —— 默认露不露（通用能力 / 领域能力 / 不收录）
//
// 为什么领域能力也要留在平台里：FlowEdge 是通用编排器，用户完全可能编一张
// 「UE5 资产批处理」的图，那时 ue5-ops 的排错参考正是他要的。不预设 ≠ 不收录。

export type SkillVerdict =
  | 'mirrored'
  | 'adopted'
  | 'partial'
  | 'harness'
  | 'project'
  | 'deprecated'
  | 'unclassified'

/** core 通用能力（默认可见）/ domain 领域能力（可搜可引用，不预设）/ none 不收录 */
export type SkillTier = 'core' | 'domain' | 'none'

export interface SkillPack {
  name: string
  path: string
  description?: string
  read_when?: string[]
  bytes: number
  files: number
  refs?: string[]
  /** 来源根的人话标签：用户级 / 冷存储 / DSH 侧 / agents / 项目级·XX / 插件·XX */
  source?: string
  verdict: SkillVerdict
  reason: string
  evidence?: string[]
  card?: string
  tier: SkillTier
  retain: boolean
  category: string
}

export interface SkillPackSummary {
  total: number
  core: number
  domain: number
  none: number
  /** 收录在平台里的（core + domain） */
  retained: number
  /** 去重后的卡数：两个包可以提炼成同一张卡 */
  cards: number
  mirrored: number
  /** 来自专家插件市场的包数（plugins/<插件>/skills/<技能>） */
  plugins?: number
  by_verdict: Record<string, number>
  by_tier: Record<string, number>
  by_source?: Record<string, number>
}

export interface SkillPackList {
  packs: SkillPack[]
  roots: string[]
  /** 专家插件市场的技能根（与 roots 分开：布局是两级，来源是外部专家生态） */
  plugin_roots?: string[]
  /** false = 本机没有源码，证据无法核验（分发的 exe 就是这样），不代表判定是假的 */
  verified: boolean
  problems: string[] | null
  show_all: boolean
  summary: SkillPackSummary
}

export interface SkillPackQuery {
  /** 'core' 只看通用能力；不传 = 通用 + 领域 */
  tier?: 'core' | 'domain'
  /** 连不收录（已停用）的也拿回来 */
  includeNone?: boolean
}

const EMPTY_SUMMARY: SkillPackSummary = {
  total: 0,
  core: 0,
  domain: 0,
  none: 0,
  retained: 0,
  cards: 0,
  mirrored: 0,
  by_verdict: {},
  by_tier: {},
}

export async function fetchSkillPacks(q: SkillPackQuery = {}): Promise<SkillPackList | null> {
  const params = new URLSearchParams()
  if (q.tier) params.set('tier', q.tier)
  if (q.includeNone) params.set('all', '1')
  const qs = params.toString()
  try {
    const r = await fetch(`/api/skills${qs ? `?${qs}` : ''}`)
    if (!r.ok) return null
    const d = await r.json()
    return {
      packs: Array.isArray(d?.packs) ? d.packs : [],
      roots: Array.isArray(d?.roots) ? d.roots : [],
      plugin_roots: Array.isArray(d?.plugin_roots) ? d.plugin_roots : [],
      verified: d?.verified === true,
      problems: Array.isArray(d?.problems) ? d.problems : null,
      show_all: d?.show_all === true,
      summary: { ...EMPTY_SUMMARY, ...(d?.summary || {}) },
    }
  } catch {
    return null
  }
}

export async function fetchSkillDoc(name: string): Promise<string | null> {
  try {
    const r = await fetch(`/api/skills/${encodeURIComponent(name)}`)
    if (!r.ok) return null
    const d = await r.json()
    return typeof d?.doc === 'string' ? d.doc : null
  } catch {
    return null
  }
}

export function verdictLabel(v: SkillVerdict): string {
  switch (v) {
    case 'mirrored':
      return '引擎已实现'
    case 'adopted':
      return '整包提炼'
    case 'partial':
      return '部分提炼'
    case 'harness':
      return '待提炼'
    case 'project':
      return '领域专用'
    case 'deprecated':
      return '已停用'
    default:
      return '未判定'
  }
}

/** 判定徽章配色：越靠前越"进了引擎"，越靠后越"只登记不采用"。 */
export function verdictTone(v: SkillVerdict): string {
  switch (v) {
    case 'adopted':
      return 'border-green-500/50 text-green-300'
    case 'partial':
      return 'border-teal-500/50 text-teal-300'
    case 'mirrored':
      return 'border-sky-500/50 text-sky-300'
    case 'harness':
      return 'border-violet-500/40 text-violet-300'
    case 'project':
      return 'border-amber-500/40 text-amber-300'
    case 'deprecated':
      return 'border-border text-gray-500'
    default:
      return 'border-red-500/50 text-red-300'
  }
}

export function tierLabel(t: SkillTier): string {
  switch (t) {
    case 'core':
      return '通用'
    case 'domain':
      return '领域'
    default:
      return '不收'
  }
}

export function tierTone(t: SkillTier): string {
  switch (t) {
    case 'core':
      return 'bg-primary/15 text-indigo-200 border-primary/40'
    case 'domain':
      return 'bg-amber-500/10 text-amber-300 border-amber-500/30'
    default:
      return 'bg-canvas text-gray-500 border-border'
  }
}

export function humanBytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0 B'
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(0)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}

/** 排序：core 在前，其次 domain，最后 none；同档按"进了引擎的程度"，再按名字。 */
const VERDICT_ORDER: SkillVerdict[] = ['adopted', 'partial', 'mirrored', 'harness', 'project', 'deprecated', 'unclassified']
const TIER_ORDER: SkillTier[] = ['core', 'domain', 'none']

export function sortPacks(list: SkillPack[]): SkillPack[] {
  return [...list].sort((a, b) => {
    const t = TIER_ORDER.indexOf(a.tier) - TIER_ORDER.indexOf(b.tier)
    if (t !== 0) return t
    const d = VERDICT_ORDER.indexOf(a.verdict) - VERDICT_ORDER.indexOf(b.verdict)
    return d !== 0 ? d : a.name.localeCompare(b.name)
  })
}
