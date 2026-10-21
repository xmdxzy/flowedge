import { useCallback, useEffect, useState } from 'react'
import { toast } from './Toast'

/**
 * 镜头级重跑
 *
 * 解决的问题：8 镜出片跑完，第 3 镜废了。以前只能整条重跑（20 分钟）。
 * 后端 `POST /api/runs/:id/shots/:index/reroll` 已经能只重拍一镜，
 * 这里把它接到界面上——否则这个能力等于不存在。
 *
 * 关键细节：
 *  - 镜号是 **1-based**（与分镜表里的镜号一致），不是数组下标
 *  - 重跑不会改动用户保存的工作流（reroll 只写进那一次运行的内存节点图）
 *  - 运行中的 run 不允许重跑（后端 409），按钮先置灰
 */

interface ShotJob {
  shot_index: number
  shot_id: number
  status: string
  file?: string
  frames: number
  seconds: number
  transition?: string
  error?: string
  note?: string
  elapsed_ms: number
  reused?: boolean
}

const STATUS: Record<string, { label: string; cls: string }> = {
  success: { label: '成功', cls: 'text-success' },
  running: { label: '生成中', cls: 'text-primary' },
  failed: { label: '失败', cls: 'text-error' },
}

function basename(p?: string): string {
  if (!p) return ''
  const i = Math.max(p.lastIndexOf('/'), p.lastIndexOf('\\'))
  return i >= 0 ? p.slice(i + 1) : p
}

export default function ShotReroll({
  runId,
  runStatus,
  onRerolled,
}: {
  runId: number
  runStatus?: string
  onRerolled?: () => void
}) {
  const [byNode, setByNode] = useState<Record<string, ShotJob[]>>({})
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState<string>('')
  const [err, setErr] = useState('')

  const load = useCallback(async () => {
    try {
      const resp = await fetch(`/api/runs/${runId}/shots`)
      if (resp.status === 404) {
        // 该 run 里没有分镜节点：正常情况，不弹错
        setByNode({})
        return
      }
      if (!resp.ok) throw new Error((await resp.text()).slice(0, 120))
      const d = await resp.json()
      setByNode(d?.shots || {})
      setErr('')
    } catch (e: any) {
      setErr(String(e?.message || e).slice(0, 120))
    }
  }, [runId])

  useEffect(() => {
    load()
  }, [load])

  const nodeIds = Object.keys(byNode)
  if (nodeIds.length === 0) return null

  const rows = nodeIds.flatMap((nid) => (byNode[nid] || []).map((job) => ({ nid, job })))
  const all = rows.map((r) => r.job)
  const ok = all.filter((s) => s.status === 'success').length
  const failed = all.filter((s) => s.status === 'failed').length
  const running = runStatus === 'running'

  const reroll = async (nodeId: string, job: ShotJob) => {
    const idx = job.shot_index + 1 // 转成与分镜表一致的 1-based 镜号
    const key = `${nodeId}-${idx}`
    setBusy(key)
    try {
      const resp = await fetch(`/api/runs/${runId}/shots/${idx}/reroll?node_id=${encodeURIComponent(nodeId)}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: '{}',
      })
      if (!resp.ok) {
        const t = await resp.text()
        let msg = t.slice(0, 200)
        try {
          msg = JSON.parse(t)?.error || msg
        } catch {
          /* 非 JSON 就原样显示 */
        }
        throw new Error(msg)
      }
      toast(`已提交重拍第 ${idx} 镜（其余镜头复用，不重烧显存）`, 'success')
      onRerolled?.()
      setTimeout(load, 1500)
    } catch (e: any) {
      toast('重拍失败：' + String(e?.message || e), 'error')
    } finally {
      setBusy('')
    }
  }

  return (
    <div className="mb-2 rounded-lg border border-border/60 overflow-hidden">
      <button
        onClick={() => setOpen((v) => !v)}
        className="w-full flex items-center justify-between px-2 py-1.5 bg-canvas/60 hover:bg-surface-hover text-[10px]"
      >
        <span className="text-gray-300">
          镜头 {all.length} 个
          <span className="text-success ml-2">成功 {ok}</span>
          {failed > 0 && <span className="text-error ml-1.5">失败 {failed}</span>}
        </span>
        <span className="text-gray-500">{open ? '收起' : '展开'}</span>
      </button>

      {open && (
        <div className="px-2 py-1.5">
          {err && <div className="text-[10px] text-error mb-1">{err}</div>}
          {rows.map(({ nid, job }) => {
            const st = STATUS[job.status] || { label: job.status, cls: 'text-gray-400' }
            const idx = job.shot_index + 1
            const key = `${nid}-${job.shot_index}-${job.shot_id}`
            const isBusy = busy === `${nid}-${idx}`
            return (
              <div
                key={key}
                className="flex items-center gap-2 py-1 border-t border-border/40 first:border-t-0"
              >
                {/* 缩略图：一眼看出哪镜废了，比读状态文字快 */}
                {job.file ? (
                  <img
                    src={`/api/artifacts/${encodeURIComponent(basename(job.file))}`}
                    alt={`第${idx}镜`}
                    className="w-10 h-10 object-cover rounded border border-border/60 bg-black/40"
                    loading="lazy"
                  />
                ) : (
                  <div className="w-10 h-10 rounded border border-border/40 bg-canvas flex items-center justify-center text-[9px] text-gray-600">
                    无
                  </div>
                )}
                <div className="flex-1 min-w-0">
                  <div className="text-[10px] text-gray-300">
                    第 {idx} 镜
                    <span className={`ml-2 ${st.cls}`}>{st.label}</span>
                    {job.transition && <span className="ml-2 text-gray-500">{job.transition}</span>}
                  </div>
                  <div className="text-[9px] text-gray-500 truncate" title={job.error || job.note || ''}>
                    {job.frames} 帧 · {(job.seconds || 0).toFixed(1)}s
                    {job.elapsed_ms > 0 ? ` · ${(job.elapsed_ms / 1000).toFixed(0)}s 生成` : ''}
                    {job.reused ? ' · 复用' : ''}
                    {job.error ? ` · ${job.error}` : ''}
                    {!job.error && job.note ? ` · ${job.note}` : ''}
                  </div>
                </div>
                <button
                  disabled={running || !!busy}
                  onClick={() => reroll(nid, job)}
                  className="shrink-0 px-1.5 py-0.5 rounded border border-border text-[9px] text-gray-300 hover:border-primary/60 hover:text-primary disabled:opacity-40 disabled:cursor-not-allowed"
                  title={running ? '运行中不能重拍，等这次跑完' : `只重拍第 ${idx} 镜，其余复用`}
                >
                  {isBusy ? '提交中' : '重拍'}
                </button>
              </div>
            )
          })}
          <div className="mt-1 text-[9px] text-gray-600 leading-relaxed">
            重拍只影响这一镜，其余镜头直接复用已落盘的产物；不会改动你保存的工作流。
          </div>
        </div>
      )}
    </div>
  )
}
