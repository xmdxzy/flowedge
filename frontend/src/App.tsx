import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { getDefaultNodeData } from './config/nodeDefaults'
import {
  ReactFlow,
  ReactFlowProvider,
  Background,
  Controls,
  MiniMap,
  Panel,
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
import ToastHost, { toast, confirmDialog } from './components/Toast'
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
import { useMediaQuery, NARROW_QUERY } from './hooks/useMediaQuery'
import { useWebSocket, type WSEvent } from './hooks/useWebSocket'
import ExecutionPanel from './components/ExecutionPanel'
import ContextMenu from './components/ContextMenu'
import DebugPanel from './components/DebugPanel'
import CapabilityPanel from './components/CapabilityPanel'
import TriggerPanel from './components/TriggerPanel'
import GalleryPanel from './components/GalleryPanel'
import CredentialPanel from './components/CredentialPanel'
import ConfigPanel from './components/ConfigPanel'
import ToolchainPanel from './components/ToolchainPanel'
import EvolutionPanel from './components/EvolutionPanel'
import RunEstimate from './components/RunEstimate'
import NodeInspector from './components/NodeInspector'
import CommandPalette, { type CommandItem } from './components/CommandPalette'
import KBPanel from './components/KBPanel'
import DiagnosticsPanel from './components/DiagnosticsPanel'
import ComposePanel from './components/ComposePanel'
import { PALETTE, CANVAS_ONLY_TYPES, initialNodes, initialEdges } from './config/graph'
import { autoLayout, boundingBox, nextId, stripCanvasOnlyEdges, stripCanvasOnlyNodes } from './lib/graphUtils'
import {
  applyTheme,
  CANVAS_THEME,
  getInitialPref,
  resolveTheme,
  watchSystemTheme,
  type Theme,
  type ThemePref,
} from './lib/theme'
import ThemeSwitch from './components/ThemeSwitch'
import {
  fetchCards,
  fetchProfiles,
  setCardIndex,
  type CardCatalog,
  type LLMProfile,
} from './lib/cards'
import withCardBadge from './components/withCardBadge'
import GenericNode from './nodes/GenericNode'
import { fetchToolchain } from './lib/toolchain'
import { fetchAgents, type AgentStatus } from './lib/agents'
import { fetchModules, setModules } from './lib/modules'

const baseNodeTypes: NodeTypes = {
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
  consistency_gate: EditNode,
  image2image: EditNode,
  inpaint: EditNode,
  upscale: EditNode,
  text2video: EditNode,
  subtitles: EditNode,
  post_production: EditNode,
  note: NoteNode,
  group: GroupNode,
}

// 能力卡片徽章：在节点右上角叠一枚形状徽章（圆 / 六边形 / 三层堆叠）。
//
// 为什么用 HOC 而不是改 20 个节点组件：徽章是**统一表达**，
// 改组件会得到 20 份略有差异的实现，且新加节点必然漏一次。
// 这里一次包装，所有节点（含以后新增的）自动获得。
const wrappedNodeTypes: NodeTypes = Object.fromEntries(
  Object.entries(baseNodeTypes).map(([k, C]) => [k, withCardBadge(C as React.ComponentType<any>)]),
) as NodeTypes

// 未知类型（运行期插件节点）落到通用卡片渲染。
//
// 不兜底的话：插件装好、引擎能跑，但拖到画布上 ReactFlow 找不到组件，
// 整块空白/报错——「插件生态」就在最后一步断掉。用 Proxy 而不是把表补全，
// 是因为插件类型名由用户随时新增，前端不可能预知。
const FallbackNode = withCardBadge(GenericNode as React.ComponentType<any>)
const nodeTypes = new Proxy(wrappedNodeTypes, {
  get(target, prop) {
    if (typeof prop !== 'string') return Reflect.get(target, prop)
    return target[prop] ?? FallbackNode
  },
}) as NodeTypes

// ReactFlowProvider 包裹：useReactFlow 需要 Provider 祖先
// 插件节点的显示名写进 data._label：画布上的通用卡片不认识类型名，
// 只能靠这个字段把「用户起的标题」显示出来（内置节点各自有硬编码标题，不受影响）。
function withPluginLabel(
  type: string,
  data: Record<string, unknown>,
  plugins: { type: string; label: string }[],
): Record<string, unknown> {
  const hit = plugins.find((p) => p.type === type)
  return hit ? { ...data, _label: hit.label } : data
}

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
  // 主题：pref 是用户选择（含"跟随系统"），theme 是解析后的实际主题（画布内联色要用）
  const [themePref, setThemePref] = useState<ThemePref>(() => getInitialPref())
  const [theme, setTheme] = useState<Theme>(() => resolveTheme(getInitialPref()))

  // 偏好变化 → 写 data-theme；选"跟随系统"时顺便挂系统外观监听
  useEffect(() => {
    setTheme(applyTheme(themePref))
    if (themePref !== 'system') return
    return watchSystemTheme((t) => {
      applyTheme('system')
      setTheme(t)
    })
  }, [themePref])

  // ⌘/Ctrl + Shift + L：浅色 → 深色 → 跟随系统 循环
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (!(e.metaKey || e.ctrlKey) || !e.shiftKey || e.key.toLowerCase() !== 'l') return
      e.preventDefault()
      setThemePref((p) => (p === 'light' ? 'dark' : p === 'dark' ? 'system' : 'light'))
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])
  // 能力卡片 / 本机模型 / 自定义端点：三者都是"环境信息"，拉一次就够，
  // 单独放一份而不是塞进 nodes，避免每次保存工作流把它们写进 JSON。
  const [cards, setCards] = useState<CardCatalog | null>(null)
  const [availableModels, setAvailableModels] = useState<string[]>([])
  const [llmProfiles, setLlmProfiles] = useState<LLMProfile[]>([])
  // 外部智能体名册：agent_task 节点的下拉与探活徽章
  const [agentStatuses, setAgentStatuses] = useState<AgentStatus[]>([])
  const reloadPlugins = useCallback(() => {
    fetchToolchain(false)
      .then((v) =>
        setPluginNodes(
          v.entries
            .filter((e) => e.kind === 'plugin' && e.enabled && e.meta?.node_type)
            .map((e) => ({
              type: String(e.meta?.node_type),
              label: e.name,
              icon: '⚒',
              tone: 'tone-warn',
            })),
        ),
      )
      .catch(() => setPluginNodes([]))
  }, [])

  useEffect(() => {
    reloadPlugins()
  }, [reloadPlugins])

  const reloadAgents = useCallback(() => {
    fetchAgents(true).then((list) => setAgentStatuses(list))
  }, [])
  // 外部接入方式：开始节点面板要回答"谁在调这条工作流"
  const [entry, setEntry] = useState<{ saved: boolean; webhook: boolean; cron: string }>({
    saved: false,
    webhook: false,
    cron: '',
  })
  // 模型元信息：下拉里必须能显示"叫什么、多大"，不能只写"自动探测"
  const [modelMeta, setModelMeta] = useState<{
    def: string
    sizes: Record<string, number>
    ckpt: string
    checkpoints: string[]
    vision: string
  }>({ def: '', sizes: {}, ckpt: '', checkpoints: [], vision: '' })
  const [credOpen, setCredOpen] = useState(false)
  const [configOpen, setConfigOpen] = useState(false)
  // 工具台：智能体 / 技能 / MCP / 插件四类工具的统一入口。
  // 「我刚装的东西在哪」这一类问题只有这一个面板能回答，所以给了一级按钮。
  const [toolsOpen, setToolsOpen] = useState(false)
  // 运行期插件节点（data/plugins/<name>/plugin.json）→ 左侧节点库的「插件」分组。
  // 静态 PALETTE 不可能包含用户后装的插件，所以这份清单从 /api/toolchain 拉，
  // 每次打开工具台/重扫后刷新；拉不到就退化成空分组（不影响内置节点）。
  const [pluginNodes, setPluginNodes] = useState<{ type: string; label: string; icon: string; tone: string }[]>([])
  // 估算结果里的「会不会撞墙」：由 RunEstimate 上报，运行前据此二次确认。
  // 存 ref 而不是 state——runWorkflow 是闭包，读 state 会读到旧值。
  const riskRef = useRef<{ risky: boolean; detail: string }>({ risky: false, detail: '' })
  const [lastRunId, setLastRunId] = useState<number | null>(null)
  const [canResume, setCanResume] = useState(false)
  const [kbOpen, setKbOpen] = useState(false)
  const [diagOpen, setDiagOpen] = useState(false)
  const [evoOpen, setEvoOpen] = useState(false)
  // 窄屏（≤1320）顶栏放不下 12 个入口：低频 5 个改从「⋯」进，宽屏保持快捷入口
  const narrow = useMediaQuery(NARROW_QUERY)
  const [composeOpen, setComposeOpen] = useState(false)
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
  // 节点类型的中文名（来自 Modules 契约，前端不手抄清单）
  useEffect(() => {
    fetchModules().then((mods) => mods.length && setModules(mods))
  }, [])

  // 卡片库在弹层里被编辑后重新拉目录（画布徽章的名字也要跟着变）
  const reloadCards = useCallback(() => {
    fetchCards().then((c) => {
      if (!c) return
      setCards(c)
      setCardIndex(c)
    })
  }, [])

  // 外部接入信息：保存后才知道 Webhook / 定时是否开启
  useEffect(() => {
    if (!currentWorkflowId) {
      setEntry({ saved: false, webhook: false, cron: '' })
      return
    }
    let alive = true
    fetch(`/api/workflows/${currentWorkflowId}/trigger`)
      .then((r) => (r.ok ? r.json() : null))
      .then((d) => {
        if (!alive) return
        setEntry({
          saved: true,
          webhook: Boolean(d?.trigger_enabled),
          cron: String(d?.cron_expr || ''),
        })
      })
      .catch(() => {
        if (alive) setEntry({ saved: true, webhook: false, cron: '' })
      })
    return () => {
      alive = false
    }
  }, [currentWorkflowId])

  // 卡片目录 + 本机模型 + 自定义端点（一次性拉取）
  useEffect(() => {
    let alive = true
    fetchCards().then((c) => {
      if (!alive || !c) return
      setCards(c)
      setCardIndex(c) // 徽章要在节点上显示卡片名，走模块级索引
    })
    fetch('/api/models')
      .then((r) => (r.ok ? r.json() : null))
      .then((d) => {
        if (!alive || !d) return
        if (Array.isArray(d.ollama_models)) setAvailableModels(d.ollama_models)
        setModelMeta({
          def: d.default_llm || '',
          sizes: d.ollama_sizes_gb || {},
          ckpt: d.t2i_checkpoint || '',
          checkpoints: Array.isArray(d.checkpoints) ? d.checkpoints : [],
          vision: d.vision_model || '',
        })
      })
      .catch(() => {})
    fetchProfiles().then((p) => {
      if (alive && p) setLlmProfiles(p.profiles || [])
    })
    // 名册带探活（LookPath / TCP dial），慢一点但换来"能不能用"的真答案
    fetchAgents(true).then((list) => {
      if (alive) setAgentStatuses(list)
    })
    return () => {
      alive = false
    }
  }, [])

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

  // 导出任务书：把画布编译成**可执行规格**（Markdown）。
  //
  // 与「导出 JSON」的分工：JSON 是给 FlowEdge 自己导入的机器格式；
  // 任务书是给人或智能体读的——执行顺序、每个模块吃什么吐什么、真实参数值、
  // 验收门与 FAIL 条件、产物清单、以及图里没说清的地方。
  // 编译在后端做（拓扑、端口、门阈值都只有那一份事实），前端只负责下载。
  const exportSpec = async () => {
    if (!currentWorkflowId) {
      toast('先保存工作流，任务书由后端按已保存的图编译', 'error')
      return
    }
    try {
      const res = await fetch(`/api/workflows/${currentWorkflowId}/spec?format=md`)
      if (!res.ok) {
        toast(`导出任务书失败：${res.status}`, 'error')
        return
      }
      const text = await res.text()
      const blob = new Blob([text], { type: 'text/markdown;charset=utf-8' })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = `${workflowName || 'workflow'}-任务书.md`
      a.click()
      URL.revokeObjectURL(url)
      toast('任务书已导出：可直接交给智能体执行', 'success')
    } catch (err) {
      console.error('failed to export spec', err)
      toast('导出任务书失败（引擎不可达？）', 'error')
    }
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

  // 面板里点"上游/下游"的节点名，直接选中那个节点（不用回画布上找）
  const selectNodeById = useCallback(
    (id: string) => {
      const target = nodes.find((n) => n.id === id)
      if (target) setSelectedNode(target)
    },
    [nodes],
  )

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
      data: withPluginLabel(type, getDefaultNodeData(type), pluginNodes),
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
        data: withPluginLabel(type, getDefaultNodeData(type), pluginNodes),
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
    // 估算显示会撞墙时先问一句：OOM 重跑的代价是几十秒到几分钟，
    // 而用户往往只是忘了把分辨率从上次的大档改回来。
    if (riskRef.current.risky) {
      const go = await confirmDialog(
        riskRef.current.detail || '预计峰值超过可用显存，大概率 OOM。仍要运行吗？',
        { title: '显存可能不够', okText: '仍要运行', danger: true },
      )
      if (!go) return
    }
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
      { id: 'panel-compose', label: '构图（一句话生成工作流）', icon: '◇', section: '面板', run: () => setComposeOpen(true) },
      { id: 'panel-diag', label: '运行诊断（哪里在漏）', icon: '◎', section: '面板', run: () => setDiagOpen(true) },
      { id: 'panel-evo', label: '自进化（燃料 / 失分 / 判定 / 晋升台账）', icon: '∞', section: '面板', run: () => setEvoOpen(true) },
      { id: 'panel-env', label: '查看运行环境', icon: '◍', section: '面板', run: () => setEnvOpen(true) },
      { id: 'panel-tools', label: '打开工具台（技能 / MCP / 插件 / 智能体）', icon: '⚒', section: '面板', run: () => setToolsOpen(true) },
      { id: 'panel-trigger', label: '配置触发器', icon: '⚡', section: '面板', run: () => setTriggerOpen(true) },
      { id: 'panel-gallery', label: '打开产物画廊', icon: '▣', section: '面板', run: () => setGalleryOpen(true) },
      { id: 'panel-cred', label: '管理凭据', icon: '🔑', section: '面板', run: () => setCredOpen(true) },
      { id: 'act-run', label: '运行工作流', hint: 'Ctrl+Enter', icon: '▶', section: '动作', run: () => runWorkflow() },
      { id: 'act-save', label: '保存工作流', icon: '▤', section: '动作', run: () => saveWorkflow() },
      { id: 'act-layout', label: '自动布局', icon: '⌗', section: '动作', run: () => doAutoLayout() },
      {
        id: 'act-theme',
        label: '切换外观主题（浅色 → 深色 → 跟随系统）',
        hint: 'Ctrl+Shift+L',
        icon: '☾',
        section: '动作',
        run: () => setThemePref((p) => (p === 'light' ? 'dark' : p === 'dark' ? 'system' : 'light')),
      },
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
      <div className="w-60 shrink-0 border-r border-border bg-surface p-4 flex flex-col gap-2 overflow-y-auto">
        <div className="flex items-center justify-between mb-2">
          <h3 className="text-sm font-semibold text-gray-300 whitespace-nowrap">节点库</h3>
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
          plugins={pluginNodes}
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
            className="px-2.5 py-1.5 rounded-md bg-canvas border border-border text-[12.5px] w-44 focus:border-primary outline-none shrink-0"
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
              onClick={() => setToolsOpen(true)}
              className="fe-tb"
              title="工具台：智能体 / 技能 / MCP / 插件（全盘发现，装完即见）"
            >
              工具
            </button>
            {!narrow && (
              <>
                <button
                  onClick={() => setComposeOpen(true)}
                  className="fe-tb"
              title="说一句要做什么，生成工作流与任务书（只生成，不执行）"
            >
              构图
            </button>
            <button
              onClick={() => setKbOpen(true)}
              className="fe-tb"
              title="项目专属语义知识库"
            >
              知识库
            </button>
            <button
              onClick={() => setDiagOpen(true)}
              className="fe-tb"
              title="聚合运行证据，诊断哪里在漏，并生成可试的参数候选"
            >
              诊断
            </button>
            <button
              onClick={() => setEvoOpen(true)}
              className="fe-tb"
              title="自进化：燃料够不够、失分漏在哪、配对判定、晋升台账（晋升必须人按）"
            >
              进化
            </button>
                <button
                  onClick={() => setGalleryOpen(true)}
                  className="fe-tb"
                  title="浏览生成的图片 / 视频 / 音频"
                >
                  产物
                </button>
              </>
            )}

            <div className="fe-vsep" />

            <ThemeSwitch pref={themePref} onChange={setThemePref} />

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
                  <div className="fe-menu" role="menu">
                    <div className="mi-group">内容</div>
                    {/* 窄屏专用：顶栏收起这五项后从这里进；宽屏不渲染，避免重复入口 */}
                    {narrow && (
                      <>
                    <button
                      onClick={() => {
                        setComposeOpen(true); setMoreOpen(false)
                      }}
                    >
                      构图 · 一句话生成
                    </button>
                    <button
                      
                      onClick={() => {
                        setKbOpen(true); setMoreOpen(false)
                      }}
                    >
                      知识库
                    </button>
                    <button
                      
                      onClick={() => {
                        setDiagOpen(true); setMoreOpen(false)
                      }}
                    >
                      运行诊断
                    </button>
                    <button
                      
                      onClick={() => {
                        setEvoOpen(true); setMoreOpen(false)
                      }}
                    >
                      自进化
                    </button>
                    <button
                      
                      onClick={() => {
                        setGalleryOpen(true); setMoreOpen(false)
                      }}
                    >
                      产物画廊
                    </button>
                    <div className="mi-sep" />
                      </>
                    )}
                    <button
                      onClick={() => {
                        setTplLibOpen(true); setMoreOpen(false)
                      }}
                    >
                      模板库
                      <span className="mi-k">{tplEntries.length}</span>
                    </button>
                    <button
                      onClick={() => {
                        setTriggerOpen(true); setMoreOpen(false)
                      }}
                    >
                      触发 · Webhook / 定时
                    </button>
                    <div className="mi-sep" />
                    <div className="mi-group">环境与能力</div>
                    <button
                      onClick={() => {
                        runPreflight()
                      }}
                      disabled={preflightChecking}
                    >
                      {preflightChecking ? '体检中…' : '依赖体检（不运行）'}
                    </button>
                    <button
                      onClick={() => {
                        setEnvOpen(true); setMoreOpen(false)
                      }}
                    >
                      环境自检
                    </button>
                    <button
                      onClick={() => {
                        setCredOpen(true); setMoreOpen(false)
                      }}
                    >
                      凭据管理
                    </button>
                    <button
                      onClick={() => {
                        setConfigOpen(true); setMoreOpen(false)
                      }}
                    >
                      配置 · 模型 / 实例 / 接入
                    </button>
                    <div className="mi-sep" />
                    <div className="mi-group">数据</div>
                    <button
                      onClick={() => {
                        exportSpec(); setMoreOpen(false)
                      }}
                    >
                      导出任务书（给智能体）
                    </button>
                    <button
                      onClick={() => {
                        exportWorkflow(); setMoreOpen(false)
                      }}
                    >
                      导出 JSON
                    </button>
                    <label>
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
                    <div className="mi-group">画布</div>
                    <button
                      onClick={() => {
                        doAutoLayout(); setMoreOpen(false)
                      }}
                    >
                      自动布局
                    </button>
                    <button
                      onClick={() => {
                        groupSelection(); setMoreOpen(false)
                      }}
                    >
                      分组选中节点
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

            <RunEstimate
              nodes={nodes}
              onRiskChange={(risky, detail) => {
                riskRef.current = { risky, detail }
              }}
            />

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
                /* 缩到 168×112：原来 200×150 的白盒子在浅色画布上太抢眼 */
                style={{ width: 168, height: 112 }}
                bgColor={CANVAS_THEME[theme].minimapBg}
                nodeColor={CANVAS_THEME[theme].minimapNode}
                maskColor={CANVAS_THEME[theme].minimapMask}
              />
              {/* 空画布引导：新用户第一眼看到的应该是"从哪开始"，而不是一片空网格。
                  用 Panel 而不是普通 div —— 它固定在视口、不随平移漂走。 */}
              {nodes.length === 0 && (
                <Panel position="top-center">
                  <div className="fe-empty-canvas">
                    <div className="fe-empty-canvas__ring">
                      <svg width="26" height="26" viewBox="0 0 16 16" fill="none">
                        <path d="M9 1L3 9h4l-1 6 6-8H8l1-6z" fill="url(#fe-empty-g)" />
                        <defs>
                          <linearGradient id="fe-empty-g" x1="0" y1="0" x2="16" y2="16">
                            <stop stopColor="#A5B4FC" />
                            <stop offset="1" stopColor="#67E8F9" />
                          </linearGradient>
                        </defs>
                      </svg>
                    </div>
                    <div className="fe-empty-canvas__title">从一条流开始</div>
                    <div className="fe-empty-canvas__hint">
                      从左侧拖入节点，连线即数据流
                      <br />
                      或按 <kbd>Ctrl</kbd> + <kbd>K</kbd> 打开命令面板
                    </div>
                  </div>
                </Panel>
              )}
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
      <DiagnosticsPanel open={diagOpen} onClose={() => setDiagOpen(false)} onNotify={setRunResult} />

      {/* 自进化面板：燃料 / 失分 / 候选 / 判定 / 晋升台账 */}
      {evoOpen && (
        <EvolutionPanel
          workflowId={currentWorkflowId}
          onClose={() => setEvoOpen(false)}
          onNotify={setRunResult}
        />
      )}
      <ComposePanel
        open={composeOpen}
        onClose={() => setComposeOpen(false)}
        onNotify={setRunResult}
        onLoad={(ns, es, nm) => {
          // 载入的是"还没保存的新图"：必须清掉 currentWorkflowId，
          // 否则下一步「保存」会覆盖原来那条工作流（人在画布上看的是新图，存掉的却是旧图）。
          pushHistory()
          setNodes(ns)
          setEdges(es)
          setWorkflowName(nm)
          setCurrentWorkflowId(undefined)
        }}
      />

      {/* 模板库：分组 / 搜索 / 置顶 */}
      <TemplateLibrary
        open={tplLibOpen}
        onClose={() => setTplLibOpen(false)}
        onApply={loadTemplate}
      />

      {/* 运行环境自检面板 */}
      {envOpen && <CapabilityPanel onClose={() => setEnvOpen(false)} />}

      {/* 工具台：四类工具一套面板 */}
      {toolsOpen && (
        <ToolchainPanel
          onClose={() => {
            setToolsOpen(false)
            reloadPlugins()
          }}
          onNotify={setRunResult}
        />
      )}

      {/* 触发器面板 */}
      {triggerOpen && <TriggerPanel workflowId={currentWorkflowId} onClose={() => setTriggerOpen(false)} />}

      {/* 产物画廊 */}
      {galleryOpen && <GalleryPanel onClose={() => setGalleryOpen(false)} />}

      {/* 凭据管理 */}
      {credOpen && <CredentialPanel onClose={() => setCredOpen(false)} />}
      {configOpen && <ConfigPanel onClose={() => setConfigOpen(false)} />}

      {/* 命令面板 */}
      {cmdOpen && <CommandPalette items={buildCommands()} onClose={() => setCmdOpen(false)} />}

      {/* 右侧属性面板（实现见 components/NodeInspector.tsx） */}
      <NodeInspector
        node={selectedNode}
        nodes={nodes}
        edges={edges}
        tab={propTab}
        onTabChange={setPropTab}
        onUpdate={updateSelectedNodeData}
        onNotify={setRunResult}
        savedWorkflows={savedWorkflows}
        currentWorkflowId={currentWorkflowId}
        cards={cards}
        availableModels={availableModels}
        modelMeta={modelMeta}
        llmProfiles={llmProfiles}
        agents={agentStatuses}
        onReloadAgents={reloadAgents}
        entry={entry}
        onSelectNode={selectNodeById}
        onReloadCards={reloadCards}
        onErrorAction={(a) => {
          if (a === 'open-env') setEnvOpen(true)
        }}
      />
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
