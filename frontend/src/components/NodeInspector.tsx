import type { Edge, Node } from '@xyflow/react'

import { LLM_TOOL_OPTIONS, NODE_OUTPUT_FIELDS } from '../config/graph'
import ErrorNotice from './ErrorNotice'
import type { ErrorAction } from '../lib/errorHints'

// 右侧属性面板（从 App.tsx 抽出，约 1,500 行 JSX）。
//
// 抽出的理由是**可测性**：面板是按节点类型分支的一长串 JSX，最容易在改样式时改坏，
// 却因为住在 3k 行的主组件里而无法单独渲染。现在它可以被 render 出来断言：
// 某类型该出现哪些字段、改值有没有回调到上层、错误码有没有翻成人话。
//
// 它不持有任何状态：tab 由上层给、数据改动一律 onUpdate 回调出去。
// 这样 App 仍然是唯一的状态源，拆分不会引入两份真相。

export interface NodeInspectorProps {
  node: Node | null
  nodes: Node[]
  edges: Edge[]
  tab: 'config' | 'output'
  onTabChange: (tab: 'config' | 'output') => void
  onUpdate: (key: string, value: unknown) => void
  /** 复制变量等轻提示；不传就静默 */
  onNotify?: (message: string) => void
  /** 错误卡片上的修复动作 */
  onErrorAction?: (action: ErrorAction) => void
  /** 子工作流节点要选一个已保存的工作流 */
  savedWorkflows?: any[]
  currentWorkflowId?: number
}

export default function NodeInspector({
  node,
  nodes,
  edges,
  tab,
  onTabChange,
  onUpdate,
  onNotify,
  onErrorAction,
  savedWorkflows = [],
  currentWorkflowId,
}: NodeInspectorProps) {
  return (
    <div className="w-72 border-l border-border bg-surface p-4 overflow-y-auto">
    {/* Tab 切换：此前三个按钮都没有 onClick，「输出」「日志」实际是死按钮 */}
    <div className="flex border-b border-border mb-4">
      {(
        [
          ['config', '配置'],
          ['output', '输出'],
        ] as const
      ).map(([key, label]) => (
        <button
          key={key}
          onClick={() => onTabChange(key)}
          className={`px-3 py-2 text-xs font-medium border-b-2 transition-colors ${
            tab === key
              ? 'text-primary border-primary'
              : 'text-gray-500 hover:text-gray-300 border-transparent'
          }`}
        >
          {label}
        </button>
      ))}
    </div>

    {/* 输出 Tab：完整输出与错误（P1 事件日志就绪后再接回「日志」Tab） */}
    {tab === 'output' ? (
      node ? (
        <div className="flex flex-col gap-3">
          <label className="text-xs text-gray-400 block">上一次运行的输出</label>
          {node.data?.output != null ? (
            <pre className="text-[11px] text-gray-300 font-mono bg-canvas border border-border rounded-lg p-2 whitespace-pre-wrap break-all max-h-[60vh] overflow-auto">
              {typeof node.data.output === 'string'
                ? node.data.output
                : JSON.stringify(node.data.output, null, 2)}
            </pre>
          ) : (
            <p className="text-xs text-gray-500">暂无输出，运行工作流后显示</p>
          )}
          {node.data?.error != null && (
            <ErrorNotice
              code={node.data?.error_code as string | undefined}
              message={String(node.data.error)}
              onAction={(a) => {
                if (a === 'open-env') onErrorAction?.('open-env')
              }}
            />
          )}
        </div>
      ) : (
        <p className="text-xs text-gray-500">点击画布中的节点查看输出</p>
      )
    ) : node ? (
      <div className="flex flex-col gap-4">
        <div>
          <label className="text-xs text-gray-400 block mb-1">节点类型</label>
          <p className="text-sm text-white">{node.type}</p>
        </div>

        {/* 可用变量：列出上游节点输出，点击复制引用写法 */}
        {(() => {
          const ups = nodes.filter((n) =>
            edges.some((e) => e.target === node.id && e.source === n.id),
          )
          if (ups.length === 0) return null
          return (
            <div className="mb-3 pb-3 border-b border-border">
              <label className="text-[10px] text-gray-500 uppercase tracking-wider block mb-1.5">
                可用变量（点击复制）
              </label>
              <div className="flex flex-wrap gap-1">
                {ups.flatMap((u) =>
                  ['text', ...(NODE_OUTPUT_FIELDS[u.type as string] || [])]
                    .filter((f, i, arr) => arr.indexOf(f) === i)
                    .map((f) => (
                      <button
                        key={`${u.id}.${f}`}
                        onClick={async () => {
                          const ref = `{{${u.id}.${f}}}`
                          try {
                            await navigator.clipboard.writeText(ref)
                            onNotify?.(`已复制 ${ref}`)
                          } catch {
                            onNotify?.(ref)
                          }
                        }}
                        title={`${u.id} (${u.type}) 的 ${f} 输出`}
                        className="text-[9px] px-1.5 py-0.5 rounded bg-primary/10 text-indigo-300 font-mono hover:bg-primary/25 transition-all"
                      >
                        {`{{${u.id}.${f}}}`}
                      </button>
                    )),
                )}
              </div>
              <p className="text-[10px] text-gray-600 mt-1">上游节点完成后即可引用，未完成时保留原文</p>
            </div>
          )
        })()}

        {node.type === 'start' && (
          <div>
            <label className="text-xs text-gray-400 block mb-1">输入提示词</label>
            <textarea
              value={(node.data.prompt as string) || ''}
              onChange={(e) => onUpdate('prompt', e.target.value)}
              className="w-full h-24 px-3 py-2 rounded-lg bg-canvas border border-border text-sm resize-none focus:border-primary outline-none"
              placeholder="输入要传递给下游的文本..."
            />
          </div>
        )}

        {/* 代码执行节点：Go 子集沙箱（yaegi 解释执行，stdlib 白名单） */}
        {node.type === 'code' && (
          <>
            <div>
              <label className="text-xs text-gray-400 block mb-1">
                Go 代码（把结果赋给 result）
              </label>
              <textarea
                value={(node.data.code as string) || ''}
                onChange={(e) => onUpdate('code', e.target.value)}
                className="w-full h-52 px-3 py-2 rounded-lg bg-canvas border border-border text-[11px] font-mono resize-y focus:border-primary outline-none"
                placeholder="result = strings.ToUpper(inputText)  // 可用变量：input、inputText"
              />
              <p className="text-[10px] text-gray-600 mt-1 leading-relaxed">
                可用变量：<code className="text-data">input</code>（上游输出
                JSON）、<code className="text-data">inputText</code>（上游文本）。可用包：
                strings、encoding/json、fmt、sort、strconv、math、time、regexp、bytes、
                sha256、path。禁止 import 危险包（os/exec、net、syscall 等）。
              </p>
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">超时（毫秒）</label>
              <input
                type="number"
                value={(node.data.timeout_ms as number) ?? 30000}
                onChange={(e) =>
                  onUpdate('timeout_ms', Number(e.target.value) || 0)
                }
                className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none"
                placeholder="30000"
              />
              <p className="text-[10px] text-gray-600 mt-1">
                yaegi 无法硬中断死循环，超时只会放弃等待，默认 30 秒。
              </p>
            </div>
          </>
        )}

        {/* 知识库检索：此前属性面板完全没有配置项，靠默认值盲跑 */}
        {node.type === 'rag_query' && (
          <>
            <div>
              <label className="text-xs text-gray-400 block mb-1">检索问题</label>
              <textarea
                value={(node.data.query as string) || ''}
                onChange={(e) => onUpdate('query', e.target.value)}
                className="w-full h-20 px-3 py-2 rounded-lg bg-canvas border border-border text-sm resize-none focus:border-primary outline-none"
                placeholder="留空则使用上游输入..."
              />
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">返回条数 top_k</label>
              <input
                type="number"
                min={1}
                max={10}
                value={(node.data.top_k as number) ?? 3}
                onChange={(e) =>
                  onUpdate('top_k', Number(e.target.value) || 3)
                }
                className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none"
              />
            </div>
          </>
        )}

        {node.type === 'llm' && (
          <>
            <div>
              <label className="text-xs text-gray-400 block mb-1">模型</label>
              <input
                value={(node.data.model as string) || ''}
                onChange={(e) => onUpdate('model', e.target.value)}
                className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none"
                placeholder="留空则用自动探测到的模型"
              />
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">自定义提示词（可选）</label>
              <textarea
                value={(node.data.prompt as string) || ''}
                onChange={(e) => onUpdate('prompt', e.target.value)}
                className="w-full h-24 px-3 py-2 rounded-lg bg-canvas border border-border text-sm resize-none focus:border-primary outline-none"
                placeholder="留空则使用上游输入..."
              />
            </div>

            <div>
              <label className="text-xs text-gray-400 block mb-1">工具调用（可选）</label>
              <div className="flex flex-wrap gap-1.5">
                {LLM_TOOL_OPTIONS.map((tool) => {
                  const cur = ((node.data.tools as string[]) || [])
                  const on = cur.includes(tool.id)
                  return (
                    <button
                      key={tool.id}
                      onClick={() =>
                        onUpdate(
                          'tools',
                          on ? cur.filter((x) => x !== tool.id) : [...cur, tool.id],
                        )
                      }
                      className={`px-2 py-1 rounded-md text-[11px] border transition-all ${
                        on
                          ? 'bg-ai/20 border-ai/60 text-white'
                          : 'bg-canvas border-border text-gray-400 hover:border-ai/40'
                      }`}
                    >
                      {tool.label}
                    </button>
                  )
                })}
              </div>
              <p className="text-[10px] text-gray-500 mt-1">
                勾选后模型可自行决定调用（最多 4 轮），全部在本地执行
              </p>
            </div>
          </>
        )}

        {node.type === 'text2image' && (
          <div className="flex flex-col gap-2">
            <div>
              <label className="text-xs text-gray-400 block mb-1">
                批量提示词（一行一条，留空用上游/单条）
              </label>
              <textarea
                rows={4}
                value={(node.data.prompts as string) || ''}
                onChange={(e) => onUpdate('prompts', e.target.value)}
                placeholder={'分镜1：雪山顶的白鹿，远景\n分镜2：鹿的特写，逆光\n分镜3：空镜，只留雪与天'}
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y font-mono"
              />
            </div>
            <label className="flex items-center gap-2 text-[11px] text-gray-400">
              <input
                type="checkbox"
                checked={Boolean(node.data.split_lines)}
                onChange={(e) => onUpdate('split_lines', e.target.checked)}
              />
              把上游文本按行拆成多条（接 LLM 拆分镜时用）
            </label>
            <p className="text-[10px] text-gray-500 leading-relaxed">
              多条提示词会在**同一次模型加载**里跑完，比逐条省掉每条的冷启动。
            </p>
          </div>
        )}

        {node.type === 'text2image' && (
          <>
            <div>
              <label className="text-xs text-gray-400 block mb-1">提示词</label>
              <textarea
                value={(node.data.prompt as string) || ''}
                onChange={(e) => onUpdate('prompt', e.target.value)}
                className="w-full h-20 px-3 py-2 rounded-lg bg-canvas border border-border text-sm resize-none focus:border-primary outline-none"
                placeholder="描述要生成的图片..."
              />
            </div>
            <label className="flex items-center gap-2 text-[11px] text-gray-400">
              <input
                type="checkbox"
                checked={node.data.auto_translate === true}
                onChange={(e) => onUpdate('auto_translate', e.target.checked)}
              />
              检测到中文时自动过本地 LLM 译成英文（flux 对中文弱）
            </label>
            <p className="text-[10px] text-gray-500 leading-relaxed">
              flux 的文本编码器以英文为主：纯中文提示词经常只画出「通用风景」。
              勾选后由本地 LLM 在进模型前翻译，翻译失败会自动降级用原文并在输出里留痕
              （<code>translated</code> / <code>translate_error</code>）。
            </p>
            <div className="flex gap-2">
              <div>
                <label className="text-xs text-gray-400 block mb-1">宽度</label>
                <input
                  type="number"
                  value={(node.data.width as number) || 1024}
                  onChange={(e) => onUpdate('width', e.target.value)}
                  className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none"
                />
              </div>
              <div>
                <label className="text-xs text-gray-400 block mb-1">高度</label>
                <input
                  type="number"
                  value={(node.data.height as number) || 1024}
                  onChange={(e) => onUpdate('height', e.target.value)}
                  className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none"
                />
              </div>
            </div>
          </>
        )}

        {(node.type === 'sub_workflow' || node.type === 'loop') && (
          <>
            <div>
              <label className="text-xs text-gray-400 block mb-1">引用工作流</label>
              <select
                value={(node.data.workflow_id as number) ?? ''}
                onChange={(e) => {
                  const id = e.target.value ? Number(e.target.value) : undefined
                  const wf = savedWorkflows.find((w) => w.id === id)
                  onUpdate('workflow_id', id)
                  onUpdate('workflow_name', wf?.name || '')
                }}
                className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-warning outline-none"
              >
                <option value="">选择工作流...</option>
                {savedWorkflows
                  .filter((w) => w.id !== currentWorkflowId)
                  .map((w) => (
                    <option key={w.id} value={w.id}>
                      {w.name} (#{w.id})
                    </option>
                  ))}
              </select>
              {!node.data.workflow_id && (
                <p className="text-[10px] text-warning mt-1">需先保存目标工作流，再回到此处选择</p>
              )}
            </div>

            {node.type === 'loop' && (
              <>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">循环模式</label>
                  <select
                    value={(node.data.mode as string) || 'count'}
                    onChange={(e) => onUpdate('mode', e.target.value)}
                    className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-warning outline-none"
                  >
                    <option value="count">固定次数（每轮以上一轮输出为输入）</option>
                    <option value="foreach">遍历列表（按分隔符切分上游）</option>
                    <option value="until">条件循环（满足即停）</option>
                  </select>
                </div>

                {(node.data.mode || 'count') === 'count' && (
                  <div>
                    <label className="text-xs text-gray-400 block mb-1">迭代次数</label>
                    <input
                      type="number"
                      min={1}
                      value={(node.data.iterations as number) ?? 3}
                      onChange={(e) => onUpdate('iterations', Number(e.target.value))}
                      className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-warning outline-none"
                    />
                  </div>
                )}

                {node.data.mode === 'foreach' && (
                  <div>
                    <label className="text-xs text-gray-400 block mb-1">分隔符</label>
                    <input
                      value={(node.data.separator as string) ?? ''}
                      onChange={(e) => onUpdate('separator', e.target.value)}
                      className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-warning outline-none"
                      placeholder="留空按换行切分"
                    />
                  </div>
                )}

                {node.data.mode === 'until' && (
                  <>
                    <div>
                      <label className="text-xs text-gray-400 block mb-1">停止条件</label>
                      <select
                        value={(node.data.condition_type as string) || 'not_empty'}
                        onChange={(e) => onUpdate('condition_type', e.target.value)}
                        className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-warning outline-none"
                      >
                        <option value="not_empty">输出不为空</option>
                        <option value="contains">输出包含</option>
                        <option value="not_contains">输出不包含</option>
                        <option value="equals">输出等于</option>
                      </select>
                    </div>
                    <div>
                      <label className="text-xs text-gray-400 block mb-1">比较值</label>
                      <input
                        value={(node.data.compare_value as string) || ''}
                        onChange={(e) => onUpdate('compare_value', e.target.value)}
                        className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-warning outline-none"
                        placeholder="如 DONE"
                      />
                    </div>
                  </>
                )}

                <div>
                  <label className="text-xs text-gray-400 block mb-1">最大迭代（安全阀）</label>
                  <input
                    type="number"
                    min={1}
                    max={100}
                    value={(node.data.max_iterations as number) ?? 20}
                    onChange={(e) => onUpdate('max_iterations', Number(e.target.value))}
                    className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-warning outline-none"
                  />
                  <p className="text-[10px] text-gray-500 mt-1">硬上限 100，达到即中止并返回已有结果</p>
                </div>
              </>
            )}
          </>
        )}

        {node.type === 'if_else' && (
          <>
            <div>
              <label className="text-xs text-gray-400 block mb-1">条件类型</label>
              <select
                value={(node.data.condition_type as string) || 'not_empty'}
                onChange={(e) => onUpdate('condition_type', e.target.value)}
                className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none"
              >
                <option value="not_empty">不为空</option>
                <option value="contains">包含</option>
                <option value="equals">等于</option>
              </select>
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">比较值</label>
              <input
                value={(node.data.compare_value as string) || ''}
                onChange={(e) => onUpdate('compare_value', e.target.value)}
                className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none"
                placeholder="输入比较文本..."
              />
            </div>
          </>
        )}

        {node.type === 'aggregate' && (
          <div className="flex flex-col gap-3">
            <div>
              <label className="text-xs text-gray-400 block mb-1">聚合模式</label>
              <select
                value={(node.data.mode as string) || 'object'}
                onChange={(e) => onUpdate('mode', e.target.value)}
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
              >
                <option value="object">按节点汇聚（产出 nodes 映射）</option>
                <option value="list">列表（产出 values 数组）</option>
                <option value="text">文本拼接（产出 text）</option>
                <option value="first">取第一个上游</option>
                <option value="last">取最后一个上游</option>
              </select>
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">取值字段</label>
              <input
                value={(node.data.field as string) || 'text'}
                onChange={(e) => onUpdate('field', e.target.value)}
                placeholder="text / image / audio / *（整个输出）"
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
              />
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">参与聚合的节点（可选）</label>
              <input
                value={(node.data.inputs as string) || ''}
                onChange={(e) => onUpdate('inputs', e.target.value)}
                placeholder="留空=全部上游；填 node-a,node-b 可指定顺序"
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
              />
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">文本连接符</label>
              <input
                value={(node.data.separator as string) ?? '\n\n'}
                onChange={(e) => onUpdate('separator', e.target.value)}
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
              />
            </div>
            <label className="flex items-center gap-2 text-xs text-gray-400">
              <input
                type="checkbox"
                checked={!!node.data.include_empty}
                onChange={(e) => onUpdate('include_empty', e.target.checked)}
              />
              保留空值（默认丢弃空上游）
            </label>
          </div>
        )}

        {node.type === 'note' && (
          <div className="flex flex-col gap-2">
            <label className="text-xs text-gray-400">批注内容</label>
            <textarea
              rows={6}
              value={(node.data.text as string) || ''}
              onChange={(e) => onUpdate('text', e.target.value)}
              placeholder="写给未来的自己：这里为什么这么连？"
              className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
            />
            <p className="text-[10px] text-gray-500">批注不参与执行，保存时也不会写进工作流定义。</p>
          </div>
        )}

        {node.type === 'tts' && (
          <div className="flex flex-col gap-3">
            <div>
              <label className="text-xs text-gray-400 block mb-1">文本（留空取上游）</label>
              <textarea
                rows={3}
                value={(node.data.text as string) || ''}
                onChange={(e) => onUpdate('text', e.target.value)}
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
              />
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">语音名片段（可选）</label>
              <input
                value={(node.data.voice as string) || ''}
                onChange={(e) => onUpdate('voice', e.target.value)}
                placeholder="如 Huihui；留空用系统默认"
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
              />
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">语速（-10 ~ 10）</label>
              <input
                type="number"
                min={-10}
                max={10}
                value={Number(node.data.rate ?? 0)}
                onChange={(e) => onUpdate('rate', Number(e.target.value) || 0)}
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
              />
            </div>
            <p className="text-[10px] text-gray-500">
              Windows 下走系统内置 SAPI（无需 Python 服务）；配置了 TTS_BASE_URL 时优先走服务。
            </p>
          </div>
        )}

        {node.type === 'storyboard' && (
          <div className="flex flex-col gap-3">
            <div>
              <label className="text-xs text-gray-400 block mb-1">内容需求（留空取上游 text）</label>
              <textarea
                rows={4}
                value={(node.data.brief as string) || ''}
                onChange={(e) => onUpdate('brief', e.target.value)}
                placeholder="例：师徒二人在雨夜客栈对峙，师傅要废掉徒弟的修为"
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
              />
            </div>
            <div className="grid grid-cols-2 gap-2">
              <div>
                <label className="text-[10px] text-gray-500 block mb-1">目标总时长（秒）</label>
                <input
                  type="number" min={5} max={180}
                  value={Number(node.data.seconds ?? 30)}
                  onChange={(e) => onUpdate('seconds', Number(e.target.value) || 30)}
                  className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                />
              </div>
              <div>
                <label className="text-[10px] text-gray-500 block mb-1">镜头数上限</label>
                <input
                  type="number" min={1} max={12}
                  value={Number(node.data.max_shots ?? 6)}
                  onChange={(e) => onUpdate('max_shots', Number(e.target.value) || 6)}
                  className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                />
              </div>
            </div>
            <p className="text-[10px] text-gray-500 leading-relaxed">
              输出结构化分镜表：景别 / 角度 / 轴线 / 运镜 / 动作 / 声音 / 台词 / 衔接方式。
              时长按内容给（3–8 秒，自动吸附 H3 的 17k+5 帧网格），不强制均分。
            </p>
          </div>
        )}

        {node.type === 'quality_gate' && (
          <div className="flex flex-col gap-3">
            <p className="text-[10px] text-gray-500 leading-relaxed">
              对上游产物跑声明式质检，任一项 fail 级不过即阻断下游。
              规则全部可留空（留空 = 不检）；产物取自最近一条上游的图片/音频/视频线。
            </p>
            <label className="flex items-center gap-2 text-[11px] text-gray-400">
              <input
                type="checkbox"
                checked={node.data.require_audio !== false}
                onChange={(e) => onUpdate('require_audio', e.target.checked)}
              />
              必须有音轨（防 -map 漏写产出纯视频）
            </label>
            <div className="grid grid-cols-3 gap-2">
              {([['min_frames', '最少帧数'], ['duration_min', '最短(s)'], ['duration_max', '最长(s)']] as const).map(([k, label]) => (
                <div key={k}>
                  <label className="text-[10px] text-gray-500 block mb-1">{label}</label>
                  <input
                    type="number"
                    value={Number(node.data[k] ?? 0)}
                    onChange={(e) => onUpdate(k, Number(e.target.value) || 0)}
                    className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                  />
                </div>
              ))}
            </div>
            <div className="grid grid-cols-3 gap-2">
              {([['min_width', '最小宽'], ['min_height', '最小高'], ['duration_tolerance', '音画容差(s)']] as const).map(([k, label]) => (
                <div key={k}>
                  <label className="text-[10px] text-gray-500 block mb-1">{label}</label>
                  <input
                    type="number"
                    step="0.01"
                    value={Number(node.data[k] ?? 0)}
                    onChange={(e) => onUpdate(k, Number(e.target.value) || 0)}
                    className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                  />
                </div>
              ))}
            </div>
            <div className="grid grid-cols-2 gap-2">
              {([['max_silence_gap', '最长静音(s)'], ['loudness_min_db', '响度下限(dB)']] as const).map(([k, label]) => (
                <div key={k}>
                  <label className="text-[10px] text-gray-500 block mb-1">{label}</label>
                  <input
                    type="number"
                    step="0.1"
                    value={Number(node.data[k] ?? 0)}
                    onChange={(e) => onUpdate(k, Number(e.target.value) || 0)}
                    className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                  />
                </div>
              ))}
            </div>
            <label className="flex items-center gap-2 text-[11px] text-gray-400">
              <input
                type="checkbox"
                checked={node.data.forbid_speech === true}
                onChange={(e) => onUpdate('forbid_speech', e.target.checked)}
              />
              氛围模式（疑似有人声即 FAIL，能量法判读）
            </label>
          </div>
        )}

        {node.type === 'post_production' && (
          <div className="flex flex-col gap-3">
            <div>
              <label className="text-xs text-gray-400 block mb-1">视频路径（留空取上游 video）</label>
              <input
                value={(node.data.video_path as string) || ''}
                onChange={(e) => onUpdate('video_path', e.target.value)}
                placeholder="留空则用上游连线的成片"
                className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
              />
            </div>
            <div className="grid grid-cols-2 gap-2">
              <div>
                <label className="text-[10px] text-gray-500 block mb-1">构图</label>
                <select
                  value={(node.data.aspect as string) || '9:16'}
                  onChange={(e) => onUpdate('aspect', e.target.value)}
                  className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                >
                  <option value="9:16">9:16 竖屏</option>
                  <option value="1:1">1:1 方形</option>
                  <option value="16:9">16:9 横屏</option>
                  <option value="keep">保持原构图</option>
                </select>
              </div>
              <div>
                <label className="text-[10px] text-gray-500 block mb-1">填充方式</label>
                <select
                  value={(node.data.fit as string) || 'blur'}
                  onChange={(e) => onUpdate('fit', e.target.value)}
                  className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                >
                  <option value="blur">模糊垫底</option>
                  <option value="crop">中心裁切</option>
                </select>
              </div>
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">背景音乐（留空不混）</label>
              <input
                value={(node.data.bgm as string) || ''}
                onChange={(e) => onUpdate('bgm', e.target.value)}
                placeholder="data/bgm/xxx.mp3（短于成片会自动循环）"
                className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
              />
            </div>
            <div className="grid grid-cols-2 gap-2 items-end">
              <div>
                <label className="text-[10px] text-gray-500 block mb-1">BGM 增益 dB</label>
                <input
                  type="number"
                  value={Number(node.data.bgm_gain ?? -12)}
                  onChange={(e) => onUpdate('bgm_gain', Number(e.target.value) || 0)}
                  className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                />
              </div>
              <label className="flex items-center gap-2 text-[11px] text-gray-400 pb-1">
                <input
                  type="checkbox"
                  checked={node.data.duck !== false}
                  onChange={(e) => onUpdate('duck', e.target.checked)}
                />
                人声出现时压低 BGM
              </label>
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">响度归一（两遍法，改听感但更稳）</label>
              <select
                value={String((node.data.loudness as number) || 0)}
                onChange={(e) => onUpdate('loudness', Number(e.target.value))}
                className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
              >
                <option value="0">关闭（保持原音量）</option>
                <option value="-14">-14 LUFS（短视频通用）</option>
                <option value="-16">-16 LUFS（口播 / 播客）</option>
                <option value="-23">-23 LUFS（广播）</option>
              </select>
              <p className="text-[10px] text-gray-600 mt-1">
                先渲一条混音音轨测量，再带实测值归一；不是动态压平，段内起伏保留。
              </p>
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">字幕（可选：.srt 路径）</label>
              <input
                value={(node.data.srt_path as string) || ''}
                onChange={(e) => onUpdate('srt_path', e.target.value)}
                placeholder="留空可从分镜表/逐行台词生成"
                className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
              />
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">逐行台词（可选，一行一句）</label>
              <textarea
                rows={3}
                value={(node.data.dialogues as string) || ''}
                onChange={(e) => onUpdate('dialogues', e.target.value)}
                placeholder="没有分镜表时按成片总时长均分"
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-[10px] resize-y"
              />
            </div>
            <p className="text-[11px] text-gray-500 leading-relaxed">
              竖屏 / BGM / 字幕在同一次重编码里完成——分三个节点做会掉三轮画质。
              只想烧字幕用「字幕烧录」节点更轻。
            </p>
          </div>
        )}

        {node.type === 'subtitles' && (
          <div className="flex flex-col gap-3">
            <div>
              <label className="text-xs text-gray-400 block mb-1">视频路径（留空取上游 video）</label>
              <input
                value={(node.data.video_path as string) || ''}
                onChange={(e) => onUpdate('video_path', e.target.value)}
                placeholder="留空则用上游连线的成片"
                className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
              />
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">现成字幕（.srt，填了优先用它）</label>
              <input
                value={(node.data.srt_path as string) || ''}
                onChange={(e) => onUpdate('srt_path', e.target.value)}
                placeholder="data/generated/xxx.srt"
                className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
              />
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">分镜表 JSON（可选，留空取上游 plan）</label>
              <textarea
                rows={4}
                value={(node.data.plan as string) || ''}
                onChange={(e) => onUpdate('plan', e.target.value)}
                placeholder='{"shots":[{"seconds":5,"dialogue":"少女：原话"}]}'
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-[10px] font-mono resize-y"
              />
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">逐行台词（可选，没有分镜表时按总时长均分）</label>
              <textarea
                rows={4}
                value={(node.data.dialogues as string) || ''}
                onChange={(e) => onUpdate('dialogues', e.target.value)}
                placeholder="一行一句"
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-[10px] resize-y"
              />
            </div>
            <div className="grid grid-cols-2 gap-2">
              {([['font_size', '字号'], ['margin_v', '距底边']] as const).map(([k, label]) => (
                <div key={k}>
                  <label className="text-[10px] text-gray-500 block mb-1">{label}</label>
                  <input
                    type="number"
                    value={Number(node.data[k] ?? 0)}
                    onChange={(e) => onUpdate(k, Number(e.target.value) || 0)}
                    className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                  />
                </div>
              ))}
            </div>
            <p className="text-[11px] text-gray-500 leading-relaxed">
              优先级：.srt 文件 → 分镜表 → 逐行台词。分镜台词按「身份：原话」写，字幕只显示原话；
              每镜时长取自 seconds（无则按帧数/24fps）。产物旁边会留一份 .srt 便于复查。
            </p>
          </div>
        )}

        {node.type === 'shotlist' && (
          <div className="flex flex-col gap-3">
            <div>
              <label className="text-xs text-gray-400 block mb-1">分镜表 JSON（留空取上游 plan）</label>
              <textarea
                rows={5}
                value={(node.data.plan as string) || ''}
                onChange={(e) => onUpdate('plan', e.target.value)}
                placeholder='{"title":"…","character":"…","shots":[…]}"'
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-[10px] font-mono resize-y"
              />
            </div>
            <div className="grid grid-cols-3 gap-2">
              {([['width', '宽'], ['height', '高'], ['steps', '步数']] as const).map(([k, label]) => (
                <div key={k}>
                  <label className="text-[10px] text-gray-500 block mb-1">{label}</label>
                  <input
                    type="number"
                    value={Number(node.data[k] ?? 0)}
                    onChange={(e) => onUpdate(k, Number(e.target.value) || 0)}
                    className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                  />
                </div>
              ))}
            </div>
            <label className="flex items-center gap-2 text-[11px] text-gray-400">
              <input
                type="checkbox"
                checked={node.data.turbo !== false}
                onChange={(e) => onUpdate('turbo', e.target.checked)}
              />
              涡轮 LoRA（8 步）
            </label>
            <label className="flex items-start gap-2 text-[11px] text-gray-400">
              <input
                type="checkbox"
                className="mt-0.5"
                checked={node.data.audio_check !== false}
                onChange={(e) => onUpdate('audio_check', e.target.checked)}
              />
              <span>
                音频抽检（ASR 检测到人声 → 自动压低音量，消除乱语但保留环境声）
              </span>
            </label>
            <label className="flex items-center gap-2 text-[11px] text-gray-400">
              <input
                type="checkbox"
                checked={Boolean(node.data.interp)}
                onChange={(e) => onUpdate('interp', e.target.checked)}
              />
              成片后插帧到 48fps（CPU）
            </label>
            <p className="text-[10px] text-gray-500 leading-relaxed">
              按每镜 transition 装配：cut=硬切（1 帧）、continue=末帧续接（融 1 帧）、dissolve=叠化 0.3s；
              最后统一响度归一。
            </p>
          </div>
        )}

        {node.type === 'comfy_workflow' && (
          <div className="flex flex-col gap-3">
            <div className="text-[10px] leading-relaxed text-gray-500 bg-canvas border border-border rounded-lg p-2">
              在 ComfyUI 里用菜单 <span className="text-gray-300">Workflow → Export (API)</span> 导出，
              把 JSON 粘到下面。UI 格式（含 nodes/links 数组）无法执行。
            </div>
            <div>
              <label className="text-[10px] text-gray-500 block mb-1">
                工作流文件（data/comfy_workflows/ 下的文件名，可留空）
              </label>
              <input
                value={(node.data.workflow_file as string) || ''}
                onChange={(e) => onUpdate('workflow_file', e.target.value)}
                placeholder="my_workflow"
                className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
              />
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">ComfyUI 工作流 JSON（API 格式）</label>
              <textarea
                rows={6}
                value={typeof node.data.workflow === 'string'
                  ? node.data.workflow
                  : JSON.stringify(node.data.workflow ?? {}, null, 2)}
                onChange={(e) => onUpdate('workflow', e.target.value)}
                placeholder='{"3":{"class_type":"KSampler","inputs":{...}},"9":{"class_type":"SaveImage","inputs":{...}}}'
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-[10px] font-mono resize-y"
              />
            </div>
            <div>
              <label className="text-[10px] text-gray-500 block mb-1">
                输入绑定（一行一条：节点id.输入名=值）
              </label>
              <textarea
                rows={3}
                value={Object.entries((node.data.bindings as Record<string, string>) || {})
                  .map(([k, v]) => `${k}=${v}`)
                  .join('\n')}
                onChange={(e) => {
                  // 逐行「path=value」而不是 JSON：用户正在敲 JSON 时中间态必然非法，
                  // 会在输入过程中把配置清空。这种格式任何中间态都是合法的。
                  const obj: Record<string, string> = {}
                  e.target.value.split('\n').forEach((line) => {
                    const i = line.indexOf('=')
                    if (i > 0) obj[line.slice(0, i).trim()] = line.slice(i + 1).trim()
                  })
                  onUpdate('bindings', obj)
                }}
                placeholder={'6.text={{prompt}}\n5.width=1024'}
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-[10px] font-mono resize-y"
              />
            </div>
            <div>
              <label className="text-[10px] text-gray-500 block mb-1">
                提示词（自动绑到 {'{{prompt}}'}，留空取上游 text）
              </label>
              <textarea
                rows={2}
                value={(node.data.prompt as string) || ''}
                onChange={(e) => onUpdate('prompt', e.target.value)}
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
              />
            </div>
            <div>
              <label className="text-[10px] text-gray-500 block mb-1">超时（秒）</label>
              <input
                type="number"
                min={30}
                value={Number(node.data.timeout_sec ?? 900)}
                onChange={(e) => onUpdate('timeout_sec', Number(e.target.value) || 900)}
                className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
              />
            </div>
          </div>
        )}

        {node.type === 'prompt_opt' && (
          <div className="flex flex-col gap-3">
            <div>
              <label className="text-xs text-gray-400 block mb-1">原始想法（留空取上游 text）</label>
              <textarea
                rows={4}
                value={(node.data.text as string) || ''}
                onChange={(e) => onUpdate('text', e.target.value)}
                placeholder="例：雪山上的白鹿，很美，有大片留白"
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
              />
            </div>
            <div className="grid grid-cols-2 gap-2">
              <div>
                <label className="text-[10px] text-gray-500 block mb-1">目标时长（秒）</label>
                <input
                  type="number" min={2} max={15}
                  value={Number(node.data.seconds ?? 5)}
                  onChange={(e) => onUpdate('seconds', Number(e.target.value) || 5)}
                  className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                />
              </div>
              <div>
                <label className="text-[10px] text-gray-500 block mb-1">优化目标</label>
                <select
                  value={(node.data.mode as string) || 'h3'}
                  onChange={(e) => onUpdate('mode', e.target.value)}
                  className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                >
                  <option value="h3">MiniMax H3（音视频联合）</option>
                </select>
              </div>
            </div>
            <p className="text-[10px] text-gray-500 leading-relaxed">
              按官方指南改写：单一主导动作 + 可见收尾 + 一个主运镜 + ≤2 层声音（绑定可见事件）。
              优化失败时自动透传原文，不中断工作流。
            </p>
          </div>
        )}

        {node.type === 'scene_grid' && (() => {
          const arr = String(node.data.cells || '').split('\n')
          const at = (i: number) => arr[i] ?? ''
          const setAt = (i: number, v: string) => {
            const next = Array.from({ length: 9 }, (_, k) => (k === i ? v : (arr[k] ?? '')))
            onUpdate('cells', next.join('\n'))
          }
          const labels = ['左上', '上中', '右上', '左中', '中心', '右中', '左下', '下中', '右下']
          return (
            <div className="flex flex-col gap-3">
              <div>
                <label className="text-xs text-gray-400 block mb-1">
                  场景想法（填了由 LLM 拆九格；只填格位则不花 LLM）
                </label>
                <textarea
                  rows={2}
                  value={(node.data.idea as string) || ''}
                  onChange={(e) => onUpdate('idea', e.target.value)}
                  placeholder="例：破败山门内的石殿，唯一光源是左中的铁灯，地面积水"
                  className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
                />
              </div>
              <div>
                <label className="text-[10px] text-gray-500 block mb-1">
                  九宫格（英文；写材质/颜色/形状/位置，不写形容词，用 - 留空）
                </label>
                <div className="grid grid-cols-3 gap-1">
                  {labels.map((lb, i) => (
                    <div key={lb} className="relative">
                      <span className="absolute left-1 top-0.5 text-[9px] text-gray-600 pointer-events-none">{lb}</span>
                      <textarea
                        rows={2}
                        value={at(i)}
                        onChange={(e) => setAt(i, e.target.value)}
                        className="w-full pt-3 px-1 pb-1 rounded bg-canvas border border-border text-[10px] leading-snug resize-none"
                      />
                    </div>
                  ))}
                </div>
              </div>
              <div className="grid grid-cols-2 gap-2">
                <div>
                  <label className="text-[10px] text-gray-500 block mb-1">光位（方向+色温+硬度）</label>
                  <input
                    value={(node.data.light as string) || ''}
                    onChange={(e) => onUpdate('light', e.target.value)}
                    placeholder="a lone lantern at middle-left, 2700K, hard"
                    className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-[10px]"
                  />
                </div>
                <div>
                  <label className="text-[10px] text-gray-500 block mb-1">主色关系</label>
                  <input
                    value={(node.data.palette as string) || ''}
                    onChange={(e) => onUpdate('palette', e.target.value)}
                    placeholder="cold grey stone, rust-red cloth"
                    className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-[10px]"
                  />
                </div>
              </div>
              <div>
                <label className="text-[10px] text-gray-500 block mb-1">机位（高度 + 焦段，可选）</label>
                <input
                  value={(node.data.depth as string) || ''}
                  onChange={(e) => onUpdate('depth', e.target.value)}
                  placeholder="eye level, 35mm equivalent"
                  className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-[10px]"
                />
              </div>
              <p className="text-[10px] text-gray-500 leading-relaxed">
                九格编译成一句 SCENE LOCK，逐字注入每段提示词 —— 场景一致靠的是同一批事实出现在同一空间位置，
                不是形容词更多。下游文生视频分段（segments&gt;1）会逐段重注并校验字节全等，漂移直接进 prompt_warnings。
              </p>
            </div>
          )
        })()}

        {(node.type === 'image2image' || node.type === 'inpaint' || node.type === 'upscale' || node.type === 'text2video') && (
          <div className="flex flex-col gap-3">
            {node.type !== 'upscale' && (
              <div>
                <label className="text-xs text-gray-400 block mb-1">提示词</label>
                <textarea
                  rows={3}
                  value={(node.data.prompt as string) || ''}
                  onChange={(e) => onUpdate('prompt', e.target.value)}
                  placeholder={node.type === 'text2video' ? '描述镜头与动作，如 slow push in, wind in the grass' : '改写成什么'}
                  className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
                />
              </div>
            )}
            {(node.type !== 'text2video' || node.data.backend === 'h3') && (
              <div>
                <label className="text-xs text-gray-400 block mb-1">
                  {node.type === 'text2video' ? '首帧图片（留空=文生视频，连图=图生视频）' : '输入图片（留空取上游）'}
                </label>
                <input
                  value={(node.data.image_path as string) || ''}
                  onChange={(e) => onUpdate('image_path', e.target.value)}
                  placeholder="留空 = 用上游 image 输出"
                  className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                />
              </div>
            )}
            {node.type === 'inpaint' && (
              <div>
                <label className="text-xs text-gray-400 block mb-1">蒙版路径（白色=重绘区）</label>
                <input
                  value={(node.data.mask_path as string) || ''}
                  onChange={(e) => onUpdate('mask_path', e.target.value)}
                  placeholder="与原图同尺寸的黑白图"
                  className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                />
              </div>
            )}
            {node.type === 'inpaint' && (
              <div>
                <label className="text-xs text-gray-400 block mb-1">重绘模型（留空=默认弱修补）</label>
                <input
                  value={(node.data.model as string) || ''}
                  onChange={(e) => onUpdate('model', e.target.value)}
                  placeholder="如 Flux-Fill，先落位 D:/AI/Models 再用"
                  className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                />
              </div>
            )}
            {node.type === 'image2image' && (
              <div>
                <label className="text-xs text-gray-400 block mb-1">重绘强度（0.1 微调 → 0.9 大改）</label>
                <input
                  type="number" step={0.05} min={0.05} max={1}
                  value={Number(node.data.strength ?? 0.6)}
                  onChange={(e) => onUpdate('strength', Number(e.target.value) || 0.6)}
                  className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
                />
              </div>
            )}
            {node.type === 'upscale' && (
              <div>
                <label className="text-xs text-gray-400 block mb-1">放大模型</label>
                <input
                  value={(node.data.model as string) || 'RealESRGAN_x4plus.pth'}
                  onChange={(e) => onUpdate('model', e.target.value)}
                  className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                />
              </div>
            )}
            {node.type === 'text2video' && (
              <div>
                <label className="text-xs text-gray-400 block mb-1">视频后端</label>
                <select
                  value={(node.data.backend as string) || 'wan'}
                  onChange={(e) => {
                    const v = e.target.value
                    onUpdate('backend', v)
                    // 切后端时把默认尺寸/帧数一并换掉：H3 必须是 17k+5 帧
                    if (v === 'h3') {
                      onUpdate('width', 608)
                      onUpdate('height', 352)
                      onUpdate('frames', 124)
                      onUpdate('fps', 24)
                      onUpdate('steps', 20)
                    } else {
                      onUpdate('width', 832)
                      onUpdate('height', 480)
                      onUpdate('frames', 33)
                      onUpdate('fps', 16)
                    }
                  }}
                  className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
                >
                  <option value="wan">Wan 1.3B（快，无声，2 秒）</option>
                  <option value="h3">MiniMax H3（慢，自带立体声，5 秒）</option>
                </select>
              </div>
            )}
            {node.type === 'text2video' && (
              <label className="flex items-center gap-2 text-[11px] text-gray-400">
                <input
                  type="checkbox"
                  checked={Boolean(node.data.interp)}
                  onChange={(e) => onUpdate('interp', e.target.checked)}
                />
                出片后插帧翻倍（Wan 16→32fps / H3 24→48fps，CPU 完成，不占显存）
              </label>
            )}
            {node.type === 'text2video' && node.data.backend === 'h3' && (
              <label className="flex items-start gap-2 text-[11px] text-gray-400">
                <input
                  type="checkbox"
                  className="mt-0.5"
                  checked={node.data.prompt_opt !== false}
                  onChange={(e) => onUpdate('prompt_opt', e.target.checked)}
                />
                <span>
                  自动优化提示词（推荐）—— 按官方指南改写成「镜头 + 声音分镜」再进模型，
                  走本地 LLM 约 10 秒；失败自动回退原文
                </span>
              </label>
            )}
            {node.type === 'text2video' && node.data.backend === 'h3' && (
              <div>
                <label className="text-[11px] text-gray-400 block mb-1">
                  场景九宫格锁（可留空；也可从上游「场景九宫格」节点连入）
                </label>
                <textarea
                  rows={3}
                  value={(node.data.scene_grid as string) || ''}
                  onChange={(e) => onUpdate('scene_grid', e.target.value)}
                  placeholder="九行「左上|...」；或直接粘贴已编译的 SCENE LOCK 句"
                  className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-[10px] resize-y"
                />
                <p className="text-[10px] text-gray-500 mt-1 leading-relaxed">
                  分段生成时同一句锁会被逐字重注到每一段；锁句不一致或缺失会写进 prompt_warnings。
                </p>
              </div>
            )}
            {node.type === 'text2video' && node.data.backend === 'h3' && (
              <div className="flex flex-col gap-2">
                <label className="flex items-center gap-2 text-[11px] text-gray-400">
                  <input
                    type="checkbox"
                    checked={node.data.turbo !== false}
                    onChange={(e) => {
                      onUpdate('turbo', e.target.checked)
                      // AGENTS §5.7：音频比画面晚收敛，8 步会乱语；turbo 12 步、非 turbo 20 步
                      onUpdate('steps', e.target.checked ? 12 : 20)
                    }}
                  />
                  涡轮 LoRA（20 步 → 12 步，约 1.6 倍速；步数低于 12 音频会乱语，长片必开）
                </label>
                <label className="flex items-center gap-2 text-[11px] text-gray-400">
                  <input
                    type="checkbox"
                    checked={Boolean(node.data.low_vram)}
                    onChange={(e) => onUpdate('low_vram', e.target.checked)}
                  />
                  低显存模式（注意力/FFN 分块，可上 736×416）
                </label>
                <div>
                  <label className="text-[11px] text-gray-400 block mb-1">
                    分段数（每段 5.17 秒，自动拆分镜 + 末帧续接 + 拼接，最多 8 段≈41 秒）
                  </label>
                  <input
                    type="number" min={1} max={8}
                    value={Number(node.data.segments ?? 1)}
                    onChange={(e) => onUpdate('segments', Number(e.target.value) || 1)}
                    className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                  />
                </div>
              </div>
            )}
            {node.type === 'text2video' && node.data.backend === 'h3' && (
              <div className="flex flex-col gap-2 pt-2 border-t border-border">
                <div>
                  <label className="text-[11px] text-gray-400 block mb-1">
                    念稿音轨 ref_audio（给了就走 ref2va 台词模式）
                  </label>
                  <input
                    value={(node.data.ref_audio as string) || ''}
                    onChange={(e) => onUpdate('ref_audio', e.target.value)}
                    placeholder="dub.wav 绝对路径；留空 = 模型联合生成音频"
                    className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs font-mono"
                  />
                </div>
                <div>
                  <label className="text-[11px] text-gray-400 block mb-1">
                    参考图 ref_images（锁角色外貌，每行一条，≤9）
                  </label>
                  <textarea
                    rows={2}
                    value={(node.data.ref_images as string) || ''}
                    onChange={(e) => onUpdate('ref_images', e.target.value)}
                    placeholder="定妆图路径，换行分隔"
                    className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs font-mono resize-y"
                  />
                </div>
                <div>
                  <label className="text-[11px] text-gray-400 block mb-1">
                    最终音轨 audio_track（出片后 1:1 覆盖，保证不丢帧）
                  </label>
                  <input
                    value={(node.data.audio_track as string) || ''}
                    onChange={(e) => onUpdate('audio_track', e.target.value)}
                    placeholder="留空 = 用模型生成的音轨"
                    className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs font-mono"
                  />
                  <p className="text-[10px] text-gray-500 mt-1">
                    实测本机 ref2va 不执行 fully_copy、音轨会被重新生成。要让台词跨镜连续、
                    音景不重启，把同一段 wav 填这里；代价是口型不再与它同步。
                  </p>
                </div>
              </div>
            )}
            {node.type === 'text2video' && (
              <div className="grid grid-cols-2 gap-2">
                {([['width', '宽'], ['height', '高'], ['frames', '帧数'], ['fps', '帧率'], ['steps', '步数']] as const).map(([k, label]) => (
                  <div key={k}>
                    <label className="text-[10px] text-gray-500 block mb-1">{label}</label>
                    <input
                      type="number"
                      value={Number(node.data[k] ?? 0)}
                      onChange={(e) => onUpdate(k, Number(e.target.value) || 0)}
                      className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                    />
                  </div>
                ))}
              </div>
            )}
            <p className="text-[10px] text-gray-500 leading-relaxed">
              {node.type === 'text2video'
                ? (node.data.backend === 'h3'
                    ? 'MiniMax H3：一次推理同时出画面 + 立体声，608×352×124 帧≈5 秒（实测 172 秒）。会先停掉默认 ComfyUI 腾显存，用完可切回。'
                    : 'Wan2.1-T2V-1.3B：832×480×33 帧≈2 秒，约 64 秒出片，无音轨。')
                : '走 ComfyUI 本地节点，图片不出本机。'}
            </p>
          </div>
        )}

        {node.type === 'select' && (
          <div className="flex flex-col gap-3">
            <div>
              <label className="text-xs text-gray-400 block mb-1">参考主题（这张图本来该画什么）</label>
              <textarea
                rows={2}
                value={(node.data.reference as string) || ''}
                onChange={(e) => onUpdate('reference', e.target.value)}
                placeholder="留空 = 自动取上游的 prompt / original_prompt"
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
              />
              <p className="text-[10px] text-gray-500 mt-1">
                不给参考主题，模型只能按画面自评，结论会和你的要求无关。
              </p>
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">额外要求（可选）</label>
              <input
                value={(node.data.criteria as string) || ''}
                onChange={(e) => onUpdate('criteria', e.target.value)}
                placeholder="例如：不要出现文字；人物必须朝左"
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
              />
            </div>
            <div className="grid grid-cols-2 gap-2">
              <div>
                <label className="text-xs text-gray-400 block mb-1">采用阈值</label>
                <input
                  type="number"
                  min={0}
                  max={100}
                  value={(node.data.threshold as number) ?? 60}
                  onChange={(e) => onUpdate('threshold', Number(e.target.value) || 0)}
                  className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
                />
              </div>
              <div>
                <label className="text-xs text-gray-400 block mb-1">视觉模型（留空自动挑）</label>
                <input
                  value={(node.data.model as string) || ''}
                  onChange={(e) => onUpdate('model', e.target.value)}
                  placeholder="qwen2.5vl:7b"
                  className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                />
              </div>
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">候选图路径（可选，每行一条）</label>
              <textarea
                rows={3}
                value={(node.data.images as string) || ''}
                onChange={(e) => onUpdate('images', e.target.value)}
                placeholder="留空 = 收上游 image / images / values"
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono resize-y"
              />
            </div>
            <p className="text-[10px] text-gray-500">
              用法：文生图 batch=3 → 择优挑选 → 下游只拿最高分那张。只有 1 张候选时不打分（省一次换侧）。
            </p>
          </div>
        )}

        {node.type === 'vision' && (
          <div className="flex flex-col gap-3">
            <div>
              <label className="text-xs text-gray-400 block mb-1">提问</label>
              <textarea
                rows={4}
                value={(node.data.prompt as string) || ''}
                onChange={(e) => onUpdate('prompt', e.target.value)}
                placeholder="想问什么？例如：这张图里的人物衣着是什么朝代？"
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
              />
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">视觉模型（留空自动挑）</label>
              <input
                value={(node.data.model as string) || ''}
                onChange={(e) => onUpdate('model', e.target.value)}
                placeholder="qwen2.5vl:7b"
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
              />
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">图片路径（可选，覆盖上游）</label>
              <input
                value={(node.data.image_path as string) || ''}
                onChange={(e) => onUpdate('image_path', e.target.value)}
                placeholder="留空 = 用上游 image 输出"
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
              />
            </div>
            <p className="text-[10px] text-gray-500 leading-relaxed">
              走本地 Ollama（/api/chat + base64），图片不出本机。单图上限 8MB。
            </p>
          </div>
        )}

        {node.type === 'asr' && (
          <div className="flex flex-col gap-2">
            <label className="text-xs text-gray-400">音频路径（留空取上游 audio）</label>
            <input
              value={(node.data.audio_path as string) || ''}
              onChange={(e) => onUpdate('audio_path', e.target.value)}
              placeholder="data/generated/tts-xxx.wav"
              className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
            />
            <div className="flex gap-2">
              <div className="flex-1">
                <label className="text-[10px] text-gray-500 block mb-1">模型档位（留空=服务默认）</label>
                <select
                  value={(node.data.model as string) || ''}
                  onChange={(e) => onUpdate('model', e.target.value)}
                  className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
                >
                  <option value="">默认（small）</option>
                  <option value="tiny">tiny（最快，试听用）</option>
                  <option value="base">base</option>
                  <option value="small">small（默认）</option>
                  <option value="medium">medium（更准，更慢）</option>
                  <option value="large-v3">large-v3（台词镜头用）</option>
                </select>
              </div>
              <div className="w-24">
                <label className="text-[10px] text-gray-500 block mb-1">设备</label>
                <select
                  value={(node.data.device as string) || ''}
                  onChange={(e) => onUpdate('device', e.target.value)}
                  className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
                >
                  <option value="">默认（CPU）</option>
                  <option value="cpu">CPU（不占显存）</option>
                  <option value="cuda">CUDA（快，吃 3GB）</option>
                  <option value="auto">auto</option>
                </select>
              </div>
            </div>
            <p className="text-[10px] text-gray-500 leading-relaxed">
              档位按镜头挑：台词镜头 large-v3 保准确，氛围镜头 small 省时间。
              cuda 会占用约 3GB 显存——出片工作流里建议留 CPU。
            </p>
          </div>
        )}

        {node.type === 'http_request' && (
          <div className="flex flex-col gap-3">
            <div>
              <label className="text-xs text-gray-400 block mb-1">方法</label>
              <select
                value={(node.data.method as string) || 'GET'}
                onChange={(e) => onUpdate('method', e.target.value)}
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
              >
                {['GET', 'POST', 'PUT', 'PATCH', 'DELETE'].map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </select>
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">URL</label>
              <input
                value={(node.data.url as string) || ''}
                onChange={(e) => onUpdate('url', e.target.value)}
                placeholder="https://api.example.com/v1/chat"
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
              />
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">凭据名（推荐，免明文 Key）</label>
              <input
                value={(node.data.credential as string) || ''}
                onChange={(e) => onUpdate('credential', e.target.value)}
                placeholder="如 openai —— 在「凭据」面板里维护"
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
              />
            </div>
            <div>
              <label className="text-xs text-gray-400 block mb-1">请求体（JSON）</label>
              <textarea
                rows={3}
                value={(node.data.body as string) || ''}
                onChange={(e) => onUpdate('body', e.target.value)}
                className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono resize-y"
              />
            </div>
          </div>
        )}

        {node.type === 'end' && (
          <p className="text-xs text-gray-500">
            结束节点，输出最终结果
          </p>
        )}

        {/* 输出预览 */}
        {(() => {
          const out = node.data?.output
          if (out == null) return null
          const text = typeof out === 'string' ? out : JSON.stringify(out, null, 2)
          return (
            <div className="mt-2 pt-3 border-t border-border">
              <label className="text-xs text-gray-400 block mb-2">输出预览</label>
              <div className="p-2 rounded bg-canvas max-h-32 overflow-y-auto">
                <pre className="text-xs text-success font-mono whitespace-pre-wrap">{text}</pre>
              </div>
            </div>
          )
        })()}

        {/* 错误信息：错误码翻成人话 + 可执行修复动作（不再裸抛 timeout/config_missing） */}
        {node.data?.error != null && (
          <ErrorNotice
            code={node.data?.error_code as string | undefined}
            message={String(node.data.error)}
            onAction={(a) => {
              if (a === 'open-env') onErrorAction?.('open-env')
            }}
          />
        )}
      </div>
    ) : (
      <p className="text-xs text-gray-500">点击画布中的节点进行配置</p>
    )}
  </div>
  )
}
