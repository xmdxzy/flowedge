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
  {
    id: 'podcast-voice',
    name: '文案转配音',
    description: 'LLM 润色文案 → 语音合成 → 产出 wav（Windows 内置语音，无需 Python）',
    category: '多模态',
    nodes: [
      { id: 'start-1', type: 'start', position: { x: 40, y: 160 }, data: { prompt: '写一段 60 秒的播客开场白，主题是本地 AI 工作流' } },
      {
        id: 'llm-1',
        type: 'llm',
        position: { x: 300, y: 160 },
        data: { model: '', prompt: '把下面内容改写成口语化、适合朗读的短段落，不要 Markdown：' },
      },
      { id: 'tts-1', type: 'tts', position: { x: 580, y: 160 }, data: { text: '', voice: '', rate: 0 } },
      { id: 'end-1', type: 'end', position: { x: 850, y: 160 }, data: {} },
    ],
    edges: [
      { id: 'e1', source: 'start-1', target: 'llm-1' },
      { id: 'e2', source: 'llm-1', target: 'tts-1' },
      { id: 'e3', source: 'tts-1', target: 'end-1' },
    ],
  },
  {
    id: 'storyboard-video',
    name: '分镜成片（全链路）',
    description: 'LLM 拆分镜 → 两路文生图 → 变量聚合 → 语音合成 → 视频合成',
    category: '多模态',
    nodes: [
      { id: 'start-1', type: 'start', position: { x: 40, y: 220 }, data: { prompt: '一座悬浮在云海之上的中式楼阁，黄昏，电影感' } },
      {
        id: 'llm-1',
        type: 'llm',
        position: { x: 280, y: 220 },
        data: { model: '', prompt: '把主题拆成两个镜头描述，每行一个，不要编号，不要解释：' },
      },
      {
        id: 'agg-1',
        type: 'aggregate',
        position: { x: 560, y: 60 },
        data: { mode: 'list', field: 'text', inputs: '', separator: '\n' },
      },
      { id: 't2i-1', type: 'text2image', position: { x: 560, y: 200 }, data: { prompt: '', width: 1024, height: 1024 } },
      { id: 't2i-2', type: 'text2image', position: { x: 560, y: 350 }, data: { prompt: '', width: 1024, height: 1024 } },
      { id: 'tts-1', type: 'tts', position: { x: 560, y: 500 }, data: { text: '', voice: '', rate: -1 } },
      { id: 'video-1', type: 'video_compose', position: { x: 850, y: 260 }, data: { image_duration: 3 } },
      { id: 'end-1', type: 'end', position: { x: 1120, y: 260 }, data: {} },
    ],
    edges: [
      { id: 'e1', source: 'start-1', target: 'llm-1' },
      { id: 'e2', source: 'llm-1', target: 't2i-1' },
      { id: 'e3', source: 'llm-1', target: 't2i-2' },
      { id: 'e4', source: 'llm-1', target: 'tts-1' },
      { id: 'e5', source: 't2i-1', target: 'agg-1' },
      { id: 'e6', source: 't2i-2', target: 'agg-1' },
      { id: 'e7', source: 't2i-1', target: 'video-1' },
      { id: 'e8', source: 't2i-2', target: 'video-1' },
      { id: 'e9', source: 'tts-1', target: 'video-1' },
      { id: 'e10', source: 'video-1', target: 'end-1' },
    ],
  },
  {
    id: 'transcribe-summarize',
    name: '录音转纪要',
    description: '语音转写 → LLM 整理成结构化纪要（需要 ASR 服务就绪）',
    category: '多模态',
    nodes: [
      { id: 'asr-1', type: 'asr', position: { x: 200, y: 160 }, data: { audio_path: '' } },
      {
        id: 'llm-1',
        type: 'llm',
        position: { x: 480, y: 160 },
        data: { model: '', prompt: '把下面这段语音转写整理成会议纪要：三个要点 + 待办清单。' },
      },
      { id: 'end-1', type: 'end', position: { x: 760, y: 160 }, data: {} },
    ],
    edges: [
      { id: 'e1', source: 'asr-1', target: 'llm-1' },
      { id: 'e2', source: 'llm-1', target: 'end-1' },
    ],
  },
  {
    id: 'image-qc',
    name: '出图质检（单图）',
    description: '文生图 → 本地 VLM 按统一标准验收：主体 / 构图 / 文字 / 瑕疵，给出结论',
    category: '质检',
    nodes: [
      {
        id: 'start-1',
        type: 'start',
        position: { x: 40, y: 180 },
        data: { prompt: 'a white deer on a snowy peak at dawn, chinese ink painting, minimal, negative space' },
      },
      { id: 't2i-1', type: 'text2image', position: { x: 320, y: 180 }, data: { prompt: '', width: 1024, height: 1024 } },
      {
        id: 'vision-1',
        type: 'vision',
        position: { x: 620, y: 180 },
        data: {
          prompt: '你是出图质检员。逐项检查：1) 主体是否与要求一致 2) 构图是否失衡/裁切 3) 有无乱码文字或水印 4) 有无明显瑕疵（多肢、穿模、模糊）。最后单独一行给结论：PASS 或 FAIL，FAIL 时写明原因。',
          model: '',
          image_path: '',
        },
      },
      { id: 'end-1', type: 'end', position: { x: 900, y: 180 }, data: {} },
    ],
    edges: [
      { id: 'e1', source: 'start-1', target: 't2i-1' },
      { id: 'e2', source: 't2i-1', target: 'vision-1' },
      { id: 'e3', source: 'vision-1', target: 'end-1' },
    ],
  },
  {
    id: 'image-qc-batch',
    name: '批量出图质检（三图对比）',
    description: '同一主题出三张 → 变量聚合 → 一次 VLM 调用横向评比，挑出可用的一张',
    category: '质检',
    nodes: [
      {
        id: 'start-1',
        type: 'start',
        position: { x: 40, y: 260 },
        data: { prompt: 'a lone white deer on a snowy mountain peak, chinese ink painting' },
      },
      { id: 't2i-1', type: 'text2image', position: { x: 320, y: 120 }, data: { prompt: '', width: 768, height: 768 } },
      { id: 't2i-2', type: 'text2image', position: { x: 320, y: 300 }, data: { prompt: 'cinematic photo, soft dawn light, 85mm lens', width: 768, height: 768 } },
      { id: 't2i-3', type: 'text2image', position: { x: 320, y: 480 }, data: { prompt: 'minimalist poster, large negative space, muted tones', width: 768, height: 768 } },
      {
        id: 'agg-1',
        type: 'aggregate',
        position: { x: 620, y: 300 },
        data: { mode: 'list', field: 'image', inputs: '', separator: '\n', include_empty: false },
      },
      {
        id: 'vision-1',
        type: 'vision',
        position: { x: 880, y: 300 },
        data: {
          prompt: '下面三张是同一主题的三种风格尝试（水墨 / 摄影 / 极简海报）。请逐张点评：画面实际内容、构图是否成立、有无文字水印、有无明显瑕疵；最后一行给出排序与首选（图1/图2/图3）。注意：对照每张实际画出来的东西评价，不要假设它符合某个未说明的设定。',
          model: '',
          image_path: '',
        },
      },
      { id: 'end-1', type: 'end', position: { x: 1160, y: 300 }, data: {} },
    ],
    edges: [
      { id: 'e1', source: 'start-1', target: 't2i-1' },
      { id: 'e2', source: 'start-1', target: 't2i-2' },
      { id: 'e3', source: 'start-1', target: 't2i-3' },
      { id: 'e4', source: 't2i-1', target: 'agg-1' },
      { id: 'e5', source: 't2i-2', target: 'agg-1' },
      { id: 'e6', source: 't2i-3', target: 'agg-1' },
      { id: 'e7', source: 'agg-1', target: 'vision-1' },
      { id: 'e8', source: 'vision-1', target: 'end-1' },
    ],
  },
  {
    id: 'storyboard-batch',
    name: '分镜批量出图（一次加载）',
    description: 'LLM 拆分镜 → 文生图按行批量（5 条实测 29 秒），比逐条省掉重复装载',
    category: '批量',
    nodes: [
      {
        id: 'start-1',
        type: 'start',
        position: { x: 40, y: 200 },
        data: { prompt: '黄昏雪山之巅，一只白鹿静立。请拆成 5 个分镜，每行一个英文画面描述，不要编号、不要解释。' },
      },
      {
        id: 'llm-1',
        type: 'llm',
        position: { x: 320, y: 200 },
        data: { model: '', prompt: '' },
      },
      {
        id: 't2i-1',
        type: 'text2image',
        position: { x: 620, y: 200 },
        data: {
          prompt: '',
          prompts: '',
          split_lines: true,
          width: 1024,
          height: 1024,
          batch: 1,
        },
      },
      { id: 'end-1', type: 'end', position: { x: 920, y: 200 }, data: {} },
    ],
    edges: [
      { id: 'e1', source: 'start-1', target: 'llm-1' },
      { id: 'e2', source: 'llm-1', target: 't2i-1' },
      { id: 'e3', source: 't2i-1', target: 'end-1' },
    ],
  },
  {
    id: 'h3-t2v',
    name: 'H3 文生视频（带声音）',
    description: '一次推理同时出画面与立体声，608×352×124 帧≈5 秒，12 步实测约 3 分钟（保音频质量）',
    category: '视频',
    nodes: [
      {
        id: 'start-1',
        type: 'start',
        position: { x: 40, y: 200 },
        data: { prompt: '黄昏雪山之巅，一只白鹿静立，慢速推镜，风吹起雪粒，远处有低沉风声与一声清越铃响，水墨质感，留白充足。' },
      },
      {
        id: 't2v-1',
        type: 'text2video',
        position: { x: 380, y: 200 },
        data: {
          prompt: '',
          backend: 'h3',
          width: 608,
          height: 352,
          frames: 124,
          fps: 24,
          steps: 12,
          negative: '',
          low_vram: false,
          segments: 1,
          interp: false,
          turbo: true,
        },
      },
      { id: 'end-1', type: 'end', position: { x: 720, y: 200 }, data: {} },
    ],
    edges: [
      { id: 'e1', source: 'start-1', target: 't2v-1' },
      { id: 'e2', source: 't2v-1', target: 'end-1' },
    ],
  },
  {
    id: 'h3-i2v-long',
    name: 'H3 图生视频 → 10 秒长镜头',
    description: '文生图定首帧 → H3 首帧引导 + 末帧续接 2 段拼接（实测 10.38 秒，带声）',
    category: '视频',
    nodes: [
      {
        id: 'start-1',
        type: 'start',
        position: { x: 40, y: 220 },
        data: { prompt: 'a lone white deer on a snowy peak at dawn, chinese ink painting, cinematic framing' },
      },
      { id: 't2i-1', type: 'text2image', position: { x: 320, y: 220 }, data: { prompt: '', width: 608, height: 352, batch: 1 } },
      {
        id: 't2v-1',
        type: 'text2video',
        position: { x: 620, y: 220 },
        data: {
          prompt: '镜头缓慢前推，鹿的头部轻微转动，雪粒被风吹起，远处风声与铃响',
          backend: 'h3',
          image_path: '',
          width: 608,
          height: 352,
          frames: 124,
          fps: 24,
          steps: 12,
          negative: '',
          low_vram: false,
          segments: 2,
          interp: false,
          turbo: true,
        },
      },
      { id: 'end-1', type: 'end', position: { x: 980, y: 220 }, data: {} },
    ],
    edges: [
      { id: 'e1', source: 'start-1', target: 't2i-1' },
      { id: 'e2', source: 't2i-1', target: 't2v-1' },
      { id: 'e3', source: 't2v-1', target: 'end-1' },
    ],
  },
  {
    id: 'img2img-restyle',
    name: '图生图（改风格）',
    description: '以已有图为底按强度重绘，0.85 以上才明显改风格（实测）',
    category: '图像',
    nodes: [
      { id: 'start-1', type: 'start', position: { x: 40, y: 180 }, data: { prompt: 'C:/path/to/source.png' } },
      {
        id: 'i2i-1', type: 'image2image', position: { x: 340, y: 180 },
        data: { prompt: 'watercolor painting on paper, soft washes, visible brush texture', image_path: '', strength: 0.85, steps: 10 },
      },
      { id: 'end-1', type: 'end', position: { x: 640, y: 180 }, data: {} },
    ],
    edges: [
      { id: 'e1', source: 'start-1', target: 'i2i-1' },
      { id: 'e2', source: 'i2i-1', target: 'end-1' },
    ],
  },
  {
    id: 'inpaint-patch',
    name: '局部重绘（蒙版）',
    description: '蒙版白色区域按提示词重画；蒙版必须与原图同尺寸',
    category: '图像',
    nodes: [
      { id: 'start-1', type: 'start', position: { x: 40, y: 180 }, data: { prompt: 'C:/path/to/source.png' } },
      {
        id: 'inp-1', type: 'inpaint', position: { x: 340, y: 180 },
        data: { prompt: 'a red paper lantern glowing', image_path: '', mask_path: '', denoise: 1.0, steps: 10 },
      },
      { id: 'end-1', type: 'end', position: { x: 640, y: 180 }, data: {} },
    ],
    edges: [
      { id: 'e1', source: 'start-1', target: 'inp-1' },
      { id: 'e2', source: 'inp-1', target: 'end-1' },
    ],
  },
  {
    id: 'upscale-x4',
    name: '超分放大 4x',
    description: 'RealESRGAN 放大，实测 768→3072；适合出图后交付前统一走一遍',
    category: '图像',
    nodes: [
      { id: 'start-1', type: 'start', position: { x: 40, y: 180 }, data: { prompt: 'C:/path/to/source.png' } },
      { id: 'up-1', type: 'upscale', position: { x: 340, y: 180 }, data: { image_path: '', model: 'RealESRGAN_x4plus.pth' } },
      { id: 'end-1', type: 'end', position: { x: 640, y: 180 }, data: {} },
    ],
    edges: [
      { id: 'e1', source: 'start-1', target: 'up-1' },
      { id: 'e2', source: 'up-1', target: 'end-1' },
    ],
  },
  {
    id: 'wan-t2v-fast',
    name: 'Wan 快速视频（无声）',
    description: '832×480×33 帧≈2 秒，实测 64 秒出片；适合镜头预览/节奏试验',
    category: '视频',
    nodes: [
      {
        id: 'start-1', type: 'start', position: { x: 40, y: 180 },
        data: { prompt: 'a white deer standing on a snowy mountain peak at dawn, slow camera push in, cinematic' },
      },
      {
        id: 't2v-1', type: 'text2video', position: { x: 380, y: 180 },
        data: { prompt: '', backend: 'wan', width: 832, height: 480, frames: 33, fps: 16, steps: 20, negative: 'blurry, low quality, watermark', low_vram: false, segments: 1, interp: false },
      },
      { id: 'end-1', type: 'end', position: { x: 720, y: 180 }, data: {} },
    ],
    edges: [
      { id: 'e1', source: 'start-1', target: 't2v-1' },
      { id: 'e2', source: 't2v-1', target: 'end-1' },
    ],
  },
  {
    id: 'short-drama-pipeline',
    name: '短剧流水线（分镜驱动）',
    description: '剧本 → 分镜表（景别/角度/轴线/运镜/衔接）→ 逐镜生成 → 按 cut/continue/dissolve 装配 + 音频抽检',
    category: '视频',
    nodes: [
      {
        id: 'start-1',
        type: 'start',
        position: { x: 40, y: 220 },
        data: {
          prompt: '雨夜旧客栈，师徒二人隔桌对峙。师傅要废掉徒弟修为，徒弟已决意不认错。全片无对白，只有雨声与油灯爆响。',
        },
      },
      {
        id: 'sb-1',
        type: 'storyboard',
        position: { x: 380, y: 220 },
        data: { brief: '', seconds: 30, max_shots: 6 },
      },
      {
        id: 'sl-1',
        type: 'shotlist',
        position: { x: 720, y: 220 },
        data: {
          plan: '',
          backend: 'h3',
          width: 608,
          height: 352,
          steps: 12,
          turbo: true,
          audio_check: true,
          interp: false,
        },
      },
      { id: 'end-1', type: 'end', position: { x: 1040, y: 220 }, data: {} },
    ],
    edges: [
      { id: 'e1', source: 'start-1', target: 'sb-1' },
      { id: 'e2', source: 'sb-1', target: 'sl-1' },
      { id: 'e3', source: 'sl-1', target: 'end-1' },
    ],
  },
]
