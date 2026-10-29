import test from 'node:test'
import assert from 'node:assert/strict'

import { depSupportsUnload } from '../api/types'

// 「卸载显存」按钮只在真有卸载接口的依赖上出现。
//
// 后端 Unload 对 ASR/TTS 会如实返回 supported=false（它们权重在内存/CPU，
// 不在显存里）。前端要按同一条规则决定是否显示按钮，否则用户会点到一个
// 只会弹错的地方。
test('只有 ComfyUI 三实例与 Ollama 支持卸载显存', () => {
  for (const name of ['comfyui', 'comfy_h3', 'comfy_moss', 'ollama']) {
    assert.equal(depSupportsUnload(name), true, `${name} 应该支持`)
  }
  for (const name of ['asr', 'tts', 'unknown']) {
    assert.equal(depSupportsUnload(name), false, `${name} 不该显示按钮`)
  }
})
