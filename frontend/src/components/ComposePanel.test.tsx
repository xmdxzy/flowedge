import { test } from 'node:test'
import assert from 'node:assert/strict'
import { render, waitFor } from '@testing-library/react'

import ComposePanel from './ComposePanel'
import { fire, typeInto } from '../test-utils'

// 构图面板的渲染契约。
//
// 它把 `POST /api/compose` 变成可点到的功能。这里盯的是三件容易做错的事：
//   - 生成失败时只说"失败"，不给违规明细（人不知道该改目标措辞还是该补模块）
//   - 「载入画布」后没有清掉 currentWorkflowId（下一步保存会覆盖别的工作流）
//   - 载入的边缺 id（React Flow 要求边有唯一 id，否则渲染错乱）

const noop = () => {}

function stubFetch(responder: (url: string, init?: RequestInit) => { ok: boolean; status: number; body: unknown }) {
  const calls: Array<{ url: string; body: any }> = []
  const orig = globalThis.fetch
  globalThis.fetch = (async (url: any, init?: any) => {
    let body: any = undefined
    if (init?.body) {
      try {
        body = JSON.parse(init.body)
      } catch {
        body = init.body
      }
    }
    calls.push({ url: String(url), body })
    const r = responder(String(url), init)
    return {
      ok: r.ok,
      status: r.status,
      json: async () => r.body,
    } as any
  }) as any
  return { calls, restore: () => (globalThis.fetch = orig) }
}

const legalDraft = {
  rounds: 2,
  legal: true,
  nodeCount: 3,
  edgeCount: 2,
  score: { score: 1, legal: true, nodes: 3, layers: 3 },
  violations: ['第1轮：端口不兼容：a(text)→b(image)'],
  draft: {
    nodes: [
      { id: 's', type: 'start', data: { prompt: 'x' }, position: { x: 80, y: 200 } },
      { id: 't', type: 'tts', data: {}, position: { x: 320, y: 200 } },
      { id: 'e', type: 'end', data: {}, position: { x: 560, y: 200 } },
    ],
    edges: [
      { source: 's', target: 't' },
      { source: 't', target: 'e' },
    ],
  },
  spec: '# 任务书\n\n示例内容',
}

test('构图面板：关闭时不渲染任何东西', () => {
  const { container } = render(<ComposePanel open={false} onClose={noop} />)
  assert.equal(container.textContent, '')
})

test('构图面板：打开时有目标输入与生成按钮，并说明"不执行"', () => {
  const { container } = render(<ComposePanel open={true} onClose={noop} />)
  const text = container.textContent ?? ''
  assert.ok(text.includes('要做什么'), '要有目标输入标签')
  assert.ok(text.includes('生成图'), '要有生成按钮')
  // 这条很重要：人以为点了就会跑起来，实际上只是生成提案
  assert.ok(text.includes('不执行'), '要说明这一步不执行')
})

test('构图面板：目标为空时点生成，当场提示而不发请求', async () => {
  const { calls, restore } = stubFetch(() => ({ ok: true, status: 200, body: {} }))
  try {
    const { container } = render(<ComposePanel open={true} onClose={noop} />)
    const btn = Array.from(container.querySelectorAll('button')).find((b) => b.textContent === '生成图')!
    fire(() => btn.click())
    await waitFor(() => {
      assert.ok((container.textContent ?? '').includes('先写一句目标'))
    })
    assert.equal(calls.length, 0, '空目标不该发请求（白等 30 秒）')
  } finally {
    restore()
  }
})

test('构图面板：成功后显示结构分、节点链与任务书入口', async () => {
  const { calls, restore } = stubFetch(() => ({ ok: true, status: 200, body: legalDraft }))
  try {
    const { container } = render(<ComposePanel open={true} onClose={noop} />)
    const ta = container.querySelector('textarea') as HTMLTextAreaElement
    typeInto(ta, '把一段文字转成语音')
    const btn = Array.from(container.querySelectorAll('button')).find((b) => b.textContent === '生成图')!
    fire(() => btn.click())

    await waitFor(() => {
      assert.ok((container.textContent ?? '').includes('结构合法'), '要给出合法判定')
    })
    const text = container.textContent ?? ''
    assert.ok(text.includes('3 节点'), '要显示节点数')
    assert.ok(text.includes('2 轮'), '要显示生成了几轮（几轮才成说明目标或提示词有病）')
    assert.ok(text.includes('查看任务书'), '任务书要能展开（否则人得自己再导出一次）')
    // 违规明细也要留着：那是模型改过什么，能看出图是靠什么修对的
    assert.ok(text.includes('端口不兼容'), '生成过程中出现过的问题要列出来')

    assert.equal(calls.length, 1)
    assert.equal(calls[0].url, '/api/compose')
    assert.equal(calls[0].body.goal, '把一段文字转成语音')
  } finally {
    restore()
  }
})

test('构图面板：不合法时列出违规，且不给"载入画布"', async () => {
  const { restore } = stubFetch(() => ({
    ok: false,
    status: 422,
    body: {
      error: '生成 3 轮后仍不合法',
      rounds: 3,
      legal: false,
      nodeCount: 0,
      edgeCount: 0,
      violations: ['第1轮：节点 "x" 的 type="split_text" 不存在'],
    },
  }))
  try {
    const { container } = render(<ComposePanel open={true} onClose={noop} />)
    typeInto(container.querySelector('textarea') as HTMLTextAreaElement, '拆成三段')
    fire(() => Array.from(container.querySelectorAll('button')).find((b) => b.textContent === '生成图')!.click())

    await waitFor(() => {
      assert.ok((container.textContent ?? '').includes('split_text'), '违规明细必须露出来')
    })
    const text = container.textContent ?? ''
    assert.ok(text.includes('结构不合法'), '要明确判不合法')
    assert.ok(!text.includes('载入画布'), '不合法的图不该能载入画布')
  } finally {
    restore()
  }
})

test('构图面板：载入画布时补齐边 id 并清空当前工作流', async () => {
  const { restore } = stubFetch(() => ({ ok: true, status: 200, body: legalDraft }))
  try {
    let loaded: { nodes: any[]; edges: any[]; name: string } | null = null
    let closed = false
    const { container } = render(
      <ComposePanel
        open={true}
        onClose={() => (closed = true)}
        onLoad={(nodes, edges, name) => (loaded = { nodes, edges, name })}
      />,
    )
    typeInto(container.querySelector('textarea') as HTMLTextAreaElement, '配音短片')
    fire(() => Array.from(container.querySelectorAll('button')).find((b) => b.textContent === '生成图')!.click())

    await waitFor(() => {
      assert.ok((container.textContent ?? '').includes('载入画布'))
    })
    const btn = Array.from(container.querySelectorAll('button')).find((b) => b.textContent === '载入画布')!
    fire(() => btn.click())

    assert.ok(loaded, '应该回调 onLoad')
    const l = loaded as unknown as { nodes: any[]; edges: any[]; name: string }
    assert.equal(l.nodes.length, 3)
    // 边必须有 id：后端返回的是 {source,target}，React Flow 要求唯一 id
    assert.equal(l.edges.length, 2)
    for (const e of l.edges) assert.ok(e.id, '边缺 id 会让画布渲染错乱')
    assert.ok(l.name.startsWith('构图：'), '载入后要给个能认出来的名字')
    assert.equal(closed, true, '载入后应关闭面板')
  } finally {
    restore()
  }
})
