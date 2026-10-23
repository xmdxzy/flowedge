/**
 * 文件源的判定逻辑。
 *
 * 前端这里做一份，不是为了"再实现一遍"，而是为了**即时反馈**：
 * 用户填了 `a.mp3` 却选了「图片」，如果只在运行时由后端报错，
 * 他得先跑一次（可能是几分钟的图）才知道自己填错了。
 *
 * 代价是这张扩展名表有两份，会漂。所以有 `graph.test.ts` 里的
 * 「前后端扩展名表一致」契约测试盯着它：它直接读 `service/input_source.go`
 * 解析出表来比对，改一边不改另一边会当场红。
 */

export type SourceKind = 'image' | 'audio' | 'video' | 'text'

/** 扩展名（小写，带点）→ 类型。必须与 service/input_source.go 的 sourceExtKinds 一致 */
export const SOURCE_EXT_KINDS: Record<string, SourceKind> = {
  '.png': 'image',
  '.jpg': 'image',
  '.jpeg': 'image',
  '.webp': 'image',
  '.bmp': 'image',
  '.gif': 'image',
  '.mp3': 'audio',
  '.wav': 'audio',
  '.flac': 'audio',
  '.m4a': 'audio',
  '.aac': 'audio',
  '.ogg': 'audio',
  '.mp4': 'video',
  '.mov': 'video',
  '.mkv': 'video',
  '.webm': 'video',
  '.avi': 'video',
  '.txt': 'text',
  '.md': 'text',
  '.srt': 'text',
  '.json': 'text',
}

/** 占位写法：图先搭、文件后补是正常用法，不该被当成"路径填错了" */
const PLACEHOLDERS = ['待提供', '待填写', 'todo', 'tbd', 'placeholder', 'none', 'null', '-']

export function extOf(path: string): string {
  const p = (path || '').trim()
  if (!p) return ''
  const base = p.replace(/\\/g, '/').split('/').pop() || ''
  const i = base.lastIndexOf('.')
  return i > 0 ? base.slice(i).toLowerCase() : ''
}

/** 按扩展名推断；推断不出返回 ''（而不是默认 text）——让面板能区分"不知道"与"是文本" */
export function inferSourceKind(path: string): SourceKind | '' {
  return SOURCE_EXT_KINDS[extOf(path)] || ''
}

export function isPlaceholderPath(path: string): boolean {
  const p = (path || '').trim().toLowerCase()
  if (PLACEHOLDERS.includes(p)) return true
  return p.startsWith('<') || p.startsWith('{{')
}

/** 节点最终会输出哪个端口。与后端 SourceKindOf 同序：显式 kind 优先，其次扩展名，最后 text */
export function effectiveSourceKind(data: Record<string, unknown>): SourceKind {
  const raw = String(data.kind || '').trim().toLowerCase()
  if (raw === 'image' || raw === 'audio' || raw === 'video' || raw === 'text') return raw
  const inferred = inferSourceKind(String(data.path || ''))
  return inferred || 'text'
}

/** 每种类型对应的输出端口名（契约里四个端口同名） */
export const SOURCE_PORT: Record<SourceKind, string> = {
  image: 'image',
  audio: 'audio',
  video: 'video',
  text: 'text',
}

/** 这张类型卡能接什么：给出下游常见节点，帮用户判断"接下来连哪里" */
export const SOURCE_CONSUMERS: Record<SourceKind, string> = {
  image: '图生图 / 超分 / 视觉理解 / 局部重绘',
  audio: '语音转写（ASR）',
  video: '视频合成 / 后期（竖屏·BGM·字幕）',
  text: '大语言模型 / 语音合成 / 提示词优化',
}
