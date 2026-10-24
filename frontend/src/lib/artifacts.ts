import type { ArtifactItem } from '../api/types'

// 产物归类与过滤。
//
// 后端已按扩展名给出 kind（backend/handler/artifact.go artifactExt），这里再实现一份是
// 为了两件事：① 未知/缺失 kind 时前端仍能摆对位置；② 过滤是无网络的纯逻辑，可以被单测钉住。
// 扩展名表与后端保持**逐项一致**，多一个 gif / 少一个 webp 都会让两边显示不同。

export type ArtifactKind = 'image' | 'video' | 'audio'
export type ArtifactFilter = 'all' | ArtifactKind

export const ARTIFACT_KINDS: ArtifactKind[] = ['image', 'video', 'audio']

const EXT_KIND: Record<string, ArtifactKind> = {
  png: 'image',
  jpg: 'image',
  jpeg: 'image',
  webp: 'image',
  mp4: 'video',
  webm: 'video',
  wav: 'audio',
  mp3: 'audio',
}

/** 文件名 → 类别；无法判断返回 null（后端不会列出来，前端也不假装认识） */
export function kindOf(name: string, declared?: string): ArtifactKind | null {
  if (declared === 'image' || declared === 'video' || declared === 'audio') return declared
  const dot = name.lastIndexOf('.')
  if (dot < 0) return null
  return EXT_KIND[name.slice(dot + 1).toLowerCase()] ?? null
}

export function filterArtifacts(items: ArtifactItem[], filter: ArtifactFilter): ArtifactItem[] {
  if (filter === 'all') return items
  return items.filter((item) => kindOf(item.name, item.kind) === filter)
}

export function countByKind(items: ArtifactItem[]): Record<ArtifactFilter, number> {
  const counts: Record<ArtifactFilter, number> = { all: items.length, image: 0, video: 0, audio: 0 }
  for (const item of items) {
    const kind = kindOf(item.name, item.kind)
    if (kind) counts[kind] += 1
  }
  return counts
}

/** 复制给用户看的绝对路径：后端 dir 是 Windows 反斜杠，但也可能跑在 Linux 上 */
export function joinArtifactPath(dir: string, name: string): string {
  if (!dir) return name
  const sep = dir.includes('\\') ? '\\' : '/'
  return dir.endsWith(sep) ? `${dir}${name}` : `${dir}${sep}${name}`
}
