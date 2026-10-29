// 后端契约的类型镜像（唯一事实源在 backend/，前端不猜字段）。
//
//   Pipeline / PipelineField → backend/service/pipeline.go
//   DepRequirement           → backend/service/preflight.go
//   DepStatus                → backend/service/deps.go
//   Artifact                 → backend/handler/artifact.go
//   WorkflowRun / NodeInstance → backend/model/workflow.go
//   MCPConfig                → backend/handler/integration.go
//
// 改后端结构时，这里应当先报错——而不是让界面静默显示空白。

export type OutputKind = 'image' | 'video' | 'audio'

/** 后端支持的字段类型（pipeline.validatePipelineShape 里白名单） */
export type FieldType = 'text' | 'textarea' | 'path' | 'int' | 'float' | 'bool' | 'enum'

export interface PipelineField {
  key: string
  label: string
  type: FieldType
  /** 缺省值；Go 侧 omitempty，所以可能缺省 */
  default?: unknown
  min?: number
  max?: number
  /** 仅 enum 有 */
  options?: string[]
  hint?: string
  required?: boolean
}

export interface Pipeline {
  id: string
  name: string
  summary: string
  output: OutputKind | string
  /** 后端 omitempty：0 或缺失表示「未知」 */
  est_seconds?: number
  fields: PipelineField[]
}

export interface PipelineListResponse {
  pipelines: Pipeline[] | null
  count: number
}

// ---- 依赖预检 ----

export interface DepRequirement {
  node_id: string
  node_type: string
  dep: string
  label: string
  hint: string
  /** 人话修复动作（能照着做） */
  fix: string
  /** 引擎能否代劳（true 才给「一键启动」） */
  fixable: boolean
  /** 要拉起的依赖名，对应 POST /api/deps/:name/start */
  fix_dep: string
}

export interface PreflightResult {
  ok: boolean
  missing: DepRequirement[] | null
  fixable: number
  /**
   * 「能跑但不便宜」的提醒：显存会不会换页、同参数是不是刚跑过。
   * 不改变 ok，也不拦出片——拦住等于把「慢」升级成「不可用」。
   */
  warnings?: string[] | null
}

// ---- 运行 ----

export type RunStatus = 'pending' | 'running' | 'paused' | 'success' | 'failed' | 'cancelled'

/** model.NodeInstance（node_states 数组元素） */
export interface NodeState {
  id: string
  type: string
  status: string
  data?: Record<string, unknown>
  output?: unknown
  error?: string
  error_code?: string
  started_at?: string | null
  ended_at?: string | null
}

/** GET /api/runs 列表项（model.WorkflowRun） */
export interface RunSummary {
  id: number
  workflow_id: number
  status: string
  pipeline_id?: string
  error?: string
  invoked_by?: string
  /** node_states 在这里是原始 JSON，列表页不需要，故不声明 */
  started_at?: string | null
  ended_at?: string | null
  created_at: string
}

export interface RunListResponse {
  total: number
  limit: number
  offset: number
  status: string
  runs: RunSummary[] | null
}

/** GET /api/runs/:runId（handler.Producer.GetRun） */
export interface RunDetail {
  run_id: number
  pipeline: string
  status: string
  error: string
  invoked_by?: string
  params: Record<string, unknown> | null
  /** 后端 json.RawMessage：没有节点状态时是 null */
  node_states: NodeState[] | null
  started_at?: string | null
  ended_at?: string | null
  created_at: string
}

export interface ProduceAccepted {
  run_id: number
  pipeline: string
  status: string
  params?: unknown
  created_at?: string
}

export interface RerunAccepted {
  run_id: number
  pipeline: string
  status: string
}

// ---- 产物 ----

export interface ArtifactItem {
  name: string
  /** 后端按扩展名归类：image / video / audio */
  kind: string
  size: number
  /** Unix 秒 */
  mod_time: number
}

export interface ArtifactListResponse {
  dir: string
  count: number
  items: ArtifactItem[] | null
}

// ---- 依赖 ----

export interface DepStatus {
  name: string
  label: string
  /** 端口/健康检查通过，**或**进程活着只是忙（busy=true） */
  running: boolean
  /** 由引擎拉起（可停止） */
  managed: boolean
  /**
   * 进程活着但忙到不回健康检查（生成中/换页中）。
   * 这时**绝不能**显示「未启动」：用户会点启动再派生一份，两个实例抢一张卡。
   */
  busy?: boolean
  state?: 'ok' | 'busy' | 'stopped'
  autostart: boolean
  pid?: number
  endpoint: string
  command?: string
  hint?: string
  last_error?: string
}

export interface DepListResponse {
  items: DepStatus[] | null
  count: number
}

/** POST /api/deps/:name/unload 的结果（对应 backend/service/deps_unload.go） */
export interface UnloadResult {
  name: string
  /** 该依赖有没有卸载接口（ASR/TTS 没有，权重不在显存里） */
  supported: boolean
  /** 本次实际回收的显存（拿不到读数时为 0） */
  freed_mb: number
  /** 卸载后的设备级空闲显存 */
  free_mb: number
  note: string
}

/** 只有这两类依赖能"卸权重不杀进程"（ComfyUI 走 /free，Ollama 走 keep_alive=0） */
export function depSupportsUnload(name: string): boolean {
  return name.startsWith('comfy') || name === 'ollama'
}

export interface DepActionResponse {
  name?: string
  status?: string
  ready?: boolean
  error?: string
}

// ---- MCP 接入配置 ----

export interface McpServerBlock {
  command: string
  args: string[]
  env: Record<string, string>
}

export interface McpConfig {
  mcpServers: Record<string, McpServerBlock>
  exe: string
  os: string
  openai_base_url: string
  notes: string[]
}

// ---- WebSocket ----

/** service.Event 的线上形态（ws.go 直接 WriteJSON 一个 Event） */
export interface WsEvent {
  type: string
  node_id?: string
  run_id?: number
  data?: Record<string, unknown>
  message?: string
}
