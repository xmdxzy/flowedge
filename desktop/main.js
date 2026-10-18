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
const BACKEND_EXE = process.env.FLOWEDGE_BACKEND_EXE || path.join(ROOT, 'backend', 'flowedge-backend.exe')
const BACKEND_PORT = process.env.FLOWEDGE_PORT || '8080'
const BASE_URL = `http://127.0.0.1:${BACKEND_PORT}`
const LOCAL_SERVICES = path.join(ROOT, 'backend', 'local-services')

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
  win.loadURL(BASE_URL)

  // 启动 5 秒后截屏存盘（用于自动化视觉验收）
  win.webContents.on('console-message', (_e, level, message) => {
    if (level >= 2) console.log(`[renderer:${level}] ${message}`)
  })
  win.webContents.on('did-finish-load', () => {
    setTimeout(async () => {
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
