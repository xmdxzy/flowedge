import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
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
import LoopNode from './nodes/LoopNode'
import HTTPNode from './nodes/HTTPNode'
import TransformNode from './nodes/TransformNode'
import RAGNode from './nodes/RAGNode'
import CodeNode from './nodes/CodeNode'
import AggregateNode from './nodes/AggregateNode'
import NoteNode from './nodes/NoteNode'
import GroupNode from './nodes/GroupNode'
import VisionNode from './nodes/VisionNode'
import EditNode from './nodes/EditNode'
import { type WorkflowTemplate } from './templates/workflowTemplates'
import ToastHost, { toast } from './components/Toast'
import ErrorNotice from './components/ErrorNotice'
import {
  buildEntries,
  bumpUsage,
  groupOf,
  readUsage,
  topEntries,
  type UsageMap,
} from './templates/templateCatalog'
import TemplateLibrary from './components/TemplateLibrary'
import StatusBar from './components/StatusBar'
import NodePalette from './components/NodePalette'
import { bumpNodeUsage, readNodeUsage, type NodeUsageMap } from './lib/nodeUsage'
import { useHotkeys } from './hooks/useHotkeys'
import { useWebSocket, type WSEvent } from './hooks/useWebSocket'
import ExecutionPanel from './components/ExecutionPanel'
import ContextMenu from './components/ContextMenu'
import DebugPanel from './components/DebugPanel'
import CapabilityPanel from './components/CapabilityPanel'
import TriggerPanel from './components/TriggerPanel'
import GalleryPanel from './components/GalleryPanel'
import CredentialPanel from './components/CredentialPanel'
import CommandPalette, { type CommandItem } from './components/CommandPalette'
import KBPanel from './components/KBPanel'
import {
  PALETTE,
  NODE_OUTPUT_FIELDS,
  LLM_TOOL_OPTIONS,
  CANVAS_ONLY_TYPES,
  initialNodes,
  initialEdges,
} from './config/graph'
import { autoLayout, boundingBox, nextId, stripCanvasOnlyEdges, stripCanvasOnlyNodes } from './lib/graphUtils'
import { applyTheme, CANVAS_THEME, getInitialTheme, type Theme } from './lib/theme'

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
  loop: LoopNode,
  http_request: HTTPNode,
  data_transform: TransformNode,
  rag_query: RAGNode,
  code: CodeNode,
  aggregate: AggregateNode,
  vision: VisionNode,
  select: EditNode,
  storyboard: TransformNode,
  shotlist: EditNode,
  quality_gate: EditNode,
  image2image: EditNode,
  inpaint: EditNode,
  upscale: EditNode,
  text2video: EditNode,
  note: NoteNode,
  group: GroupNode,
}

// ReactFlowProvider 包裹：useReactFlow 需要 Provider 祖先
// 毫秒转人类可读时长：短任务用秒，长任务用分钟（多模态单节点常跑一两分钟）
function fmtDuration(ms: number): string {
  if (ms >= 60000) return `${(ms / 60000).toFixed(1)}min`
  return `${Math.max(1, Math.round(ms / 1000))}s`
}

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
  // 调试：debugMode 只是「这次运行按断点执行」的开关，真正的暂停状态来自后端 run.status=paused
  const [debugMode, setDebugMode] = useState(false)
  const [preflightResult, setPreflightResult] = useState<
    { node_id: string; label: string; hint: string; fix: string; fixable: boolean; fix_dep: string }[]
  >([])
  const [preflightChecking, setPreflightChecking] = useState(false)
  const [runStatus, setRunStatus] = useState<string>('')
  // P0 可观测性：运行中节点的实时进度（已用 / 预计），来自后端 WS node_progress 心跳
  const [liveProgress, setLiveProgress] = useState<{ nodeId: string; elapsedMs: number; etaMs: number } | null>(null)
  const [pausedAt, setPausedAt] = useState('')
  const [pauseReason, setPauseReason] = useState('')
  const [debugBusy, setDebugBusy] = useState(false)
  const [theme, setTheme] = useState<Theme>(() => getInitialTheme())
  const [credOpen, setCredOpen] = useState(false)
  const [lastRunId, setLastRunId] = useState<number | null>(null)
  const [canResume, setCanResume] = useState(false)
  const [kbOpen, setKbOpen] = useState(false)
  const [envOpen, setEnvOpen] = useState(false)
  const [triggerOpen, setTriggerOpen] = useState(false)
  const [galleryOpen, setGalleryOpen] = useState(false)
  const [cmdOpen, setCmdOpen] = useState(false)
  const [tplLibOpen, setTplLibOpen] = useState(false)
  const [moreOpen, setMoreOpen] = useState(false)
  // 模板使用频次：驱动左栏「常用」排序，存在 localStorage
  const [tplUsage, setTplUsage] = useState<UsageMap>(() => readUsage())
  const tplEntries = useMemo(() => buildEntries(tplUsage), [tplUsage])
  const tplTop = useMemo(() => topEntries(tplEntries, 4), [tplEntries])
  // 节点使用频次：驱动「常用节点」
  const [nodeUsage, setNodeUsage] = useState<NodeUsageMap>(() => readNodeUsage())
  const [propTab, setPropTab] = useState<'config' | 'output'>('config')
  // 环境就绪状态：用于把「当前跑不了」的节点标灰（仍可拖入画布，方便先编排）
  const [envReady, setEnvReady] = useState<Record<string, boolean> | null>(null)

  const refreshEnvReady = useCallback(() => {
    fetch('/api/capabilities')
      .then((r) => (r.ok ? r.json() : null))
      .then((d) => {
        if (d?.capabilities?.ready) setEnvReady(d.capabilities.ready)
      })
      .catch(() => {})
  }, [])

  useEffect(() => {
    refreshEnvReady()
    // 依赖启停成功后由 DepsSection 派发事件 → 标灰状态当场刷新。
    // 旧版只在挂载时取一次，于是「在面板里启动了 ComfyUI，左栏节点还是灰的」，
    // 必须整页刷新才生效。窗口重新获得焦点时也补一次（应对外部手动起服务）。
    const onEnv = () => refreshEnvReady()
    window.addEventListener('flowedge:env-changed', onEnv)
    window.addEventListener('focus', onEnv)
    return () => {
      window.removeEventListener('flowedge:env-changed', onEnv)
      window.removeEventListener('focus', onEnv)
    }
  }, [refreshEnvReady])

  // 主题：切换写 data-theme，颜色由 CSS 变量接管
  useEffect(() => {
    applyTheme(theme)
  }, [theme])
  const streamBufRef = useRef<Map<string, string>>(new Map())
  const streamFlushTimer = useRef<number | null>(null)
  const nodeIdRef = useRef(100)

  // 撤销 / 重做：只记录用户主动的结构性变更。
  // 运行期状态刷新（WS 事件）不入栈，否则一次运行会把历史刷爆。
  const undoStack = useRef<{ nodes: Node[]; edges: Edge[] }[]>([])
  const redoStack = useRef<{ nodes: Node[]; edges: Edge[] }[]>([])
  const graphRef = useRef<{ nodes: Node[]; edges: Edge[] }>({ nodes, edges })
  useEffect(() => {
    graphRef.current = { nodes, edges }
  }, [nodes, edges])

  const pushHistory = useCallback(() => {
    undoStack.current.push({ nodes: graphRef.current.nodes, edges: graphRef.current.edges })
    if (undoStack.current.length > 50) undoStack.current.shift()
    redoStack.current = []
  }, [])

  const undo = useCallback(() => {
    const prev = undoStack.current.pop()
    if (!prev) return
    redoStack.current.push({ nodes: graphRef.current.nodes, edges: graphRef.current.edges })
    setNodes(prev.nodes)
    setEdges(prev.edges)
  }, [setNodes, setEdges])

  const redo = useCallback(() => {
    const next = redoStack.current.pop()
    if (!next) return
    undoStack.current.push({ nodes: graphRef.current.nodes, edges: graphRef.current.edges })
    setNodes(next.nodes)
    setEdges(next.edges)
  }, [setNodes, setEdges])

  // 命令面板：Ctrl/Cmd + K 随时唤起
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setCmdOpen((v) => !v)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

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
      // 批注 / 分组是画布装饰，不进工作流定义（否则后端会当成未知节点执行失败）
      nodes: stripCanvasOnlyNodes(nodes),
      edges: stripCanvasOnlyEdges(nodes, edges),
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
      nodes: stripCanvasOnlyNodes(nodes),
      edges: stripCanvasOnlyEdges(nodes, edges),
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

  // 深链：?workflow=<id> 直接打开指定工作流（Electron 启动参数 / 分享链接用）
  useEffect(() => {
    const wfId = new URLSearchParams(window.location.search).get('workflow')
    if (wfId && /^\d+$/.test(wfId)) {
      loadWorkflow(Number(wfId))
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // WebSocket 实时事件处理
  const handleWSEvent = (event: WSEvent) => {
    if (event.type === 'node_started' && event.node_id) {
      streamBufRef.current.set(event.node_id, '')
      setNodes((nds) =>
        nds.map((n) =>
          n.id === event.node_id
            ? { ...n, data: { ...n.data, status: 'running', streamText: '', loop_iteration: 0 } }
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
    } else if (event.type === 'node_progress' && event.node_id) {
      // P0 可观测性：长任务实时进度。此前界面只有一个转圈，看不到跑到哪了。
      const elapsedMs = Number(event.data?.elapsed_ms ?? 0)
      const etaMs = Number(event.data?.eta_ms ?? 0)
      setLiveProgress({ nodeId: event.node_id, elapsedMs, etaMs })
      setNodes((nds) =>
        nds.map((n) =>
          n.id === event.node_id
            ? { ...n, data: { ...n.data, elapsed_ms: elapsedMs, eta_ms: etaMs } }
            : n,
        ),
      )
    } else if (event.type === 'node_finished' && event.node_id) {
      const finalIter = (event.data?.output as Record<string, any> | undefined)?.iterations
      setNodes((nds) =>
        nds.map((n) =>
          n.id === event.node_id
            ? {
                ...n,
                data: {
                  ...n.data,
                  status: 'success',
                  // 写入真实输出与空错误：属性面板「输出」Tab 依赖 data.output，
                  // 此前 WS 回调从不写该字段，导致输出预览恒为空
                  output: event.data?.output,
                  error: undefined,
                  ...(typeof finalIter === 'number' ? { loop_iteration: finalIter } : {}),
                },
              }
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
            ? { ...n, data: { ...n.data, status: 'failed', error: event.message } }
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
    } else if (event.type === 'loop_iteration' && event.node_id) {
      // 循环节点每轮进度（低频，直接刷）
      setNodes((nds) =>
        nds.map((n) =>
          n.id === event.node_id
            ? { ...n, data: { ...n.data, loop_iteration: event.data?.iteration as number } }
            : n,
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
    (params: Connection) => {
      pushHistory()
      setEdges((eds) => addEdge(params, eds))
    },
    [setEdges, pushHistory],
  )

  // 连线校验：禁自环、禁重复边、禁把 start 当下游。
  // 没有它，用户可以连出成环的图——引擎侧会拒绝执行，但应在拖拽阶段就阻止。
  const isValidConnection = useCallback(
    (conn: Connection | Edge) => {
      const source = conn.source ?? ''
      const target = conn.target ?? ''
      if (!source || !target || source === target) return false

      const tgt = nodes.find((n) => n.id === target)
      if (tgt?.type === 'start') return false

      const dup = edges.some(
        (e) =>
          e.source === source &&
          e.target === target &&
          (e.sourceHandle ?? '') === (conn.sourceHandle ?? ''),
      )
      return !dup
    },
    [nodes, edges],
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
    pushHistory()
    setNodes((nds) => nds.filter((n) => n.id !== nodeId))
    setEdges((eds) => eds.filter((e) => e.source !== nodeId && e.target !== nodeId))
    setContextMenu(null)
  }

  // 复制节点
  const duplicateNode = (nodeId: string) => {
    const node = nodes.find((n) => n.id === nodeId)
    if (!node) return
    pushHistory()
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
    if (type === 'group') {
      groupSelection()
      return
    }
    setNodeUsage(bumpNodeUsage(type))
    pushHistory()
    const id = `node-${nodeIdRef.current++}`
    const newNode: Node = {
      id,
      type,
      // 错开排布：此前用 Math.random() 落点，连续添加会重叠成一坨
      position: { x: 200 + (nodeIdRef.current % 5) * 60, y: 150 + (nodeIdRef.current % 5) * 60 },
      data: getDefaultNodeData(type),
    }
    // 分组框永远垫在最底层，避免盖住节点
    setNodes((nds) => (type === 'group' ? [newNode, ...nds] : [...nds, newNode]))
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
      pushHistory()
      setNodes((nds) => [...nds, newNode])
    },
    [screenToFlowPosition, setNodes, pushHistory],
  )

  // 开始拖拽
  const onDragStart = (event: React.DragEvent, nodeType: string) => {
    setNodeUsage(bumpNodeUsage(nodeType))
    event.dataTransfer.setData('application/reactflow', nodeType)
    event.dataTransfer.effectAllowed = 'move'
  }

  // 更新选中节点的数据
  const updateSelectedNodeData = (key: string, value: unknown) => {
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
  // 加载模板：若模板内含「嵌套内层定义」（_inner），先自动创建内层工作流
  // 并把返回的 id 回填到 loop / sub_workflow 节点，模板开箱即用
  const loadTemplate = async (template: WorkflowTemplate) => {
    // 记录使用频次：置顶之外，用得多也会自然浮到前面
    setTplUsage(bumpUsage(template.id))
    const nodes = JSON.parse(JSON.stringify(template.nodes)) as any[]

    for (const n of nodes) {
      const inner = n?.data?._inner
      if (!inner) continue
      try {
        const resp = await fetch('/api/workflows', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(inner),
        })
        const wf = await resp.json()
        n.data.workflow_id = wf.id
        n.data.workflow_name = wf.name
      } catch (err) {
        console.error('failed to create inner workflow for template', err)
      }
      delete n.data._inner
    }

    setNodes(nodes)
    setEdges(JSON.parse(JSON.stringify(template.edges)))
    setSelectedNode(null)
    loadWorkflows()
  }

  // 执行工作流
  // ===== 运行 / 调试 =====

  /** 把后端 node_states 回写到画布（状态 + 输出 + 错误） */
  const applyRunStates = useCallback(
    (states: any[]) => {
      if (!Array.isArray(states) || states.length === 0) return
      const map = new Map(states.map((s) => [s.id, s]))
      setNodes((nds) =>
        nds.map((n) => {
          const st = map.get(n.id)
          if (!st) return n
          return {
            ...n,
            data: {
              ...n.data,
              status: st.status,
              output: st.output,
              error: st.error,
              error_code: st.error_code,
            },
          }
        }),
      )
    },
    [setNodes],
  )

  /** 轮询到终态（success / failed / paused / interrupted） */
  const pollRun = useCallback((runId: number, intervalMs = 800) => {
    return new Promise<any>((resolve) => {
      const timer = window.setInterval(async () => {
        try {
          const resp = await fetch(`/api/runs/${runId}`)
          const data = await resp.json()
          if (['success', 'failed', 'paused', 'interrupted'].includes(data.status)) {
            window.clearInterval(timer)
            resolve(data)
          }
        } catch {
          /* 网络抖动继续等 */
        }
      }, intervalMs)
    })
  }, [])

  const runWorkflow = async () => {
    setIsRunning(true)
    setRunResult('')
    setPausedAt('')
    setPauseReason('')
    setRunStatus('running')

    setNodes((nds) => nds.map((n) => ({ ...n, data: { ...n.data, status: 'running' } })))

    try {
      // 批注 / 分组只存在于画布，绝不能提交给引擎
      const apiNodes = stripCanvasOnlyNodes(nodes)
      const apiEdges = stripCanvasOnlyEdges(nodes, edges)

      // 复用当前工作流：以前每次运行都 POST 新建一条，跑十次左栏「已保存」就多十条垃圾，
      // 而且用户以为自己只是在跑，实际在偷偷建副本。只有真的没有 id（或被删）才新建。
      const body = JSON.stringify({ name: workflowName || '未命名工作流', nodes: apiNodes, edges: apiEdges })
      const headers = { 'Content-Type': 'application/json' }
      let wfId: number | undefined = currentWorkflowId
      if (wfId) {
        const put = await fetch(`/api/workflows/${wfId}`, { method: 'PUT', headers, body })
        if (!put.ok) wfId = undefined // 已被删掉 → 回退为新建
      }
      if (!wfId) {
        const created = await fetch('/api/workflows', { method: 'POST', headers, body })
        const wf = await created.json()
        wfId = wf.id
        setCurrentWorkflowId(wfId)
      }

      const runResp = await fetch(`/api/workflows/${wfId}/run`, { method: 'POST' })
      const runData = await runResp.json()
      setLastRunId(runData.run_id)

      const data = await pollRun(runData.run_id)
      applyRunStates(data.node_states)

      if (data.status === 'paused') {
        setRunStatus('paused')
        setPausedAt(data.paused_at || '')
        setPauseReason(data.pause_reason || 'breakpoint')
        setRunResult(`已暂停于 ${data.paused_at || '断点'}`)
        setIsRunning(false)
        return
      }

      setRunStatus(data.status)
      setIsRunning(false)
      if (data.status === 'success') {
        setRunResult('执行成功')
        setCanResume(false)
      } else {
        setRunResult(`执行失败: ${data.error}`)
        setCanResume(true)
      }
    } catch (err) {
      setIsRunning(false)
      setRunStatus('failed')
      setRunResult(`错误: ${err}`)
    }
  }

  /** 调试推进（step / continue）：都是异步的，落库后靠轮询收结果 */
  const advanceRun = async (mode: 'step' | 'continue') => {
    if (!lastRunId) {
      setRunResult('先运行一次工作流，再单步')
      return
    }
    setDebugBusy(true)
    setRunResult(mode === 'step' ? '单步执行中…' : '继续执行中…')
    try {
      const breakpoints = nodes.filter((n) => n.data?.breakpoint).map((n) => n.id)
      const resp = await fetch(`/api/runs/${lastRunId}/${mode}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ breakpoints }),
      })
      if (!resp.ok) {
        const d = await resp.json().catch(() => ({}))
        setRunResult(d.error || '推进失败')
        setDebugBusy(false)
        return
      }
      setRunStatus('running')
      const data = await pollRun(lastRunId, 600)
      applyRunStates(data.node_states)
      if (data.status === 'paused') {
        setRunStatus('paused')
        setPausedAt(data.paused_at || '')
        setPauseReason(data.pause_reason || 'step')
        setRunResult(`已暂停于 ${data.paused_at || '断点'}`)
      } else {
        setRunStatus(data.status)
        setRunResult(data.status === 'success' ? '执行成功' : `执行失败: ${data.error}`)
      }
    } catch (e) {
      setRunResult(`错误: ${e}`)
    } finally {
      setDebugBusy(false)
      setIsRunning(false)
    }
  }

  const stopRun = async () => {
    if (!lastRunId) return
    await fetch(`/api/runs/${lastRunId}/cancel`, { method: 'POST' })
    // 主动中止 ≠ 失败：后端写的是 error_code=cancelled。以前这里写 'failed'，
    // 界面把「我自己按停的」显示成「执行失败」，是状态谎言（审计 C 类）。
    setRunStatus('cancelled')
    setRunResult('已中止（已完成节点的结果保留，可续跑）')
    setIsRunning(false)
    toast('已中止本次运行，上游已完成的结果保留', 'info')
  }

  /** 从指定节点重跑：上游输出保持不变，只重算该节点及其下游 */
  const rerunFrom = async (nodeId: string) => {
    setContextMenu(null)
    if (!lastRunId) {
      setRunResult('先完整运行一次，再从节点重跑')
      return
    }
    setRunStatus('running')
    setRunResult(`从 ${nodeId} 重跑中…`)
    try {
      const resp = await fetch(`/api/runs/${lastRunId}/rerun-from`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ node_id: nodeId }),
      })
      if (!resp.ok) {
        const d = await resp.json().catch(() => ({}))
        setRunResult(d.error || '重跑失败')
        setRunStatus('failed')
        return
      }
      const data = await pollRun(lastRunId)
      applyRunStates(data.node_states)
      setRunStatus(data.status)
      setRunResult(data.status === 'success' ? '重跑完成' : `重跑失败: ${data.error}`)
    } catch (e) {
      setRunResult(`错误: ${e}`)
    }
  }

  /**
   * 依赖体检（P3-B）：只体检不运行，把当前画布送后端核一遍依赖。
   * 返回的不只是「缺什么」，还有每项能不能一键拉起、该拉哪个。
   */
  const runPreflight = async () => {
    setMoreOpen(false)
    setPreflightChecking(true)
    try {
      const resp = await fetch('/api/preflight', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ nodes: nodes, edges: edges }),
      })
      const data = await resp.json()
      if (!resp.ok) {
        setRunResult(data.error || '体检失败')
        return
      }
      setPreflightResult(data.missing || [])
      if ((data.count || 0) === 0) {
        setRunResult('依赖体检通过：当前画布需要的服务都已就绪')
        toast('依赖体检通过', 'success')
      } else {
        setRunResult(`依赖体检：${data.count} 项未就绪（${data.fixable} 项可一键修复）`)
      }
    } catch (e) {
      setRunResult(`依赖体检失败: ${e}`)
    } finally {
      setPreflightChecking(false)
    }
  }

  /** 一键修复：拉起体检报告里可修的那一项（复用依赖面板的启动接口） */
  const fixDep = async (dep: string) => {
    toast(`正在启动 ${dep}…`, 'info')
    try {
      const resp = await fetch(`/api/deps/${dep}/start`, { method: 'POST' })
      if (!resp.ok) {
        toast(`启动 ${dep} 失败`, 'error')
        return
      }
      toast(`${dep} 启动中，约 20–40 秒后再次体检确认`, 'success')
    } catch (e) {
      toast(`启动 ${dep} 失败: ${e}`, 'error')
    }
  }

  /** 切换断点：红点由 CSS 画，状态存进节点 data（保存后依然生效） */
  const toggleBreakpoint = (nodeId: string) => {
    pushHistory()
    setNodes((nds) =>
      nds.map((n) =>
        n.id === nodeId ? { ...n, data: { ...n.data, breakpoint: !n.data?.breakpoint } } : n,
      ),
    )
    setContextMenu(null)
  }

  /** 自动布局：分层拓扑排版，反复执行结果稳定 */
  const doAutoLayout = () => {
    pushHistory()
    setNodes((nds) => autoLayout(nds, edges))
    setRunResult('已重新布局')
  }

  /** 把选中节点装进一个分组框 */
  const groupSelection = () => {
    const picked = nodes.filter((n) => n.selected && !CANVAS_ONLY_TYPES.has(n.type ?? '') && !n.parentId)
    if (picked.length < 2) {
      setRunResult('先框选至少两个节点再分组')
      return
    }
    const box = boundingBox(nodes, picked.map((n) => n.id))
    if (!box) return
    pushHistory()
    const groupId = nextId('group', new Set(nodes.map((n) => n.id)))
    const ids = new Set(picked.map((n) => n.id))
    // 父节点必须排在子节点前面，否则 React Flow 计算位置会错乱
    const groupNode: Node = {
      id: groupId,
      type: 'group',
      position: { x: box.x, y: box.y },
      data: { label: '分组' },
      style: { width: box.width, height: box.height },
      zIndex: -1,
    }
    setNodes((nds) => [
      groupNode,
      ...nds.map((n) =>
        ids.has(n.id)
          ? {
              ...n,
              parentId: groupId,
              extent: 'parent' as const,
              position: { x: n.position.x - box.x, y: n.position.y - box.y },
            }
          : n,
      ),
    ])
    setRunResult('已分组（拖动框可整体移动）')
  }

  /** 解散分组：把子节点坐标从相对转回全局 */
  const ungroup = (groupId: string) => {
    const group = nodes.find((n) => n.id === groupId)
    if (!group) return
    pushHistory()
    setNodes((nds) =>
      nds
        .filter((n) => n.id !== groupId)
        .map((n) =>
          n.parentId === groupId
            ? {
                ...n,
                parentId: undefined,
                extent: undefined,
                position: { x: n.position.x + group.position.x, y: n.position.y + group.position.y },
              }
            : n,
        ),
    )
    setContextMenu(null)
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
    onSave: saveWorkflow,
    onUndo: undo,
    onRedo: redo,
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

  // 命令面板条目：节点 / 模板 / 面板 / 动作
  const buildCommands = (): CommandItem[] => {
    const list: CommandItem[] = []
    PALETTE.forEach((sec) =>
      sec.items.forEach((it) =>
        list.push({
          id: `node-${it.type}`,
          label: `添加节点：${it.label}`,
          icon: it.icon,
          section: '节点',
          run: () => addNode(it.type),
        }),
      ),
    )
    // 模板按「置顶 > 频次 > 成熟度」排，命令面板里也保持同一顺序
    tplEntries.forEach((e) =>
      list.push({
        id: `tpl-${e.tpl.id}`,
        label: `套用模板：${e.tpl.name}`,
        hint: `${groupOf(e.meta.group).label} · ${e.meta.maturity === 'stable' ? '稳定' : '试用'} · ${
          e.meta.useCase || e.tpl.description
        }`,
        icon: '▦',
        section: (e.meta.pinned || 0) > 0 ? '模板 · 置顶' : '模板',
        run: () => loadTemplate(e.tpl),
      }),
    )
    list.push({
      id: 'act-tpl-lib',
      label: '打开模板库',
      hint: '分组 / 搜索 / 置顶',
      icon: '▦',
      section: '面板',
      run: () => setTplLibOpen(true),
    })
    list.push(
      { id: 'panel-kb', label: '打开知识库', icon: '⌕', section: '面板', run: () => setKbOpen(true) },
      { id: 'panel-env', label: '查看运行环境', icon: '◍', section: '面板', run: () => setEnvOpen(true) },
      { id: 'panel-trigger', label: '配置触发器', icon: '⚡', section: '面板', run: () => setTriggerOpen(true) },
      { id: 'panel-gallery', label: '打开产物画廊', icon: '▣', section: '面板', run: () => setGalleryOpen(true) },
      { id: 'panel-cred', label: '管理凭据', icon: '🔑', section: '面板', run: () => setCredOpen(true) },
      { id: 'act-run', label: '运行工作流', hint: 'Ctrl+Enter', icon: '▶', section: '动作', run: () => runWorkflow() },
      { id: 'act-save', label: '保存工作流', icon: '▤', section: '动作', run: () => saveWorkflow() },
      { id: 'act-layout', label: '自动布局', icon: '⌗', section: '动作', run: () => doAutoLayout() },
      { id: 'act-theme', label: '切换深色 / 浅色主题', icon: '☾', section: '动作', run: () => setTheme((t) => (t === 'dark' ? 'light' : 'dark')) },
    )
    return list
  }

  // 断点 / 暂停态是纯视觉标记，挂在节点 wrapper 的 className 上
  const displayNodes = useMemo(
    () =>
      nodes.map((n) => ({
        ...n,
        className: [
          n.data?.breakpoint ? 'has-breakpoint' : '',
          pausedAt && pausedAt === n.id ? 'is-paused' : '',
        ]
          .filter(Boolean)
          .join(' '),
      })),
    [nodes, pausedAt],
  )

  // 右键菜单项：随目标节点类型变化（分组要能解散，批注没有断点）
  const contextMenuItems = () => {
    const cm = contextMenu
    if (!cm) return []
    if (cm.nodeId) {
      const node = nodes.find((n) => n.id === cm.nodeId)
      const isGroup = node?.type === 'group'
      const isNote = node?.type === 'note'
      const items: { label: string; icon?: string; danger?: boolean; onClick: () => void }[] = []
      if (!isGroup) {
        items.push({ label: '复制节点', icon: '📋', onClick: () => duplicateNode(cm.nodeId!) })
      }
      if (!isGroup && !isNote) {
        items.push({
          label: node?.data?.breakpoint ? '取消断点' : '设置断点',
          icon: '🔴',
          onClick: () => toggleBreakpoint(cm.nodeId!),
        })
        items.push({
          label: '从该节点重跑',
          icon: '⟲',
          onClick: () => rerunFrom(cm.nodeId!),
        })
      }
      if (isGroup) {
        items.push({ label: '解散分组', icon: '⧈', onClick: () => ungroup(cm.nodeId!) })
      }
      items.push({ label: '删除节点', icon: '🗑️', danger: true, onClick: () => deleteNode(cm.nodeId!) })
      return items
    }
    return [
      { label: '添加批注', icon: '✎', onClick: () => { addNode('note'); setContextMenu(null) } },
      { label: '添加开始节点', icon: '▶️', onClick: () => { addNode('start'); setContextMenu(null) } },
      { label: '添加 LLM 节点', icon: '🤖', onClick: () => { addNode('llm'); setContextMenu(null) } },
      { label: '自动布局', icon: '⌗', onClick: () => { doAutoLayout(); setContextMenu(null) } },
      { label: '清空画布', icon: '🗑️', danger: true, onClick: () => { setNodes([]); setEdges([]); setContextMenu(null) } },
    ]
  }

  return (
    <div className="flex flex-col h-screen w-screen">
      {/* 全局轻提示 / 确认框（替代原生 alert、confirm） */}
      <ToastHost />
      <div className="flex flex-1 min-h-0">
      {/* 左侧节点库 */}
      <div className="w-60 border-r border-border bg-surface p-4 flex flex-col gap-2 overflow-y-auto">
        <div className="flex items-center justify-between mb-2">
          <h3 className="text-sm font-semibold text-gray-300">节点库</h3>
          <button
            onClick={() => setCmdOpen(true)}
            title="命令面板（Ctrl/Cmd + K）"
            className="text-[10px] text-gray-500 hover:text-white border border-border rounded px-1.5 py-0.5 font-mono"
          >
            ⌘K
          </button>
        </div>

        <NodePalette
          envReady={envReady}
          usage={nodeUsage}
          onAdd={addNode}
          onDragStart={onDragStart}
        />

        {/* 模板：左栏只放置顶的几条，全量在模板库面板里 */}
        <div className="flex items-center justify-between mt-3 mb-1">
          <div className="fe-section-label">模板</div>
          <button
            onClick={() => setTplLibOpen(true)}
            className="text-[10px] text-gray-500 hover:text-white transition-colors"
            title="打开模板库：分组、搜索、置顶"
          >
            全部 {tplEntries.length} →
          </button>
        </div>
        {tplTop.map((e) => (
          <button
            key={e.tpl.id}
            onClick={() => loadTemplate(e.tpl)}
            title={e.meta.pinNote || e.meta.useCase || e.tpl.description}
            className="w-full text-left px-2.5 py-1.5 fe-palette-item mb-1"
          >
            <div className="flex items-center gap-1.5">
              <span className="text-[11.5px] font-medium text-white truncate flex-1">
                {e.tpl.name}
              </span>
              <span className={`fe-tag ${e.meta.maturity === 'stable' ? 'fe-tag-ok' : 'fe-tag-beta'}`}>
                {e.meta.maturity === 'stable' ? '稳定' : '试用'}
              </span>
            </div>
            <div className="text-[10px] fe-dim truncate mt-0.5">
              {e.meta.useCase || e.tpl.description}
            </div>
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
        <div className="fe-topbar flex items-center px-3.5 gap-2">
          {/* 品牌 + 当前图名 */}
          <div className="flex items-center gap-2.5 mr-1 shrink-0">
            <div className="fe-logo-mark">
              <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
                <path d="M9 1L3 9h4l-1 6 6-8H8l1-6z" fill="white" />
              </svg>
            </div>
            <div className="leading-tight">
              <h1 className="fe-title text-[14px] font-bold">FlowEdge</h1>
              <div className="text-[9.5px] fe-dim3 -mt-px">多模态工作流</div>
            </div>
          </div>

          <div className="fe-vsep" />

          <input
            value={workflowName}
            onChange={(e) => setWorkflowName(e.target.value)}
            className="px-2.5 py-1.5 rounded-md bg-[#0A0E14] border border-border text-[12.5px] w-44 focus:border-primary outline-none shrink-0"
            placeholder="工作流名称"
          />

          <span className={`fe-pill ${wsConnected ? 'is-online' : 'is-offline'}`}>
            <span
              className={`w-1.5 h-1.5 rounded-full ${wsConnected ? 'bg-success' : 'bg-gray-500'}`}
            />
            {wsConnected ? '实时' : '离线'}
          </span>

          {runResult && (
            <span className="text-[11px] fe-dim truncate max-w-[300px]" title={runResult}>
              {runResult}
            </span>
          )}

          {/* 依赖体检报告：每项 FAIL 都带可执行修复动作，可修的直接给一键拉起 */}
          {preflightResult.length > 0 && (
            <div className="fe-preflight">
              <div className="fe-preflight-hd">
                <span>依赖未就绪 {preflightResult.length} 项</span>
                <button onClick={() => setPreflightResult([])} title="关闭">✕</button>
              </div>
              <ul className="fe-preflight-list">
                {preflightResult.map((m, i) => (
                  <li key={i}>
                    <div className="fp-title">
                      {m.label}
                      <span className="fp-node">节点 {m.node_id}</span>
                    </div>
                    {m.hint && <div className="fp-hint">{m.hint}</div>}
                    {m.fix && <div className="fp-fix">→ {m.fix}</div>}
                    {m.fixable && m.fix_dep && (
                      <button className="fp-btn" onClick={() => fixDep(m.fix_dep)}>
                        一键修复
                      </button>
                    )}
                  </li>
                ))}
              </ul>
            </div>
          )}

          <div className="ml-auto flex items-center gap-1.5">
            {/* 一级：高频文件与内容操作 */}
            <button onClick={saveWorkflow} className="fe-tb" title="保存到本地库">
              保存
            </button>
            <button
              onClick={() => setKbOpen(true)}
              className="fe-tb"
              title="项目专属语义知识库"
            >
              知识库
            </button>
            <button
              onClick={() => setGalleryOpen(true)}
              className="fe-tb"
              title="浏览生成的图片 / 视频 / 音频"
            >
              产物
            </button>

            <div className="fe-vsep" />

            {/* 二级：收进「更多」，需要时展开 */}
            <div className="fe-menu-wrap">
              <button
                onClick={() => setMoreOpen((v) => !v)}
                className={`fe-tb ${moreOpen ? 'is-on' : ''}`}
                title="更多操作"
              >
                ⋯
              </button>
              {moreOpen && (
                <>
                  <div className="fixed inset-0 z-[55]" onClick={() => setMoreOpen(false)} />
                  <div className="fe-menu">
                    <button
                      onClick={() => {
                        setTplLibOpen(true)
                        setMoreOpen(false)
                      }}
                    >
                      <span>模板库</span>
                      <span className="mi-k">{tplEntries.length}</span>
                    </button>
                    <button
                      onClick={() => {
                        setTriggerOpen(true)
                        setMoreOpen(false)
                      }}
                    >
                      触发 · Webhook / 定时
                    </button>
                    <button
                      onClick={runPreflight}
                      disabled={preflightChecking}
                    >
                      {preflightChecking ? '体检中…' : '依赖体检（不运行）'}
                    </button>
                    <button
                      onClick={() => {
                        setEnvOpen(true)
                        setMoreOpen(false)
                      }}
                    >
                      环境自检
                    </button>
                    <button
                      onClick={() => {
                        setCredOpen(true)
                        setMoreOpen(false)
                      }}
                    >
                      凭据管理
                    </button>
                    <div className="mi-sep" />
                    <button
                      onClick={() => {
                        exportWorkflow()
                        setMoreOpen(false)
                      }}
                    >
                      导出 JSON
                    </button>
                    <label className="fe-menu-item">
                      导入 JSON
                      <input
                        type="file"
                        accept=".json"
                        onChange={(e) => {
                          importWorkflow(e)
                          setMoreOpen(false)
                        }}
                        className="hidden"
                      />
                    </label>
                    <div className="mi-sep" />
                    <button
                      onClick={() => {
                        doAutoLayout()
                        setMoreOpen(false)
                      }}
                    >
                      自动布局
                    </button>
                    <button
                      onClick={() => {
                        groupSelection()
                        setMoreOpen(false)
                      }}
                    >
                      分组选中节点
                    </button>
                    <button
                      onClick={() => {
                        setTheme((t) => (t === 'dark' ? 'light' : 'dark'))
                        setMoreOpen(false)
                      }}
                    >
                      {theme === 'dark' ? '切到浅色主题' : '切到深色主题'}
                    </button>
                  </div>
                </>
              )}
            </div>

            <div className="fe-vsep" />

            <DebugPanel
              debugMode={debugMode}
              runId={lastRunId}
              runStatus={runStatus}
              pausedAt={pausedAt}
              pauseReason={pauseReason}
              breakpointCount={nodes.filter((n) => n.data?.breakpoint).length}
              busy={debugBusy}
              onStartDebug={() => {
                setDebugMode(true)
                setRunResult('调试模式：在节点上右键可加断点，运行后停在你关心的地方')
              }}
              onStep={() => advanceRun('step')}
              onContinue={() => advanceRun('continue')}
              onStop={stopRun}
            />

            {canResume && !isRunning && (
              <button
                onClick={resumeWorkflow}
                className="fe-tb"
                title="复用已完成节点的输出，只重跑失败部分"
              >
                ⟲ 续跑
              </button>
            )}

            {isRunning && liveProgress && liveProgress.elapsedMs > 0 && (
              <span
                className="px-2 py-1 rounded-lg text-xs font-mono bg-canvas border border-border text-gray-300"
                title={`节点 ${liveProgress.nodeId} 实时进度`}
              >
                已用 {fmtDuration(liveProgress.elapsedMs)}
                {liveProgress.etaMs > 0 ? ` · 预计 ${fmtDuration(liveProgress.etaMs)}` : ''}
              </span>
            )}

            <button
              onClick={runWorkflow}
              disabled={isRunning}
              className={`fe-run-btn ${isRunning ? 'is-going' : ''}`}
            >
              {isRunning ? '执行中…' : '▶ 运行'}
            </button>
          </div>
        </div>

        {/* 画布 */}
        <div className="flex-1 flex flex-col">
          <div className="flex-1 relative">
            {/* 空状态引导 */}
            {nodes.length === 0 && (
              <div className="absolute inset-0 z-10 flex flex-col items-center justify-center pointer-events-none">
                <div className="pointer-events-auto flex flex-col items-center gap-3 px-8 py-7 rounded-2xl border border-border/70 bg-surface/60 backdrop-blur-sm">
                  <div className="fe-logo-mark" style={{ width: 40, height: 40, borderRadius: 12 }}>
                    <svg width="20" height="20" viewBox="0 0 16 16" fill="none">
                      <path d="M9 1L3 9h4l-1 6 6-8H8l1-6z" fill="white" />
                    </svg>
                  </div>
                  <p className="text-sm text-white font-medium">画布是空的</p>
                  <p className="text-[11px] text-gray-500 -mt-1">从左侧拖一个节点，或直接套用模板</p>
                  <div className="flex items-center gap-2 mt-1">
                    <button
                      onClick={() => setCmdOpen(true)}
                      className="fe-btn-primary px-3 py-1.5 rounded-lg text-xs"
                    >
                      打开命令面板
                    </button>
                    <button
                      onClick={() => addNode('start')}
                      className="fe-btn-ghost px-3 py-1.5 rounded-lg text-xs"
                    >
                      添加开始节点
                    </button>
                  </div>
                </div>
              </div>
            )}

            <ReactFlow
              nodes={displayNodes}
              edges={edges}
              onNodesChange={onNodesChange}
              onEdgesChange={onEdgesChange}
              onConnect={onConnect}
              isValidConnection={isValidConnection}
              snapToGrid
              snapGrid={[16, 16]}
              onNodeDragStop={pushHistory}
              selectionOnDrag
              multiSelectionKeyCode={['Control', 'Meta']}
              deleteKeyCode={['Delete', 'Backspace']}
              minZoom={0.2}
              maxZoom={2.5}
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
              <Background color={CANVAS_THEME[theme].dot} gap={22} size={1.2} />
              <Controls />
              <MiniMap
                pannable
                zoomable
                bgColor={CANVAS_THEME[theme].minimapBg}
                nodeColor={CANVAS_THEME[theme].minimapNode}
                maskColor={CANVAS_THEME[theme].minimapMask}
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
          items={contextMenuItems()}
        />
      )}

      {/* 项目知识库面板 */}
      <KBPanel open={kbOpen} onClose={() => setKbOpen(false)} />

      {/* 模板库：分组 / 搜索 / 置顶 */}
      <TemplateLibrary
        open={tplLibOpen}
        onClose={() => setTplLibOpen(false)}
        onApply={loadTemplate}
      />

      {/* 运行环境自检面板 */}
      {envOpen && <CapabilityPanel onClose={() => setEnvOpen(false)} />}

      {/* 触发器面板 */}
      {triggerOpen && <TriggerPanel workflowId={currentWorkflowId} onClose={() => setTriggerOpen(false)} />}

      {/* 产物画廊 */}
      {galleryOpen && <GalleryPanel onClose={() => setGalleryOpen(false)} />}

      {/* 凭据管理 */}
      {credOpen && <CredentialPanel onClose={() => setCredOpen(false)} />}

      {/* 命令面板 */}
      {cmdOpen && <CommandPalette items={buildCommands()} onClose={() => setCmdOpen(false)} />}

      {/* 右侧属性面板 */}
      <div className="w-72 border-l border-border bg-surface p-4 overflow-y-auto">
        {/* Tab 切换：此前三个按钮都没有 onClick，「输出」「日志」实际是死按钮 */}
        <div className="flex border-b border-border mb-4">
          {(
            [
              ['config', '配置'],
              ['output', '输出'],
            ] as const
          ).map(([key, label]) => (
            <button
              key={key}
              onClick={() => setPropTab(key)}
              className={`px-3 py-2 text-xs font-medium border-b-2 transition-colors ${
                propTab === key
                  ? 'text-primary border-primary'
                  : 'text-gray-500 hover:text-gray-300 border-transparent'
              }`}
            >
              {label}
            </button>
          ))}
        </div>

        {/* 输出 Tab：完整输出与错误（P1 事件日志就绪后再接回「日志」Tab） */}
        {propTab === 'output' ? (
          selectedNode ? (
            <div className="flex flex-col gap-3">
              <label className="text-xs text-gray-400 block">上一次运行的输出</label>
              {selectedNode.data?.output != null ? (
                <pre className="text-[11px] text-gray-300 font-mono bg-canvas border border-border rounded-lg p-2 whitespace-pre-wrap break-all max-h-[60vh] overflow-auto">
                  {typeof selectedNode.data.output === 'string'
                    ? selectedNode.data.output
                    : JSON.stringify(selectedNode.data.output, null, 2)}
                </pre>
              ) : (
                <p className="text-xs text-gray-500">暂无输出，运行工作流后显示</p>
              )}
              {selectedNode.data?.error != null && (
                <ErrorNotice
                  code={selectedNode.data?.error_code as string | undefined}
                  message={String(selectedNode.data.error)}
                  onAction={(a) => {
                    if (a === 'open-env') setEnvOpen(true)
                  }}
                />
              )}
            </div>
          ) : (
            <p className="text-xs text-gray-500">点击画布中的节点查看输出</p>
          )
        ) : selectedNode ? (
          <div className="flex flex-col gap-4">
            <div>
              <label className="text-xs text-gray-400 block mb-1">节点类型</label>
              <p className="text-sm text-white">{selectedNode.type}</p>
            </div>

            {/* 可用变量：列出上游节点输出，点击复制引用写法 */}
            {(() => {
              const ups = nodes.filter((n) =>
                edges.some((e) => e.target === selectedNode.id && e.source === n.id),
              )
              if (ups.length === 0) return null
              return (
                <div className="mb-3 pb-3 border-b border-border">
                  <label className="text-[10px] text-gray-500 uppercase tracking-wider block mb-1.5">
                    可用变量（点击复制）
                  </label>
                  <div className="flex flex-wrap gap-1">
                    {ups.flatMap((u) =>
                      ['text', ...(NODE_OUTPUT_FIELDS[u.type as string] || [])]
                        .filter((f, i, arr) => arr.indexOf(f) === i)
                        .map((f) => (
                          <button
                            key={`${u.id}.${f}`}
                            onClick={async () => {
                              const ref = `{{${u.id}.${f}}}`
                              try {
                                await navigator.clipboard.writeText(ref)
                                setRunResult(`已复制 ${ref}`)
                              } catch {
                                setRunResult(ref)
                              }
                            }}
                            title={`${u.id} (${u.type}) 的 ${f} 输出`}
                            className="text-[9px] px-1.5 py-0.5 rounded bg-primary/10 text-indigo-300 font-mono hover:bg-primary/25 transition-all"
                          >
                            {`{{${u.id}.${f}}}`}
                          </button>
                        )),
                    )}
                  </div>
                  <p className="text-[10px] text-gray-600 mt-1">上游节点完成后即可引用，未完成时保留原文</p>
                </div>
              )
            })()}

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

            {/* 代码执行节点：Go 子集沙箱（yaegi 解释执行，stdlib 白名单） */}
            {selectedNode.type === 'code' && (
              <>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">
                    Go 代码（把结果赋给 result）
                  </label>
                  <textarea
                    value={(selectedNode.data.code as string) || ''}
                    onChange={(e) => updateSelectedNodeData('code', e.target.value)}
                    className="w-full h-52 px-3 py-2 rounded-lg bg-canvas border border-border text-[11px] font-mono resize-y focus:border-primary outline-none"
                    placeholder="result = strings.ToUpper(inputText)  // 可用变量：input、inputText"
                  />
                  <p className="text-[10px] text-gray-600 mt-1 leading-relaxed">
                    可用变量：<code className="text-data">input</code>（上游输出
                    JSON）、<code className="text-data">inputText</code>（上游文本）。可用包：
                    strings、encoding/json、fmt、sort、strconv、math、time、regexp、bytes、
                    sha256、path。禁止 import 危险包（os/exec、net、syscall 等）。
                  </p>
                </div>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">超时（毫秒）</label>
                  <input
                    type="number"
                    value={(selectedNode.data.timeout_ms as number) ?? 30000}
                    onChange={(e) =>
                      updateSelectedNodeData('timeout_ms', Number(e.target.value) || 0)
                    }
                    className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none"
                    placeholder="30000"
                  />
                  <p className="text-[10px] text-gray-600 mt-1">
                    yaegi 无法硬中断死循环，超时只会放弃等待，默认 30 秒。
                  </p>
                </div>
              </>
            )}

            {/* 知识库检索：此前属性面板完全没有配置项，靠默认值盲跑 */}
            {selectedNode.type === 'rag_query' && (
              <>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">检索问题</label>
                  <textarea
                    value={(selectedNode.data.query as string) || ''}
                    onChange={(e) => updateSelectedNodeData('query', e.target.value)}
                    className="w-full h-20 px-3 py-2 rounded-lg bg-canvas border border-border text-sm resize-none focus:border-primary outline-none"
                    placeholder="留空则使用上游输入..."
                  />
                </div>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">返回条数 top_k</label>
                  <input
                    type="number"
                    min={1}
                    max={10}
                    value={(selectedNode.data.top_k as number) ?? 3}
                    onChange={(e) =>
                      updateSelectedNodeData('top_k', Number(e.target.value) || 3)
                    }
                    className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none"
                  />
                </div>
              </>
            )}

            {selectedNode.type === 'llm' && (
              <>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">模型</label>
                  <input
                    value={(selectedNode.data.model as string) || ''}
                    onChange={(e) => updateSelectedNodeData('model', e.target.value)}
                    className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-primary outline-none"
                    placeholder="留空则用自动探测到的模型"
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

                <div>
                  <label className="text-xs text-gray-400 block mb-1">工具调用（可选）</label>
                  <div className="flex flex-wrap gap-1.5">
                    {LLM_TOOL_OPTIONS.map((tool) => {
                      const cur = ((selectedNode.data.tools as string[]) || [])
                      const on = cur.includes(tool.id)
                      return (
                        <button
                          key={tool.id}
                          onClick={() =>
                            updateSelectedNodeData(
                              'tools',
                              on ? cur.filter((x) => x !== tool.id) : [...cur, tool.id],
                            )
                          }
                          className={`px-2 py-1 rounded-md text-[11px] border transition-all ${
                            on
                              ? 'bg-ai/20 border-ai/60 text-white'
                              : 'bg-canvas border-border text-gray-400 hover:border-ai/40'
                          }`}
                        >
                          {tool.label}
                        </button>
                      )
                    })}
                  </div>
                  <p className="text-[10px] text-gray-500 mt-1">
                    勾选后模型可自行决定调用（最多 4 轮），全部在本地执行
                  </p>
                </div>
              </>
            )}

            {selectedNode.type === 'text2image' && (
              <div className="flex flex-col gap-2">
                <div>
                  <label className="text-xs text-gray-400 block mb-1">
                    批量提示词（一行一条，留空用上游/单条）
                  </label>
                  <textarea
                    rows={4}
                    value={(selectedNode.data.prompts as string) || ''}
                    onChange={(e) => updateSelectedNodeData('prompts', e.target.value)}
                    placeholder={'分镜1：雪山顶的白鹿，远景\n分镜2：鹿的特写，逆光\n分镜3：空镜，只留雪与天'}
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y font-mono"
                  />
                </div>
                <label className="flex items-center gap-2 text-[11px] text-gray-400">
                  <input
                    type="checkbox"
                    checked={Boolean(selectedNode.data.split_lines)}
                    onChange={(e) => updateSelectedNodeData('split_lines', e.target.checked)}
                  />
                  把上游文本按行拆成多条（接 LLM 拆分镜时用）
                </label>
                <p className="text-[10px] text-gray-500 leading-relaxed">
                  多条提示词会在**同一次模型加载**里跑完，比逐条省掉每条的冷启动。
                </p>
              </div>
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
                <label className="flex items-center gap-2 text-[11px] text-gray-400">
                  <input
                    type="checkbox"
                    checked={selectedNode.data.auto_translate === true}
                    onChange={(e) => updateSelectedNodeData('auto_translate', e.target.checked)}
                  />
                  检测到中文时自动过本地 LLM 译成英文（flux 对中文弱）
                </label>
                <p className="text-[10px] text-gray-500 leading-relaxed">
                  flux 的文本编码器以英文为主：纯中文提示词经常只画出「通用风景」。
                  勾选后由本地 LLM 在进模型前翻译，翻译失败会自动降级用原文并在输出里留痕
                  （<code>translated</code> / <code>translate_error</code>）。
                </p>
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

            {(selectedNode.type === 'sub_workflow' || selectedNode.type === 'loop') && (
              <>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">引用工作流</label>
                  <select
                    value={(selectedNode.data.workflow_id as number) ?? ''}
                    onChange={(e) => {
                      const id = e.target.value ? Number(e.target.value) : undefined
                      const wf = savedWorkflows.find((w) => w.id === id)
                      updateSelectedNodeData('workflow_id', id)
                      updateSelectedNodeData('workflow_name', wf?.name || '')
                    }}
                    className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-warning outline-none"
                  >
                    <option value="">选择工作流...</option>
                    {savedWorkflows
                      .filter((w) => w.id !== currentWorkflowId)
                      .map((w) => (
                        <option key={w.id} value={w.id}>
                          {w.name} (#{w.id})
                        </option>
                      ))}
                  </select>
                  {!selectedNode.data.workflow_id && (
                    <p className="text-[10px] text-warning mt-1">需先保存目标工作流，再回到此处选择</p>
                  )}
                </div>

                {selectedNode.type === 'loop' && (
                  <>
                    <div>
                      <label className="text-xs text-gray-400 block mb-1">循环模式</label>
                      <select
                        value={(selectedNode.data.mode as string) || 'count'}
                        onChange={(e) => updateSelectedNodeData('mode', e.target.value)}
                        className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-warning outline-none"
                      >
                        <option value="count">固定次数（每轮以上一轮输出为输入）</option>
                        <option value="foreach">遍历列表（按分隔符切分上游）</option>
                        <option value="until">条件循环（满足即停）</option>
                      </select>
                    </div>

                    {(selectedNode.data.mode || 'count') === 'count' && (
                      <div>
                        <label className="text-xs text-gray-400 block mb-1">迭代次数</label>
                        <input
                          type="number"
                          min={1}
                          value={(selectedNode.data.iterations as number) ?? 3}
                          onChange={(e) => updateSelectedNodeData('iterations', Number(e.target.value))}
                          className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-warning outline-none"
                        />
                      </div>
                    )}

                    {selectedNode.data.mode === 'foreach' && (
                      <div>
                        <label className="text-xs text-gray-400 block mb-1">分隔符</label>
                        <input
                          value={(selectedNode.data.separator as string) ?? ''}
                          onChange={(e) => updateSelectedNodeData('separator', e.target.value)}
                          className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-warning outline-none"
                          placeholder="留空按换行切分"
                        />
                      </div>
                    )}

                    {selectedNode.data.mode === 'until' && (
                      <>
                        <div>
                          <label className="text-xs text-gray-400 block mb-1">停止条件</label>
                          <select
                            value={(selectedNode.data.condition_type as string) || 'not_empty'}
                            onChange={(e) => updateSelectedNodeData('condition_type', e.target.value)}
                            className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-warning outline-none"
                          >
                            <option value="not_empty">输出不为空</option>
                            <option value="contains">输出包含</option>
                            <option value="not_contains">输出不包含</option>
                            <option value="equals">输出等于</option>
                          </select>
                        </div>
                        <div>
                          <label className="text-xs text-gray-400 block mb-1">比较值</label>
                          <input
                            value={(selectedNode.data.compare_value as string) || ''}
                            onChange={(e) => updateSelectedNodeData('compare_value', e.target.value)}
                            className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-warning outline-none"
                            placeholder="如 DONE"
                          />
                        </div>
                      </>
                    )}

                    <div>
                      <label className="text-xs text-gray-400 block mb-1">最大迭代（安全阀）</label>
                      <input
                        type="number"
                        min={1}
                        max={100}
                        value={(selectedNode.data.max_iterations as number) ?? 20}
                        onChange={(e) => updateSelectedNodeData('max_iterations', Number(e.target.value))}
                        className="w-full px-3 py-2 rounded-lg bg-canvas border border-border text-sm focus:border-warning outline-none"
                      />
                      <p className="text-[10px] text-gray-500 mt-1">硬上限 100，达到即中止并返回已有结果</p>
                    </div>
                  </>
                )}
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

            {selectedNode.type === 'aggregate' && (
              <div className="flex flex-col gap-3">
                <div>
                  <label className="text-xs text-gray-400 block mb-1">聚合模式</label>
                  <select
                    value={(selectedNode.data.mode as string) || 'object'}
                    onChange={(e) => updateSelectedNodeData('mode', e.target.value)}
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
                  >
                    <option value="object">按节点汇聚（产出 nodes 映射）</option>
                    <option value="list">列表（产出 values 数组）</option>
                    <option value="text">文本拼接（产出 text）</option>
                    <option value="first">取第一个上游</option>
                    <option value="last">取最后一个上游</option>
                  </select>
                </div>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">取值字段</label>
                  <input
                    value={(selectedNode.data.field as string) || 'text'}
                    onChange={(e) => updateSelectedNodeData('field', e.target.value)}
                    placeholder="text / image / audio / *（整个输出）"
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                  />
                </div>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">参与聚合的节点（可选）</label>
                  <input
                    value={(selectedNode.data.inputs as string) || ''}
                    onChange={(e) => updateSelectedNodeData('inputs', e.target.value)}
                    placeholder="留空=全部上游；填 node-a,node-b 可指定顺序"
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                  />
                </div>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">文本连接符</label>
                  <input
                    value={(selectedNode.data.separator as string) ?? '\n\n'}
                    onChange={(e) => updateSelectedNodeData('separator', e.target.value)}
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                  />
                </div>
                <label className="flex items-center gap-2 text-xs text-gray-400">
                  <input
                    type="checkbox"
                    checked={!!selectedNode.data.include_empty}
                    onChange={(e) => updateSelectedNodeData('include_empty', e.target.checked)}
                  />
                  保留空值（默认丢弃空上游）
                </label>
              </div>
            )}

            {selectedNode.type === 'note' && (
              <div className="flex flex-col gap-2">
                <label className="text-xs text-gray-400">批注内容</label>
                <textarea
                  rows={6}
                  value={(selectedNode.data.text as string) || ''}
                  onChange={(e) => updateSelectedNodeData('text', e.target.value)}
                  placeholder="写给未来的自己：这里为什么这么连？"
                  className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
                />
                <p className="text-[10px] text-gray-500">批注不参与执行，保存时也不会写进工作流定义。</p>
              </div>
            )}

            {selectedNode.type === 'tts' && (
              <div className="flex flex-col gap-3">
                <div>
                  <label className="text-xs text-gray-400 block mb-1">文本（留空取上游）</label>
                  <textarea
                    rows={3}
                    value={(selectedNode.data.text as string) || ''}
                    onChange={(e) => updateSelectedNodeData('text', e.target.value)}
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
                  />
                </div>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">语音名片段（可选）</label>
                  <input
                    value={(selectedNode.data.voice as string) || ''}
                    onChange={(e) => updateSelectedNodeData('voice', e.target.value)}
                    placeholder="如 Huihui；留空用系统默认"
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
                  />
                </div>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">语速（-10 ~ 10）</label>
                  <input
                    type="number"
                    min={-10}
                    max={10}
                    value={Number(selectedNode.data.rate ?? 0)}
                    onChange={(e) => updateSelectedNodeData('rate', Number(e.target.value) || 0)}
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
                  />
                </div>
                <p className="text-[10px] text-gray-500">
                  Windows 下走系统内置 SAPI（无需 Python 服务）；配置了 TTS_BASE_URL 时优先走服务。
                </p>
              </div>
            )}

            {selectedNode.type === 'storyboard' && (
              <div className="flex flex-col gap-3">
                <div>
                  <label className="text-xs text-gray-400 block mb-1">内容需求（留空取上游 text）</label>
                  <textarea
                    rows={4}
                    value={(selectedNode.data.brief as string) || ''}
                    onChange={(e) => updateSelectedNodeData('brief', e.target.value)}
                    placeholder="例：师徒二人在雨夜客栈对峙，师傅要废掉徒弟的修为"
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
                  />
                </div>
                <div className="grid grid-cols-2 gap-2">
                  <div>
                    <label className="text-[10px] text-gray-500 block mb-1">目标总时长（秒）</label>
                    <input
                      type="number" min={5} max={180}
                      value={Number(selectedNode.data.seconds ?? 30)}
                      onChange={(e) => updateSelectedNodeData('seconds', Number(e.target.value) || 30)}
                      className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                    />
                  </div>
                  <div>
                    <label className="text-[10px] text-gray-500 block mb-1">镜头数上限</label>
                    <input
                      type="number" min={1} max={12}
                      value={Number(selectedNode.data.max_shots ?? 6)}
                      onChange={(e) => updateSelectedNodeData('max_shots', Number(e.target.value) || 6)}
                      className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                    />
                  </div>
                </div>
                <p className="text-[10px] text-gray-500 leading-relaxed">
                  输出结构化分镜表：景别 / 角度 / 轴线 / 运镜 / 动作 / 声音 / 台词 / 衔接方式。
                  时长按内容给（3–8 秒，自动吸附 H3 的 17k+5 帧网格），不强制均分。
                </p>
              </div>
            )}

            {selectedNode.type === 'quality_gate' && (
              <div className="flex flex-col gap-3">
                <p className="text-[10px] text-gray-500 leading-relaxed">
                  对上游产物跑声明式质检，任一项 fail 级不过即阻断下游。
                  规则全部可留空（留空 = 不检）；产物取自最近一条上游的图片/音频/视频线。
                </p>
                <label className="flex items-center gap-2 text-[11px] text-gray-400">
                  <input
                    type="checkbox"
                    checked={selectedNode.data.require_audio !== false}
                    onChange={(e) => updateSelectedNodeData('require_audio', e.target.checked)}
                  />
                  必须有音轨（防 -map 漏写产出纯视频）
                </label>
                <div className="grid grid-cols-3 gap-2">
                  {([['min_frames', '最少帧数'], ['duration_min', '最短(s)'], ['duration_max', '最长(s)']] as const).map(([k, label]) => (
                    <div key={k}>
                      <label className="text-[10px] text-gray-500 block mb-1">{label}</label>
                      <input
                        type="number"
                        value={Number(selectedNode.data[k] ?? 0)}
                        onChange={(e) => updateSelectedNodeData(k, Number(e.target.value) || 0)}
                        className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                      />
                    </div>
                  ))}
                </div>
                <div className="grid grid-cols-3 gap-2">
                  {([['min_width', '最小宽'], ['min_height', '最小高'], ['duration_tolerance', '音画容差(s)']] as const).map(([k, label]) => (
                    <div key={k}>
                      <label className="text-[10px] text-gray-500 block mb-1">{label}</label>
                      <input
                        type="number"
                        step="0.01"
                        value={Number(selectedNode.data[k] ?? 0)}
                        onChange={(e) => updateSelectedNodeData(k, Number(e.target.value) || 0)}
                        className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                      />
                    </div>
                  ))}
                </div>
                <div className="grid grid-cols-2 gap-2">
                  {([['max_silence_gap', '最长静音(s)'], ['loudness_min_db', '响度下限(dB)']] as const).map(([k, label]) => (
                    <div key={k}>
                      <label className="text-[10px] text-gray-500 block mb-1">{label}</label>
                      <input
                        type="number"
                        step="0.1"
                        value={Number(selectedNode.data[k] ?? 0)}
                        onChange={(e) => updateSelectedNodeData(k, Number(e.target.value) || 0)}
                        className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                      />
                    </div>
                  ))}
                </div>
                <label className="flex items-center gap-2 text-[11px] text-gray-400">
                  <input
                    type="checkbox"
                    checked={selectedNode.data.forbid_speech === true}
                    onChange={(e) => updateSelectedNodeData('forbid_speech', e.target.checked)}
                  />
                  氛围模式（疑似有人声即 FAIL，能量法判读）
                </label>
              </div>
            )}

            {selectedNode.type === 'shotlist' && (
              <div className="flex flex-col gap-3">
                <div>
                  <label className="text-xs text-gray-400 block mb-1">分镜表 JSON（留空取上游 plan）</label>
                  <textarea
                    rows={5}
                    value={(selectedNode.data.plan as string) || ''}
                    onChange={(e) => updateSelectedNodeData('plan', e.target.value)}
                    placeholder='{"title":"…","character":"…","shots":[…]}"'
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-[10px] font-mono resize-y"
                  />
                </div>
                <div className="grid grid-cols-3 gap-2">
                  {([['width', '宽'], ['height', '高'], ['steps', '步数']] as const).map(([k, label]) => (
                    <div key={k}>
                      <label className="text-[10px] text-gray-500 block mb-1">{label}</label>
                      <input
                        type="number"
                        value={Number(selectedNode.data[k] ?? 0)}
                        onChange={(e) => updateSelectedNodeData(k, Number(e.target.value) || 0)}
                        className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                      />
                    </div>
                  ))}
                </div>
                <label className="flex items-center gap-2 text-[11px] text-gray-400">
                  <input
                    type="checkbox"
                    checked={selectedNode.data.turbo !== false}
                    onChange={(e) => updateSelectedNodeData('turbo', e.target.checked)}
                  />
                  涡轮 LoRA（8 步）
                </label>
                <label className="flex items-start gap-2 text-[11px] text-gray-400">
                  <input
                    type="checkbox"
                    className="mt-0.5"
                    checked={selectedNode.data.audio_check !== false}
                    onChange={(e) => updateSelectedNodeData('audio_check', e.target.checked)}
                  />
                  <span>
                    音频抽检（ASR 检测到人声 → 自动压低音量，消除乱语但保留环境声）
                  </span>
                </label>
                <label className="flex items-center gap-2 text-[11px] text-gray-400">
                  <input
                    type="checkbox"
                    checked={Boolean(selectedNode.data.interp)}
                    onChange={(e) => updateSelectedNodeData('interp', e.target.checked)}
                  />
                  成片后插帧到 48fps（CPU）
                </label>
                <p className="text-[10px] text-gray-500 leading-relaxed">
                  按每镜 transition 装配：cut=硬切（1 帧）、continue=末帧续接（融 1 帧）、dissolve=叠化 0.3s；
                  最后统一响度归一。
                </p>
              </div>
            )}

            {selectedNode.type === 'prompt_opt' && (
              <div className="flex flex-col gap-3">
                <div>
                  <label className="text-xs text-gray-400 block mb-1">原始想法（留空取上游 text）</label>
                  <textarea
                    rows={4}
                    value={(selectedNode.data.text as string) || ''}
                    onChange={(e) => updateSelectedNodeData('text', e.target.value)}
                    placeholder="例：雪山上的白鹿，很美，有大片留白"
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
                  />
                </div>
                <div className="grid grid-cols-2 gap-2">
                  <div>
                    <label className="text-[10px] text-gray-500 block mb-1">目标时长（秒）</label>
                    <input
                      type="number" min={2} max={15}
                      value={Number(selectedNode.data.seconds ?? 5)}
                      onChange={(e) => updateSelectedNodeData('seconds', Number(e.target.value) || 5)}
                      className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                    />
                  </div>
                  <div>
                    <label className="text-[10px] text-gray-500 block mb-1">优化目标</label>
                    <select
                      value={(selectedNode.data.mode as string) || 'h3'}
                      onChange={(e) => updateSelectedNodeData('mode', e.target.value)}
                      className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                    >
                      <option value="h3">MiniMax H3（音视频联合）</option>
                    </select>
                  </div>
                </div>
                <p className="text-[10px] text-gray-500 leading-relaxed">
                  按官方指南改写：单一主导动作 + 可见收尾 + 一个主运镜 + ≤2 层声音（绑定可见事件）。
                  优化失败时自动透传原文，不中断工作流。
                </p>
              </div>
            )}

            {selectedNode.type === 'scene_grid' && (() => {
              const arr = String(selectedNode.data.cells || '').split('\n')
              const at = (i: number) => arr[i] ?? ''
              const setAt = (i: number, v: string) => {
                const next = Array.from({ length: 9 }, (_, k) => (k === i ? v : (arr[k] ?? '')))
                updateSelectedNodeData('cells', next.join('\n'))
              }
              const labels = ['左上', '上中', '右上', '左中', '中心', '右中', '左下', '下中', '右下']
              return (
                <div className="flex flex-col gap-3">
                  <div>
                    <label className="text-xs text-gray-400 block mb-1">
                      场景想法（填了由 LLM 拆九格；只填格位则不花 LLM）
                    </label>
                    <textarea
                      rows={2}
                      value={(selectedNode.data.idea as string) || ''}
                      onChange={(e) => updateSelectedNodeData('idea', e.target.value)}
                      placeholder="例：破败山门内的石殿，唯一光源是左中的铁灯，地面积水"
                      className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
                    />
                  </div>
                  <div>
                    <label className="text-[10px] text-gray-500 block mb-1">
                      九宫格（英文；写材质/颜色/形状/位置，不写形容词，用 - 留空）
                    </label>
                    <div className="grid grid-cols-3 gap-1">
                      {labels.map((lb, i) => (
                        <div key={lb} className="relative">
                          <span className="absolute left-1 top-0.5 text-[9px] text-gray-600 pointer-events-none">{lb}</span>
                          <textarea
                            rows={2}
                            value={at(i)}
                            onChange={(e) => setAt(i, e.target.value)}
                            className="w-full pt-3 px-1 pb-1 rounded bg-canvas border border-border text-[10px] leading-snug resize-none"
                          />
                        </div>
                      ))}
                    </div>
                  </div>
                  <div className="grid grid-cols-2 gap-2">
                    <div>
                      <label className="text-[10px] text-gray-500 block mb-1">光位（方向+色温+硬度）</label>
                      <input
                        value={(selectedNode.data.light as string) || ''}
                        onChange={(e) => updateSelectedNodeData('light', e.target.value)}
                        placeholder="a lone lantern at middle-left, 2700K, hard"
                        className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-[10px]"
                      />
                    </div>
                    <div>
                      <label className="text-[10px] text-gray-500 block mb-1">主色关系</label>
                      <input
                        value={(selectedNode.data.palette as string) || ''}
                        onChange={(e) => updateSelectedNodeData('palette', e.target.value)}
                        placeholder="cold grey stone, rust-red cloth"
                        className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-[10px]"
                      />
                    </div>
                  </div>
                  <div>
                    <label className="text-[10px] text-gray-500 block mb-1">机位（高度 + 焦段，可选）</label>
                    <input
                      value={(selectedNode.data.depth as string) || ''}
                      onChange={(e) => updateSelectedNodeData('depth', e.target.value)}
                      placeholder="eye level, 35mm equivalent"
                      className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-[10px]"
                    />
                  </div>
                  <p className="text-[10px] text-gray-500 leading-relaxed">
                    九格编译成一句 SCENE LOCK，逐字注入每段提示词 —— 场景一致靠的是同一批事实出现在同一空间位置，
                    不是形容词更多。下游文生视频分段（segments&gt;1）会逐段重注并校验字节全等，漂移直接进 prompt_warnings。
                  </p>
                </div>
              )
            })()}

            {(selectedNode.type === 'image2image' || selectedNode.type === 'inpaint' || selectedNode.type === 'upscale' || selectedNode.type === 'text2video') && (
              <div className="flex flex-col gap-3">
                {selectedNode.type !== 'upscale' && (
                  <div>
                    <label className="text-xs text-gray-400 block mb-1">提示词</label>
                    <textarea
                      rows={3}
                      value={(selectedNode.data.prompt as string) || ''}
                      onChange={(e) => updateSelectedNodeData('prompt', e.target.value)}
                      placeholder={selectedNode.type === 'text2video' ? '描述镜头与动作，如 slow push in, wind in the grass' : '改写成什么'}
                      className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
                    />
                  </div>
                )}
                {(selectedNode.type !== 'text2video' || selectedNode.data.backend === 'h3') && (
                  <div>
                    <label className="text-xs text-gray-400 block mb-1">
                      {selectedNode.type === 'text2video' ? '首帧图片（留空=文生视频，连图=图生视频）' : '输入图片（留空取上游）'}
                    </label>
                    <input
                      value={(selectedNode.data.image_path as string) || ''}
                      onChange={(e) => updateSelectedNodeData('image_path', e.target.value)}
                      placeholder="留空 = 用上游 image 输出"
                      className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                    />
                  </div>
                )}
                {selectedNode.type === 'inpaint' && (
                  <div>
                    <label className="text-xs text-gray-400 block mb-1">蒙版路径（白色=重绘区）</label>
                    <input
                      value={(selectedNode.data.mask_path as string) || ''}
                      onChange={(e) => updateSelectedNodeData('mask_path', e.target.value)}
                      placeholder="与原图同尺寸的黑白图"
                      className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                    />
                  </div>
                )}
                {selectedNode.type === 'inpaint' && (
                  <div>
                    <label className="text-xs text-gray-400 block mb-1">重绘模型（留空=默认弱修补）</label>
                    <input
                      value={(selectedNode.data.model as string) || ''}
                      onChange={(e) => updateSelectedNodeData('model', e.target.value)}
                      placeholder="如 Flux-Fill，先落位 D:/AI/Models 再用"
                      className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                    />
                  </div>
                )}
                {selectedNode.type === 'image2image' && (
                  <div>
                    <label className="text-xs text-gray-400 block mb-1">重绘强度（0.1 微调 → 0.9 大改）</label>
                    <input
                      type="number" step={0.05} min={0.05} max={1}
                      value={Number(selectedNode.data.strength ?? 0.6)}
                      onChange={(e) => updateSelectedNodeData('strength', Number(e.target.value) || 0.6)}
                      className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
                    />
                  </div>
                )}
                {selectedNode.type === 'upscale' && (
                  <div>
                    <label className="text-xs text-gray-400 block mb-1">放大模型</label>
                    <input
                      value={(selectedNode.data.model as string) || 'RealESRGAN_x4plus.pth'}
                      onChange={(e) => updateSelectedNodeData('model', e.target.value)}
                      className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                    />
                  </div>
                )}
                {selectedNode.type === 'text2video' && (
                  <div>
                    <label className="text-xs text-gray-400 block mb-1">视频后端</label>
                    <select
                      value={(selectedNode.data.backend as string) || 'wan'}
                      onChange={(e) => {
                        const v = e.target.value
                        updateSelectedNodeData('backend', v)
                        // 切后端时把默认尺寸/帧数一并换掉：H3 必须是 17k+5 帧
                        if (v === 'h3') {
                          updateSelectedNodeData('width', 608)
                          updateSelectedNodeData('height', 352)
                          updateSelectedNodeData('frames', 124)
                          updateSelectedNodeData('fps', 24)
                          updateSelectedNodeData('steps', 20)
                        } else {
                          updateSelectedNodeData('width', 832)
                          updateSelectedNodeData('height', 480)
                          updateSelectedNodeData('frames', 33)
                          updateSelectedNodeData('fps', 16)
                        }
                      }}
                      className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
                    >
                      <option value="wan">Wan 1.3B（快，无声，2 秒）</option>
                      <option value="h3">MiniMax H3（慢，自带立体声，5 秒）</option>
                    </select>
                  </div>
                )}
                {selectedNode.type === 'text2video' && (
                  <label className="flex items-center gap-2 text-[11px] text-gray-400">
                    <input
                      type="checkbox"
                      checked={Boolean(selectedNode.data.interp)}
                      onChange={(e) => updateSelectedNodeData('interp', e.target.checked)}
                    />
                    出片后插帧翻倍（Wan 16→32fps / H3 24→48fps，CPU 完成，不占显存）
                  </label>
                )}
                {selectedNode.type === 'text2video' && selectedNode.data.backend === 'h3' && (
                  <label className="flex items-start gap-2 text-[11px] text-gray-400">
                    <input
                      type="checkbox"
                      className="mt-0.5"
                      checked={selectedNode.data.prompt_opt !== false}
                      onChange={(e) => updateSelectedNodeData('prompt_opt', e.target.checked)}
                    />
                    <span>
                      自动优化提示词（推荐）—— 按官方指南改写成「镜头 + 声音分镜」再进模型，
                      走本地 LLM 约 10 秒；失败自动回退原文
                    </span>
                  </label>
                )}
                {selectedNode.type === 'text2video' && selectedNode.data.backend === 'h3' && (
                  <div>
                    <label className="text-[11px] text-gray-400 block mb-1">
                      场景九宫格锁（可留空；也可从上游「场景九宫格」节点连入）
                    </label>
                    <textarea
                      rows={3}
                      value={(selectedNode.data.scene_grid as string) || ''}
                      onChange={(e) => updateSelectedNodeData('scene_grid', e.target.value)}
                      placeholder="九行「左上|...」；或直接粘贴已编译的 SCENE LOCK 句"
                      className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-[10px] resize-y"
                    />
                    <p className="text-[10px] text-gray-500 mt-1 leading-relaxed">
                      分段生成时同一句锁会被逐字重注到每一段；锁句不一致或缺失会写进 prompt_warnings。
                    </p>
                  </div>
                )}
                {selectedNode.type === 'text2video' && selectedNode.data.backend === 'h3' && (
                  <div className="flex flex-col gap-2">
                    <label className="flex items-center gap-2 text-[11px] text-gray-400">
                      <input
                        type="checkbox"
                        checked={selectedNode.data.turbo !== false}
                        onChange={(e) => {
                          updateSelectedNodeData('turbo', e.target.checked)
                          // AGENTS §5.7：音频比画面晚收敛，8 步会乱语；turbo 12 步、非 turbo 20 步
                          updateSelectedNodeData('steps', e.target.checked ? 12 : 20)
                        }}
                      />
                      涡轮 LoRA（20 步 → 12 步，约 1.6 倍速；步数低于 12 音频会乱语，长片必开）
                    </label>
                    <label className="flex items-center gap-2 text-[11px] text-gray-400">
                      <input
                        type="checkbox"
                        checked={Boolean(selectedNode.data.low_vram)}
                        onChange={(e) => updateSelectedNodeData('low_vram', e.target.checked)}
                      />
                      低显存模式（注意力/FFN 分块，可上 736×416）
                    </label>
                    <div>
                      <label className="text-[11px] text-gray-400 block mb-1">
                        分段数（每段 5.17 秒，自动拆分镜 + 末帧续接 + 拼接，最多 8 段≈41 秒）
                      </label>
                      <input
                        type="number" min={1} max={8}
                        value={Number(selectedNode.data.segments ?? 1)}
                        onChange={(e) => updateSelectedNodeData('segments', Number(e.target.value) || 1)}
                        className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                      />
                    </div>
                  </div>
                )}
                {selectedNode.type === 'text2video' && selectedNode.data.backend === 'h3' && (
                  <div className="flex flex-col gap-2 pt-2 border-t border-border">
                    <div>
                      <label className="text-[11px] text-gray-400 block mb-1">
                        念稿音轨 ref_audio（给了就走 ref2va 台词模式）
                      </label>
                      <input
                        value={(selectedNode.data.ref_audio as string) || ''}
                        onChange={(e) => updateSelectedNodeData('ref_audio', e.target.value)}
                        placeholder="dub.wav 绝对路径；留空 = 模型联合生成音频"
                        className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs font-mono"
                      />
                    </div>
                    <div>
                      <label className="text-[11px] text-gray-400 block mb-1">
                        参考图 ref_images（锁角色外貌，每行一条，≤9）
                      </label>
                      <textarea
                        rows={2}
                        value={(selectedNode.data.ref_images as string) || ''}
                        onChange={(e) => updateSelectedNodeData('ref_images', e.target.value)}
                        placeholder="定妆图路径，换行分隔"
                        className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs font-mono resize-y"
                      />
                    </div>
                    <div>
                      <label className="text-[11px] text-gray-400 block mb-1">
                        最终音轨 audio_track（出片后 1:1 覆盖，保证不丢帧）
                      </label>
                      <input
                        value={(selectedNode.data.audio_track as string) || ''}
                        onChange={(e) => updateSelectedNodeData('audio_track', e.target.value)}
                        placeholder="留空 = 用模型生成的音轨"
                        className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs font-mono"
                      />
                      <p className="text-[10px] text-gray-500 mt-1">
                        实测本机 ref2va 不执行 fully_copy、音轨会被重新生成。要让台词跨镜连续、
                        音景不重启，把同一段 wav 填这里；代价是口型不再与它同步。
                      </p>
                    </div>
                  </div>
                )}
                {selectedNode.type === 'text2video' && (
                  <div className="grid grid-cols-2 gap-2">
                    {([['width', '宽'], ['height', '高'], ['frames', '帧数'], ['fps', '帧率'], ['steps', '步数']] as const).map(([k, label]) => (
                      <div key={k}>
                        <label className="text-[10px] text-gray-500 block mb-1">{label}</label>
                        <input
                          type="number"
                          value={Number(selectedNode.data[k] ?? 0)}
                          onChange={(e) => updateSelectedNodeData(k, Number(e.target.value) || 0)}
                          className="w-full px-2 py-1 rounded-lg bg-canvas border border-border text-xs"
                        />
                      </div>
                    ))}
                  </div>
                )}
                <p className="text-[10px] text-gray-500 leading-relaxed">
                  {selectedNode.type === 'text2video'
                    ? (selectedNode.data.backend === 'h3'
                        ? 'MiniMax H3：一次推理同时出画面 + 立体声，608×352×124 帧≈5 秒（实测 172 秒）。会先停掉默认 ComfyUI 腾显存，用完可切回。'
                        : 'Wan2.1-T2V-1.3B：832×480×33 帧≈2 秒，约 64 秒出片，无音轨。')
                    : '走 ComfyUI 本地节点，图片不出本机。'}
                </p>
              </div>
            )}

            {selectedNode.type === 'select' && (
              <div className="flex flex-col gap-3">
                <div>
                  <label className="text-xs text-gray-400 block mb-1">参考主题（这张图本来该画什么）</label>
                  <textarea
                    rows={2}
                    value={(selectedNode.data.reference as string) || ''}
                    onChange={(e) => updateSelectedNodeData('reference', e.target.value)}
                    placeholder="留空 = 自动取上游的 prompt / original_prompt"
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
                  />
                  <p className="text-[10px] text-gray-500 mt-1">
                    不给参考主题，模型只能按画面自评，结论会和你的要求无关。
                  </p>
                </div>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">额外要求（可选）</label>
                  <input
                    value={(selectedNode.data.criteria as string) || ''}
                    onChange={(e) => updateSelectedNodeData('criteria', e.target.value)}
                    placeholder="例如：不要出现文字；人物必须朝左"
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
                  />
                </div>
                <div className="grid grid-cols-2 gap-2">
                  <div>
                    <label className="text-xs text-gray-400 block mb-1">采用阈值</label>
                    <input
                      type="number"
                      min={0}
                      max={100}
                      value={(selectedNode.data.threshold as number) ?? 60}
                      onChange={(e) => updateSelectedNodeData('threshold', Number(e.target.value) || 0)}
                      className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
                    />
                  </div>
                  <div>
                    <label className="text-xs text-gray-400 block mb-1">视觉模型（留空自动挑）</label>
                    <input
                      value={(selectedNode.data.model as string) || ''}
                      onChange={(e) => updateSelectedNodeData('model', e.target.value)}
                      placeholder="qwen2.5vl:7b"
                      className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                    />
                  </div>
                </div>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">候选图路径（可选，每行一条）</label>
                  <textarea
                    rows={3}
                    value={(selectedNode.data.images as string) || ''}
                    onChange={(e) => updateSelectedNodeData('images', e.target.value)}
                    placeholder="留空 = 收上游 image / images / values"
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono resize-y"
                  />
                </div>
                <p className="text-[10px] text-gray-500">
                  用法：文生图 batch=3 → 择优挑选 → 下游只拿最高分那张。只有 1 张候选时不打分（省一次换侧）。
                </p>
              </div>
            )}

            {selectedNode.type === 'vision' && (
              <div className="flex flex-col gap-3">
                <div>
                  <label className="text-xs text-gray-400 block mb-1">提问</label>
                  <textarea
                    rows={4}
                    value={(selectedNode.data.prompt as string) || ''}
                    onChange={(e) => updateSelectedNodeData('prompt', e.target.value)}
                    placeholder="想问什么？例如：这张图里的人物衣着是什么朝代？"
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs resize-y"
                  />
                </div>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">视觉模型（留空自动挑）</label>
                  <input
                    value={(selectedNode.data.model as string) || ''}
                    onChange={(e) => updateSelectedNodeData('model', e.target.value)}
                    placeholder="qwen2.5vl:7b"
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                  />
                </div>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">图片路径（可选，覆盖上游）</label>
                  <input
                    value={(selectedNode.data.image_path as string) || ''}
                    onChange={(e) => updateSelectedNodeData('image_path', e.target.value)}
                    placeholder="留空 = 用上游 image 输出"
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                  />
                </div>
                <p className="text-[10px] text-gray-500 leading-relaxed">
                  走本地 Ollama（/api/chat + base64），图片不出本机。单图上限 8MB。
                </p>
              </div>
            )}

            {selectedNode.type === 'asr' && (
              <div className="flex flex-col gap-2">
                <label className="text-xs text-gray-400">音频路径（留空取上游 audio）</label>
                <input
                  value={(selectedNode.data.audio_path as string) || ''}
                  onChange={(e) => updateSelectedNodeData('audio_path', e.target.value)}
                  placeholder="data/generated/tts-xxx.wav"
                  className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                />
                <div className="flex gap-2">
                  <div className="flex-1">
                    <label className="text-[10px] text-gray-500 block mb-1">模型档位（留空=服务默认）</label>
                    <select
                      value={(selectedNode.data.model as string) || ''}
                      onChange={(e) => updateSelectedNodeData('model', e.target.value)}
                      className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
                    >
                      <option value="">默认（small）</option>
                      <option value="tiny">tiny（最快，试听用）</option>
                      <option value="base">base</option>
                      <option value="small">small（默认）</option>
                      <option value="medium">medium（更准，更慢）</option>
                      <option value="large-v3">large-v3（台词镜头用）</option>
                    </select>
                  </div>
                  <div className="w-24">
                    <label className="text-[10px] text-gray-500 block mb-1">设备</label>
                    <select
                      value={(selectedNode.data.device as string) || ''}
                      onChange={(e) => updateSelectedNodeData('device', e.target.value)}
                      className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
                    >
                      <option value="">默认（CPU）</option>
                      <option value="cpu">CPU（不占显存）</option>
                      <option value="cuda">CUDA（快，吃 3GB）</option>
                      <option value="auto">auto</option>
                    </select>
                  </div>
                </div>
                <p className="text-[10px] text-gray-500 leading-relaxed">
                  档位按镜头挑：台词镜头 large-v3 保准确，氛围镜头 small 省时间。
                  cuda 会占用约 3GB 显存——出片工作流里建议留 CPU。
                </p>
              </div>
            )}

            {selectedNode.type === 'http_request' && (
              <div className="flex flex-col gap-3">
                <div>
                  <label className="text-xs text-gray-400 block mb-1">方法</label>
                  <select
                    value={(selectedNode.data.method as string) || 'GET'}
                    onChange={(e) => updateSelectedNodeData('method', e.target.value)}
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs"
                  >
                    {['GET', 'POST', 'PUT', 'PATCH', 'DELETE'].map((m) => (
                      <option key={m} value={m}>
                        {m}
                      </option>
                    ))}
                  </select>
                </div>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">URL</label>
                  <input
                    value={(selectedNode.data.url as string) || ''}
                    onChange={(e) => updateSelectedNodeData('url', e.target.value)}
                    placeholder="https://api.example.com/v1/chat"
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                  />
                </div>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">凭据名（推荐，免明文 Key）</label>
                  <input
                    value={(selectedNode.data.credential as string) || ''}
                    onChange={(e) => updateSelectedNodeData('credential', e.target.value)}
                    placeholder="如 openai —— 在「凭据」面板里维护"
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono"
                  />
                </div>
                <div>
                  <label className="text-xs text-gray-400 block mb-1">请求体（JSON）</label>
                  <textarea
                    rows={3}
                    value={(selectedNode.data.body as string) || ''}
                    onChange={(e) => updateSelectedNodeData('body', e.target.value)}
                    className="w-full px-2 py-1.5 rounded-lg bg-canvas border border-border text-xs font-mono resize-y"
                  />
                </div>
              </div>
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

            {/* 错误信息：错误码翻成人话 + 可执行修复动作（不再裸抛 timeout/config_missing） */}
            {selectedNode.data?.error != null && (
              <ErrorNotice
                code={selectedNode.data?.error_code as string | undefined}
                message={String(selectedNode.data.error)}
                onAction={(a) => {
                  if (a === 'open-env') setEnvOpen(true)
                }}
              />
            )}
          </div>
        ) : (
          <p className="text-xs text-gray-500">点击画布中的节点进行配置</p>
        )}
      </div>
      </div>

      {/* 底部状态栏：计数 / 运行状态 / 显存 / 缩放 */}
      <StatusBar
        nodesCount={nodes.length}
        edgesCount={edges.length}
        isRunning={isRunning}
        runStatus={runStatus}
        gpu={gpuStatus?.gpu}
      />
    </div>
  )
}

function getDefaultNodeData(type: string): Record<string, any> {
  switch (type) {
    case 'start':
      return { prompt: '' }
    case 'llm':
      return { model: 'qwen2.5-coder:32b', prompt: '', tools: [] }
    case 'end':
      return {}
    case 'text2image':
      return { prompt: '', width: 1024, height: 1024, batch: 1, prompts: '', split_lines: false }
    case 'asr':
      return { audio_path: '', model: '', device: '' }
    case 'tts':
      return { text: '' }
    case 'video_compose':
      return { output_path: 'data/generated/video.mp4' }
    case 'if_else':
      return { condition_type: 'not_empty', compare_value: '' }
    case 'sub_workflow':
      return { workflow_id: undefined, workflow_name: '' }
    case 'loop':
      return {
        workflow_id: undefined,
        workflow_name: '',
        mode: 'count',
        iterations: 3,
        max_iterations: 20,
        condition_type: 'not_empty',
        compare_value: '',
      }
    case 'http_request':
      return { method: 'GET', url: '', headers: {}, body: '' }
    case 'data_transform':
      return { template: '' }
    case 'rag_query':
      return { query: '', top_k: 3 }
    case 'code':
      return {
        code: 'result = strings.ToUpper(inputText)',
        timeout_ms: 30000,
      }
    case 'aggregate':
      return { mode: 'object', field: 'text', inputs: '', separator: '\n\n', include_empty: false }
    case 'vision':
      return { prompt: '描述这张图片的主体、构图、风格；如果有文字请原样抄下来。', model: '', image_path: '' }
    case 'prompt_opt':
      return { mode: 'h3', text: '', seconds: 5 }
    case 'scene_grid':
      return { cells: '', idea: '', style: '', light: '', palette: '', depth: '' }
    case 'storyboard':
      return { brief: '', seconds: 30, max_shots: 6 }
    case 'shotlist':
      return {
        plan: '', backend: 'h3', width: 608, height: 352, steps: 8,
        turbo: true, audio_check: true, interp: false,
      }
    case 'image2image':
      return { prompt: '', image_path: '', strength: 0.6, steps: 8 }
    case 'inpaint':
      return { prompt: '', image_path: '', mask_path: '', model: '', denoise: 1.0, steps: 10 }
    case 'upscale':
      return { prompt: '', image_path: '', model: 'RealESRGAN_x4plus.pth' }
    case 'text2video':
      return { prompt: '', backend: 'wan', width: 832, height: 480, frames: 33, fps: 16, steps: 20, negative: '', low_vram: false, segments: 1, interp: false, prompt_opt: true, turbo: true, ref_audio: '', ref_images: '', audio_track: '', scene_grid: '' }
    case 'select':
      return { images: '', reference: '', criteria: '', threshold: 60, model: '' }
    case 'quality_gate':
      return {
        require_audio: true,
        min_frames: 0, duration_min: 0, duration_max: 0,
        duration_tolerance: 0.15,
        min_width: 0, min_height: 0,
        loudness_min_db: 0, max_silence_gap: 0,
        forbid_speech: false, speech_max_seconds: 1.5,
      }
    case 'note':
      return { text: '' }
    case 'group':
      return { label: '分组' }
    default:
      return {}
  }
}
