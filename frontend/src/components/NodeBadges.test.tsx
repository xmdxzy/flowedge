import { test } from 'node:test'
import assert from 'node:assert/strict'
import { render } from '@testing-library/react'

import NodeBadges from './NodeBadges'
import { setCardIndex } from '../lib/cards'

// 徽章是画布上唯一的"这个节点被谁接管"的视觉信号。
// 三条硬约束：顺序 = 执行顺序、超过 3 枚折成 +N、没有卡就不渲染。

setCardIndex({
  agents: [{ id: 'a1', name: '导演', role: '', system: '', category: '' }],
  skills: [
    { id: 's1', name: '中译英', desc: '', system: '', category: '' },
    { id: 's2', name: '抽 JSON', desc: '', system: '', category: '' },
  ],
  workflows: [{ id: '7', name: '整条流水线', nodes: 3, desc: '' }],
})

test('没有卡片时不渲染任何东西', () => {
  const { container } = render(<NodeBadges bindings={[]} />)
  assert.equal(container.innerHTML, '')
})

test('多卡按执行阶段排序（输入→本体→输出→替换）', () => {
  const { container } = render(
    <NodeBadges
      bindings={[
        { kind: 'workflow', id: '7', stage: 'replace' },
        { kind: 'skill', id: 's2', stage: 'output' },
        { kind: 'agent', id: 'a1', stage: 'body' },
        { kind: 'skill', id: 's1', stage: 'input' },
      ]}
    />,
  )
  const labels = Array.from(container.querySelectorAll('.fe-node-badge__label')).map((e) => e.textContent)
  assert.deepEqual(labels, ['中译英', '导演'])
  // 其余折成 +2（4 张卡只露前两张）
  assert.ok(container.textContent?.includes('+2'), '超出 2 枚应折成 +N')
})

test('徽章用形状区分类型（class 上带 kind）', () => {
  const { container } = render(
    <NodeBadges bindings={[{ kind: 'skill', id: 's1', stage: 'input' }]} />,
  )
  assert.ok(container.querySelector('.fe-node-badge--skill'), '技能卡徽章应带 --skill 类')
})
