import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import { act, fireEvent, render, waitFor } from '@testing-library/react'

// 收尾：异步 act 会让 React 调度器建 MessageChannel，在 Node 里就是 ref 住的 MessagePort。
// 用例全绿但进程会挂在事件循环上不退出（实测 51 条全过、exit 码迟迟不回）。
// 只 unref 这些端口，不碰断言、不改退出码——测试结果仍是 node:test 说了算。
after(() => {
  const handles = (process as unknown as { _getActiveHandles: () => unknown[] })._getActiveHandles()
  for (const handle of handles) {
    const port = handle as { constructor?: { name?: string }; unref?: () => void }
    if (port.constructor?.name === 'MessagePort' && typeof port.unref === 'function') {
      port.unref()
    }
  }
})
import type { Pipeline } from '../api/types'
import ProducePage from './ProducePage'
import { typeInto } from '../test-utils'

// 出片页的行为锁：卡片/表单/体检门控都用真实组件 + 假 fetch 跑一遍。
// 这里钉的是「体检不过绝不提交」——这是产品最容易悄悄退化的一条。

const PIPELINES: Pipeline[] = [
  {
    id: 'image',
    name: '文生图',
    summary: '一句话出一个画面',
    output: 'image',
    est_seconds: 60,
    fields: [
      { key: 'prompt', label: '画面描述', type: 'textarea', required: true },
      { key: 'width', label: '宽', type: 'int', default: 1024, min: 384, max: 1536 },
      { key: 'qc', label: '质量循环', type: 'bool', default: false },
    ],
  },
  {
    id: 'voice',
    name: '文案转配音',
    summary: '把文案读出来',
    output: 'audio',
    est_seconds: 30,
    fields: [{ key: 'text', label: '文案', type: 'textarea', required: true }],
  },
]

interface StubReply {
  status?: number
  body: unknown
}

const calls: { url: string; init?: RequestInit }[] = []

function stubFetch(handler: (url: string, init?: RequestInit) => StubReply) {
  calls.length = 0
  const fakeFetch = async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    calls.push({ url, init })
    const reply = handler(url, init)
    const status = reply.status ?? 200
    return {
      ok: status >= 200 && status < 300,
      status,
      statusText: '',
      text: async () => JSON.stringify(reply.body),
    } as unknown as Response
  }
  ;(globalThis as unknown as { fetch: typeof fetch }).fetch = fakeFetch as unknown as typeof fetch
}

const PREFLIGHT_BLOCKED = {
  ok: false,
  fixable: 1,
  missing: [
    {
      node_id: 't2i-1',
      node_type: 'text2image',
      dep: 'comfyui',
      label: '文生图/图像编辑（ComfyUI）',
      hint: '',
      fix: '点「一键修复」启动 ComfyUI（冷启动约 20–40 秒）',
      fixable: true,
      fix_dep: 'comfyui',
    },
  ],
}

function renderPage(onProduced: (runId: number) => void = () => {}) {
  return render(
    <ProducePage pipelines={PIPELINES} loading={false} loadError="" onReload={() => {}} onProduced={onProduced} />,
  )
}

test('卡片按 output 分组，并显示人类可读的估时', () => {
  stubFetch(() => ({ body: {} }))
  const { container, getAllByText } = renderPage()
  const text = container.textContent ?? ''
  assert.ok(text.includes('图像') && text.includes('音频'), `分组缺失：${text.slice(0, 120)}`)
  assert.ok(text.includes('约 1 分钟'), 'est_seconds=60 应显示「约 1 分钟」')
  assert.ok(text.includes('约 30 秒'), 'est_seconds=30 应显示「约 30 秒」')
  assert.ok(getAllByText('文生图').length >= 1)
})

test('必填为空时「出片」禁用，并点名缺什么', () => {
  stubFetch(() => ({ body: {} }))
  const { container, getByRole } = renderPage()
  const button = getByRole('button', { name: '出片' }) as HTMLButtonElement
  assert.equal(button.disabled, true, '必填为空时不允许点出片')
  assert.ok((container.textContent ?? '').includes('还缺必填：画面描述'))
  assert.equal(calls.length, 0, '按钮禁用时不该发任何请求')
})

test('体检 ok=false：列出 missing + fix + 一键启动，且不提交出片', async () => {
  stubFetch((url) => {
    if (url === '/api/preflight') return { body: PREFLIGHT_BLOCKED }
    return { body: {} }
  })
  const { container, getByRole, findByText } = renderPage()

  const textarea = container.querySelector('textarea')
  assert.ok(textarea, '应该有画面描述输入框')
  typeInto(textarea as HTMLTextAreaElement, '雪山日出')

  await act(async () => {
    fireEvent.click(getByRole('button', { name: '出片' }))
  })

  await findByText('依赖没就绪，没有提交出片')
  assert.ok(calls.some((c) => c.url === '/api/preflight'), '必须先体检')
  assert.equal(calls.some((c) => c.url === '/api/produce'), false, '体检不过时绝不能提交')
  assert.ok((container.textContent ?? '').includes('点「一键修复」启动 ComfyUI'))
  assert.ok(getByRole('button', { name: '一键启动' }), '可修的缺失项要给一键启动')

  // 请求体就是 buildParams 的结果：prompt 带值、width/qc 用默认值
  const preflightCall = calls.find((c) => c.url === '/api/preflight')
  const body = JSON.parse(String(preflightCall?.init?.body)) as {
    pipeline: string
    params: Record<string, unknown>
  }
  assert.equal(body.pipeline, 'image')
  assert.deepEqual(body.params, { prompt: '雪山日出', width: 1024, qc: false })
})

test('体检 ok：提交出片并把 run_id 交给上层（invoked_by=manual）', async () => {
  stubFetch((url) => {
    if (url === '/api/preflight') return { body: { ok: true, missing: [], fixable: 0 } }
    if (url === '/api/produce') {
      return { status: 202, body: { run_id: 42, pipeline: 'image', status: 'running' } }
    }
    return { body: {} }
  })
  let produced: number | null = null
  const { container, getByRole } = renderPage((id) => {
    produced = id
  })

  const textarea = container.querySelector('textarea') as HTMLTextAreaElement
  typeInto(textarea, '雪山日出')

  await act(async () => {
    fireEvent.click(getByRole('button', { name: '出片' }))
  })

  await waitFor(() => assert.equal(produced, 42))
  const produceCall = calls.find((c) => c.url === '/api/produce')
  assert.ok(produceCall, '体检通过后必须提交')
  const body = JSON.parse(String(produceCall?.init?.body)) as Record<string, unknown>
  assert.equal(body.invoked_by, 'manual')
  assert.equal(body.pipeline, 'image')
})

test('后端错误原文要显示出来（中文 error 不吞）', async () => {
  stubFetch((url) => {
    if (url === '/api/preflight') {
      return { status: 503, body: { error: '依赖服务未就绪，未开始出片：TTS → 点「一键修复」启动 TTS 服务' } }
    }
    return { body: {} }
  })
  const { container, getByRole, findByText } = renderPage()

  const textarea = container.querySelector('textarea') as HTMLTextAreaElement
  typeInto(textarea, '雪山日出')
  await act(async () => {
    fireEvent.click(getByRole('button', { name: '出片' }))
  })

  await findByText('依赖服务未就绪，未开始出片：TTS → 点「一键修复」启动 TTS 服务')
  assert.ok((container.textContent ?? '').includes('请求失败'))
})
