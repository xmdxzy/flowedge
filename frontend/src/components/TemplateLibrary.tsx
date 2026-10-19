import { useEffect, useMemo, useState } from 'react'
import type { WorkflowTemplate } from '../templates/workflowTemplates'
import {
  TEMPLATE_GROUPS,
  buildEntries,
  bumpUsage,
  groupOf,
  readUsage,
  resetUsage,
  type TemplateGroupId,
  type UsageMap,
} from '../templates/templateCatalog'

interface Props {
  open: boolean
  onClose: () => void
  onApply: (tpl: WorkflowTemplate) => void
}

// 模板库：分组 + 搜索 + 置顶常用。左栏只留一个入口，全量在这里看。
// 排序由 templateCatalog 统一决定（置顶 > 使用频次 > 成熟度），本组件不再二次排序。
export default function TemplateLibrary({ open, onClose, onApply }: Props) {
  const [usage, setUsage] = useState<UsageMap>({})
  const [group, setGroup] = useState<TemplateGroupId | 'all' | 'pinned'>('all')
  const [query, setQuery] = useState('')

  useEffect(() => {
    if (!open) return
    setUsage(readUsage())
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose])

  const entries = useMemo(() => buildEntries(usage), [usage])

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    return entries.filter((e) => {
      if (group === 'pinned' && !(e.meta.pinned || 0)) return false
      if (group !== 'all' && group !== 'pinned' && e.meta.group !== group) return false
      if (!q) return true
      const hay = [
        e.tpl.name,
        e.tpl.description,
        e.meta.useCase || '',
        e.meta.pinNote || '',
        e.tpl.category,
        (e.meta.tags || []).join(' '),
        groupOf(e.meta.group).label,
      ]
        .join(' ')
        .toLowerCase()
      return hay.includes(q)
    })
  }, [entries, group, query])

  const apply = (tpl: WorkflowTemplate) => {
    setUsage(bumpUsage(tpl.id))
    onApply(tpl)
    onClose()
  }

  if (!open) return null

  const pinnedCount = entries.filter((e) => (e.meta.pinned || 0) > 0).length
  const countOf = (id: TemplateGroupId) => entries.filter((e) => e.meta.group === id).length

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/55 backdrop-blur-[2px]"
      onClick={onClose}
    >
      <div
        className="fe-lib w-[1000px] max-w-[94vw] h-[74vh] flex flex-col overflow-hidden"
        onClick={(e) => e.stopPropagation()}
      >
        {/* 头 */}
        <div className="fe-lib-head flex items-center gap-3 px-4 h-12 flex-none">
          <span className="fe-lib-mark">T</span>
          <div className="leading-tight">
            <div className="text-[13px] font-semibold text-white">模板库</div>
            <div className="text-[10px] fe-dim">
              {entries.length} 个模板 · 置顶 {pinnedCount} · 按置顶与使用频次排序
            </div>
          </div>
          <div className="ml-auto flex items-center gap-2">
            <input
              autoFocus
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="搜模板 / 标签 / 场景…"
              className="fe-lib-search w-56 px-2.5 py-1.5 rounded-md text-xs"
            />
            <button onClick={onClose} className="fe-btn-ghost px-2.5 py-1.5 rounded-md text-xs">
              关闭
            </button>
          </div>
        </div>

        <div className="flex flex-1 min-h-0">
          {/* 分组导航 */}
          <div className="fe-lib-side w-44 flex-none overflow-y-auto py-2">
            <NavItem
              active={group === 'all'}
              label="全部"
              hint={`${entries.length}`}
              onClick={() => setGroup('all')}
            />
            <NavItem
              active={group === 'pinned'}
              label="置顶常用"
              hint={`${pinnedCount}`}
              accent
              onClick={() => setGroup('pinned')}
            />
            <div className="fe-lib-sep" />
            {TEMPLATE_GROUPS.map((g) => (
              <NavItem
                key={g.id}
                active={group === g.id}
                label={g.label}
                icon={g.icon}
                hint={`${countOf(g.id)}`}
                title={g.hint}
                onClick={() => setGroup(g.id)}
              />
            ))}
            <div className="fe-lib-sep" />
            <button
              onClick={() => {
                resetUsage()
                setUsage({})
              }}
              className="w-full text-left px-3 py-1.5 text-[10px] fe-dim hover:text-gray-300"
              title="清空本地记录的模板使用次数"
            >
              清空使用记录
            </button>
          </div>

          {/* 卡片区 */}
          <div className="flex-1 overflow-y-auto px-4 py-3">
            {group !== 'all' && group !== 'pinned' && (
              <div className="mb-2 text-[10.5px] fe-dim">{groupOf(group).hint}</div>
            )}
            {filtered.length === 0 ? (
              <div className="fe-lib-empty">没有匹配的模板</div>
            ) : (
              <div className="grid grid-cols-2 gap-2">
                {filtered.map((e) => (
                  <button
                    key={e.tpl.id}
                    onClick={() => apply(e.tpl)}
                    className="fe-tpl-card text-left flex flex-col gap-1.5 p-3"
                  >
                    <div className="flex items-center gap-1.5">
                      <span className="text-[12.5px] font-medium text-white flex-1 truncate">
                        {e.tpl.name}
                      </span>
                      {(e.meta.pinned || 0) > 0 && <span className="fe-tag fe-tag-pin">置顶</span>}
                      <span
                        className={`fe-tag ${e.meta.maturity === 'stable' ? 'fe-tag-ok' : 'fe-tag-beta'}`}
                      >
                        {e.meta.maturity === 'stable' ? '稳定' : '试用'}
                      </span>
                    </div>

                    <div className="text-[11px] fe-dim2 leading-snug line-clamp-2">
                      {e.meta.pinNote || e.meta.useCase || e.tpl.description}
                    </div>

                    <div className="flex items-center gap-1.5 flex-wrap mt-0.5">
                      <span className="fe-tag fe-tag-quiet">{groupOf(e.meta.group).label}</span>
                      {(e.meta.tags || []).slice(0, 2).map((t) => (
                        <span key={t} className="fe-tag fe-tag-quiet">
                          {t}
                        </span>
                      ))}
                      {e.meta.cost && <span className="fe-tag fe-tag-cost">{e.meta.cost}</span>}
                    </div>

                    {e.usage.count > 0 && (
                      <div className="text-[9.5px] fe-dim3 mt-auto pt-1">
                        已用 {e.usage.count} 次 · {formatWhen(e.usage.lastUsed)}
                      </div>
                    )}
                  </button>
                ))}
              </div>
            )}
          </div>
        </div>

        <div className="fe-lib-foot flex items-center gap-3 px-4 h-8 flex-none text-[10px] fe-dim">
          <span>点击卡片直接套用，替换当前画布</span>
          <span className="ml-auto">排序：置顶权重 → 使用次数 → 成熟度</span>
        </div>
      </div>
    </div>
  )
}

function NavItem({
  active,
  label,
  hint,
  icon,
  title,
  accent,
  onClick,
}: {
  active: boolean
  label: string
  hint?: string
  icon?: string
  title?: string
  accent?: boolean
  onClick: () => void
}) {
  return (
    <button
      onClick={onClick}
      title={title}
      className={`fe-lib-nav w-full flex items-center gap-2 px-3 py-1.5 text-[11.5px] ${active ? 'is-active' : ''} ${
        accent ? 'is-accent' : ''
      }`}
    >
      {icon && <span className="w-3 text-center fe-dim3 text-[10px]">{icon}</span>}
      <span className="flex-1 text-left truncate">{label}</span>
      {hint && <span className="fe-num text-[10px] fe-dim3">{hint}</span>}
    </button>
  )
}

function formatWhen(ts: number): string {
  if (!ts) return ''
  const diff = Date.now() - ts
  const day = 86400000
  if (diff < 3600000) return '刚刚'
  if (diff < day) return `${Math.floor(diff / 3600000)} 小时前`
  if (diff < day * 30) return `${Math.floor(diff / day)} 天前`
  return new Date(ts).toLocaleDateString('zh-CN')
}
