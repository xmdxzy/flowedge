// 自定义模型端点（data/llm-profiles.json）的前端读写。
//
// 只有一个用途：节点面板的「我的模型」下拉 + 配置面板里的编辑器。
// 真正的存储在后端 /api/llm/profiles，这里不缓存、不派生第二份状态。

export interface LLMProfile {
  name: string
  base_url: string
  model: string
  backend?: string
  credential?: string
  note?: string
}

export async function fetchProfiles(): Promise<{ profiles: LLMProfile[]; default: string } | null> {
  try {
    const r = await fetch('/api/llm/profiles')
    if (!r.ok) return null
    return await r.json()
  } catch {
    return null
  }
}

export async function saveProfiles(profiles: LLMProfile[], def = ''): Promise<string | null> {
  try {
    const r = await fetch('/api/llm/profiles', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ profiles, default: def }),
    })
    if (!r.ok) {
      const d = await r.json().catch(() => null)
      return (d && (d.error as string)) || `HTTP ${r.status}`
    }
    return null
  } catch (e) {
    return String(e)
  }
}
