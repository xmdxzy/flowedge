import { test } from 'node:test'
import assert from 'node:assert/strict'
import { fireEvent, render } from '@testing-library/react'

import ErrorNotice from './ErrorNotice'
import { hintFor } from '../lib/errorHints'

// 组件级测试的第一批：只挑「纯 props → DOM」的组件。
// 画布与属性面板依赖 React Flow 的上下文，要单独做 harness，不在这批里硬塞。

test('按错误码渲染人话标题，而不是把机器词丢给用户', () => {
  const { getByRole } = render(<ErrorNotice code="config_missing" />)
  const alert = getByRole('alert')
  const text = alert.textContent ?? ''
  assert.ok(text.includes(hintFor('config_missing').title), `应显示人话标题，实际：${text}`)
  assert.ok(!text.startsWith('config_missing'), '标题行不该以裸错误码开头')
  assert.ok(text.includes('config_missing'), '错误码仍应保留（便于排查），只是不占主位')
})

test('有修复动作时才出按钮，点击回传 action', () => {
  const events: string[] = []
  const { getByText } = render(<ErrorNotice code="config_missing" onAction={(a) => events.push(a)} />)
  const btn = getByText('打开环境面板')
  fireEvent.click(btn)
  assert.deepEqual(events, ['open-env'])
})

test('没有 onAction 时不给按钮（避免点了没反应）', () => {
  const { queryByText } = render(<ErrorNotice code="config_missing" />)
  assert.equal(queryByText('打开环境面板'), null)
})

test('原始报错原样保留，便于复制排查', () => {
  const raw = 'tts service returned status 500: No module named \'ordered_set\''
  const { getByRole } = render(<ErrorNotice code="config_missing" message={raw} />)
  assert.ok((getByRole('alert').textContent ?? '').includes(raw))
})

test('compact 模式只留标题与原始报错（列表行里用）', () => {
  const { getByRole } = render(<ErrorNotice code="timeout" compact message="boom" />)
  const text = getByRole('alert').textContent ?? ''
  assert.ok(text.includes(hintFor('timeout').title))
  assert.ok(text.includes('boom'))
  assert.ok(!text.includes(hintFor('timeout').detail), 'compact 不该展开原因长文')
})

test('未知错误码走兜底文案，但原始报错必须显示', () => {
  const { getByRole } = render(<ErrorNotice code="brand_new_code" message="something odd" />)
  const text = getByRole('alert').textContent ?? ''
  assert.ok(text.includes('something odd'))
  assert.ok(text.length > 20, '兜底文案也不能是空的')
})

test('不带 code 也不崩', () => {
  const { getByRole } = render(<ErrorNotice message="裸报错" />)
  assert.ok((getByRole('alert').textContent ?? '').includes('裸报错'))
})
