import { useEffect, useMemo, useRef, useState } from 'react'

export interface CommandItem {
  id: string
  label: string
  hint?: string
  icon?: string
  section: string
  run: () => void
}

interface Props {
  items: CommandItem[]
  onClose: () => void
}

// 命令面板（Ctrl/Cmd + K）：节点、模板、面板、动作统一搜索
export default function CommandPalette({ items, onClose }: Props) {
  const [query, setQuery] = useState('')
  const [cursor, setCursor] = useState(0)
  const inputRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    inputRef.current?.focus()
  }, [])

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!q) return items
    return items.filter(
      (i) =>
        i.label.toLowerCase().includes(q) ||
        (i.hint || '').toLowerCase().includes(q) ||
        i.section.toLowerCase().includes(q),
    )
  }, [items, query])

  useEffect(() => {
    setCursor(0)
  }, [query])

  useEffect(() => {
    const el = listRef.current?.querySelector(`[data-idx="${cursor}"]`)
    el?.scrollIntoView({ block: 'nearest' })
  }, [cursor])

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') {
      onClose()
      return
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setCursor((c) => Math.min(c + 1, filtered.length - 1))
      return
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault()
      setCursor((c) => Math.max(c - 1, 0))
      return
    }
    if (e.key === 'Enter') {
      e.preventDefault()
      const item = filtered[cursor]
      if (item) {
        item.run()
        onClose()
      }
    }
  }

  // 按 section 分组展示
  let lastSection = ''

  return (
    <div
      className="fixed inset-0 z-[70] bg-black/60 backdrop-blur-sm flex items-start justify-center pt-[12vh]"
      onClick={onClose}
    >
      <div
        className="w-[560px] rounded-2xl border border-border bg-[#0D121C] shadow-2xl overflow-hidden"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center gap-2 px-4 py-3 border-b border-border">
          <span className="text-gray-500 text-sm">⌕</span>
          <input
            ref={inputRef}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={onKeyDown}
            placeholder="搜索节点、模板、面板…"
            className="flex-1 bg-transparent text-sm text-white placeholder-gray-600 outline-none"
          />
          <kbd className="text-[10px] text-gray-600 border border-border rounded px-1.5 py-0.5">ESC</kbd>
        </div>

        <div ref={listRef} className="max-h-[46vh] overflow-y-auto py-1">
          {filtered.length === 0 && (
            <p className="px-4 py-6 text-center text-xs text-gray-500">没有匹配项</p>
          )}
          {filtered.map((item, idx) => {
            const showSection = item.section !== lastSection
            lastSection = item.section
            return (
              <div key={item.id}>
                {showSection && (
                  <div className="px-4 pt-2 pb-1 text-[10px] text-gray-600 uppercase tracking-wider">
                    {item.section}
                  </div>
                )}
                <div
                  data-idx={idx}
                  onMouseEnter={() => setCursor(idx)}
                  onClick={() => {
                    item.run()
                    onClose()
                  }}
                  className={`mx-2 px-2.5 py-2 rounded-lg flex items-center gap-2.5 cursor-pointer transition-colors ${
                    idx === cursor ? 'bg-primary/20' : 'hover:bg-surface-hover'
                  }`}
                >
                  <span className="fe-palette-icon tone-muted">{item.icon || '·'}</span>
                  <span className="text-xs text-white">{item.label}</span>
                  {item.hint && <span className="text-[10px] text-gray-500 ml-auto">{item.hint}</span>}
                </div>
              </div>
            )
          })}
        </div>

        <div className="px-4 py-2 border-t border-border flex items-center gap-3 text-[10px] text-gray-600">
          <span>↑↓ 选择</span>
          <span>↵ 执行</span>
          <span className="ml-auto">Ctrl/Cmd + K 随时唤起</span>
        </div>
      </div>
    </div>
  )
}
