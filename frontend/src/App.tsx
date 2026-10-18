import { useCallback, useEffect, useRef, useState } from 'react'
import {
  ReactFlow,
  ReactFlowProvider,
  Background,
  Controls,
  MiniMap,
  addEdge,
  useNodesState,
  useEdgesState,
  useReactFlow,
  type Node,
  type Edge,
  type Connection,
  type NodeTypes,
} from '@xyflow/react'
import '@xyflow/react/dist/style.css'

import StartNode from './nodes/StartNode'
import LLMNode from './nodes/LLMNode'
import EndNode from './nodes/EndNode'
import Text2ImageNode from './nodes/Text2ImageNode'
import ASRNode from './nodes/ASRNode'
import TTSNode from './nodes/TTSNode'
import VideoNode from './nodes/VideoNode'
import IfElseNode from './nodes/IfElseNode'
import SubWorkflowNode from './nodes/SubWorkflowNode'
import HTTPNode from './nodes/HTTPNode'
import TransformNode from './nodes/TransformNode'
import RAGNode from './nodes/RAGNode'
import { workflowTemplates, type WorkflowTemplate } from './templates/workflowTemplates'
import { useHotkeys } from './hooks/useHotkeys'
import { useWebSocket, type WSEvent } from './hooks/useWebSocket'
import ExecutionPanel from './components/ExecutionPanel'
import ContextMenu from './components/ContextMenu'
import DebugPanel from './components/DebugPanel'
import KBPanel from './components/KBPanel'

const nodeTypes: NodeTypes = {
  start: StartNode,
  llm: LLMNode,
  end: EndNode,
  text2image: Text2ImageNode,
  asr: ASRNode,
  tts: TTSNode,
  video_compose: VideoNode,
  if_else: IfElseNode,
  sub_workflow: SubWorkflowNode,
  http_request: HTTPNode,
  data_transform: TransformNode,
  rag_query: RAGNode,
}

// 初始示例工作流
const initialNodes: Node[] = [
  {
    id: 'start-1',
    type: 'start',
    position: { x: 80, y: 200 },
    data: { prompt: '用三句话介绍一下人工智能的发展历程' },
  },
  {
    id: 'llm-1',
    type: 'llm',
    position: { x: 350, y: 200 },
    data: { model: 'qwen2.5:7b', prompt: '' },
  },
  {
    id: 'end-1',
    type: 'end',
    position: { x: 620, y: 200 },
    data: {},
  },
]

const initialEdges: Edge[] = [
  { id: 'e1-2', source: 'start-1', target: 'llm-1' },
  { id: 'e2-3', source: 'llm-1', target: 'end-1' },
]

// ReactFlowProvider 包裹：useReactFlow 需要 Provider 祖先
export default function App() {
  return (
    <ReactFlowProvider>
      <FlowEditor />
    </ReactFlowProvider>
  )
}

function FlowEditor() {
  const [nodes, setNodes, onNodesChange] = useNodesState(initialNodes)
  const [edges, setEdges, onEdgesChange] = useEdgesState(initialEdges)
  const [selectedNode, setSelectedNode] = useState<Node | null>(null)
  const [isRunning, setIsRunning] = useState(false)
  const [runResult, setRunResult] = useState<string>('')
  const [gpuStatus, setGpuStatus] = useState<any>(null)
  const [currentWorkflowId, setCurrentWorkflowId] = useState<number | undefined>()
  const [workflowName, setWorkflowName] = useState('未命名工作流')
  const [savedWorkflows, setSavedWorkflows] = useState<any[]>([])
  const [contextMenu, setContextMenu] = useState<{ x: number; y: number; nodeId?: string } | null>(null)
  const [isDebugMode, setIsDebugMode] = useState(false)
  const [pausedNodeId, setPausedNodeId] = useState<string | null>(null)
  const [lastRunId, setLastRunId] = useState<number | null>(null)
  const [canResume, setCanResume] = useState(false)
  const [kbOpen, setKbOpen] = useState(false)
  const streamBufRef = useRef<Map<string, string>>(new Map())
  const streamFlushTimer = useRef<number | null>(null)
  const nodeIdRef = useRef(100)

  // 加载已保存的工作流列表
  const loadWorkflows = async () => {
    try {
      const resp = await fetch('/api/workflows')
      const data = await resp.json()
      setSavedWorkflows(data)
    } catch (err) {
      console.error('failed to load workflows', err)
    }
  }

  // 保存工作流
  const saveWorkflow = async () => {
    const data = {
      name: workflowName,
      nodes,
      edges,
    }

    try {
      if (currentWorkflowId) {
        // 更新
        await fetch(`/api/workflows/${currentWorkflowId}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(data),
        })
      } else {
        // 新建
        const resp = await fetch('/api/workflows', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(data),
        })
        const wf = await resp.json()
        setCurrentWorkflowId(wf.id)
      }
      loadWorkflows()
    } catch (err) {
      console.error('failed to save workflow', err)
    }
  }

  // 导出工作流为 JSON
  const exportWorkflow = () => {
    const data = {
      name: workflowName,
      nodes: nodes,
      edges: edges,
      exported_at: new Date().toISOString(),
    }
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `${workflowName || 'workflow'}.json`
    a.click()
    URL.revokeObjectURL(url)
  }

  // 导入工作流 JSON
  const importWorkflow = (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]
    if (!file) return

    const reader = new FileReader()
    reader.onload = (e) => {
      try {
        const data = JSON.parse(e.target?.result as string)
        if (data.name) setWorkflowName(data.name)
        if (data.nodes) setNodes(data.nodes)
        if (data.edges) setEdges(data.edges)
      } catch (err) {
        console.error('failed to parse workflow file', err)
      }
    }
    reader.readAsText(file)
    event.target.value = ''
  }

  // 加载已有工作流
  const loadWorkflow = async (id: number) => {
    try {
      const resp = await fetch(`/api/workflows/${id}`)
      const wf = await resp.json()
      setWorkflowName(wf.name)
      setCurrentWorkflowId(wf.id)

      // 解析节点和边
      if (wf.nodes) {
        const parsedNodes = typeof wf.nodes === 'string' ? JSON.parse(wf.nodes) : wf.nodes
        setNodes(parsedNodes)
      }
      if (wf.edges) {
        const parsedEdges = typeof wf.edges === 'string' ? JSON.parse(wf.edges) : wf.edges
        setEdges(parsedEdges)
      }
    } catch (err) {
      console.error('failed to load workflow', err)
    }
  }

  // WebSocket 实时事件处理
  const handleWSEvent = (event: WSEvent) => {
    if (event.type === 'node_started' && event.node_id) {
      streamBufRef.current.set(event.node_id, '')
      setNodes((nds) =>
        nds.map((n) =>
          n.id === event.node_id
            ? { ...n, data: { ...n.data, status: 'running', streamText: '' } }
            : n,
        ),
      )
      // 给从该节点出发的连线加动画
      setEdges((eds) =>
        eds.map((e) =>
          e.source === event.node_id
            ? { ...e, animated: true }
            : e,
        ),
      )
    } else if (event.type === 'node_finished' && event.node_id) {
      setNodes((nds) =>
        nds.map((n) =>
          n.id === event.node_id
            ? { ...n, data: { ...n.data, status: 'success' } }
            : n,
        ),
      )
      // 移除连线动画
      setEdges((eds) =>
        eds.map((e) =>
          e.source === event.node_id
            ? { ...e, animated: false }
            : e,
        ),
      )
    } else if (event.type === 'node_failed' && event.node_id) {
      setNodes((nds) =>
        nds.map((n) =>
          n.id === event.node_id
            ? { ...n, data: { ...n.data, status: 'failed' } }
            : n,
        ),
      )
      setEdges((eds) =>
        eds.map((e) =>
          e.source === event.node_id
            ? { ...e, animated: false }
            : e,
        ),
      )
    } else if (event.type === 'node_token' && event.node_id) {
      // LLM 流式打字机：缓冲 120ms 批量刷入节点，避免逐 token 重渲染
      const prev = streamBufRef.current.get(event.node_id) || ''
      streamBufRef.current.set(event.node_id, prev + ((event.data?.delta as string) || ''))
      if (streamFlushTimer.current == null) {
        streamFlushTimer.current = window.setTimeout(() => {
          streamFlushTimer.current = null
          const buf = new Map(streamBufRef.current)
          setNodes((nds) =>
            nds.map((n) =>
              buf.has(n.id)
                ? { ...n, data: { ...n.data, streamText: buf.get(n.id) } }
                : n,
            ),
          )
        }, 120)
      }
    }
  }

  const { connected: wsConnected } = useWebSocket(handleWSEvent)

  // 初始化时加载工作流列表
  useEffect(() => {
    loadWorkflows()
  }, [])

  const onConnect = useCallback(
    (params: Connection) => setEdges((eds) => addEdge(params, eds)),
    [setEdges],
  )

  const onNodeClick = useCallback((_: React.MouseEvent, node: Node) => {
    setSelectedNode(node)
  }, [])

  const onPaneClick = useCallback(() => {
    setSelectedNode(null)
    setContextMenu(null)
  }, [])

  // 画布右键
  const onPaneContextMenu = useCallback((event: MouseEvent | React.MouseEvent) => {
    event.preventDefault()
    setContextMenu({ x: event.clientX, y: event.clientY })
  }, [])

  // 节点右键
  const onNodeContextMenu = useCallback((event: React.MouseEvent, node: Node) => {
    event.preventDefault()
    event.stopPropagation()
    setContextMenu({ x: event.clientX, y: event.clientY, nodeId: node.id })
  }, [])

  // 删除节点
  const deleteNode = (nodeId: string) => {
    setNodes((nds) => nds.filter((n) => n.id !== nodeId))
    setEdges((eds) => eds.filter((e) => e.source !== nodeId && e.target !== nodeId))
    setContextMenu(null)
  }

  // 复制节点
  const duplicateNode = (nodeId: string) => {
    const node = nodes.find((n) => n.id === nodeId)
    if (!node) return
    const newId = `node-${nodeIdRef.current++}`
    setNodes((nds) => [
      ...nds,
      {
        ...node,
        id: newId,
        position: { x: node.position.x + 50, y: node.position.y + 50 },
      },
    ])
    setContextMenu(null)
  }

  // 添加节点
  const addNode = (type: string) => {
    const id = `node-${nodeIdRef.current++}`
    const newNode: Node = {
      id,
      type,
      position: { x: 200 + Math.random() * 200, y: 150 + Math.random() * 200 },
      data: getDefaultNodeData(type),
    }
    setNodes((nds) => [...nds, newNode])
  }

  // 拖拽放置节点
  const { screenToFlowPosition } = useReactFlow()
  const onDragOver = useCallback((event: React.DragEvent) => {
    event.preventDefault()
    event.dataTransfer.dropEffect = 'move'
  }, [])

  const onDrop = useCallback(
    (event: React.DragEvent) => {
      event.preventDefault()
      const type = event.dataTransfer.getData('application/reactflow')
      if (!type) return

      const position = screenToFlowPosition({
        x: event.clientX,
        y: event.clientY,
      })

      const id = `node-${nodeIdRef.current++}`
      const newNode: Node = {
        id,
        type,
        position,
        data: getDefaultNodeData(type),
      }
      setNodes((nds) => [...nds, newNode])
    },
    [screenToFlowPosition, setNodes],
  )

  // 开始拖拽
  const onDragStart = (event: React.DragEvent, nodeType: string) => {
    event.dataTransfer.setData('application/reactflow', nodeType)
    event.dataTransfer.effectAllowed = 'move'
  }

  // 更新选中节点的数据
  const updateSelectedNodeData = (key: string, value: string) => {
    if (!selectedNode) return
    setNodes((nds) =>
      nds.map((n) =>
        n.id === selectedNode.id
          ? { ...n, data: { ...n.data, [key]: value } }
          : n,
      ),
    )
    setSelectedNode((prev) =>
      prev ? { ...prev, data: { ...prev.data, [key]: value } } : prev,
    )
  }

  // 加载模板
  const loadTemplate = (template: WorkflowTemplate) => {
    setNodes(template.nodes)
    setEdges(template.edges)
    setSelectedNode(null)
  }

  // 执行工作流
  const runWorkflow = async () => {
    setIsRunning(true)
    setRunResult('')

    setNodes((nds) =>
      nds.map((n) => ({ ...n, data: { ...n.data, status: 'running' } })),
    )

    try {
      const response = await fetch('/api/workflows', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: 'MVP Demo',
          nodes,
          edges,
        }),
      })

      const wf = await response.json()
      setCurrentWorkflowId(wf.id)

      const runResp = await fetch(`/api/workflows/${wf.id}/run`, {
        method: 'POST',
      })
      const runData = await runResp.json()
      setLastRunId(runData.run_id)

      const pollInterval = setInterval(async () => {
        const statusResp = await fetch(`/api/runs/${runData.run_id}`)
        const statusData = await statusResp.json()

        if (statusData.status === 'success' || statusData.status === 'failed') {
          clearInterval(pollInterval)
          setIsRunning(false)

          if (statusData.status === 'success') {
            setRunResult('执行成功')
            setCanResume(false)
            setNodes((nds) =>
              nds.map((n) => ({
                ...n,
                data: { ...n.data, status: 'success' },
              })),
            )
          } else {
            setRunResult(`执行失败: ${statusData.error}`)
            setCanResume(true)
            setNodes((nds) =>
              nds.map((n) => ({
                ...n,
                data: { ...n.data, status: n.data.status === 'failed' ? 'failed' : n.data.status },
              })),
            )
          }
        }
      }, 1000)
    } catch (err) {
      setIsRunning(false)
      setRunResult(`错误: ${err}`)
    }
  }

  // 断点续跑：复用已完成节点输出，只重跑失败部分
  const resumeWorkflow = async () => {
    if (!lastRunId) return
    setIsRunning(true)
    setRunResult('断点续跑中...')

    try {
      await fetch(`/api/runs/${lastRunId}/resume`, { method: 'POST' })

      const pollInterval = setInterval(async () => {
        const statusResp = await fetch(`/api/runs/${lastRunId}`)
        const statusData = await statusResp.json()

        if (statusData.status === 'success' || statusData.status === 'failed') {
          clearInterval(pollInterval)
          setIsRunning(false)

          if (statusData.status === 'success') {
            setRunResult('续跑成功')
            setCanResume(false)
            setNodes((nds) =>
              nds.map((n) => ({ ...n, data: { ...n.data, status: 'success' } })),
            )
          } else {
            setRunResult(`续跑失败: ${statusData.error}`)
            setCanResume(true)
          }
        }
      }, 1000)
    } catch (err) {
      setIsRunning(false)
      setRunResult(`错误: ${err}`)
    }
  }

  // 快捷键（必须在 runWorkflow 声明之后）
  useHotkeys({
    onRun: runWorkflow,
    onSave: () => console.log('save'),
    onDelete: () => {
      if (selectedNode) {
        setNodes((nds) => nds.filter((n) => n.id !== selectedNode.id))
        setEdges((eds) => eds.filter((e) => e.source !== selectedNode.id && e.target !== selectedNode.id))
        setSelectedNode(null)
      }
    },
  })

  // 刷新 GPU 状态
  const refreshGpuStatus = async () => {
    try {
      const resp = await fetch('/api/gpu/status')
      const data = await resp.json()
      setGpuStatus(data)
    } catch (err) {
      console.error('failed to fetch gpu status', err)
    }
  }

  return (
    <div className="flex h-screen w-screen">
      {/* 左侧节点库 */}
      <div className="w-56 border-r border-border bg-surface p-4 flex flex-col gap-2 overflow-y-auto">
        <h3 className="text-sm font-semibold text-gray-300 mb-2">节点库</h3>

        <div className="fe-section-label mb-1">控制（拖拽到画布）</div>
        <div
          draggable
          onDragStart={(e) => onDragStart(e, 'start')}
          onClick={() => addNode('start')}
          className="fe-palette-item flex items-center gap-2 px-3 py-2 text-left cursor-grab active:cursor-grabbing"
        >
          <div className="w-3 h-3 rounded-full bg-control" />
          <span className="text-sm">开始</span>
        </div>
        <div
          draggable
          onDragStart={(e) => onDragStart(e, 'end')}
          onClick={() => addNode('end')}
          className="flex items-center gap-2 px-3 py-2 rounded-lg fe-palette-item border border-border hover:border-output transition-all text-left cursor-grab active:cursor-grabbing"
        >
          <div className="w-3 h-3 rounded-full bg-output" />
          <span className="text-sm">结束</span>
        </div>
        <div
          draggable
          onDragStart={(e) => onDragStart(e, 'if_else')}
          onClick={() => addNode('if_else')}
          className="flex items-center gap-2 px-3 py-2 rounded-lg fe-palette-item border border-border hover:border-warning transition-all text-left cursor-grab active:cursor-grabbing"
        >
          <div className="w-3 h-3 rounded-full bg-warning" />
          <span className="text-sm">条件分支</span>
        </div>

        <div className="fe-section-label mt-3 mb-1">AI 推理</div>
        <div
          draggable
          onDragStart={(e) => onDragStart(e, 'llm')}
          onClick={() => addNode('llm')}
          className="flex items-center gap-2 px-3 py-2 rounded-lg fe-palette-item border border-border hover:border-ai transition-all text-left cursor-grab active:cursor-grabbing"
        >
          <div className="w-3 h-3 rounded-full bg-ai" />
          <span className="text-sm">大语言模型</span>
        </div>
        <div
          draggable
          onDragStart={(e) => onDragStart(e, 'text2image')}
          onClick={() => addNode('text2image')}
          className="flex items-center gap-2 px-3 py-2 rounded-lg fe-palette-item border border-border hover:border-data transition-all text-left cursor-grab active:cursor-grabbing"
        >
          <div className="w-3 h-3 rounded-full bg-data" />
          <span className="text-sm">文生图</span>
        </div>
        <div
          draggable
          onDragStart={(e) => onDragStart(e, 'rag_query')}
          onClick={() => addNode('rag_query')}
          className="flex items-center gap-2 px-3 py-2 rounded-lg fe-palette-item border border-border hover:border-indigo-500 transition-all text-left cursor-grab active:cursor-grabbing"
        >
          <div className="w-3 h-3 rounded-full bg-indigo-500" />
          <span className="text-sm">知识库检索</span>
        </div>

        <div className="fe-section-label mt-3 mb-1">多模态</div>
        <div
          draggable
          onDragStart={(e) => onDragStart(e, 'asr')}
          onClick={() => addNode('asr')}
          className="flex items-center gap-2 px-3 py-2 rounded-lg fe-palette-item border border-border hover:border-warning transition-all text-left cursor-grab active:cursor-grabbing"
        >
          <div className="w-3 h-3 rounded-full bg-warning" />
          <span className="text-sm">语音转写</span>
        </div>
        <div
          draggable
          onDragStart={(e) => onDragStart(e, 'tts')}
          onClick={() => addNode('tts')}
          className="flex items-center gap-2 px-3 py-2 rounded-lg fe-palette-item border border-border hover:border-success transition-all text-left cursor-grab active:cursor-grabbing"
        >
          <div className="w-3 h-3 rounded-full bg-success" />
          <span className="text-sm">语音合成</span>
        </div>
        <div
          draggable
          onDragStart={(e) => onDragStart(e, 'video_compose')}
          onClick={() => addNode('video_compose')}
          className="flex items-center gap-2 px-3 py-2 rounded-lg fe-palette-item border border-border hover:border-purple transition-all text-left cursor-grab active:cursor-grabbing"
        >
          <div className="w-3 h-3 rounded-full bg-purple" />
          <span className="text-sm">视频合成</span>
        </div>
        <div
          draggable
          onDragStart={(e) => onDragStart(e, 'sub_workflow')}
          onClick={() => addNode('sub_workflow')}
          className="flex items-center gap-2 px-3 py-2 rounded-lg fe-palette-item border border-border hover:border-purple transition-all text-left cursor-grab active:cursor-grabbing"
        >
          <div className="w-3 h-3 rounded-full bg-purple" />
          <span className="text-sm">子工作流</span>
        </div>

        <div className="fe-section-label mt-3 mb-1">数据处理</div>
        <div
          draggable
          onDragStart={(e) => onDragStart(e, 'http_request')}
          onClick={() => addNode('http_request')}
          className="flex items-center gap-2 px-3 py-2 rounded-lg fe-palette-item border border-border hover:border-cyan-500 transition-all text-left cursor-grab active:cursor-grabbing"
        >
          <div className="w-3 h-3 rounded-full bg-cyan-500" />
          <span className="text-sm">HTTP 请求</span>
        </div>
        <div
          draggable
          onDragStart={(e) => onDragStart(e, 'data_transform')}
          onClick={() => addNode('data_transform')}
          className="flex items-center gap-2 px-3 py-2 rounded-lg fe-palette-item border border-border hover:border-gray-500 transition-all text-left cursor-grab active:cursor-grabbing"
        >
          <div className="w-3 h-3 rounded-full bg-gray-500" />
          <span className="text-sm">数据转换</span>
        </div>

        {/* 模板库 */}
        <div className="fe-section-label mt-3 mb-1">模板</div>
        {workflowTemplates.map((tpl) => (
          <button
            key={tpl.id}
            onClick={() => loadTemplate(tpl)}
            className="w-full text-left px-3 py-2 rounded-lg fe-palette-item border border-border hover:border-primary transition-all mb-1"
          >
            <p className="text-xs font-medium text-white">{tpl.name}</p>
            <p className="text-[10px] text-gray-500 truncate">{tpl.description}</p>
          </button>
        ))}

        {/* 已保存工作流 */}
        <div className="fe-section-label mt-3 mb-1">已保存</div>
        {savedWorkflows.length === 0 ? (
          <p className="text-[10px] text-gray-600 px-1">暂无保存的工作流</p>
        ) : (
          savedWorkflows.slice(0, 5).map((wf) => (
            <button
              key={wf.id}
              onClick={() => loadWorkflow(wf.id)}
              className={`w-full text-left px-3 py-1.5 rounded-lg bg-canvas border transition-all mb-1 text-xs
                ${currentWorkflowId === wf.id ? 'border-primary text-white' : 'border-border text-gray-400 hover:border-gray-500'}
              `}
            >
              {wf.name}
            </button>
          ))
        )}

        <div className="mt-auto pt-4 border-t border-border">
          <button
            onClick={refreshGpuStatus}
            className="w-full px-3 py-2 rounded-lg fe-palette-item border border-border hover:border-primary text-xs"
          >
            刷新 GPU 状态
          </button>
          {gpuStatus && (
            <div className="mt-2 p-2 rounded bg-canvas text-[10px] text-gray-400">
              <p>显存: {gpuStatus.gpu?.usedMB || 0}MB / {gpuStatus.gpu?.totalMB || 0}MB</p>
              <p>利用率: {gpuStatus.gpu?.utilization || 0}%</p>
            </div>
          )}
        </div>
      </div>

      {/* 中间画布区域 */}
      <div className="flex-1 flex flex-col">
        {/* 顶部工具栏 */}
        <div className="fe-topbar h-14 flex items-center px-4 gap-4">
          <div className="flex items-center gap-2.5">
            <div className="fe-logo-mark">
              <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
                <path d="M9 1L3 9h4l-1 6 6-8H8l1-6z" fill="white" />
              </svg>
            </div>
            <h1 className="fe-title text-lg font-bold">FlowEdge</h1>
          </div>

          <input
            value={workflowName}
            onChange={(e) => setWorkflowName(e.target.value)}
            className="px-3 py-1.5 rounded-lg bg-[#0D111B] border border-border text-sm w-48 focus:border-primary outline-none"
            placeholder="工作流名称"
          />

          <div className="flex items-center gap-1.5">
            <div className={`w-2 h-2 rounded-full ${wsConnected ? 'bg-success' : 'bg-gray-500'}`} />
            <span className="text-[10px] text-gray-500">{wsConnected ? '实时' : '离线'}</span>
          </div>

          <div className="ml-auto flex items-center gap-2">
            {runResult && (
              <span className="text-xs text-gray-400">{runResult}</span>
            )}
            <button onClick={exportWorkflow} className="fe-btn-ghost px-3 py-1.5 rounded-lg text-sm">
              导出
            </button>
            <label className="fe-btn-ghost px-3 py-1.5 rounded-lg text-sm cursor-pointer">
              导入
              <input type="file" accept=".json" onChange={importWorkflow} className="hidden" />
            </label>
            <button onClick={saveWorkflow} className="fe-btn-ghost px-3 py-1.5 rounded-lg text-sm">
              保存
            </button>
            <button
              onClick={() => setKbOpen(true)}
              className="fe-btn-ghost px-3 py-1.5 rounded-lg text-sm"
              title="项目专属语义知识库"
            >
              知识库
            </button>

            <DebugPanel
              isDebugging={isDebugMode}
              pausedNodeId={pausedNodeId}
              onStartDebug={() => setIsDebugMode(true)}
              onPause={() => setPausedNodeId('current')}
              onResume={() => setPausedNodeId(null)}
              onStep={() => setPausedNodeId(null)}
              onStop={() => { setIsDebugMode(false); setPausedNodeId(null) }}
            />

            {canResume && !isRunning && (
              <button
                onClick={resumeWorkflow}
                className="fe-btn-ghost px-3 py-1.5 rounded-lg text-sm"
                title="复用已完成节点的输出，只重跑失败部分"
              >
                ⟲ 断点续跑
              </button>
            )}

            <button
              onClick={runWorkflow}
              disabled={isRunning}
              className="fe-btn-primary px-5 py-1.5 rounded-lg text-sm font-medium"
            >
              {isRunning ? '执行中...' : '▶ 运行工作流'}
            </button>
          </div>
        </div>

        {/* 画布 */}
        <div className="flex-1 flex flex-col">
          <div className="flex-1">
            <ReactFlow
              nodes={nodes}
              edges={edges}
              onNodesChange={onNodesChange}
              onEdgesChange={onEdgesChange}
              onConnect={onConnect}
              onNodeClick={onNodeClick}
              onNodeContextMenu={onNodeContextMenu}
              onPaneClick={onPaneClick}
              onPaneContextMenu={onPaneContextMenu}
              onDrop={onDrop}
              onDragOver={onDragOver}
              nodeTypes={nodeTypes}
              defaultEdgeOptions={{ type: 'smoothstep' }}
              fitView
              className="bg-canvas"
            >
              <Background color="#1C2436" gap={22} size={1.2} />
              <Controls />
              <MiniMap
                pannable
                zoomable
                bgColor="#0D111B"
                nodeColor="#6366F1"
                maskColor="rgba(10, 13, 20, 0.75)"
              />
            </ReactFlow>
          </div>

          {/* 执行历史面板 */}
          <ExecutionPanel workflowId={currentWorkflowId} />
        </div>
      </div>

      {/* 右键菜单 */}
      {contextMenu && (
        <ContextMenu
          x={contextMenu.x}
          y={contextMenu.y}
          onClose={() => setContextMenu(null)}
          items={
            contextMenu.nodeId
              ? [
                  { label: '复制节点', icon: '📋', onClick: () => duplicateNode(contextMenu.nodeId!) },
                  { label: '删除节点', icon: '🗑️', danger: true, onClick: () => deleteNode(contextMenu.nodeId!) },
                ]
              : [
                  { label: '添加开始节点', icon: '▶️', onClick: () => { addNode('start'); setContextMenu(null) } },
                  { label: '添加 LLM 节点', icon: '🤖', onClick: () => { addNode('llm'); setContextMenu(null) } },
                  { label: '清空画布', icon: '🗑️', danger: true, onClick: () => { setNodes([]); setEdges([]); setContextMenu(null) } },
                ]
          }
        />
      )}

      {/* 项目知识库面板 */}
      <KBPanel open={kbOpen} onClose={() => setKbOpen(false)} />

      {/* 右侧属性面板 */}
      <div className="w-72 border-l border-border bg-surface p-4 overflow-y-auto">
        {/* Tab 切换 */}
        <div className="flex border-b border-border mb-4">
          <button className="px-3 py-2 text-xs font-medium text-primary border-b-2 border-primary">
            配置
          </button>
          <button className="px-3 py-2 text-xs font-medium text-gray-500 hover:text-gray-300">
            输出
          </button>
          <button className="px-3 py-2 text-xs font-medium text-gray-500 hover:text-gray-300">
            日志
          </button>
        </div>

        {selectedNode ? (
          <div className="flex flex-col gap-4">
            <div>
              <label className="text-xs text-gray-400 block mb-1">节点类型</label>
              <p className="text-sm text-white">{selectedNode.type}</p>
            </div>

            {selectedNode.type === 'start' && (
              <div>
                <label className="text-xs text-gray-400 block mb-1">输入提示词</label>
                <textarea
                  value={(selectedNode.data.prompt as string) || ''}
                  onChange={(e) => updateSelectedNodeData('prompt', e.target.value)}
                  className="w-full h-24 px-3 py-2 rounded-lg bg-canvas border border-border text-sm resize-none focus:border-primary outline-none"
                  placeholder="输入要传递给下游的文本..."
                />
              </div>
            )}

            {selectedNode.type === 'llm' && (
              <>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">模型</label>
                  <input
                    value={(selectedNode.data.model as string) || ''}
                    onChange={(e) => updateSelectedNodeData('model', e.target.value)}
                    className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none"
                    placeholder="qwen2.5:7b"
                  />
                </div>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">自定义提示词（可选）</label>
                  <textarea
                    value={(selectedNode.data.prompt as string) || ''}
                    onChange={(e) => updateSelectedNodeData('prompt', e.target.value)}
                    className="w-full h-24 px-3 py-2 rounded-lg bg-canvas border border-border text-sm resize-none focus:border-primary outline-none"
                    placeholder="留空则使用上游输入..."
                  />
                </div>
              </>
            )}

            {selectedNode.type === 'text2image' && (
              <>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">提示词</label>
                  <textarea
                    value={(selectedNode.data.prompt as string) || ''}
                    onChange={(e) => updateSelectedNodeData('prompt', e.target.value)}
                    className="w-full h-20 px-3 py-2 rounded-lg bg-canvas border border-border text-sm resize-none focus:border-primary outline-none"
                    placeholder="描述要生成的图片..."
                  />
                </div>
                <div className="flex gap-2">
                  <div>
                    <label className="text-xs text-gray-400 block mb-1">宽度</label>
                    <input
                      type="number"
                      value={(selectedNode.data.width as number) || 1024}
                      onChange={(e) => updateSelectedNodeData('width', e.target.value)}
                      className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none"
                    />
                  </div>
                  <div>
                    <label className="text-xs text-gray-400 block mb-1">高度</label>
                    <input
                      type="number"
                      value={(selectedNode.data.height as number) || 1024}
                      onChange={(e) => updateSelectedNodeData('height', e.target.value)}
                      className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none"
                    />
                  </div>
                </div>
              </>
            )}

            {selectedNode.type === 'if_else' && (
              <>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">条件类型</label>
                  <select
                    value={(selectedNode.data.condition_type as string) || 'not_empty'}
                    onChange={(e) => updateSelectedNodeData('condition_type', e.target.value)}
                    className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none"
                  >
                    <option value="not_empty">不为空</option>
                    <option value="contains">包含</option>
                    <option value="equals">等于</option>
                  </select>
                </div>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">比较值</label>
                  <input
                    value={(selectedNode.data.compare_value as string) || ''}
                    onChange={(e) => updateSelectedNodeData('compare_value', e.target.value)}
                    className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none"
                    placeholder="输入比较文本..."
                  />
                </div>
              </>
            )}

            {selectedNode.type === 'end' && (
              <p className="text-xs text-gray-500">
                结束节点，输出最终结果
              </p>
            )}

            {/* 输出预览 */}
            {(() => {
              const out = selectedNode.data?.output
              if (out == null) return null
              const text = typeof out === 'string' ? out : JSON.stringify(out, null, 2)
              return (
                <div className="mt-2 pt-3 border-t border-border">
                  <label className="text-xs text-gray-400 block mb-2">输出预览</label>
                  <div className="p-2 rounded bg-canvas max-h-32 overflow-y-auto">
                    <pre className="text-xs text-success font-mono whitespace-pre-wrap">{text}</pre>
                  </div>
                </div>
              )
            })()}

            {/* 错误信息 */}
            {selectedNode.data?.error != null && (
              <div className="p-2 rounded bg-error/10 border border-error/30">
                <p className="text-xs text-error">{String(selectedNode.data.error)}</p>
              </div>
            )}
          </div>
        ) : (
          <p className="text-xs text-gray-500">点击画布中的节点进行配置</p>
        )}
      </div>
    </div>
  )
}

function getDefaultNodeData(type: string): Record<string, any> {
  switch (type) {
    case 'start':
      return { prompt: '' }
    case 'llm':
      return { model: 'qwen2.5:7b', prompt: '' }
    case 'end':
      return {}
    case 'text2image':
      return { prompt: '', width: 1024, height: 1024 }
    case 'asr':
      return { audio_path: '' }
    case 'tts':
      return { text: '' }
    case 'video_compose':
      return { output_path: 'data/generated/video.mp4' }
    case 'if_else':
      return { condition_type: 'not_empty', compare_value: '' }
    case 'sub_workflow':
      return { workflow_id: undefined, workflow_name: '' }
    case 'http_request':
      return { method: 'GET', url: '', headers: {}, body: '' }
    case 'data_transform':
      return { template: '' }
    case 'rag_query':
      return { query: '' }
    default:
      return {}
  }
}
