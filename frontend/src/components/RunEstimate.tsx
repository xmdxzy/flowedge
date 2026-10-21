import { useEffect, useMemo, useRef, useState } from 'react'

/**
 * 运行前估算条
 *
 * 放在运行按钮左边：点运行之前就知道「这条要占多少显存、跑多久、会不会撞墙」。
 * 数据来自引擎的实测档案（data/vram_profiles.json），跨档位时只给上界并标注外推。
 *
 * 设计约束：
 *  1. **没有媒体节点就不显示**——纯 LLM/HTTP 流程显示「估算」只会制造噪音
 *  2. **没样本时不说数**——显示「无实测样本」，不编一个数字
 *  3. 数字是区间不是承诺——标题里写明 confidence 与依据
 *  4. 只在「媒体节点集合变了」时重新请求：拖节点位置也会触发 nodes 变化，
 *     每次都打接口会让后端每秒算几十次
 */

const MEDIA_TYPES = new Set(['text2image', 'image2image', 'inpaint', 'upscale', 'text2video', 'shotlist'])

interface EstimateItem {
  kind: string
  peak_mb: number
  peak_min_mb: number
  peak_max_mb: number
  wall_min_ms: number
  wall_max_ms: number
  samples: number
  confidence: string
  basis: string
  extrapolated: boolean
  peak_underestimated?: boolean
  warning?: string
}

interface EstimateResult {
  items: EstimateItem[]
  peak_mb: number
  peak_max_mb: number
  wall_min_ms: number
  wall_max_ms: number
  total_vram_mb: number
  fits: boolean
  confidence: string
  notes?: string[]
  warning?: string
}

function gb(mb: number): string {
  if (mb <= 0) return '?'
  return (mb / 1024).toFixed(1) + 'GB'
}

function dur(ms: number): string {
  if (ms <= 0) return '?'
  const m = Math.round(ms / 60000)
  if (m < 1) return '<1min'
  if (m < 60) return m + 'min'
  const h = Math.floor(m / 60)
  return h + 'h' + (m % 60 ? (m % 60) + 'm' : '')
}

const CONF_LABEL: Record<string, { text: string; cls: string }> = {
  high: { text: '高置信（3+ 次实测）', cls: 'text-success' },
  medium: { text: '中置信（样本不足 3 次）', cls: 'text-warning' },
  low: { text: '低置信（外推或体积估算）', cls: 'text-warning' },
  none: { text: '无实测样本', cls: 'text-gray-500' },
}

export default function RunEstimate({
  nodes,
  onRiskChange,
}: {
  nodes: any[]
  /** 把「会不会撞墙」上报给父组件：运行前据此二次确认 */
  onRiskChange?: (risky: boolean, detail: string) => void
}) {
  const [res, setRes] = useState<EstimateResult | null>(null)
  const [loading, setLoading] = useState(false)
  const [open, setOpen] = useState(false)
  const [err, setErr] = useState('')

  // 只把「媒体节点的关键参数」作为请求依据：位置/尺寸/样式变化不该触发重算
  const mediaKey = useMemo(() => {
    const compact = (nodes || [])
      .filter((n) => n && MEDIA_TYPES.has(n.type))
      .map((n) => {
        const d = n.data || {}
        return {
          type: n.type,
          width: d.width,
          height: d.height,
          frames: d.frames,
          steps: d.steps,
          model: d.model,
          shots: Array.isArray(d.shots) ? d.shots.length : undefined,
        }
      })
    return JSON.stringify(compact)
  }, [nodes])

  const mediaCount = useMemo(
    () => (nodes || []).filter((n) => n && MEDIA_TYPES.has(n.type)).length,
    [nodes],
  )

  const timer = useRef<number | undefined>(undefined)
  const abort = useRef<AbortController | null>(null)
  // 回调存 ref：父组件传的是内联箭头函数，每次渲染都会变，
  // 直接放进 useEffect 依赖会变成无限循环。
  const riskCb = useRef(onRiskChange)
  riskCb.current = onRiskChange

  useEffect(() => {
    if (!res) {
      riskCb.current?.(false, '')
      return
    }
    const risky = res.fits === false && res.confidence !== 'none'
    riskCb.current?.(risky, res.warning || '')
  }, [res])

  useEffect(() => {
    if (mediaCount === 0) {
      setRes(null)
      setErr('')
      return
    }
    // debounce：拖节点时 nodes 连续变化，只在停手 700ms 后算一次
    if (timer.current) window.clearTimeout(timer.current)
    timer.current = window.setTimeout(async () => {
      if (abort.current) abort.current.abort()
      const ctl = new AbortController()
      abort.current = ctl
      setLoading(true)
      setErr('')
      try {
        const mediaNodes = (nodes || []).filter((n) => n && MEDIA_TYPES.has(n.type))
        const resp = await fetch('/api/estimate', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ nodes: mediaNodes }),
          signal: ctl.signal,
        })
        if (!resp.ok) {
          const t = await resp.text()
          throw new Error(t.slice(0, 160))
        }
        setRes(await resp.json())
      } catch (e: any) {
        if (e?.name !== 'AbortError') setErr(String(e?.message || e).slice(0, 120))
      } finally {
        if (!ctl.signal.aborted) setLoading(false)
      }
    }, 700)
    return () => {
      if (timer.current) window.clearTimeout(timer.current)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mediaKey, mediaCount])

  if (mediaCount === 0) return null

  if (err) {
    return (
      <span className="px-2 py-1 rounded-lg text-[11px] bg-canvas border border-border text-gray-500" title={err}>
        估算不可用
      </span>
    )
  }
  if (loading && !res) {
    return (
      <span className="px-2 py-1 rounded-lg text-[11px] bg-canvas border border-border text-gray-500">
        估算中…
      </span>
    )
  }
  if (!res) return null

  const conf = CONF_LABEL[res.confidence] || CONF_LABEL.none
  const noSample = res.confidence === 'none' || res.peak_max_mb === 0
  const risky = !res.fits

  return (
    <div className="relative">
      <button
        onClick={() => setOpen((v) => !v)}
        className={`px-2 py-1 rounded-lg text-[11px] font-mono border transition-colors ${
          risky
            ? 'bg-error/10 border-error/50 text-error hover:bg-error/20'
            : 'bg-canvas border-border text-gray-300 hover:border-primary/50'
        }`}
        title={risky ? '预计峰值超过可用显存，点击运行前请先看估算详情' : '点击查看估算依据'}
      >
        {risky ? '⚠ ' : ''}
        {noSample ? '无实测样本' : `≈${gb(res.peak_mb)} · ${dur(res.wall_min_ms)}–${dur(res.wall_max_ms)}`}
      </button>

      {open && (
        <>
          <div className="fixed inset-0 z-[55]" onClick={() => setOpen(false)} />
          <div className="absolute right-0 top-full mt-1 z-[56] w-[340px] rounded-xl border border-border bg-surface shadow-xl p-3">
            <div className="flex items-center justify-between mb-2">
              <span className="text-xs font-medium text-gray-200">提交前估算</span>
              <span className={`text-[10px] ${conf.cls}`}>{conf.text}</span>
            </div>

            {risky && (
              <div className="mb-2 text-[10px] leading-relaxed text-error bg-error/10 border border-error/30 rounded-lg p-2">
                {res.warning || '预计峰值上界超过可用显存，大概率 OOM。'}
              </div>
            )}

            {res.items.length > 0 && (
              <table className="w-full text-[10px] font-mono mb-2">
                <thead>
                  <tr className="text-gray-500">
                    <th className="text-left font-normal pb-1">节点</th>
                    <th className="text-right font-normal pb-1">峰值</th>
                    <th className="text-right font-normal pb-1">耗时</th>
                    <th className="text-right font-normal pb-1">样本</th>
                  </tr>
                </thead>
                <tbody className="text-gray-300">
                  {res.items.map((it, i) => (
                    <tr key={i} className="border-t border-border/40">
                      <td className="py-1">
                        {it.kind}
                        {it.extrapolated && <span className="text-warning ml-1">外推</span>}
                        {it.peak_underestimated && <span className="text-warning ml-1">下界</span>}
                      </td>
                      <td className="text-right py-1">
                        {it.peak_mb > 0 ? gb(it.peak_mb) : '—'}
                        {it.peak_max_mb > it.peak_mb && it.peak_mb > 0 ? (
                          <span className="text-gray-500">~{gb(it.peak_max_mb)}</span>
                        ) : null}
                      </td>
                      <td className="text-right py-1">
                        {it.wall_min_ms > 0 ? `${dur(it.wall_min_ms)}–${dur(it.wall_max_ms)}` : '—'}
                      </td>
                      <td className="text-right py-1">{it.samples || 0}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}

            <div className="text-[10px] text-gray-500 leading-relaxed">
              卡上限 {gb(res.total_vram_mb)}；「能不能跑」按峰值**上界**判断。
              {res.notes?.length ? <div className="mt-1">{res.notes.join('；')}</div> : null}
              <div className="mt-1 text-gray-600">
                数字来自实测档案，样本会在每次运行后自动累积。
              </div>
            </div>
          </div>
        </>
      )}
    </div>
  )
}
