// 模板目录层：分组 / 成熟度 / 置顶 / 使用频次
//
// 设计意图：
// - 模板本体留在 workflowTemplates.ts（纯图数据），元数据集中在这里，改排序不用碰图。
// - 排序规则固定为「手工置顶 > 使用频次 > 成熟度 > 分组顺序 > 名称」，
//   所以「用得多」和「经得起用」都会自然浮到前面，以后加模板只需补一条 meta。
// - 频次存 localStorage，不上后端：单人本地工具，跨设备同步没有收益。

import { workflowTemplates, type WorkflowTemplate } from './workflowTemplates'

export type Maturity = 'stable' | 'beta'

export type TemplateGroupId =
  | 'starter'
  | 'image'
  | 'video'
  | 'longform'
  | 'audio'
  | 'knowledge'
  | 'flow'
  | 'qc'

export interface TemplateGroup {
  id: TemplateGroupId
  label: string
  hint: string
  icon: string
}

export const TEMPLATE_GROUPS: TemplateGroup[] = [
  { id: 'starter', label: '起步', hint: '第一次用先跑这三个，链路最短', icon: '◈' },
  { id: 'image', label: '图像', hint: '改风格 / 局部重绘 / 超分', icon: '▣' },
  { id: 'video', label: '视频 · 单镜', hint: '一个镜头，几分钟内出片', icon: '▶' },
  { id: 'longform', label: '视频 · 长片', hint: '分镜驱动，多段拼接有接缝约束', icon: '⧉' },
  { id: 'audio', label: '语音', hint: '配音与转写', icon: '♪' },
  { id: 'knowledge', label: '知识与应用', hint: 'RAG 检索、外部 API 串联', icon: '◍' },
  { id: 'flow', label: '流程编排', hint: '循环、批处理、条件分支', icon: '⟳' },
  { id: 'qc', label: '质检', hint: '出图后自动挑毛病', icon: '✓' },
]

export interface TemplateMeta {
  group: TemplateGroupId
  maturity: Maturity
  /** 手工置顶权重：>0 的永远排在未置顶之前，数字越大越靠前 */
  pinned?: number
  /** 置顶理由，显示在卡片上。写「为什么它值得先试」，不写功能复述 */
  pinNote?: string
  /** 一句场景提示，比 description 更短更具体 */
  useCase?: string
  /** 实测耗时 / 资源提示，来自 AGENTS.md 的真实数据，不要编 */
  cost?: string
  tags?: string[]
}

/**
 * 22 个内置模板的元数据。
 * 置顶原则（以后维护照这个来）：
 *  5 = 实测过的旗舰路径，第一次打开就该看到
 *  3 = 稳定好用的日常工具
 *  0 = 保留但低频，靠搜索或分组进入
 */
export const TEMPLATE_META: Record<string, TemplateMeta> = {
  // ── 起步 ──────────────────────────────────────────────
  'text-summarize': {
    group: 'starter',
    maturity: 'stable',
    pinned: 3,
    pinNote: '最小可用链路：一个节点就能看清引擎怎么跑',
    useCase: '丢一段长文，出三条要点',
    cost: '秒级',
    tags: ['入门', 'LLM'],
  },
  'image-generate': {
    group: 'starter',
    maturity: 'stable',
    pinned: 3,
    pinNote: '出图最短路径，先确认扩散链路通不通',
    useCase: '一句话出图，1024×1024',
    cost: '视模型而定',
    tags: ['入门', '出图'],
  },
  'conditional-branch': {
    group: 'starter',
    maturity: 'stable',
    useCase: '按内容分流，两条分支各走各的',
    tags: ['入门', '控制流'],
  },

  // ── 图像 ──────────────────────────────────────────────
  'img2img-restyle': {
    group: 'image',
    maturity: 'stable',
    useCase: '已有图换风格，强度 0.85 以上才明显',
    cost: '单张数十秒',
    tags: ['图生图'],
  },
  'inpaint-patch': {
    group: 'image',
    maturity: 'stable',
    useCase: '白蒙版区域重画，蒙版须与原图同尺寸',
    tags: ['重绘', '蒙版'],
  },
  'upscale-x4': {
    group: 'image',
    maturity: 'stable',
    pinned: 3,
    pinNote: '交付前统一过一遍，768 → 3072',
    useCase: 'RealESRGAN 四倍放大',
    cost: '单张数十秒',
    tags: ['超分'],
  },

  // ── 视频 · 单镜 ────────────────────────────────────────
  'h3-t2v': {
    group: 'video',
    maturity: 'stable',
    pinned: 5,
    pinNote: '唯一一条实测跑通的带声视频链路，先跑这个',
    useCase: '一句提示词 → 5 秒带立体声的镜头',
    cost: '608×352×124 帧 ≈ 2 分钟',
    tags: ['H3', '有声'],
  },
  'h3-i2v-long': {
    group: 'video',
    maturity: 'stable',
    useCase: '首帧定调性，末帧续接凑到 10 秒',
    cost: '2 段 ≈ 5 分钟',
    tags: ['H3', '首帧引导'],
  },
  'wan-t2v-fast': {
    group: 'video',
    maturity: 'stable',
    useCase: '无声快速试镜，适合先看节奏',
    cost: '832×480×33 帧 ≈ 64 秒',
    tags: ['Wan', '无声'],
  },

  // ── 视频 · 长片 ────────────────────────────────────────
  'storyboard-batch': {
    group: 'longform',
    maturity: 'stable',
    pinned: 5,
    pinNote: '批量出图实测 6.9 倍提速，别再用循环单张',
    useCase: 'LLM 拆分镜 → 一次提交多行提示词',
    cost: '5 条 ≈ 29 秒',
    tags: ['批量', '出图'],
  },
  'video-pipeline': {
    group: 'longform',
    maturity: 'beta',
    useCase: '文案 → 分镜 → 生图 → 配音 → 合成',
    cost: '链路长，建议先单段验证',
    tags: ['全链路'],
  },
  'storyboard-video': {
    group: 'longform',
    maturity: 'beta',
    useCase: '分镜表直接驱动成片，带运镜与衔接约束',
    cost: '多段，分钟级到十分钟级',
    tags: ['分镜', '成片'],
  },
  'short-drama-pipeline': {
    group: 'longform',
    maturity: 'beta',
    useCase: '剧本 → 景别/角度/轴线 → 逐镜生成 → 装配',
    cost: '段落越多越慢，注意接缝',
    tags: ['短剧', '分镜'],
  },

  // ── 语音 ──────────────────────────────────────────────
  'podcast-voice': {
    group: 'audio',
    maturity: 'stable',
    useCase: '文案转配音，CPU 跑不占显存',
    tags: ['TTS'],
  },
  'transcribe-summarize': {
    group: 'audio',
    maturity: 'stable',
    useCase: '录音转文字再出纪要',
    tags: ['ASR', '纪要'],
  },

  // ── 知识与应用 ────────────────────────────────────────
  'rag-qa': {
    group: 'knowledge',
    maturity: 'stable',
    useCase: '先检索自己的资料，再让模型作答',
    tags: ['RAG', '知识库'],
  },
  'http-api-chain': {
    group: 'knowledge',
    maturity: 'stable',
    useCase: '外部接口取数 → 处理 → 模型分析',
    tags: ['HTTP'],
  },

  // ── 流程编排 ──────────────────────────────────────────
  'loop-refine': {
    group: 'flow',
    maturity: 'stable',
    useCase: '同一任务打磨 N 轮，上轮结果做下轮输入',
    tags: ['循环'],
  },
  'loop-batch': {
    group: 'flow',
    maturity: 'stable',
    useCase: '多行输入逐条处理',
    tags: ['循环', '遍历'],
  },
  'loop-until': {
    group: 'flow',
    maturity: 'stable',
    useCase: '反复生成直到出现指定标记，带次数上限',
    tags: ['循环', '重试'],
  },

  // ── 质检 ──────────────────────────────────────────────
  'image-qc': {
    group: 'qc',
    maturity: 'beta',
    useCase: '视觉模型给单张图挑毛病',
    tags: ['VLM', '质检'],
  },
  'image-qc-batch': {
    group: 'qc',
    maturity: 'beta',
    useCase: '三图对比，选最优',
    tags: ['VLM', '批选'],
  },
}

const FALLBACK_META: TemplateMeta = { group: 'starter', maturity: 'beta' }

export function metaOf(id: string): TemplateMeta {
  return TEMPLATE_META[id] || FALLBACK_META
}

// ── 使用统计（localStorage）────────────────────────────────

const USAGE_KEY = 'flowedge.template.usage.v1'

export interface UsageRecord {
  count: number
  lastUsed: number
}

export type UsageMap = Record<string, UsageRecord>

export function readUsage(): UsageMap {
  try {
    const raw = localStorage.getItem(USAGE_KEY)
    if (!raw) return {}
    const parsed = JSON.parse(raw)
    return parsed && typeof parsed === 'object' ? (parsed as UsageMap) : {}
  } catch {
    return {}
  }
}

export function bumpUsage(id: string): UsageMap {
  const usage = readUsage()
  const prev = usage[id]
  usage[id] = { count: (prev?.count || 0) + 1, lastUsed: Date.now() }
  try {
    localStorage.setItem(USAGE_KEY, JSON.stringify(usage))
  } catch {
    /* 隐私模式下写不进去，不影响本次会话 */
  }
  return usage
}

export function resetUsage(): void {
  try {
    localStorage.removeItem(USAGE_KEY)
  } catch {
    /* 忽略 */
  }
}

// ── 排名 ─────────────────────────────────────────────────

export interface TemplateEntry {
  tpl: WorkflowTemplate
  meta: TemplateMeta
  usage: UsageRecord
  /** 计算出来的排名分，仅用于排序与调试 */
  score: number
}

const GROUP_ORDER = new Map(TEMPLATE_GROUPS.map((g, i) => [g.id, i]))

/** 综合打分：置顶权重压倒一切，其次真实使用频次，再次成熟度 */
export function scoreOf(meta: TemplateMeta, usage: UsageRecord): number {
  const pin = meta.pinned || 0
  const uses = usage.count || 0
  const maturity = meta.maturity === 'stable' ? 3 : 0
  return pin * 1000 + Math.min(uses, 20) * 30 + maturity
}

export function buildEntries(usage: UsageMap = {}): TemplateEntry[] {
  return workflowTemplates
    .map((tpl) => {
      const meta = metaOf(tpl.id)
      const u = usage[tpl.id] || { count: 0, lastUsed: 0 }
      return { tpl, meta, usage: u, score: scoreOf(meta, u) }
    })
    .sort((a, b) => {
      if (b.score !== a.score) return b.score - a.score
      const ga = GROUP_ORDER.get(a.meta.group) ?? 99
      const gb = GROUP_ORDER.get(b.meta.group) ?? 99
      if (ga !== gb) return ga - gb
      return a.tpl.name.localeCompare(b.tpl.name, 'zh-Hans-CN')
    })
}

/** 左栏只放这几条：置顶的，且不超过 n 条 */
export function topEntries(entries: TemplateEntry[], n = 4): TemplateEntry[] {
  const pinned = entries.filter((e) => (e.meta.pinned || 0) > 0)
  return (pinned.length ? pinned : entries).slice(0, n)
}

export function groupOf(id: TemplateGroupId): TemplateGroup {
  return TEMPLATE_GROUPS.find((g) => g.id === id) || TEMPLATE_GROUPS[0]
}
