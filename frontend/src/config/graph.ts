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
      // 外部文件入口。与 start 同组、同一角色（无输入），但它吐的是文件路径而不是文本。
      // 没有它，「转写这段录音」在图上根本表达不出来：start 只输出 text，接不到 audio 端口。
      { type: 'input_source', label: '文件源', icon: '▤', tone: 'tone-control' },
      { type: 'end', label: '结束', icon: '■', tone: 'tone-control' },
      { type: 'if_else', label: '条件分支', icon: '⑂', tone: 'tone-warn' },
      { type: 'loop', label: '循环', icon: '⟳', tone: 'tone-warn' },
  { type: 'quality_loop', label: '质量循环', icon: '↻', tone: 'tone-warn' },
          ],
  },
  {
    label: 'AI 推理',
    items: [
      { type: 'llm', label: '大语言模型', icon: '✦', tone: 'tone-ai' },
            // 外部 MCP 服务器的一个工具。选中后在右侧属性面板挑服务器与工具，
      // 服务器清单来自工具台（自动扫 WorkBuddy / Cursor / Claude 等本机配置）。
            { type: 'text2image', label: '文生图', icon: '◈', tone: 'tone-ai' },
          ],
  },
  {
    label: '视频',
    items: [
      { type: 'text2video', label: '文生视频', icon: '▷', tone: 'tone-purple' },
      { type: 'scene_grid', label: '场景九宫格', icon: '▦', tone: 'tone-ai' },
      { type: 'storyboard', label: '分镜规划', icon: '⊞', tone: 'tone-ai' },
      { type: 'shotlist', label: '分镜执行', icon: '⊙', tone: 'tone-purple' },
      { type: 'video_compose', label: '视频合成', icon: '▤', tone: 'tone-purple' },
      // 字幕是短视频交付的准入门槛：成片 + 台词 → 硬字幕（另存 .srt 便于复查）
      { type: 'subtitles', label: '字幕烧录', icon: '⌶', tone: 'tone-multimodal' },
      // 竖屏重构 / BGM 闪避 / 字幕三件一起做只重编码一次（分三个节点会掉三轮画质）
      { type: 'post_production', label: '后期包装', icon: '⛭', tone: 'tone-purple' },
    ],
  },
  {
    label: '语音与视觉',
    items: [
      { type: 'asr', label: '语音转写', icon: '♪', tone: 'tone-multimodal' },
      { type: 'tts', label: '语音合成', icon: '◍', tone: 'tone-multimodal' },
      { type: 'vision', label: '视觉理解', icon: '◉', tone: 'tone-multimodal' },
      { type: 'select', label: '择优挑选', icon: '⌁', tone: 'tone-ai' },
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
    label: 'ComfyUI 生态',
    items: [
      // 导入任意 ComfyUI「导出 (API)」工作流直接执行。
      // 这是把社区几万个 workflow JSON 接进来的入口——不用等我们写节点。
      { type: 'comfy_workflow', label: 'ComfyUI 工作流', icon: '⌘', tone: 'tone-purple' },
    ],
  },
  {
    label: '数据处理',
    items: [
                  // 本地命令：跑测试 / 构建 / 校验脚本。agent_task 的对偶——agent 改，它验。
                  // 文本拆分：长文案按句/段/固定段数/字数切开。以前没有它，模型会编一个 split_text
      { type: 'text_split', label: '文本拆分', icon: '⋮⋮', tone: 'tone-data' },
            { type: 'quality_gate', label: '质检门', icon: '✓', tone: 'tone-warn' },
      // 一致性门：跨镜头接缝 / 场景漂移的数值门（另一个会话新增，这里补上入口，否则测试点不到）
      { type: 'consistency_gate', label: '一致性门', icon: '≋', tone: 'tone-warn' },
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
  // 四个端口互斥：实际只有 kind 对应的那一个键有值（其余不出现）
  input_source: ['kind', 'text', 'image', 'audio', 'video', 'path', 'bytes', 'source'],
  text_split: ['text', 'parts', 'count', 'chars', 'mode', 'merged_overflow'],
  if_else: ['text', 'branch'],
  loop: ['text', 'all', 'iterations', 'stopped_by'],
  quality_loop: ['text', 'gate', 'score', 'rounds', 'best_round', 'path', 'checks', 'report', 'image', 'video', 'audio'],
  text2image: [
    'image', 'images', 'count', 'prompts', 'translated', 'translate_error',
    'scene_lock', 'scene_lock_injected', // P2 跨镜头一致性
  ],
  video_compose: ['video'],
  tts: ['audio', 'backend'],
  asr: ['text', 'model'],
  vision: ['text', 'model', 'images', 'tokens', 'reference'],
  select: ['image', 'score', 'passed', 'threshold', 'picked', 'candidates', 'note'],
  prompt_opt: ['text', 'original', 'mode', 'scene_lock'],
  scene_grid: ['text', 'lock', 'grid', 'cells', 'cells_text', 'source'],
  storyboard: ['text', 'plan', 'shots', 'title', 'style'],
  shotlist: ['video', 'kind', 'text', 'shots', 'seconds', 'detail'],
  image2image: ['image', 'kind', 'source', 'strength'],
  inpaint: ['image', 'kind', 'source', 'mask'],
  upscale: ['image', 'kind', 'source', 'width', 'height'],
  text2video: [
    'video', 'kind', 'text', 'original_prompt', 'beats', 'parts', 'segments',
    'audio_muxed', 'audio_track', 'audio_error',
    'degraded', 'degrade_level', 'degrade_note', // P3 显存降级留痕
  ],
  quality_gate: ['gate', 'checks', 'path', 'kind', 'width', 'height', 'frames', 'seconds', 'text'],
  consistency_gate: ['gate', 'segments', 'seams', 'drifts', 'anchors', 'worst', 'checks', 'thresholds', 'order', 'failures', 'text'],
  subtitles: ['video', 'srt', 'lines', 'source'],
  post_production: ['video', 'aspect', 'width', 'height', 'bgm', 'duck', 'srt', 'lines', 'source'],
  comfy_workflow: ['files', 'count', 'image', 'images', 'video', 'videos', 'bindings_used'],
}

// 节点 → 依赖的环境能力（缺了就标灰；用户仍可先编排，环境补齐后直接跑）
//
// 语义必须与后端 ready 对齐（capabilities.go）：
//   image = 主实例 8188（出图/重绘/超分）
//   h3    = H3 实例 8189（音视频联合生成）
//   comfy = 任一 ComfyUI 在跑（文生视频按 backend 选实例，运行期再判）
// 把 text2video 绑在 image 上就是「H3 在跑却标灰」的根因；绑在 comfy 上才诚实。
export const NODE_REQUIRES: Record<string, string> = {
  llm: 'llm',
  storyboard: 'llm', // 分镜规划：纯 LLM
  prompt_opt: 'llm', // 提示词优化：纯 LLM
  scene_grid: 'llm', // 场景九宫格：手填不花 LLM，从想法拆格才要
  text2image: 'image',
  image2image: 'image',
  inpaint: 'image',
  upscale: 'image',
  asr: 'audio',
  tts: 'audio',
  video_compose: 'video',
  vision: 'vision',
  select: 'vision', // 择优要靠 VLM 打分
  text2video: 'comfy',
  shotlist: 'comfy', // 分镜执行：H3 或 Wan 任一实例
  quality_gate: 'video', // 质检需要 ffmpeg/ffprobe
  consistency_gate: 'video', // 一致性门要抽帧，同样依赖 ffmpeg/ffprobe
  subtitles: 'video', // 烧字幕只需要 ffmpeg（带 libass）
  post_production: 'video', // 竖屏/BGM/字幕都走 ffmpeg
  quality_loop: 'comfy', // 质量循环跑的是生成工作流
}

// 能力键 → 人话（标灰时的提示文案，避免把 'image' / 'comfy' 这种键名直接甩给用户）
export const ENV_LABEL: Record<string, string> = {
  llm: '本地 LLM（Ollama）',
  image: '文生图（ComfyUI 8188）',
  h3: '音视频生成（ComfyUI-H3 8189）',
  comfy: '任一 ComfyUI 实例',
  audio: '语音（ASR + TTS）',
  video: '视频合成（ffmpeg）',
  vision: '视觉理解（VLM）',
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

// LLM 节点的「任务预设」。
//
// 为什么用预设而不是一个空 textarea：空框里模型会把输入当聊天问题回答，
// 用户以为在"改写提示词"，实际拿到一段客套话。预设把角色与输出格式写死，
// 只有「自定义」才交还控制权。
export const LLM_TASK_PRESETS = [
  {
    id: 'image',
    label: '画面改写（→ 出图提示词）',
    hint: '把想法改写成 flux 可用的英文提示词，逗号分隔、只写可见元素',
    prompt:
      '你是画面提示词改写器。把用户输入改写成文生图模型可用的英文提示词：' +
      '逗号分隔的短句，只写可见元素（主体 / 动作 / 环境 / 光线 / 镜头 / 画质），' +
      '不写感受与解释，不加引号与前缀。只输出改写结果本身。',
  },
  {
    id: 'translate',
    label: '中文 → 英文',
    hint: '只翻译，不改写',
    prompt:
      'Translate the input into English. Output ONLY the translation — no explanations, ' +
      "no quotes, no 'Translation:' prefix. Keep proper nouns and composition terms intact.",
  },
  {
    id: 'storyboard',
    label: '剧本 → 分镜表',
    hint: '输出严格 JSON：shots[] 含 frames / prompt / line',
    prompt:
      '把剧本拆成分镜表，只输出 JSON：' +
      '{"title":"…","character":"…","shots":[{"id":1,"frames":24,"transition":"cut","prompt":"…","line":"…"}]}。' +
      '一镜只讲一件事；prompt 用英文画面描述；不改剧情、不加旁白。',
  },
  {
    id: 'summary',
    label: '三句话摘要',
    hint: '是什么 / 结论 / 下一步',
    prompt:
      '把输入压缩成三句话：第一句说是什么，第二句说关键结论，第三句说下一步。不要客套，不要复述原文。',
  },
  {
    id: 'custom',
    label: '自定义…',
    hint: '自己写系统提示词',
    prompt: '',
  },
] as const
