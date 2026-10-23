import { useMemo, useState } from 'react'
import { PALETTE, NODE_REQUIRES, ENV_LABEL, type PaletteItem } from '../config/graph'
import {
  readCollapsed,
  topNodeTypes,
  writeCollapsed,
  type NodeUsageMap,
} from '../lib/nodeUsage'

interface Props {
  envReady: Record<string, boolean> | null
  /** 运行期插件节点（来自 /api/toolchain）：静态 PALETTE 不可能预见用户后装的插件 */
  plugins?: PaletteItem[]
  /** 使用频次由上层维护：addNode / 拖拽都会计数，避免这里和上层各记一次 */
  usage: NodeUsageMap
  onAdd: (type: string) => void
  onDragStart: (e: React.DragEvent, type: string) => void
}

// 左栏节点库：搜索 + 常用 + 分组折叠
// 25 个节点平铺会把左栏推到两屏之外，所以默认只展开前两组，其余折叠并记忆状态。
export default function NodePalette({ envReady, usage, plugins = [], onAdd, onDragStart }: Props) {
  const [query, setQuery] = useState('')
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>(() => {
    const saved = readCollapsed()
    if (saved) return saved
    // 首屏默认：控制 + AI 推理展开，其余收起
    const init: Record<string, boolean> = {}
    // 只按内置分组算默认折叠：sections 含动态插件分组，此时还没定义（且不该在这里依赖）
    PALETTE.forEach((sec, i) => {
      if (i > 1) init[sec.label] = true
    })
    return init
  })

  // 插件节点与内置节点同等对待：搜索、常用、拖拽走同一条路径。
  // 「插件」分组只在真的装了插件时出现，不给空分组占位。
  const sections = useMemo(
    () => (plugins.length > 0 ? [...PALETTE, { label: '插件', items: plugins }] : PALETTE),
    [plugins],
  )

  const allItems = useMemo(() => sections.flatMap((s) => s.items), [sections])

  const q = query.trim().toLowerCase()
  const matches = useMemo(() => {
    if (!q) return null
    return allItems.filter(
      (it) => it.label.toLowerCase().includes(q) || it.type.toLowerCase().includes(q),
    )
  }, [q, allItems])

  // 常用：真实使用过的前 4 个，没有就不显示这一区
  const frequent = useMemo(() => {
    const types = topNodeTypes(usage, 4)
    return types
      .map((t) => allItems.find((it) => it.type === t))
      .filter((x): x is NonNullable<typeof x> => Boolean(x))
  }, [usage, allItems])

  const toggle = (label: string) => {
    const next = { ...collapsed, [label]: !collapsed[label] }
    setCollapsed(next)
    writeCollapsed(next)
  }

  const pick = (type: string) => {
    onAdd(type)
  }

  const disabledOf = (type: string) =>
    Boolean(NODE_REQUIRES[type] && envReady && !envReady[NODE_REQUIRES[type]])

  const renderItem = (item: (typeof allItems)[number]) => (
    <div
      key={item.type}
      draggable
      onDragStart={(e) => onDragStart(e, item.type)}
      onClick={() => pick(item.type)}
      title={
        disabledOf(item.type)
          ? `当前环境未就绪（缺 ${ENV_LABEL[NODE_REQUIRES[item.type]] ?? NODE_REQUIRES[item.type]}），可以先摆好节点，补齐环境后再运行`
          : `${item.label} · ${item.type}`
      }
      className={`fe-palette-item group flex items-center gap-2 px-2.5 py-1.5 text-left cursor-grab active:cursor-grabbing ${
        disabledOf(item.type) ? 'opacity-45' : ''
      }`}
    >
      <span className={`fe-palette-icon ${item.tone}`}>{item.icon}</span>
      <span className="text-xs flex-1 truncate">{item.label}</span>
      <span className="fe-dim3 text-[9px] fe-num opacity-0 group-hover:opacity-100">+</span>
    </div>
  )

  return (
    <div>
      {/* 搜索：粘在滚动容器顶部 */}
      <div className="sticky top-0 z-10 bg-surface -mx-4 px-4 pt-0.5 pb-2">
        <div className="relative">
          <input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="搜索节点…"
            className="w-full pl-6 pr-2 py-1.5 rounded-md bg-canvas border border-border text-[11.5px] outline-none focus:border-primary"
          />
          <span className="absolute left-2 top-1/2 -translate-y-1/2 text-[10px] fe-dim3 pointer-events-none">
            ⌕
          </span>
          {query && (
            <button
              onClick={() => setQuery('')}
              className="absolute right-1.5 top-1/2 -translate-y-1/2 text-[11px] fe-dim3 hover:text-white px-1"
              title="清空"
            >
              ×
            </button>
          )}
        </div>
      </div>

      {/* 搜索结果：扁平列出，不带分组 */}
      {matches ? (
        matches.length === 0 ? (
          <p className="text-[10px] fe-dim3 px-1 py-2">没有匹配的节点</p>
        ) : (
          <>
            <div className="fe-section-label mt-1 mb-1">匹配 {matches.length} 个</div>
            <div className="flex flex-col gap-1">{matches.map(renderItem)}</div>
          </>
        )
      ) : (
        <>
          {/* 常用 */}
          {frequent.length > 0 && (
            <>
              <div className="flex items-center justify-between mt-0.5 mb-1">
                <div className="fe-section-label">常用</div>
                <span className="text-[9px] fe-dim3">按使用次数</span>
              </div>
              <div className="flex flex-col gap-1 mb-1">{frequent.map(renderItem)}</div>
            </>
          )}

          {/* 分组 */}
          {sections.map((section) => {
            const isCollapsed = Boolean(collapsed[section.label])
            return (
              <div key={section.label}>
                <button
                  onClick={() => toggle(section.label)}
                  className="w-full flex items-center gap-1 mt-2 mb-1 group"
                >
                  <span
                    className="fe-dim3 text-[8px] transition-transform"
                    style={{ transform: isCollapsed ? 'none' : 'rotate(90deg)' }}
                  >
                    ▶
                  </span>
                  <span className="fe-section-label group-hover:text-gray-300">
                    {section.label}
                  </span>
                  <span className="fe-dim3 text-[9px] fe-num ml-0.5">{section.items.length}</span>
                </button>
                {!isCollapsed && (
                  <div className="flex flex-col gap-1">{section.items.map(renderItem)}</div>
                )}
              </div>
            )
          })}
        </>
      )}
    </div>
  )
}
