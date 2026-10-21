// 组件测试的浏览器环境垫片（只在测试进程里跑，不进产物）。
//
// 为什么自己搭而不是用 jsdom 的 testEnvironment 集成：这个项目的测试跑批器是
// 裸 node + esbuild（见 scripts/run-tests.mjs），没有 vitest 的 environment 概念。
// 所以在这里手动把 jsdom 的 window/document 挂到 globalThis 上，
// 让 @testing-library/react 能以「浏览器」的方式渲染。

import { JSDOM } from 'jsdom'
import { afterEach } from 'node:test'
import { cleanup } from '@testing-library/react'

const dom = new JSDOM('<!doctype html><html><body></body></html>', {
  url: 'http://localhost/', // 给 localStorage 一个合法 origin，否则访问会抛
  pretendToBeVisual: true,
})

const g = globalThis as unknown as Record<string, unknown>
const w = dom.window as unknown as Record<string, unknown>

g.window = dom.window
g.document = dom.window.document
// DOM 的构造器要显式搬过来：组件代码与 RTL 都会直接引用 HTMLInputElement 这类全局类
// （例如用原生 setter 改 value 触发 React 的 onChange），漏一个就会 "not defined"。
for (const name of [
  'HTMLElement', 'HTMLInputElement', 'HTMLTextAreaElement', 'HTMLSelectElement',
  'HTMLButtonElement', 'HTMLDivElement', 'HTMLAnchorElement', 'HTMLImageElement',
  'Element', 'Node', 'NodeList', 'Text', 'DocumentFragment', 'SVGElement',
  'CustomEvent', 'DOMParser', 'XMLHttpRequest',
] as const) {
  const ctor = (dom.window as unknown as Record<string, unknown>)[name]
  if (ctor) g[name] = ctor
}
g.Event = dom.window.Event
g.MouseEvent = dom.window.MouseEvent
g.KeyboardEvent = dom.window.KeyboardEvent
g.getComputedStyle = dom.window.getComputedStyle.bind(dom.window)
g.localStorage = dom.window.localStorage
g.requestAnimationFrame = (cb: FrameRequestCallback) => setTimeout(() => cb(Date.now()), 0)
g.cancelAnimationFrame = (id: number) => clearTimeout(id as unknown as NodeJS.Timeout)

// Node 22 自带只读 navigator，直接赋值会抛；用 defineProperty 覆盖
for (const key of ['navigator', 'self']) {
  Object.defineProperty(globalThis, key, {
    value: key === 'self' ? dom.window : (w[key] as unknown),
    configurable: true,
    writable: true,
  })
}

// React 18 要求：在测试环境里必须显式声明，否则 act() 会警告「不支持并发环境」
;(g as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true

// 组件里常见的 IntersectionObserver / ResizeObserver，jsdom 没有实现
class NoopObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
  takeRecords() {
    return []
  }
}
g.IntersectionObserver = NoopObserver
g.ResizeObserver = NoopObserver
g.matchMedia = () => ({
  matches: false,
  media: '',
  addEventListener() {},
  removeEventListener() {},
  addListener() {},
  removeListener() {},
  dispatchEvent() {
    return false
  },
})

// 每个用例后卸载组件树。放在这里而不是各测试文件里：
// 只要有一个用例在断言处抛错、跳过了自己的 cleanup，下一个用例就会看到两份 DOM，
// 报出「Found multiple elements」这种和真实缺陷无关的错。
afterEach(() => cleanup())
