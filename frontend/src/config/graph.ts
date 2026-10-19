import type { Edge, Node } from '@xyflow/react'

// 画布级配置：从 App.tsx 抽出来，避免主组件被常量表淹没。
// 新增节点类型时只改这里 + nodeTypes 注册，不再动 1500 行的主文件。

export interface PaletteItem {
  type: string
  label: string
  icon: string
  tone: string
}

export interface PaletteSection {
  label: string
  items: PaletteItem[]
}

export const PALETTE: PaletteSection[] = [
  {
    label: '控制',
    items: [
      { type: 'start', label: '开始', icon: '▶', tone: 'tone-control' },
      { type: 'end', label: '结束', icon: '■', tone: 'tone-control' },
      { type: 'if_else', label: '条件分支', icon: '⑂', tone: 'tone-warn' },
      { type: 'loop', label: '循环', icon: '⟳', tone: 'tone-warn' },
      { type: 'sub_workflow', label: '子工作流', icon: '⧉', tone: 'tone-purple' },
    ],
  },
  {
    label: 'AI 推理',
    items: [
      { type: 'llm', label: '大语言模型', icon: '✦', tone: 'tone-ai' },
      { type: 'text2image', label: '文生图', icon: '◈', tone: 'tone-ai' },
      { type: 'rag_query', label: '知识库检索', icon: '⌕', tone: 'tone-ai' },
    ],
  },
  {
    label: '视频',
    items: [
      { type: 'text2video', label: '文生视频', icon: '▷', tone: 'tone-purple' },
      { type: 'storyboard', label: '分镜规划', icon: '⊞', tone: 'tone-ai' },
      { type: 'shotlist', label: '分镜执行', icon: '⊙', tone: 'tone-purple' },
      { type: 'video_compose', label: '视频合成', icon: '▤', tone: 'tone-purple' },
    ],
  },
  {
    label: '语音与视觉',
    items: [
      { type: 'asr', label: '语音转写', icon: '♪', tone: 'tone-multimodal' },
      { type: 'tts', label: '语音合成', icon: '◍', tone: 'tone-multimodal' },
      { type: 'vision', label: '视觉理解', icon: '◉', tone: 'tone-multimodal' },
      { type: 'prompt_opt', label: '提示词优化', icon: '✧', tone: 'tone-ai' },
    ],
  },
  {
    label: '图像编辑',
    items: [
      { type: 'image2image', label: '图生图', icon: '◑', tone: 'tone-ai' },
      { type: 'inpaint', label: '局部重绘', icon: '◐', tone: 'tone-warn' },
      { type: 'upscale', label: '超分放大', icon: '⤢', tone: 'tone-multimodal' },
    ],
  },
  {
    label: '数据处理',
    items: [
      { type: 'http_request', label: 'HTTP 请求', icon: '⇄', tone: 'tone-data' },
      { type: 'code', label: '代码执行', icon: '{ }', tone: 'tone-data' },
      { type: 'data_transform', label: '数据转换', icon: '⇋', tone: 'tone-muted' },
      { type: 'aggregate', label: '变量聚合', icon: '⋃', tone: 'tone-muted' },
    ],
  },
  {
    label: '画布',
    items: [
      { type: 'note', label: '批注', icon: '✎', tone: 'tone-muted' },
      { type: 'group', label: '分组', icon: '▢', tone: 'tone-muted' },
    ],
  },
]

// 各节点类型的常用输出字段（用于变量提示；与后端 defaultOutputFields 对齐）
export const NODE_OUTPUT_FIELDS: Record<string, string[]> = {
  http_request: ['text', 'status_code'],
  if_else: ['text', 'branch'],
  loop: ['text', 'all', 'iterations', 'stopped_by'],
  text2image: ['image'],
  video_compose: ['video'],
  tts: ['audio', 'backend'],
  asr: ['text'],
  rag_query: ['text', 'context', 'sources', 'hits'],
  code: ['text', 'code_out'],
  aggregate: ['text', 'count', 'values', 'nodes', 'aggregate'],
  vision: ['text', 'model', 'images', 'tokens'],
  prompt_opt: ['text', 'original', 'mode'],
  storyboard: ['text', 'plan', 'shots', 'title', 'style'],
  shotlist: ['video', 'kind', 'text', 'shots', 'seconds', 'detail'],
  image2image: ['image', 'kind', 'source', 'strength'],
  inpaint: ['image', 'kind', 'source', 'mask'],
  upscale: ['image', 'kind', 'source', 'width', 'height'],
  text2video: ['video', 'kind', 'text', 'original_prompt', 'beats', 'parts', 'segments'],
}

// 节点 → 依赖的环境能力（缺了就标灰；用户仍可先编排，环境补齐后直接跑）
export const NODE_REQUIRES: Record<string, string> = {
  llm: 'llm',
  text2image: 'image',
  asr: 'audio',
  tts: 'audio',
  video_compose: 'video',
  vision: 'vision',
  image2image: 'image',
  inpaint: 'image',
  upscale: 'image',
  text2video: 'image',
}

// LLM 节点可用的内置工具（后端 service/tools.go 实现，全部本地执行）
export const LLM_TOOL_OPTIONS = [
  { id: 'kb_search', label: '知识库检索' },
  { id: 'http_get', label: 'HTTP 查询' },
  { id: 'calc', label: '计算器' },
  { id: 'now', label: '当前时间' },
]

// 仅仅存在于画布、不参与执行的节点类型（保存与运行时必须剔除）
export const CANVAS_ONLY_TYPES = new Set(['note', 'group'])

// 初始示例工作流
export const initialNodes: Node[] = [
  {
    id: 'start-1',
    type: 'start',
    position: { x: 80, y: 200 },
    data: { prompt: '用三句话介绍一下人工智能的发展历程' },
  },
  {
    id: 'llm-1',
    type: 'llm',
    position: { x: 350, y: 200 },
    data: { model: '', prompt: '' }, // 留空 = 用服务端自动探测到的本地模型
  },
  {
    id: 'end-1',
    type: 'end',
    position: { x: 620, y: 200 },
    data: {},
  },
]

export const initialEdges: Edge[] = [
  { id: 'e1-2', source: 'start-1', target: 'llm-1' },
  { id: 'e2-3', source: 'llm-1', target: 'end-1' },
]
