// 节点默认数据：新建节点时 data 的初始值。
//
// 从 App.tsx 抽出来（原本是主组件里的一个 switch，约 100 行）。抽出的理由不是"行数好看"，
// 而是它**是纯数据**：能被契约测试覆盖（调色板里每种节点都必须有默认 data，
// 否则新建出来的节点字段全是 undefined），改它也不需要理解 React 状态。

export function getDefaultNodeData(type: string): Record<string, any> {
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
    case 'subtitles':
      // 三个来源按优先级：现成 srt → 分镜表 → 逐行台词（均分）
      return { video_path: '', srt_path: '', plan: '', dialogues: '', font_size: 20, margin_v: 28 }
    case 'post_production':
      // 竖屏默认 9:16 + 模糊垫底（横屏改竖屏不丢内容）；BGM 留空则不混
      return {
        aspect: '9:16', fit: 'blur', video_path: '',
        bgm: '', bgm_gain: -12, duck: true, loudness: 0, true_peak: -1.5,
        srt_path: '', plan: '', dialogues: '', font_size: 20, margin_v: 28,
      }
    case 'comfy_workflow':
      // workflow 存字符串（后端同时接受字符串与对象）；bindings 在面板里用
      // 「path=value」逐行编辑，避免用户正在敲 JSON 时被中间态打断。
      return { workflow: '', workflow_file: '', bindings: {}, prompt: '', timeout_sec: 900 }
    default:
      return {}
  }
}
