import { hintFor, type ErrorAction } from '../lib/errorHints'

// 错误展示的统一组件。
//
// 背景：后端有 7 个结构化错误码，但前端此前只把原始字符串丢给用户，
// 用户看到 `timeout` / `config_missing` 这种机器词，既不知道发生了什么也不知道点哪里。
// 这里统一渲染成「标题 + 人话原因 + 修复动作 + 原始报错」。

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
  return (
    <div
      className={`rounded-lg border p-2.5 ${
        hint.level === 'warn' ? 'border-warn/40 bg-warn/10' : 'border-error/40 bg-error/10'
      } ${className ?? ''}`}
      role="alert"
    >
      <div className="flex items-start gap-2">
        <span className={`text-[11px] leading-5 ${hint.level === 'warn' ? 'text-warn' : 'text-error'}`}>
          {hint.level === 'warn' ? '!' : '×'}
        </span>
        <div className="flex-1 min-w-0">
          <p className={`text-[12px] font-medium ${hint.level === 'warn' ? 'text-warn' : 'text-error'}`}>
            {hint.title}
            {code ? <span className="ml-1.5 text-[10px] font-normal opacity-60 font-mono">{code}</span> : null}
          </p>
          {!compact && (
            <p className="text-[11px] text-gray-400 mt-1 leading-relaxed">{hint.detail}</p>
          )}
          {!compact && hint.fix && (
            <p className="text-[11px] text-gray-300 mt-1 leading-relaxed">→ {hint.fix}</p>
          )}
          {message ? (
            <pre className="mt-1.5 text-[10.5px] text-gray-400 font-mono whitespace-pre-wrap break-all max-h-32 overflow-auto">
              {message}
            </pre>
          ) : null}
        </div>
        {action && onAction ? (
          <button
            onClick={() => onAction(action)}
            className="shrink-0 text-[10.5px] px-2 py-1 rounded border border-border text-gray-300 hover:text-white hover:border-primary/60"
          >
            {ACTION_LABEL[action]}
          </button>
        ) : null}
      </div>
    </div>
  )
}
