import { useState } from 'react'

import CardLibrary from './CardLibrary'
import { sideHint } from '../lib/modules'
import { TOOL_META } from '../lib/tools'
import {
  cardName,
  readPipeline,
  STAGE_HINT,
  STAGE_LABEL,
  writePipeline,
  type Binding,
  type CardCatalog,
  type CardKind,
  type Pipeline,
  type Stage,
} from '../lib/cards'

// 节点处理链：接线 + 三段管线。
//
// 这一块回答用户三个问题：
//   1. 谁喂我、我喂谁（接线）
//   2. 我的输入/本体/输出分别由谁处理（三段管线）
//   3. 这会花多少次模型调用（成本）
//
// 三段而不是"一张卡"，是因为这三件事在语义上就不一样：
// 输入段是预处理、本体段是角色、输出段是整形。以前挤成一个 binding，
// 用户只能挂一张，等于逼他把"翻译→改写→出图"拆成三个节点。

export interface PeerRef {
  id: string
  type: string
  /** 这条边连到对方哪个端口（没有就是默认端口） */
  port?: string
}

interface Props {
  data: Record<string, unknown>
  cards: CardCatalog | null
  /** 上游 / 下游节点（用于接线展示） */
  upstream: PeerRef[]
  downstream: PeerRef[]
  /** 该节点是否吃文本（不吃的话输出段无意义） */
  textIn: boolean
  textOut: boolean
  /** 产出是图片/视频：输出段只对「看图」类卡有意义 */
  mediaOut?: boolean
  /** 本体角色段是否可用：只有 AI 推理节点有"角色"可替换 */
  bodyCapable: boolean
  /** 节点类型：用来显示显存侧别（排布效率的关键信息） */
  nodeType?: string
  /** 只渲染"接线"（开始节点用：它不需要处理链） */
  lineageOnly?: boolean
  /** 外部入口源（manual / workbuddy / dsh）。给了才渲染选择器 */
  entrySource?: string
  onEntrySourceChange?: (v: string) => void
  /** 这条工作流的其它外部入口状态（只读一行，代替原先单独的一块"外部接入"） */
  entryExtra?: { webhook: boolean; cron: string; saved: boolean }
  /** 工具配置（只有 ai 推理节点有）：并进本体段，不再单开一块 */
  tools?: {
    list: string[]
    maxRounds: number
    strict: boolean
    trace: { tool?: string; status?: string; arguments?: string }[]
    onChange: (patch: { tools?: string[]; maxRounds?: number; strict?: boolean }) => void
  }
  onChange: (bindings: Binding[]) => void
  onJump?: (nodeId: string) => void
  onNotify?: (msg: string) => void
  /** 卡片库被编辑后，让上层重新拉目录 */
  onReloadCards?: () => void
}

const KIND_GLYPH: Record<CardKind, string> = { agent: '●', skill: '⬡', workflow: '▤' }

export default function NodePipeline({
  data,
  cards,
  upstream,
  downstream,
  textIn,
  textOut,
  mediaOut,
  bodyCapable,
  nodeType,
  lineageOnly,
  entrySource,
  onEntrySourceChange,
  entryExtra,
  tools,
  onChange,
  onJump,
  onNotify,
  onReloadCards,
}: Props) {
  const pipe = readPipeline(data)
  const [openStage, setOpenStage] = useState<Stage | null>(null)
  const [toolsOpen, setToolsOpen] = useState(false)

  const calls = pipe.input.length + pipe.output.length + (pipe.body ? 1 : 0)

  const commit = (next: Pipeline) => onChange(writePipeline(next))

  const addCard = (stage: Stage, b: Binding) => {
    const next: Pipeline = { ...pipe, input: [...pipe.input], output: [...pipe.output] }
    if (stage === 'body') {
      if (next.body) {
        onNotify?.('本体角色只能有 1 张，已替换')
      }
      next.body = b
    } else if (stage === 'replace') {
      if (next.replace) onNotify?.('整节点替换只能有 1 张，已替换')
      next.replace = b
    } else if (stage === 'input') {
      next.input.push(b)
    } else {
      next.output.push(b)
    }
    commit(next)
  }

  const removeAt = (stage: Stage, idx: number) => {
    const next: Pipeline = { ...pipe, input: [...pipe.input], output: [...pipe.output] }
    if (stage === 'input') next.input.splice(idx, 1)
    else if (stage === 'output') next.output.splice(idx, 1)
    else if (stage === 'body') next.body = null
    else next.replace = null
    commit(next)
  }

  const move = (stage: 'input' | 'output', idx: number, dir: -1 | 1) => {
    const arr = stage === 'input' ? [...pipe.input] : [...pipe.output]
    const to = idx + dir
    if (to < 0 || to >= arr.length) return
    ;[arr[idx], arr[to]] = [arr[to], arr[idx]]
    commit({ ...pipe, [stage]: arr } as Pipeline)
  }

  return (
    <div className="flex flex-col gap-2.5">
      {/* 接线：谁进、谁出。开始节点多一行"外部源"——它没有上游节点，
          但一定有"谁把任务交进来"，这是节点级接线表达不了的工作流级事实。 */}
      <div className="rounded-lg border border-border bg-canvas/50 px-2.5 py-2">
        <p className="text-[10px] text-gray-500 uppercase tracking-wider mb-1">接线</p>
        {onEntrySourceChange && (
          <div className="flex items-center gap-1.5 py-0.5">
            <span className="text-[10px] text-ai mt-0.5">←</span>
            <select
              value={entrySource || 'manual'}
              onChange={(e) => onEntrySourceChange(e.target.value)}
              className="flex-1 min-w-0 px-1.5 py-0.5 rounded bg-surface border border-border text-[10px] text-gray-200 outline-none focus:border-primary"
            >
              <option value="manual">手动运行（画布「运行」）</option>
              <option value="workbuddy">WorkBuddy（外部智能体）</option>
              <option value="dsh">DSH（外部智能体）</option>
            </select>
          </div>
        )}
        {/* 有外部源时不再显示"无上游"：开始节点本来就没有上游节点，这行是噪音 */}
        <PeerRow dir="in" peers={upstream} onJump={onJump} hideWhenEmpty={!!onEntrySourceChange} />
        <PeerRow dir="out" peers={downstream} onJump={onJump} />
        {entryExtra && (
          <p className="text-[9px] text-gray-600 pt-1 mt-1 border-t border-border/60 leading-relaxed">
            Webhook {entryExtra.webhook ? '已开启' : '未开启'} · 定时 {entryExtra.cron || '未设置'}
            {!entryExtra.saved && <span className="text-amber-500/90"> · 未保存，外部接不进来</span>}
          </p>
        )}
        {upstream.length === 0 && downstream.length === 0 && !onEntrySourceChange && (
          <p className="text-[10px] text-gray-600">还没接线：拖一条线到这个节点，它才有输入</p>
        )}
      </div>

      {/* 三段管线（开始节点不渲染：它只有提示词） */}
      {!lineageOnly && (
      <div className="rounded-lg border border-border bg-canvas/50 px-2.5 py-2">
        <div className="flex items-center justify-between mb-1">
          <p className="text-[10px] text-gray-500 uppercase tracking-wider">处理链</p>
          <span className={`text-[9px] ${calls >= 3 ? 'text-amber-400' : 'text-gray-600'}`}>
            {calls > 0 ? `会调用 ${calls} 次模型` : '纯参数执行'}
          </span>
        </div>
        {nodeType && (
          <p className="text-[9px] text-gray-600 -mt-0.5 mb-1" title="调度口径：按节点起飞时进入的第一侧计算">
            {sideHint(nodeType)}
          </p>
        )}

        <StageRow
          stage="input"
          refs={pipe.input}
          disabled={!textIn}
          disabledNote="该节点不吃文本，输入段无意义"
          onAdd={() => setOpenStage('input')}
          onRemove={(i) => removeAt('input', i)}
          onMove={(i, d) => move('input', i, d)}
        />
        <StageRow
          stage="body"
          refs={pipe.body ? [pipe.body] : []}
          disabled={!bodyCapable || !!pipe.replace}
          disabledNote={
            !bodyCapable
              ? '本体角色只对「AI 推理」节点生效：其他节点没有可替换的角色设定'
              : '已用工作流卡替换整个节点，本体角色没有落点'
          }
          onAdd={() => setOpenStage('body')}
          onRemove={() => removeAt('body', 0)}
        />
        {/* 工具调用并进本体段：工具是"这个角色能用的手"，不是独立一块配置。
            卡片自带工具 + 这里勾的 = 实际可用工具集（节点勾选优先）。 */}
        {tools && (
          <div className="ml-[58px] -mt-0.5 mb-1">
            <button
              type="button"
              onClick={() => setToolsOpen((v) => !v)}
              className="text-[10px] text-gray-500 hover:text-gray-300"
            >
              工具 {tools.list.length}/{TOOL_META.length} {toolsOpen ? '▾' : '▸'}
              {tools.list.length > 0 && (
                <span className="text-gray-600">
                  {' '}· {tools.list.map((id) => TOOL_META.find((t) => t.id === id)?.label || id).join('、')}
                </span>
              )}
            </button>
            {toolsOpen && (
              <div className="mt-1 flex flex-col gap-1">
                {TOOL_META.map((t) => {
                  const on = tools.list.includes(t.id)
                  return (
                    <button
                      key={t.id}
                      type="button"
                      onClick={() =>
                        tools.onChange({
                          tools: on ? tools.list.filter((x) => x !== t.id) : [...tools.list, t.id],
                        })
                      }
                      className={`flex items-start gap-1.5 px-1.5 py-1 rounded border text-left ${
                        on ? 'bg-ai/15 border-ai/50' : 'bg-canvas border-border hover:border-ai/40'
                      }`}
                    >
                      <span
                        className={`mt-0.5 w-2.5 h-2.5 rounded-[2px] border flex-none grid place-items-center text-[7px] ${
                          on ? 'bg-ai border-ai text-black' : 'border-gray-600'
                        }`}
                      >
                        {on ? '✓' : ''}
                      </span>
                      <span className="min-w-0">
                        <span className="block text-[10px] text-gray-200">{t.label}</span>
                        <span className="block text-[9px] text-gray-600 leading-snug">{t.when}</span>
                      </span>
                    </button>
                  )
                })}
                <div className="grid grid-cols-2 gap-1.5 items-center mt-0.5">
                  <label className="text-[9px] text-gray-500">
                    最多轮数
                    <input
                      type="number"
                      min={1}
                      max={12}
                      value={tools.maxRounds}
                      onChange={(e) => tools.onChange({ maxRounds: Number(e.target.value) || 4 })}
                      className="ml-1 w-10 px-1 py-0.5 rounded bg-canvas border border-border text-[10px]"
                    />
                  </label>
                  <label className="flex items-center gap-1 text-[9px] text-gray-400">
                    <input
                      type="checkbox"
                      checked={tools.strict}
                      onChange={(e) => tools.onChange({ strict: e.target.checked })}
                    />
                    工具失败即中止
                  </label>
                </div>
                {tools.trace.length > 0 && (
                  <div className="mt-0.5">
                    <p className="text-[9px] text-gray-600">上次调用 {tools.trace.length} 次</p>
                    {tools.trace.slice(0, 2).map((t, i) => (
                      <p key={i} className="text-[9px] text-gray-500 truncate">
                        <span className={t.status === 'error' ? 'text-error' : 'text-success'}>
                          {t.status === 'error' ? '✕' : '✓'}
                        </span>{' '}
                        {t.tool}
                      </p>
                    ))}
                  </div>
                )}
              </div>
            )}
          </div>
        )}

        <StageRow
          stage="output"
          refs={pipe.output}
          disabled={!textOut && !mediaOut}
          disabledNote={
            mediaOut && !textOut
              ? '产出不是文本：只有标了「看图」的卡会生效（画面审片 / 图转描述）'
              : '该节点的产出不是文本，输出段无意义'
          }
          onAdd={() => setOpenStage('output')}
          onRemove={(i) => removeAt('output', i)}
          onMove={(i, d) => move('output', i, d)}
        />
        <StageRow
          stage="replace"
          refs={pipe.replace ? [pipe.replace] : []}
          warn="会改执行结构：这个节点将被一条工作流整体替换"
          onAdd={() => setOpenStage('replace')}
          onRemove={() => removeAt('replace', 0)}
        />
      </div>

      )}

      {/* 上次运行的卡片执行痕迹：贴了卡没生效时，用户要能立刻看出是哪张、为什么 */}
      {(() => {
        const out = data.output as Record<string, unknown> | undefined
        const tr = out?._cards
        if (!Array.isArray(tr) || tr.length === 0) return null
        return (
          <div className="rounded-lg border border-border bg-canvas/50 px-2.5 py-1.5">
            <p className="text-[10px] text-gray-500 mb-1">上次运行的卡片</p>
            {tr.slice(0, 4).map((c, i) => {
              const rec = c as { stage?: string; kind?: string; id?: string; ok?: boolean; error?: string }
              return (
                <p key={i} className="text-[10px] truncate">
                  <span className={rec.ok ? 'text-success' : 'text-error'}>{rec.ok ? '✓' : '✕'}</span>{' '}
                  <span className="text-gray-400">{STAGE_LABEL[(rec.stage as Stage) || 'body']}</span>{' '}
                  <span className="text-gray-300">{cardName(rec.kind as CardKind, rec.id || '')}</span>
                  {rec.error && <span className="text-error/80"> · {rec.error.slice(0, 40)}</span>}
                </p>
              )
            })}
          </div>
        )
      })()}

      {openStage && (
        <CardLibrary
          catalog={cards}
          stage={openStage}
          onPick={(b) => addCard(openStage, b)}
          onClose={() => setOpenStage(null)}
          onReload={onReloadCards}
        />
      )}
    </div>
  )
}

function PeerRow({
  dir,
  peers,
  onJump,
  hideWhenEmpty,
}: {
  dir: 'in' | 'out'
  peers: PeerRef[]
  onJump?: (id: string) => void
  hideWhenEmpty?: boolean
}) {
  if (hideWhenEmpty && peers.length === 0) return null
  return (
    <div className="flex items-start gap-1.5 py-0.5">
      <span className={`text-[10px] mt-0.5 ${dir === 'in' ? 'text-ai' : 'text-success'}`}>
        {dir === 'in' ? '←' : '→'}
      </span>
      <div className="flex flex-wrap gap-1">
        {peers.length === 0 && (
          <span className="text-[10px] text-gray-600">{dir === 'in' ? '无上游' : '无下游'}</span>
        )}
        {peers.map((p) => (
          <button
            key={p.id + (p.port || '')}
            onClick={() => onJump?.(p.id)}
            title={`${p.type}${p.port ? ' · ' + p.port : ''}（点击选中）`}
            className="px-1.5 py-0.5 rounded bg-surface border border-border text-[10px] text-gray-300 hover:border-primary/60"
          >
            {p.id}
            <span className="text-gray-600 ml-1">{p.type}</span>
          </button>
        ))}
      </div>
    </div>
  )
}

function StageRow({
  stage,
  refs,
  disabled,
  disabledNote,
  warn,
  onAdd,
  onRemove,
  onMove,
}: {
  stage: Stage
  refs: Binding[]
  disabled?: boolean
  disabledNote?: string
  warn?: string
  onAdd: () => void
  onRemove: (i: number) => void
  onMove?: (i: number, dir: -1 | 1) => void
}) {
  return (
    <div className={`py-1 ${disabled ? 'opacity-45' : ''}`}>
      <div className="flex items-center gap-1.5">
        <span className="text-[10px] text-gray-500 w-[52px] flex-none">{STAGE_LABEL[stage]}</span>
        <div className="flex flex-wrap gap-1 flex-1 min-w-0">
          {refs.length === 0 && <span className="text-[10px] text-gray-600">未设置</span>}
          {refs.map((b, i) => (
            <span
              key={`${b.kind}:${b.id}:${i}`}
              className={`inline-flex items-center gap-1 pl-1.5 pr-1 py-0.5 rounded border text-[10px] fe-chip--${b.kind}`}
              title={STAGE_HINT[stage]}
            >
              <span className="opacity-70">{KIND_GLYPH[b.kind]}</span>
              <span className="max-w-[86px] truncate">{cardName(b.kind, b.id)}</span>
              {onMove && refs.length > 1 && (
                <span className="flex flex-col leading-none">
                  <button className="text-[7px] text-gray-500 hover:text-white" onClick={() => onMove(i, -1)}>
                    ▲
                  </button>
                  <button className="text-[7px] text-gray-500 hover:text-white" onClick={() => onMove(i, 1)}>
                    ▼
                  </button>
                </span>
              )}
              <button className="text-gray-500 hover:text-error text-[10px]" onClick={() => onRemove(i)}>
                ×
              </button>
            </span>
          ))}
        </div>
        <button
          disabled={disabled}
          onClick={onAdd}
          className={`flex-none px-1.5 py-0.5 rounded border text-[10px] ${
            disabled
              ? 'border-border text-gray-600 cursor-not-allowed'
              : 'border-border text-gray-400 hover:border-primary/60 hover:text-white'
          }`}
        >
          +
        </button>
      </div>
      {disabled && disabledNote && <p className="text-[9px] text-gray-600 ml-[58px] mt-0.5">{disabledNote}</p>}
      {warn && refs.length > 0 && <p className="text-[9px] text-amber-500/90 ml-[58px] mt-0.5">{warn}</p>}
    </div>
  )
}
