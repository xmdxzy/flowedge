import { postJSON, request } from './client'
import type {
  CatalogNode,
  DryRunResult,
  GraphEdge,
  GraphNode,
  GraphReport,
  GraphTemplate,
  WorkflowDetail,
  WorkflowSummary,
} from './graphTypes'

// 画布面的后端调用。与 middleware 一致：错误原文来自后端 `error` 字段，不二次包装。

export async function fetchNodeCatalog(): Promise<CatalogNode[]> {
  const res = await request<{ nodes: CatalogNode[] | null; count: number }>('/api/node-catalog')
  return res.nodes ?? []
}

export async function fetchTemplates(): Promise<GraphTemplate[]> {
  const res = await request<{ templates: GraphTemplate[] | null }>('/api/workflows/templates')
  return res.templates ?? []
}

export async function listWorkflows(): Promise<{ items: WorkflowSummary[]; total: number }> {
  const res = await request<{ workflows: WorkflowSummary[] | null; total: number }>(
    '/api/workflows?limit=100',
  )
  return { items: res.workflows ?? [], total: res.total ?? 0 }
}

export function fetchWorkflow(id: number): Promise<WorkflowDetail> {
  return request<WorkflowDetail>(`/api/workflows/${id}`)
}

export function saveWorkflow(input: {
  id?: number
  name: string
  nodes: GraphNode[]
  edges: GraphEdge[]
}): Promise<{ id: number; name: string }> {
  const payload = { name: input.name, nodes: input.nodes, edges: input.edges }
  if (input.id) {
    return request<{ id: number; name: string }>(`/api/workflows/${input.id}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    })
  }
  return postJSON('/api/workflows', payload) as Promise<{ id: number; name: string }>
}

export function deleteWorkflow(id: number): Promise<{ deleted: number }> {
  return request<{ deleted: number }>(`/api/workflows/${id}`, { method: 'DELETE' })
}

export function validateGraph(nodes: GraphNode[], edges: GraphEdge[]): Promise<GraphReport> {
  return postJSON('/api/graph/validate', { nodes, edges }) as Promise<GraphReport>
}

export function dryRunGraph(nodes: GraphNode[], edges: GraphEdge[]): Promise<DryRunResult> {
  return postJSON('/api/graph/dry-run', { nodes, edges }) as Promise<DryRunResult>
}

/** 运行一张已保存的图。与流水线出片共用同一条队列与门控。
 *
 * qc=true 时服务端把这张图落成 qc: 临时图，外层换成 start → quality_loop → end，
 * 跑完择优并删除临时图（与流水线出片的质检语义完全一致）。
 */
export function runWorkflow(
  id: number,
  opts: { invokedBy?: string; qc?: boolean; qcRounds?: number } = {},
): Promise<{ run_id: number; workflow_id: number; pipeline: string; status: string }> {
  const payload: Record<string, unknown> = { invoked_by: opts.invokedBy ?? 'manual' }
  if (opts.qc) {
    payload.qc = true
    payload.qc_rounds = opts.qcRounds ?? 3
  }
  return postJSON(`/api/workflows/${id}/run`, payload) as Promise<{
    run_id: number
    workflow_id: number
    pipeline: string
    status: string
  }>
}