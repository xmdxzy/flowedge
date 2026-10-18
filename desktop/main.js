// FlowEdge 桌面客户端 —— Electron 监督者
// 职责：拉起 Go 后端 + 本地 ASR/TTS 服务 → 健康检查 → 开窗加载 UI → 退出时清理子进程
const { app, BrowserWindow, shell } = require('electron')

// 高显存占用 / 远程会话下 GPU 合成可能失败导致窗口空白——软件渲染保底
app.disableHardwareAcceleration()
const { spawn } = require('child_process')
const http = require('http')
const path = require('path')
const fs = require('fs')

const ROOT = path.resolve(__dirname, '..')

// 后端 exe 定位（三种运行形态）：
//  1. 安装包：resources/backend/FlowEdge.exe
//  2. 源码开发：../dist/FlowEdge.exe（单文件构建，前端已内嵌）
//  3. 兜底：../backend/flowedge-backend.exe
function resolveBackendExe() {
  if (process.env.FLOWEDGE_BACKEND_EXE) return process.env.FLOWEDGE_BACKEND_EXE
  const candidates = []
  if (app.isPackaged) {
    candidates.push(path.join(process.resourcesPath, 'backend', 'FlowEdge.exe'))
  }
  candidates.push(path.join(ROOT, 'dist', 'FlowEdge.exe'))
  candidates.push(path.join(ROOT, 'backend', 'flowedge-backend.exe'))
  for (const p of candidates) {
    if (fs.existsSync(p)) return p
  }
  return candidates[candidates.length - 1]
}

// 本地语音脚本目录：安装包在 resources/local-services，开发在 backend/local-services
function resolveLocalServices() {
  if (app.isPackaged) {
    const packed = path.join(process.resourcesPath, 'local-services')
    if (fs.existsSync(packed)) return packed
  }
  return path.join(ROOT, 'backend', 'local-services')
}

const BACKEND_EXE = resolveBackendExe()
const BACKEND_PORT = process.env.FLOWEDGE_PORT || '8080'
const BASE_URL = `http://127.0.0.1:${BACKEND_PORT}`
const LOCAL_SERVICES = resolveLocalServices()

let win = null
let children = []

function spawnChild(label, exe, args, opts = {}) {
  if (!fs.existsSync(exe)) {
    console.warn(`[flowedge] skip ${label}: not found ${exe}`)
    return null
  }
  const child = spawn(exe, args, {
    cwd: opts.cwd || path.dirname(exe),
    env: { ...process.env, ...(opts.env || {}) },
    stdio: ['ignore', 'pipe', 'pipe'],
    windowsHide: true,
  })
  child.stdout.on('data', (d) => console.log(`[${label}] ${d.toString().trim()}`))
  child.stderr.on('data', (d) => console.error(`[${label}] ${d.toString().trim()}`))
  child.on('exit', (code) => console.log(`[${label}] exited (${code})`))
  children.push(child)
  console.log(`[flowedge] started ${label} (pid ${child.pid})`)
  return child
}

function killAll() {
  for (const c of children) {
    try {
      if (c.pid && !c.killed) {
        // Windows 下递归终止进程树
        if (process.platform === 'win32') {
          spawn('taskkill', ['/PID', String(c.pid), '/T', '/F'], { windowsHide: true })
        } else {
          c.kill('SIGTERM')
        }
      }
    } catch (_) { /* ignore */ }
  }
  children = []
}

function waitFor(url, timeoutMs, intervalMs) {
  const started = Date.now()
  return new Promise((resolve, reject) => {
    const tick = () => {
      const req = http.get(url, (res) => {
        res.resume()
        resolve(res.statusCode)
      })
      req.on('error', () => {
        if (Date.now() - started > timeoutMs) reject(new Error(`timeout waiting ${url}`))
        else setTimeout(tick, intervalMs)
      })
      req.setTimeout(Math.min(intervalMs, 2000), () => req.destroy())
    }
    tick()
  })
}

function createWindow() {
  win = new BrowserWindow({
    width: 1500,
    height: 940,
    minWidth: 1200,
    minHeight: 760,
    backgroundColor: '#0A0D14',
    autoHideMenuBar: true,
    show: false,
    title: 'FlowEdge',
    webPreferences: {
      contextIsolation: true,
      nodeIntegration: false,
    },
  })

  win.once('ready-to-show', () => win.show())
  win.webContents.setWindowOpenHandler(({ url }) => {
    shell.openExternal(url)
    return { action: 'deny' }
  })
  const openWf = process.env.FLOWEDGE_OPEN_WORKFLOW
  win.loadURL(openWf ? `${BASE_URL}/?workflow=${openWf}` : BASE_URL)

  // 启动 5 秒后截屏存盘（用于自动化视觉验收）
  win.webContents.on('console-message', (_e, level, message) => {
    if (level >= 2) console.log(`[renderer:${level}] ${message}`)
  })
  win.webContents.on('did-finish-load', () => {
    setTimeout(async () => {
      // 可选：自动点选某个节点（UI 验收时把配置面板一并截进来）
      const clickText = process.env.FLOWEDGE_CLICK_TEXT
      if (clickText) {
        try {
          await win.webContents.executeJavaScript(
            `(() => {
              const btns = Array.from(document.querySelectorAll('button'))
              const btn = btns.find((b) => (b.textContent || '').trim() === ${JSON.stringify(clickText)})
              if (!btn) return 'button not found'
              btn.click()
              return 'clicked'
            })()`,
          )
          await new Promise((r) => setTimeout(r, 1800))
        } catch (err) {
          console.error('[flowedge] click text failed:', err.message)
        }
      }

      const clickNode = process.env.FLOWEDGE_CLICK_NODE
      if (clickNode) {
        try {
          await win.webContents.executeJavaScript(
            `(() => {
              const el = document.querySelector('[data-id="${clickNode}"]')
              if (!el) return 'node not found'
              el.dispatchEvent(new MouseEvent('pointerdown', { bubbles: true }))
              el.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }))
              el.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }))
              el.dispatchEvent(new MouseEvent('click', { bubbles: true }))
              return 'clicked'
            })()`,
          )
          await new Promise((r) => setTimeout(r, 1200))
        } catch (err) {
          console.error('[flowedge] click node failed:', err.message)
        }
      }
      try {
        const image = await win.webContents.capturePage()
        const out = process.env.FLOWEDGE_SCREENSHOT || path.join(__dirname, 'window-screenshot.png')
        fs.writeFileSync(out, image.toPNG())
        console.log(`[flowedge] screenshot saved: ${out}`)
      } catch (err) {
        console.error('[flowedge] screenshot failed:', err.message)
      }
    }, 8000)
  })
}

app.whenReady().then(async () => {
  // 1) 本地 ASR/TTS 服务（存在才拉起）
  const pyExe = path.join(LOCAL_SERVICES, '.venv', 'Scripts', 'python.exe')
  spawnChild('asr', pyExe, ['asr_server.py'], {
    cwd: LOCAL_SERVICES,
    env: {
      ASR_PORT: '8101',
      WHISPER_MODEL: 'small',
      HF_ENDPOINT: 'https://hf-mirror.com',
      HF_HUB_DISABLE_XET: '1',
    },
  })
  spawnChild('tts', pyExe, ['tts_server.py'], {
    cwd: LOCAL_SERVICES,
    env: { TTS_PORT: '8102' },
  })

  // 2) Go 后端（托管 API + 前端静态资源）
  spawnChild('backend', BACKEND_EXE, [], {
    cwd: path.dirname(BACKEND_EXE),
    env: {
      PORT: BACKEND_PORT,
      OLLAMA_BASE_URL: 'http://127.0.0.1:11434',
      ASR_BASE_URL: 'http://127.0.0.1:8101',
      TTS_BASE_URL: 'http://127.0.0.1:8102',
      COMFYUI_BASE_URL: 'http://127.0.0.1:8188',
    },
  })

  // 3) 健康检查 → 开窗
  try {
    await waitFor(`${BASE_URL}/api/workflows`, 15000, 500)
    console.log('[flowedge] backend healthy, opening window')
  } catch (err) {
    console.error('[flowedge]', err.message, '— opening window anyway')
  }
  createWindow()
})

app.on('window-all-closed', () => {
  killAll()
  app.quit()
})

app.on('before-quit', killAll)
process.on('exit', killAll)
