import { Component, type ErrorInfo, type ReactNode } from 'react'

// 顶层错误边界：渲染期抛错收在这里，而不是变成一张白屏。
//
// 为什么必须有：出片一次跑几分钟，界面崩了用户至少还能「重载」并把报错原文带回；
// 白屏时既没有日志也没有按钮，等于让人自己猜。published 版本连控制台都没人开。

interface Props {
  children: ReactNode
}

interface State {
  error: Error | null
  copied: boolean
}

/** 崩溃记录只留最近一次，写不进 localStorage（隐私模式）不算失败 */
const CRASH_KEY = 'flowedge.lastCrash'

export default class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null, copied: false }

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    try {
      localStorage.setItem(
        CRASH_KEY,
        JSON.stringify({
          message: error.message,
          stack: (error.stack ?? '').split('\n').slice(0, 12).join('\n'),
          componentStack: (info.componentStack ?? '').trim().split('\n').slice(0, 12).join('\n'),
          at: new Date().toISOString(),
          page: window.location.pathname + window.location.search,
        }),
      )
    } catch {
      /* 写不了就不写：展示不依赖它 */
    }
  }

  private copy = async () => {
    const { error } = this.state
    if (!error) return
    const text = [error.message, error.stack ?? ''].filter(Boolean).join('\n\n')
    try {
      await navigator.clipboard.writeText(text)
      this.setState({ copied: true })
    } catch {
      // 剪贴板需要安全上下文/权限：拿不到就让用户手动选中
      this.setState({ copied: false })
    }
  }

  private reload = () => {
    window.location.reload()
  }

  render() {
    const { error } = this.state
    if (!error) return this.props.children

    return (
      <div className="crash" role="alert">
        <div className="crash__card">
          <p className="crash__title">界面遇到未处理的错误</p>
          <p className="crash__hint">
            已生成的任务不受影响，可以在「运行」页继续查看。重载后如果反复出现，
            请把下面的报错原文贴给维护者。
          </p>
          <pre className="crash__raw mono">{error.message}</pre>
          <div className="crash__actions">
            <button className="btn btn--primary" onClick={this.reload}>
              重新加载
            </button>
            <button className="btn btn--ghost" onClick={() => void this.copy()}>
              {this.state.copied ? '已复制' : '复制报错'}
            </button>
          </div>
        </div>
      </div>
    )
  }
}
