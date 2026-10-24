import { hintFor, type ErrorAction } from '../lib/errorHints'

// 错误展示的统一组件。
//
// 后端有结构化错误码（config_missing / timeout …），但原始字符串才是排错依据：
// 这里统一渲染成「标题 + 人话原因 + 修复动作 + 原始报错」，两者都不丢。

const ACTION_LABEL: Record<ErrorAction, string> = {
  'open-env': '打开环境面板',
  'open-history': '查看执行历史',
  'retry-node': '重跑此节点',
  'open-kb': '打开知识库',
}

interface Props {
  code?: string
  message?: string
  onAction?: (action: ErrorAction) => void
  /** 紧凑模式：只显示标题 + 原始报错（列表行里用） */
  compact?: boolean
  className?: string
}

export default function ErrorNotice({ code, message, onAction, compact, className }: Props) {
  const hint = hintFor(code)
  const action = hint.action
  const level = hint.level === 'warn' ? 'warn' : 'error'
  return (
    <div className={`notice notice--${level} ${className ?? ''}`} role="alert">
      <div className="notice__row">
        <span className="notice__mark" aria-hidden="true">
          {hint.level === 'warn' ? '!' : '×'}
        </span>
        <div className="notice__body">
          <p className="notice__title">
            {hint.title}
            {code ? <span className="notice__code mono">{code}</span> : null}
          </p>
          {!compact && <p className="notice__detail">{hint.detail}</p>}
          {!compact && hint.fix && <p className="notice__fix">→ {hint.fix}</p>}
          {message ? <pre className="notice__raw mono">{message}</pre> : null}
        </div>
        {action && onAction ? (
          <button onClick={() => onAction(action)} className="btn btn--ghost btn--sm">
            {ACTION_LABEL[action]}
          </button>
        ) : null}
      </div>
    </div>
  )
}
