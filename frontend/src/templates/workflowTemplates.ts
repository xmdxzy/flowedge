// 预置工作流模板
export interface WorkflowTemplate {
  id: string
  name: string
  description: string
  category: string
  nodes: any[]
  edges: any[]
}

export const workflowTemplates: WorkflowTemplate[] = [
  {
    id: 'text-summarize',
    name: '文本总结',
    description: '输入长文本，AI 自动生成摘要',
    category: '文本处理',
    nodes: [
      {
        id: 'start-1',
        type: 'start',
        position: { x: 50, y: 150 },
        data: { prompt: '' },
      },
      {
        id: 'llm-1',
        type: 'llm',
        position: { x: 320, y: 150 },
        data: {
          model: 'qwen2.5-coder:32b',
          prompt: '请将以下文本总结为3个要点，简洁明了：',
        },
      },
      {
        id: 'end-1',
        type: 'end',
        position: { x: 600, y: 150 },
        data: {},
      },
    ],
    edges: [
      { id: 'e1-2', source: 'start-1', target: 'llm-1' },
      { id: 'e2-3', source: 'llm-1', target: 'end-1' },
    ],
  },
  {
    id: 'image-generate',
    name: '文生图',
    description: '输入描述，AI 生成对应图片',
    category: '图像生成',
    nodes: [
      {
        id: 'start-1',
        type: 'start',
        position: { x: 50, y: 150 },
        data: { prompt: '一只在月球上的猫，科幻风格' },
      },
      {
        id: 't2i-1',
        type: 'text2image',
        position: { x: 320, y: 150 },
        data: { prompt: '', width: 1024, height: 1024 },
      },
      {
        id: 'end-1',
        type: 'end',
        position: { x: 600, y: 150 },
        data: {},
      },
    ],
    edges: [
      { id: 'e1-2', source: 'start-1', target: 't2i-1' },
      { id: 'e2-3', source: 't2i-1', target: 'end-1' },
    ],
  },
  {
    id: 'conditional-branch',
    name: '条件分支',
    description: '根据输入内容走不同分支',
    category: '控制流',
    nodes: [
      {
        id: 'start-1',
        type: 'start',
        position: { x: 50, y: 200 },
        data: { prompt: '' },
      },
      {
        id: 'if-1',
        type: 'if_else',
        position: { x: 300, y: 200 },
        data: { condition_type: 'not_empty', compare_value: '' },
      },
      {
        id: 'llm-true',
        type: 'llm',
        position: { x: 560, y: 100 },
        data: { model: 'qwen2.5-coder:32b', prompt: '处理有内容的情况：' },
      },
      {
        id: 'llm-false',
        type: 'llm',
        position: { x: 560, y: 300 },
        data: { model: 'qwen2.5-coder:32b', prompt: '处理空内容的情况：' },
      },
      {
        id: 'end-1',
        type: 'end',
        position: { x: 820, y: 200 },
        data: {},
      },
    ],
    edges: [
      { id: 'e1-2', source: 'start-1', target: 'if-1' },
      { id: 'e2-true', source: 'if-1', sourceHandle: 'true', target: 'llm-true' },
      { id: 'e2-false', source: 'if-1', sourceHandle: 'false', target: 'llm-false' },
      { id: 'e3-1', source: 'llm-true', target: 'end-1' },
      { id: 'e3-2', source: 'llm-false', target: 'end-1' },
    ],
  },
  {
    id: 'rag-qa',
    name: 'RAG 知识库问答',
    description: '基于知识库检索后回答问题',
    category: 'AI 应用',
    nodes: [
      {
        id: 'start-1',
        type: 'start',
        position: { x: 50, y: 200 },
        data: { prompt: '请介绍一下 FlowEdge 的核心功能' },
      },
      {
        id: 'rag-1',
        type: 'rag_query',
        position: { x: 320, y: 200 },
        data: { query: '' },
      },
      {
        id: 'llm-1',
        type: 'llm',
        position: { x: 600, y: 200 },
        data: { model: 'qwen2.5-coder:32b', prompt: '仅根据以下参考资料回答问题；资料不足时明确说明，不要编造：' },
      },
      {
        id: 'end-1',
        type: 'end',
        position: { x: 880, y: 200 },
        data: {},
      },
    ],
    edges: [
      { id: 'e1-2', source: 'start-1', target: 'rag-1' },
      { id: 'e2-3', source: 'rag-1', target: 'llm-1' },
      { id: 'e3-4', source: 'llm-1', target: 'end-1' },
    ],
  },
  {
    id: 'video-pipeline',
    name: '短视频生成流水线',
    description: '文案→分镜→生图→配音→合成视频',
    category: '多模态',
    nodes: [
      {
        id: 'start-1',
        type: 'start',
        position: { x: 30, y: 300 },
        data: { prompt: '介绍人工智能的一分钟短视频脚本' },
      },
      {
        id: 'llm-script',
        type: 'llm',
        position: { x: 280, y: 300 },
        data: { model: 'qwen2.5-coder:32b', prompt: '生成短视频分镜脚本，包含画面描述和配音文案' },
      },
      {
        id: 't2i-1',
        type: 'text2image',
        position: { x: 560, y: 200 },
        data: { prompt: '', width: 1024, height: 576 },
      },
      {
        id: 'tts-1',
        type: 'tts',
        position: { x: 560, y: 400 },
        data: { text: '' },
      },
      {
        id: 'video-1',
        type: 'video_compose',
        position: { x: 840, y: 300 },
        data: { output_path: 'data/generated/video.mp4' },
      },
      {
        id: 'end-1',
        type: 'end',
        position: { x: 1100, y: 300 },
        data: {},
      },
    ],
    edges: [
      { id: 'e1-2', source: 'start-1', target: 'llm-script' },
      { id: 'e2-3a', source: 'llm-script', target: 't2i-1' },
      { id: 'e2-3b', source: 'llm-script', target: 'tts-1' },
      { id: 'e3a-4', source: 't2i-1', target: 'video-1' },
      { id: 'e3b-4', source: 'tts-1', target: 'video-1' },
      { id: 'e4-5', source: 'video-1', target: 'end-1' },
    ],
  },
  {
    id: 'http-api-chain',
    name: 'API 链式调用',
    description: 'HTTP 请求→数据处理→AI 分析',
    category: '数据处理',
    nodes: [
      {
        id: 'start-1',
        type: 'start',
        position: { x: 50, y: 200 },
        data: { prompt: '' },
      },
      {
        id: 'http-1',
        type: 'http_request',
        position: { x: 300, y: 200 },
        data: {
          method: 'GET',
          url: 'https://api.example.com/data',
          headers: { 'Content-Type': 'application/json' },
          body: '',
        },
      },
      {
        id: 'llm-1',
        type: 'llm',
        position: { x: 580, y: 200 },
        data: { model: 'qwen2.5-coder:32b', prompt: '分析以下API返回的数据，总结关键信息：' },
      },
      {
        id: 'end-1',
        type: 'end',
        position: { x: 860, y: 200 },
        data: {},
      },
    ],
    edges: [
      { id: 'e1-2', source: 'start-1', target: 'http-1' },
      { id: 'e2-3', source: 'http-1', target: 'llm-1' },
      { id: 'e3-4', source: 'llm-1', target: 'end-1' },
    ],
  },
  {
    id: 'loop-refine',
    name: '迭代精炼',
    description: '同一个任务反复打磨 N 轮，每轮以上一轮结果为输入（循环·固定次数）',
    category: '循环与迭代',
    nodes: [
      {
        id: 'start-1',
        type: 'start',
        position: { x: 40, y: 180 },
        data: { prompt: '写一句关于秋天的短诗' },
      },
      {
        id: 'loop-1',
        type: 'loop',
        position: { x: 300, y: 180 },
        data: {
          mode: 'count',
          iterations: 3,
          max_iterations: 20,
          _inner: {
            name: '迭代精炼·内层',
            nodes: [
              { id: 's', type: 'start', position: { x: 0, y: 0 }, data: {} },
              {
                id: 'l',
                type: 'llm',
                position: { x: 220, y: 0 },
                data: {
                  model: 'qwen2.5-coder:32b',
                  prompt: '请改进下面这段文字，让它更精炼、更有画面感。只输出改进后的结果，不要解释：',
                },
              },
              { id: 'e', type: 'end', position: { x: 440, y: 0 }, data: {} },
            ],
            edges: [
              { id: 'ie1', source: 's', target: 'l' },
              { id: 'ie2', source: 'l', target: 'e' },
            ],
          },
        },
      },
      {
        id: 'end-1',
        type: 'end',
        position: { x: 580, y: 180 },
        data: {},
      },
    ],
    edges: [
      { id: 'e1', source: 'start-1', target: 'loop-1' },
      { id: 'e2', source: 'loop-1', target: 'end-1' },
    ],
  },
  {
    id: 'loop-batch',
    name: '批量处理',
    description: '多行输入逐条处理（循环·遍历列表），适合批量改写、批量描述',
    category: '循环与迭代',
    nodes: [
      {
        id: 'start-1',
        type: 'start',
        position: { x: 40, y: 180 },
        data: { prompt: '丹炉\n剑冢\n符箓' },
      },
      {
        id: 'loop-1',
        type: 'loop',
        position: { x: 300, y: 180 },
        data: {
          mode: 'foreach',
          max_iterations: 20,
          _inner: {
            name: '批量处理·内层',
            nodes: [
              { id: 's', type: 'start', position: { x: 0, y: 0 }, data: {} },
              {
                id: 'l',
                type: 'llm',
                position: { x: 220, y: 0 },
                data: {
                  model: 'qwen2.5-coder:32b',
                  prompt: '为下面这个修仙物品写一句 20 字以内的描述，只输出描述本身：',
                },
              },
              { id: 'e', type: 'end', position: { x: 440, y: 0 }, data: {} },
            ],
            edges: [
              { id: 'ie1', source: 's', target: 'l' },
              { id: 'ie2', source: 'l', target: 'e' },
            ],
          },
        },
      },
      {
        id: 'end-1',
        type: 'end',
        position: { x: 580, y: 180 },
        data: {},
      },
    ],
    edges: [
      { id: 'e1', source: 'start-1', target: 'loop-1' },
      { id: 'e2', source: 'loop-1', target: 'end-1' },
    ],
  },
  {
    id: 'loop-until',
    name: '重试到合格',
    description: '反复生成直到输出包含指定标记（循环·条件循环，带最大次数保护）',
    category: '循环与迭代',
    nodes: [
      {
        id: 'start-1',
        type: 'start',
        position: { x: 40, y: 180 },
        data: { prompt: '一句话介绍工作流引擎' },
      },
      {
        id: 'loop-1',
        type: 'loop',
        position: { x: 300, y: 180 },
        data: {
          mode: 'until',
          condition_type: 'contains',
          compare_value: '合格',
          max_iterations: 5,
          _inner: {
            name: '重试到合格·内层',
            nodes: [
              { id: 's', type: 'start', position: { x: 0, y: 0 }, data: {} },
              {
                id: 'l',
                type: 'llm',
                position: { x: 220, y: 0 },
                data: {
                  model: 'qwen2.5-coder:32b',
                  prompt: '审校下面这句话：若它已经通顺明确，就在结尾加上「合格」；否则只输出改写后的版本。',
                },
              },
              { id: 'e', type: 'end', position: { x: 440, y: 0 }, data: {} },
            ],
            edges: [
              { id: 'ie1', source: 's', target: 'l' },
              { id: 'ie2', source: 'l', target: 'e' },
            ],
          },
        },
      },
      {
        id: 'end-1',
        type: 'end',
        position: { x: 580, y: 180 },
        data: {},
      },
    ],
    edges: [
      { id: 'e1', source: 'start-1', target: 'loop-1' },
      { id: 'e2', source: 'loop-1', target: 'end-1' },
    ],
  },
]
