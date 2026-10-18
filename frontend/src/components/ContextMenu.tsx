import { useEffect, useRef } from 'react'

interface MenuItem {
  label: string
  icon?: string
  danger?: boolean
  onClick: () => void
}

interface ContextMenuProps {
  x: number
  y: number
  items: MenuItem[]
  onClose: () => void
}

export default function ContextMenu({ x, y, items, onClose }: ContextMenuProps) {
  const menuRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const handleClick = () => onClose()
    const handleEsc = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('click', handleClick)
    window.addEventListener('keydown', handleEsc)
    return () => {
      window.removeEventListener('click', handleClick)
      window.removeEventListener('keydown', handleEsc)
    }
  }, [onClose])

  return (
    <div
      ref={menuRef}
      className="fixed z-50 min-w-[160px] rounded-lg bg-surface border border-border shadow-2xl py-1"
      style={{ left: x, top: y }}
      onClick={(e) => e.stopPropagation()}
    >
      {items.map((item, i) => (
        <button
          key={i}
          onClick={item.onClick}
          className={`w-full px-3 py-2 text-left text-sm flex items-center gap-2 hover:bg-surface-hover transition-colors
            ${item.danger ? 'text-error hover:bg-error/10' : 'text-gray-200'}
          `}
        >
          {item.icon && <span>{item.icon}</span>}
          {item.label}
        </button>
      ))}
    </div>
  )
}
