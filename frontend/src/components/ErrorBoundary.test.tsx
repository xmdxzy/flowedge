import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { ReactElement } from 'react'
import { render } from '@testing-library/react'

import ErrorBoundary from './ErrorBoundary'

// 错误边界的契约只有两条：正常放行、崩溃有兜底。
// 兜底页本身必须带「重新加载」与报错原文——白屏时用户没有任何可点的东西。

function Boom(): ReactElement {
  throw new Error('渲染炸了')
}

/** React 会对被边界接住的错误打 console.error，测试里静音免得污染输出 */
function withSilencedError<T>(fn: () => T): T {
  const original = console.error
  console.error = () => {}
  try {
    return fn()
  } finally {
    console.error = original
  }
}

test('没出错时原样渲染子树', () => {
  const { getByText } = render(
    <ErrorBoundary>
      <p>正常内容</p>
    </ErrorBoundary>,
  )
  assert.ok(getByText('正常内容'))
})

test('子组件抛错时渲染兜底页，并给出报错原文与重载按钮', () => {
  const { getByRole, getByText } = withSilencedError(() =>
    render(
      <ErrorBoundary>
        <Boom />
      </ErrorBoundary>,
    ),
  )
  const alert = getByRole('alert')
  const text = alert.textContent ?? ''
  assert.ok(text.includes('界面遇到未处理的错误'), `兜底标题缺失：${text}`)
  assert.ok(text.includes('渲染炸了'), '报错原文必须显示出来（否则没法反馈）')
  assert.ok(getByText('重新加载'), '兜底页必须能重载')
})

test('崩溃记到 localStorage，只留最近一次', () => {
  localStorage.clear()
  withSilencedError(() =>
    render(
      <ErrorBoundary>
        <Boom />
      </ErrorBoundary>,
    ),
  )
  const raw = localStorage.getItem('flowedge.lastCrash')
  assert.ok(raw, '崩溃应落一条记录，便于用户回传')
  const parsed = JSON.parse(raw) as { message?: string }
  assert.equal(parsed.message, '渲染炸了')
})
