#!/usr/bin/env node
// 前端测试跑批器：零新增依赖。
//
// 为什么不用 vitest：这个项目是「单 exe 离线交付」，不希望为跑测试再拉一套依赖树。
// Vite 已经带 esbuild，于是直接：
//   1. esbuild 把 src/**/*.test.ts 打成一个 ESM 包（类型只做剥离，不做类型检查）
//   2. 用 node 跑这个包（测试用 node:test，断言用 node:assert）
// 类型检查仍然由 `tsc --noEmit` 负责，两者分工明确。
//
// 用法：cd frontend && npm test   （或 node scripts/run-tests.mjs）

import { build } from 'esbuild'
import { readdir, rm, mkdir } from 'node:fs/promises'
import { spawnSync } from 'node:child_process'
import path from 'node:path'

const ROOT = path.resolve(import.meta.dirname, '..')
const SRC = path.join(ROOT, 'src')
const OUT_DIR = path.join(ROOT, '.test-build')

async function collect(dir, acc = []) {
  const entries = await readdir(dir, { withFileTypes: true })
  for (const e of entries) {
    const p = path.join(dir, e.name)
    if (e.isDirectory()) {
      if (e.name === 'node_modules' || e.name === '.test-build') continue
      await collect(p, acc)
    } else if (/\.test\.tsx?$/.test(e.name)) {
      acc.push(p)
    }
  }
  return acc
}

const files = await collect(SRC)
if (files.length === 0) {
  console.error('[test] 没找到任何 *.test.ts —— 测试跑批器空转，按失败处理')
  process.exit(1)
}

await rm(OUT_DIR, { recursive: true, force: true })
await mkdir(OUT_DIR, { recursive: true })

// 一次打成一个包：node:test 会把同一个进程里注册的所有用例跑完。
// 拆成多包反而要多次起进程，且共享的模块状态会被重复初始化。
const entry = path.join(OUT_DIR, 'entry.ts')
const fs = await import('node:fs/promises')
await fs.writeFile(
  entry,
  files.map((f) => `import ${JSON.stringify(f.replace(/\\/g, '/'))}`).join('\n') + '\n',
  'utf8',
)

const outfile = path.join(OUT_DIR, 'bundle.mjs')
await build({
  entryPoints: [entry],
  bundle: true,
  platform: 'node',
  format: 'esm',
  target: 'node20',
  outfile,
  logLevel: 'warning',
  // 打包后 import.meta.url 指向 .test-build，测试里找仓库根要靠 cwd 上溯，
  // 见 src/config/graph.test.ts 的 findRepoRoot()。
  banner: { js: '// bundled by scripts/run-tests.mjs' },
})

console.log(`[test] ${files.length} 个测试文件 → ${path.relative(ROOT, outfile)}`)
const res = spawnSync(process.execPath, [outfile], { stdio: 'inherit', cwd: ROOT })
process.exit(res.status ?? 1)
