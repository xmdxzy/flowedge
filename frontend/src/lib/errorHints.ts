// 错误码 → 可执行文案
//
// 背景（2026-10-19 审计）：后端在 `backend/model/workflow.go` 里定义了 7 个结构化错误码，
// 但前端只把 `error_code` 塞进节点 data 再没人读，失败原因一律裸字符串丢给用户，
// 节点上只有一个小红点。用户看到的是 `timeout` / `config_missing` 这种机器词，
// 既不知道发生了什么，也不知道下一步该点哪里。
//
// 这里把每个码翻译成「发生了什么 + 为什么 + 点哪里能修」。

export type ErrorAction = 'open-env' | 'open-history' | 'retry-node' | 'open-kb'

export interface ErrorHint {
  /** 一句话说清发生了什么 */
  title: string
  /** 为什么会这样（人话，不是复述错误码） */
  detail: string
  /** 下一步该做什么 */
  fix?: string
  /** 可执行的修复动作（由渲染方决定按钮行为） */
  action?: ErrorAction
  /** 危险程度：error 需要人处理；warn 可重试 */
  level?: 'error' | 'warn'
}

const HINTS: Record<string, ErrorHint> = {
  config_missing: {
    title: '依赖服务没起来',
    detail:
      '这个节点需要本地服务（ComfyUI / ASR / TTS 之一），但引擎连不上或没配置端点。注意：连不上和「没有结果」是两件事。',
    fix: '去环境面板一键启动，或看它给的安装提示',
    action: 'open-env',
  },
  timeout: {
    title: '节点执行超时',
    detail:
      '超过了这个节点的时间预算（默认 180 秒；批量/长片会按工作量自动放宽）。常见于首次加载大权重、或分辨率/帧数设得过高。',
    fix: '在节点配置里调大超时，或降低分辨率/帧数后重跑',
    level: 'warn',
  },
  upstream_failed: {
    title: '上游节点失败了',
    detail: '这个节点本身没跑，因为它的输入来自一个失败的节点。先修上面那个。',
    fix: '从失败的上游节点单独重跑',
    action: 'open-history',
  },
  retry_exhausted: {
    title: '重试次数用尽',
    detail: '自动重试到上限仍然失败，多半是持久性问题（模型文件缺失、图结构不对、显存不足）。',
    fix: '看最后一次的原始报错，别反复重试',
    action: 'open-history',
  },
  cancelled: {
    title: '已取消',
    detail: '这次运行被你主动中止了。上游已完成的节点结果仍然保留。',
    fix: '需要可从中断处续跑',
    action: 'open-history',
    level: 'warn',
  },
  cycle: {
    title: '图里有环',
    detail: '存在循环依赖，引擎拒绝执行——环上的节点永远等不到对方先完成。',
    fix: '删掉回指的那条连线',
  },
  execution_error: {
    title: '节点执行出错',
    detail: '节点内部抛错。具体原因看下面的原始报错。',
    fix: '看原始报错，或从该节点重跑',
    action: 'retry-node',
    level: 'warn',
  },
}

const FALLBACK: ErrorHint = {
  title: '执行失败',
  detail: '引擎没有给出结构化错误码，下面是原始报错。',
  level: 'error',
}

/** 按错误码取可执行文案；无码时用兜底文案 + 原始报错 */
export function hintFor(code?: string, message?: string): ErrorHint {
  const base = (code && HINTS[code]) || FALLBACK
  if (!code && !message) return base
  return base
}

/** 是否是「可恢复」的失败（用于决定要不要显示重试按钮） */
export function isRetryable(code?: string): boolean {
  return code === 'timeout' || code === 'execution_error' || code === 'upstream_failed'
}
