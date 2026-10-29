// 画布面的协议类型（与 backend/service/nodecatalog.go、graphcheck.go、graph_estimate.go 对齐）。
//
// 一条纪律：这里的类型只描述**后端真的会返回什么**。
// 前端不许自己发明节点类型或字段默认值——那些来自 GET /api/node-catalog，
// 是 v1「元数据双写」的反面。

export type PortKind = 'text' | 'image' | 'video' | 'audio' | 'any'

export interface PortDef {
  key: string
  kind: PortKind
  /** 只对输入有意义：没有来源就跑不起来 */
  required?: boolean
}

export type FieldKind = 'text' | 'textarea' | 'int' | 'float' | 'bool' | 'enum' | 'file'

export interface FieldDef {
  key: string
  label: string
  kind: FieldKind
  default?: unknown
  min?: number
  max?: number
  options?: string[]
  hint?: string
}

export interface NodeCost {
  gpu_class: string
  base_sec: number
  cold_load_sec: number
  sec_per_unit: number
  unit: string
  ref_width?: number
  ref_height?: number
  vram_mb: number
}

export interface CatalogNode {
  type: string
  /** 人类可读名，来自后端契约（前端不维护节点名字典，否则又是一处双写） */
  label: string
  side: string
  heavy: boolean
  media: boolean
  in: PortDef[] | null
  out: PortDef[] | null
  fields: FieldDef[] | null
  cost: NodeCost
}

export interface GraphNode {
  id: string
  type: string
  position?: { x: number; y: number }
  data: Record<string, unknown>
  status?: string
  error?: string
  error_code?: string
}

export interface GraphEdge {
  id: string
  source: string
  target: string
  sourceHandle?: string
  targetHandle?: string
}

export interface DagIssue {
  code: string
  level: 'error' | 'warn'
  message: string
  nodes?: string[]
}

export interface PortIssue {
  code: string
  level: 'error' | 'warn'
  message: string
  nodes?: string[]
  edge_id?: string
}

export interface ParamIssue {
  node_id: string
  node_type: string
  field: string
  message: string
}

export interface GraphReport {
  ok: boolean
  node_count: number
  edge_count: number
  blocking: number
  warnings: number
  dag: DagIssue[] | null
  ports: PortIssue[] | null
  params: ParamIssue[] | null
  blocking_codes?: string[] | null
}

export interface EstimateLine {
  node_id: string
  node_type: string
  unit: string
  count: number
  area_mul: number
  seconds: number
  note?: string
}

export interface GraphEstimate {
  optimistic_sec: number
  p50_sec: number
  p90_sec: number
  worst_sec: number
  cold_load_sec: number
  vram_peak_mb: number
  lines: EstimateLine[] | null
  uncertain?: string[] | null
  caveat: string
}

export interface GraphDep {
  node_id: string
  node_type: string
  dep: string
  label: string
  hint?: string
  fix: string
  fixable: boolean
  fix_dep: string
}

export interface GraphTemplate {
  pipeline_id: string
  name: string
  summary: string
  output: string
  est_seconds?: number
  nodes: GraphNode[] | null
  edges: GraphEdge[] | null
  report?: GraphReport
}

export interface WorkflowSummary {
  id: number
  name: string
  origin: string
  schema_version: number
  node_count: number
  edge_count: number
  created_at: string
  updated_at: string
}

export interface WorkflowDetail {
  id: number
  name: string
  origin: string
  schema_version: number
  nodes: GraphNode[]
  edges: GraphEdge[]
  report: GraphReport
}

export interface DryRunResult {
  ok: boolean
  run_ready: boolean
  report: GraphReport
  deps: GraphDep[] | null
  estimate: GraphEstimate
  /** skipped = 没探测（不可达 ≠ 空结果） */
  probe_state: 'skipped' | 'probed'
}
