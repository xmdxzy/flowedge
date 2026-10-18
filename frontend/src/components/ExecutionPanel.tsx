import { useState, useEffect } from 'react'

interface RunRecord {
  id: number
  workflow_id: number
  status: string
  error?: string
  started_at?: string
  ended_at?: string
  created_at: string
}

export default function ExecutionPanel({ workflowId }: { workflowId?: number }) {
  const [runs, setRuns] = useState<RunRecord[]>([])
  const [selectedRun, setSelectedRun] = useState<RunRecord | null>(null)
  const [isOpen, setIsOpen] = useState(false)

  // 加载执行历史
  const loadRuns = async () => {
    if (!workflowId) return
    try {
      const resp = await fetch(`/api/workflows/${workflowId}/runs`)
      const data = await resp.json()
      setRuns(data)
    } catch (err) {
      console.error('failed to load runs', err)
    }
  }

  useEffect(() => {
    loadRuns()
  }, [workflowId])

  const statusColor = {
    pending: 'bg-gray-500',
    running: 'bg-primary animate-pulse',
    success: 'bg-success',
    failed: 'bg-error',
  }

  return (
    <div className="border-t border-border bg-surface">
      {/* 面板头部 */}
      <div
        className="flex items-center justify-between px-4 py-2 cursor-pointer hover:bg-surface-hover"
        onClick={() => setIsOpen(!isOpen)}
      >
        <div className="flex items-center gap-2">
          <span className="text-xs font-semibold text-gray-300">执行历史</span>
          <span className="text-[10px] text-gray-500">{runs.length} 条记录</span>
        </div>
        <span className="text-xs text-gray-500">{isOpen ? '▼' : '▲'}</span>
      </div>

      {/* 面板内容 */}
      {isOpen && (
        <div className="max-h-48 overflow-y-auto">
          {runs.length === 0 ? (
            <p className="px-4 py-3 text-xs text-gray-500">暂无执行记录</p>
          ) : (
            runs.map((run) => (
              <div
                key={run.id}
                className={`px-4 py-2 border-b border-border/50 cursor-pointer hover:bg-surface-hover
                  ${selectedRun?.id === run.id ? 'bg-surface-hover' : ''}
                `}
                onClick={() => setSelectedRun(run)}
              >
                <div className="flex items-center gap-2">
                  <div className={`w-2 h-2 rounded-full ${statusColor[run.status as keyof typeof statusColor] || 'bg-gray-500'}`} />
                  <span className="text-xs text-white">Run #{run.id}</span>
                  <span className="text-[10px] text-gray-500 ml-auto">
                    {new Date(run.created_at).toLocaleString()}
                  </span>
                </div>
                {run.error && (
                  <p className="text-[10px] text-error mt-1 truncate">{run.error}</p>
                )}
              </div>
            ))
          )}
        </div>
      )}
    </div>
  )
}
