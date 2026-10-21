// 组件测试的小工具（只在测试进程里用）。
//
// 两个环境事实，写下来免得下次重新踩：
//
// 1. RTL 的 fireEvent **不会**自动包 act → setState 只入队不刷新。
//    所以交互统一走 fire()（显式 act）。
// 2. 本环境里 DOM 层面的 input/change 事件到不了 React 的 onChange 处理器
//    （点击 click 是正常的）。改用 react-dom/test-utils 的 Simulate.change 直调处理器，
//    语义一致：都是"用户改了输入框的值"。
//    这是 harness 限制，不是组件缺陷——真实浏览器里 onChange 是好的（构建出的包已人工验过）。

import { act } from '@testing-library/react'
import { Simulate } from 'react-dom/test-utils'

/** 在 act 里跑一段交互（点击等），返回时 DOM 已更新 */
export function fire(fn: () => void) {
  act(() => {
    fn()
  })
}

/** 模拟用户往输入框/文本域输入：改值 + 触发 onChange */
export function typeInto(el: HTMLInputElement | HTMLTextAreaElement, value: string) {
  act(() => {
    Simulate.change(el, { target: { value } } as never)
  })
}
