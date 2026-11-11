import type {
  ArtifactItem,
  ArtifactListResponse,
  DepListResponse,
  DepStatus,
  McpConfig,
  OmAudioBody,
  OmAutoProduceBody,
  OmCreateProjectBody,
  OmEditBody,
  OmGenerateBody,
  OmProject,
  OmProjectDetail,
  OmProjectListResponse,
  OmScriptBody,
  OmStoryboardBody,
  Pipeline,
  PipelineListResponse,
  PreflightResult,
  ProduceAccepted,
  RunDetail,
  RunListResponse,
  RunSummary,
  RerunAccepted,
  UnloadResult,
} from './types'

// 后端调用层：只有三件事——取数据、把后端的中文错误原样抛出来、别吞 HTTP 状态。
//
// 为什么错误必须原样抛：后端 `error` 字段是写给人的（「依赖服务未就绪，未开始出片：…」），
// 包一层「请求失败」等于把唯一有用的信息删掉。

export class ApiError extends Error {
  /** 0 = 连不上（fetch 直接 reject） */
  readonly status: number

  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

/** 统一把 unknown 异常转成可显示的一行文字（不吞后端原文） */
export function errorText(err: unknown): string {
  if (err instanceof ApiError) return err.message
  if (err instanceof Error) return err.message
  return String(err)
}

export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let resp: Response
  try {
    resp = await fetch(path, init)
  } catch (err) {
    throw new ApiError(`连不上引擎：${err instanceof Error ? err.message : String(err)}`, 0)
  }

  const text = await resp.text()
  let body: unknown = null
  if (text) {
    try {
      body = JSON.parse(text)
    } catch {
      body = null
    }
  }

  if (!resp.ok) {
    const backendError =
      body && typeof body === 'object' && typeof (body as { error?: unknown }).error === 'string'
        ? (body as { error: string }).error
        : ''
    if (backendError.trim()) throw new ApiError(backendError.trim(), resp.status)
    const snippet = text && !body ? `：${text.slice(0, 200)}` : ''
    throw new ApiError(`HTTP ${resp.status} ${resp.statusText}${snippet}`, resp.status)
  }

  return (body ?? {}) as T
}

export function postJSON(path: string, payload?: unknown): Promise<unknown> {
  return request<unknown>(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: payload === undefined ? undefined : JSON.stringify(payload),
  })
}

// ---- 流水线 ----

export async function fetchPipelines(): Promise<Pipeline[]> {
  const data = await request<PipelineListResponse>('/api/pipelines')
  const list = Array.isArray(data.pipelines) ? data.pipelines : []
  // fields 在后端可能是 null（JSON 里没写 fields），统一成数组：
  // 后面每一处都按「有字段」渲染，不给 null 留分支。
  return list.map((p) => ({ ...p, fields: Array.isArray(p.fields) ? p.fields : [] }))
}

export function preflight(pipeline: string, params: Record<string, unknown>): Promise<PreflightResult> {
  return request<PreflightResult>('/api/preflight', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ pipeline, params }),
  }).then((r) => ({ ...r, missing: Array.isArray(r.missing) ? r.missing : [] }))
}

export async function produce(
  pipeline: string,
  params: Record<string, unknown>,
): Promise<ProduceAccepted> {
  const data = await postJSON('/api/produce', { pipeline, params, invoked_by: 'manual' })
  return data as ProduceAccepted
}

// ---- 运行 ----

export async function fetchRuns(opts: {
  limit?: number
  offset?: number
  status?: string
}): Promise<RunListResponse & { runs: RunSummary[] }> {
  const limit = opts.limit ?? 50
  const offset = opts.offset ?? 0
  const status = opts.status ?? ''
  const qs = new URLSearchParams({ limit: String(limit), offset: String(offset) })
  if (status) qs.set('status', status)
  const data = await request<RunListResponse>(`/api/runs?${qs.toString()}`)
  return { ...data, runs: Array.isArray(data.runs) ? data.runs : [] }
}

export function fetchRun(runId: number): Promise<RunDetail> {
  return request<RunDetail>(`/api/runs/${runId}`)
}

export function cancelRun(runId: number): Promise<unknown> {
  return postJSON(`/api/runs/${runId}/cancel`)
}

export async function rerunRun(runId: number): Promise<RerunAccepted> {
  return (await postJSON(`/api/runs/${runId}/rerun`)) as RerunAccepted
}

/**
 * 从断点续跑：成功的节点复用产物，只重跑没成功的。
 *
 * 引擎被外部杀掉（沙箱回收、关机、任务管理器）时运行会标成 interrupted，
 * 这时候「重跑」等于从零再烧一遍，续跑才是对的。
 */
export async function resumeRun(
  runId: number,
): Promise<{ run_id: number; status?: string; reused_nodes?: number }> {
  return (await postJSON(`/api/runs/${runId}/resume`)) as {
    run_id: number
    status?: string
    reused_nodes?: number
  }
}

// ---- 产物 ----

export async function fetchArtifacts(): Promise<ArtifactListResponse & { items: ArtifactItem[] }> {
  const data = await request<ArtifactListResponse>('/api/artifacts')
  return { ...data, items: Array.isArray(data.items) ? data.items : [] }
}

export function artifactURL(name: string): string {
  return `/api/artifacts/${encodeURIComponent(name)}`
}

export function deleteArtifact(name: string): Promise<unknown> {
  return request<unknown>(artifactURL(name), { method: 'DELETE' })
}

// ---- 依赖 ----

export async function fetchDeps(): Promise<DepListResponse & { items: DepStatus[] }> {
  const data = await request<DepListResponse>('/api/deps')
  return { ...data, items: Array.isArray(data.items) ? data.items : [] }
}

export function startDep(name: string): Promise<unknown> {
  return postJSON(`/api/deps/${encodeURIComponent(name)}/start`)
}

export function stopDep(name: string): Promise<unknown> {
  return postJSON(`/api/deps/${encodeURIComponent(name)}/stop`)
}

/**
 * 卸载模型但保留进程：把显存要回来，又不付冷启动代价。
 *
 * 和 stop 的区别很关键：stop 杀进程（下次冷启动 30–60 秒 + 权重重载 3 分钟），
 * unload 只让 ComfyUI 调 /free、让 Ollama 走 keep_alive=0，进程还在。
 */
export async function unloadDep(name: string): Promise<UnloadResult> {
  const data = await postJSON(`/api/deps/${encodeURIComponent(name)}/unload`)
  return data as UnloadResult
}

/** 重读 data/deps.json（不重启引擎、不动已跑的进程） */
export function reloadDeps(): Promise<unknown> {
  return postJSON('/api/deps/reload')
}

/**
 * 单次查询「是否就绪」。后端 /wait 最多阻塞 120 秒，这里主动 8 秒放弃并返回 false，
 * 由调用方继续轮询——否则界面会卡在两分钟没有反馈的空窗里。
 */
export async function waitDepReady(name: string, timeoutMs = 8000): Promise<boolean> {
  const ctrl = new AbortController()
  const timer = setTimeout(() => ctrl.abort(), timeoutMs)
  try {
    const resp = await fetch(`/api/deps/${encodeURIComponent(name)}/wait`, { signal: ctrl.signal })
    if (!resp.ok) return false
    const body = (await resp.json().catch(() => null)) as { ready?: boolean } | null
    return body?.ready === true
  } catch {
    // 超时/断连都只代表「还没好」，不是错误——继续轮询即可
    return false
  } finally {
    clearTimeout(timer)
  }
}

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms))

/**
 * 一键修复：POST start 之后轮询 /wait 直到就绪或超时。
 * 返回说明文字给 toast，异常抛出交给调用方显示。
 */
export async function startAndWaitDep(
  name: string,
  opts: { timeoutMs?: number; pollMs?: number; onTick?: (attempt: number) => void } = {},
): Promise<void> {
  const timeoutMs = opts.timeoutMs ?? 180_000
  const pollMs = opts.pollMs ?? 1000
  await startDep(name)
  const deadline = Date.now() + timeoutMs
  let attempt = 0
  while (Date.now() < deadline) {
    const remaining = deadline - Date.now()
    if (await waitDepReady(name, Math.min(8000, Math.max(1000, remaining)))) return
    attempt += 1
    opts.onTick?.(attempt)
    await sleep(pollMs)
  }
  throw new ApiError(`${name} 启动超时（已等 ${Math.round(timeoutMs / 1000)} 秒），看后端控制台输出`, 0)
}

// ---- MCP 接入 ----

export function fetchMcpConfig(): Promise<McpConfig> {
  return request<McpConfig>('/api/mcp/config').then((c) => ({
    ...c,
    notes: Array.isArray(c.notes) ? c.notes : [],
  }))
}

// ---- OpenMontage 代理（转发到独立部署的 OpenMontage Drama Studio，默认 :8787）----

export async function fetchOmHealth(): Promise<unknown> {
  return request<unknown>('/api/openmontage/health')
}

export async function fetchOmProjects(): Promise<OmProject[]> {
  const data = await request<OmProjectListResponse>('/api/openmontage/projects')
  return Array.isArray(data.projects) ? data.projects : []
}

export function fetchOmProject(id: number | string): Promise<OmProjectDetail> {
  return request<OmProjectDetail>(`/api/openmontage/projects/${id}`)
}

export function createOmProject(body: OmCreateProjectBody): Promise<OmProjectDetail> {
  return postJSON('/api/openmontage/projects', body) as Promise<OmProjectDetail>
}

export function generateOmScript(id: number | string, body: OmScriptBody): Promise<unknown> {
  return postJSON(`/api/openmontage/projects/${id}/script`, body)
}

export function generateOmStoryboard(id: number | string, body: OmStoryboardBody): Promise<unknown> {
  return postJSON(`/api/openmontage/projects/${id}/storyboard`, body)
}

export function generateOmVideo(id: number | string, body: OmGenerateBody): Promise<unknown> {
  return postJSON(`/api/openmontage/projects/${id}/generate`, body)
}

export function generateOmAudio(id: number | string, body: OmAudioBody): Promise<unknown> {
  return postJSON(`/api/openmontage/projects/${id}/audio`, body)
}

export function saveOmEdit(id: number | string, body: OmEditBody): Promise<unknown> {
  return postJSON(`/api/openmontage/projects/${id}/edits`, body)
}

export function autoProduceOm(body: OmAutoProduceBody): Promise<unknown> {
  return postJSON('/api/openmontage/auto-produce', body)
}
