// 展示层格式化：只做「数字/时间 → 人话」，不碰网络与 React。
// 这些函数被单测钉住（est_seconds 的 30s / 90s / 1800s 边界在 format.test.ts 里）。

/** 去掉多余小数：1 → "1"，1.5 → "1.5"，1.55 → "1.6" */
function trimNumber(n: number): string {
  const rounded = Math.round(n * 10) / 10
  return Number.isInteger(rounded) ? String(rounded) : rounded.toFixed(1)
}

/**
 * est_seconds → 「约 1 分钟 / 约 30 分钟」。
 * 0 / 缺失 = 流水线没写估时（后端 omitempty），显示「时长未知」而不是「约 0 秒」。
 */
export function formatEstSeconds(sec: number | null | undefined): string {
  if (sec == null || !Number.isFinite(sec) || sec <= 0) return '时长未知'
  if (sec < 60) return `约 ${Math.round(sec)} 秒`
  if (sec < 3600) return `约 ${trimNumber(sec / 60)} 分钟`
  return `约 ${trimNumber(sec / 3600)} 小时`
}

/** 耗时毫秒 → 「12 秒 / 1 分 05 秒 / 1 时 02 分」 */
export function formatDurationMs(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return '—'
  const total = Math.floor(ms / 1000)
  if (total < 60) return `${total} 秒`
  const minutes = Math.floor(total / 60)
  const seconds = total % 60
  if (minutes < 60) return seconds ? `${minutes} 分 ${String(seconds).padStart(2, '0')} 秒` : `${minutes} 分`
  const hours = Math.floor(minutes / 60)
  const rest = minutes % 60
  return rest ? `${hours} 时 ${String(rest).padStart(2, '0')} 分` : `${hours} 时`
}

/** 字节 → KB/MB/GB（产物卡片） */
export function formatBytes(bytes: number | null | undefined): string {
  if (bytes == null || !Number.isFinite(bytes) || bytes < 0) return '—'
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(0)} KB`
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MB`
  return `${(bytes / 1024 / 1024 / 1024).toFixed(2)} GB`
}

/** RFC3339 → 本地「MM-DD HH:mm:ss」；解析失败给「—」，不抛 */
export function formatTime(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

/** started_at → ended_at 的毫秒差；running（无 ended_at）用 now 兜底 */
export function elapsedMs(
  startedAt: string | null | undefined,
  endedAt: string | null | undefined,
  now: number = Date.now(),
): number | null {
  if (!startedAt) return null
  const start = new Date(startedAt).getTime()
  if (Number.isNaN(start)) return null
  const end = endedAt ? new Date(endedAt).getTime() : now
  if (Number.isNaN(end)) return null
  return Math.max(0, end - start)
}
