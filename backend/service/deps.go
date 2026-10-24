package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 依赖进程管理器
//
// 背景：ComfyUI / ASR 服务在「裸 exe + 双击」的交付形态下不该要求用户
// 先开三个终端窗口。这里把它们变成引擎可托管的后台依赖：
//   - 一键启动 / 停止（POST /api/deps/:name/start|stop）
//   - 启动前先探活，已在运行就复用（不会拉起第二个实例抢显存）
//   - 能力面板直接显示「谁在跑、PID 多少、命令是什么」
//
// 配置优先级：显式环境变量 > data/deps.json > 常见路径自动探测。

// DepStatus 依赖进程对外状态
type DepStatus struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	Running   bool   `json:"running"`   // 端口/健康检查通过
	Managed   bool   `json:"managed"`   // 由本进程拉起（可停止）
	Autostart bool   `json:"autostart"` // 启动时自动拉起
	PID       int    `json:"pid,omitempty"`
	Endpoint  string `json:"endpoint"`
	Command   string `json:"command,omitempty"`
	Hint      string `json:"hint,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// DepSpec 一个可托管依赖的启动描述
type DepSpec struct {
	Name       string
	Label      string
	Binary     string
	Args       []string
	WorkDir    string
	Env        []string
	Endpoint   string
	HealthPath string
	Autostart  bool
	Hint       string
	// Exclusive 指定互斥依赖：启动本项前先停掉对方。
	// 24GB 上两个 ComfyUI 实例不可能同时驻留（H3 峰值已 21.4GB）。
	Exclusive string
}

// Supervisor 依赖进程管理器
type Supervisor struct {
	mu      sync.Mutex
	specs   map[string]*DepSpec
	procs   map[string]*exec.Cmd
	started map[string]bool // 是否由本进程尝试启动过（用于区分「启动后秒退」与「从未启动」）
	lastErr map[string]string
	client  *http.Client
}

type depsFile struct {
	ComfyUI struct {
		Home      string   `json:"home"`
		Python    string   `json:"python"`
		Args      []string `json:"args"`
		Autostart bool     `json:"autostart"`
	} `json:"comfyui"`
	ASR struct {
		Home      string `json:"home"`
		Python    string `json:"python"`
		Autostart bool   `json:"autostart"`
	} `json:"asr"`
	TTS struct {
		Home      string `json:"home"`
		Script    string `json:"script"`
		Python    string `json:"python"`
		Port      string `json:"port"`
		BaseURL   string `json:"base_url"`
		Autostart bool   `json:"autostart"`
		// IdleUnload 空闲多少秒后释放模型显存（0 = 不释放）。
		// 「引擎启动不拉起」只解决开机那段，真正吃显存的是「用过一次之后一直挂着」——
		// 这个值让服务自己把显存还回去，不用人去点停止。
		IdleUnload int `json:"idle_unload"`
	} `json:"tts"`
}

// depsJSONPath 依赖配置文件的相对路径（引擎启动后会 chdir 到部署目录）
const depsJSONPath = "data/deps.json"

// LoadDepsFile 读 data/deps.json（不存在或损坏时返回零值，不报错）
func LoadDepsFile() depsFile {
	var conf depsFile
	if raw, err := os.ReadFile(depsJSONPath); err == nil {
		_ = json.Unmarshal(raw, &conf)
	}
	return conf
}

// TTSBaseURL 从配置里取 TTS 服务地址。
// 供 main.go 解析 TTS_BASE_URL 用：只靠环境变量的话，切换后端就必须重启外壳
// （外壳 spawn 引擎时不传 env，纯 os.Getenv 也没有 .env 支持），太脆。
func TTSBaseURL() string {
	return strings.TrimSpace(LoadDepsFile().TTS.BaseURL)
}

// NewSupervisor 依据配置构造依赖规格；comfyBase/asrBase 用于推导端口
func NewSupervisor(comfyBase, asrBase, ttsBase string) *Supervisor {
	s := &Supervisor{
		specs:   map[string]*DepSpec{},
		procs:   map[string]*exec.Cmd{},
		started: map[string]bool{},
		lastErr: map[string]string{},
		client:  &http.Client{Timeout: 1500 * time.Millisecond},
	}

	var conf = LoadDepsFile()

	// ---- ComfyUI ----
	comfyHome := firstNonEmpty(os.Getenv("COMFYUI_HOME"), conf.ComfyUI.Home, detectComfyHome())
	comfyPython := firstNonEmpty(os.Getenv("COMFYUI_PYTHON"), conf.ComfyUI.Python, detectVenvPython(comfyHome))
	comfyPort := portFromURL(comfyBase, "8188")
	comfyArgs := conf.ComfyUI.Args
	if raw := strings.TrimSpace(os.Getenv("COMFYUI_ARGS")); raw != "" {
		comfyArgs = strings.Fields(raw)
	}
	if len(comfyArgs) == 0 {
		// 常驻权重 + 预留 2GB：
		//   之前用 --disable-smart-memory 每张图都要把 17GB 重新装进显存，
		//   实测 5 张 768² 要 199s；改成常驻后同样 5 张只要 29s（6.9 倍）。
		//   换到语言侧时由引擎调 /free 并等 vram_free 真回来，不会互相抢爆。
		comfyArgs = []string{"--listen", "127.0.0.1", "--port", comfyPort, "--reserve-vram", "2"}
	}
	comfyAutostart := conf.ComfyUI.Autostart || os.Getenv("COMFYUI_AUTOSTART") == "1"
	comfyHint := "未找到 ComfyUI：设置 COMFYUI_HOME 指向安装目录（内含 main.py），或在 data/deps.json 里配置"
	if comfyHome != "" {
		comfyHint = ""
	}
	s.specs["comfyui"] = &DepSpec{
		Name:       "comfyui",
		Label:      "ComfyUI（图像 / Wan 视频）",
		Binary:     comfyPython,
		Args:       append([]string{filepath.Join(comfyHome, "main.py")}, comfyArgs...),
		WorkDir:    comfyHome,
		Endpoint:   normalBase(comfyBase),
		HealthPath: "/system_stats",
		Autostart:  comfyAutostart,
		Hint:       comfyHint,
		Exclusive:  "comfy_h3",
	}

	// ---- ComfyUI-H3（MiniMax H3 专用工作树）----
	//
	// 必须独立：H3 要 torch 2.14 + 完整量化后端（int8 convrot / nvfp4），
	// 主实例是 torch 2.6 走的兼容路径；启动参数也不同（--reserve-vram 4
	// --cache-none --use-sage-attention），且不能叠加 --disable-smart-memory
	// ——叠加会把更多权重挤到系统内存，直接拖垮 24GB 场景。
	h3Home := firstNonEmpty(os.Getenv("H3_HOME"), "D:/AI/ComfyUI-H3")
	h3Python := firstNonEmpty(os.Getenv("H3_PYTHON"), filepath.ToSlash(filepath.Join(h3Home, ".venv/Scripts/python.exe")))
	h3Port := "8189"
	if u, err := url.Parse(normalBase(comfyBase)); err == nil && u.Port() != "" {
		h3Port = "8189" // 固定独立端口，避免与默认实例互相误判健康状态
	}
	h3Hint := ""
	if _, err := os.Stat(h3Python); err != nil {
		h3Hint = "未找到 " + h3Python + "：H3 工作树或独立 venv 缺失（见 D:/AI/ComfyUI-H3/MINIMAX_H3_LOCAL.md）"
	} else if _, err := os.Stat(filepath.Join(h3Home, "main.py")); err != nil {
		h3Hint = "未找到 " + filepath.Join(h3Home, "main.py")
	}
	h3Args := []string{
		filepath.Join(h3Home, "main.py"),
		"--listen", "127.0.0.1", "--port", h3Port,
		"--input-directory", filepath.Join(comfyHome, "input"),
		"--output-directory", filepath.Join(comfyHome, "output"),
		"--user-directory", filepath.Join(comfyHome, "user"),
		"--database-url", "sqlite:///D:/AI/ComfyUI/user/comfyui-h3.db",
		"--reserve-vram", "4",
		"--cache-none",
		// 不再默认带 --use-sage-attention：该 venv 里 sageattention 实际没装，
		// 带上会让进程「启动即退出」（ComfyUI 直接 ERROR 退出，不是降级运行）。
	}
	// H3_ARGS 允许整体覆盖（例如装上 sageattention 后想重新开启）
	if raw := strings.TrimSpace(os.Getenv("H3_ARGS")); raw != "" {
		h3Args = append([]string{filepath.Join(h3Home, "main.py")}, strings.Fields(raw)...)
	}
	// sageattention 装好才加 --use-sage-attention：带了但没装，ComfyUI 会直接退出。
	// 每次启动前实测一次 import（约 1.5s），比让人记住「卸载依赖也要改脚本」可靠。
	// 实测结论：本机（3090 + torch 2.14 + ComfyUI-H3）开 sage 反而慢 70%、显存更高
	// （608×352×124/20 步：206s / 23.8GB vs 不开 120s / 21.8GB），所以默认关闭。
	// 想自己试：设 H3_SAGE=1（仍需 sageattention 可导入）。
	if os.Getenv("H3_SAGE") == "1" && hasSageAttention(h3Python) {
		h3Args = append(h3Args, "--use-sage-attention")
	}
	h3Env := append(os.Environ(), "PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True")
	s.specs["comfy_h3"] = &DepSpec{
		Name:       "comfy_h3",
		Label:      "ComfyUI-H3（音视频联合生成）",
		Binary:     h3Python,
		Args:       h3Args,
		WorkDir:    h3Home,
		Env:        h3Env,
		Endpoint:   "http://127.0.0.1:" + h3Port,
		HealthPath: "/system_stats",
		Autostart:  false,
		Hint:       h3Hint,
		Exclusive:  "comfyui",
	}

	// ---- ComfyUI-MOSS（MOSS-Image 专用工作树）----
	//
	// 为什么必须独立：MOSS 的节点（UNETLoader 的 moss 权重 / CLIPLoader type=moss_image /
	// EmptyFlux2LatentImage）是 ComfyUI 0.41 才有的，主实例停在 0.31
	// （comfy-kitchen 0.2.27 + torch 2.6 的血泪组合，升级会把现有 flux 出图一起搞坏）。
	// 复用 H3 的 venv（同 commit、同 torch 2.14）而不是再装一套——省 ~10GB，
	// 且两者本来就同属 gpu-exclusive 组，不会同时驻留。
	mossHome := firstNonEmpty(os.Getenv("MOSS_HOME"), "D:/AI/ComfyUI-MOSS")
	// 注意这里必须用 firstExisting 而不是 firstNonEmpty：
	// MOSS 工作树复用 H3 的 venv，自己那份 `.venv` 并不存在。
	// firstNonEmpty 只看字符串非空，会选中不存在的路径，然后 hint 报「venv 缺失」——
	// 一个能跑的配置被警告成缺件（2026-10-23 实测踩到）。
	mossPython := firstNonEmpty(os.Getenv("MOSS_PYTHON"), firstExisting(
		filepath.ToSlash(filepath.Join(mossHome, ".venv/Scripts/python.exe")),
		filepath.ToSlash(filepath.Join(h3Home, ".venv/Scripts/python.exe"))))
	mossPort := "8190"
	if u, err := url.Parse(mossInstanceURL()); err == nil && u.Port() != "" {
		mossPort = u.Port()
	}
	mossHint := ""
	if _, err := os.Stat(mossPython); err != nil {
		mossHint = "未找到 " + mossPython + "：MOSS 工作树或可用的 torch 2.14 venv 缺失"
	} else if _, err := os.Stat(filepath.Join(mossHome, "main.py")); err != nil {
		mossHint = "未找到 " + filepath.Join(mossHome, "main.py") + "（MOSS 工作树缺失）"
	}
	mossArgs := []string{
		filepath.Join(mossHome, "main.py"),
		"--listen", "127.0.0.1", "--port", mossPort,
		"--input-directory", filepath.Join(comfyHome, "input"),
		"--output-directory", filepath.Join(comfyHome, "output"),
		"--user-directory", filepath.Join(mossHome, "user"),
		"--database-url", "sqlite:///D:/AI/ComfyUI-MOSS/user/comfyui-moss.db",
		"--reserve-vram", "2",
	}
	if raw := strings.TrimSpace(os.Getenv("MOSS_ARGS")); raw != "" {
		mossArgs = append([]string{filepath.Join(mossHome, "main.py")}, strings.Fields(raw)...)
	}
	mossEnv := append(os.Environ(), "PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True")
	s.specs["comfy_moss"] = &DepSpec{
		Name:       "comfy_moss",
		Label:      "ComfyUI-MOSS（图内文字渲染 / 布局控制）",
		Binary:     mossPython,
		Args:       mossArgs,
		WorkDir:    mossHome,
		Env:        mossEnv,
		Endpoint:   "http://127.0.0.1:" + mossPort,
		HealthPath: "/system_stats",
		Autostart:  false, // 按需拉起：只有后端选到 MOSS 才需要它
		Hint:       mossHint,
		Exclusive:  "comfyui",
	}

	// ---- ASR（faster-whisper 本地服务）----
	asrHome := firstNonEmpty(os.Getenv("ASR_HOME"), conf.ASR.Home, detectASRHome())
	asrPython := firstNonEmpty(os.Getenv("ASR_PYTHON"), conf.ASR.Python, detectVenvPython(asrHome))
	asrAutostart := conf.ASR.Autostart || os.Getenv("ASR_AUTOSTART") == "1"
	asrHint := "未找到 ASR 服务目录（需含 asr_server.py）：运行 FlowEdge.exe --export-services 导出到 local-services/ 后安装依赖"
	if asrHome != "" {
		asrHint = ""
	}
	env := append(os.Environ(), "ASR_PORT="+portFromURL(asrBase, "8101"))
	// 国内网络默认走镜像（HuggingFace 直连基本不通，模型下载会卡死）
	if os.Getenv("HF_ENDPOINT") == "" && os.Getenv("FLOWEDGE_HF_MIRROR") != "0" {
		env = append(env, "HF_ENDPOINT=https://hf-mirror.com")
	}
	s.specs["asr"] = &DepSpec{
		Name:       "asr",
		Label:      "ASR（faster-whisper）",
		Binary:     asrPython,
		Args:       []string{filepath.Join(asrHome, "asr_server.py")},
		WorkDir:    asrHome,
		Env:        env,
		Endpoint:   normalBase(asrBase),
		HealthPath: "/health",
		Autostart:  asrAutostart,
		Hint:       asrHint,
	}

	// ---- TTS（Kokoro-82M 默认 / Audio8-TTS 可选）----
	//
	// 三个脚本共享同一套 HTTP 契约（POST /tts {text,voice} -> wav），所以换引擎
	// 只需要换脚本名，不需要改节点、能力探测或预检：
	//   TTS_SCRIPT=tts_audio8_pt_server.py TTS_PYTHON=D:/AI/envs/torch-cu124/Scripts/python.exe
	//   TTS_PORT=8103 TTS_BASE_URL=http://127.0.0.1:8103
	// Kokoro 有固定音色 + 语速；Audio8 支持零样本克隆 + 44.1kHz，但没有语速。
	// Audio8 有两个后端：0.1B ONNX（CPU，本机实测产出无效语音）与 0.6B PyTorch（CUDA，可用）。
	ttsHome := firstNonEmpty(os.Getenv("TTS_HOME"), conf.TTS.Home, asrHome)
	ttsScriptName := firstNonEmpty(os.Getenv("TTS_SCRIPT"), conf.TTS.Script, "tts_kokoro_server.py")
	ttsScript := ttsScriptName
	if !filepath.IsAbs(ttsScriptName) {
		ttsScript = filepath.Join(ttsHome, ttsScriptName)
	}
	ttsPython := firstNonEmpty(os.Getenv("TTS_PYTHON"), conf.TTS.Python, detectVenvPython(ttsHome))
	ttsAutostart := conf.TTS.Autostart || os.Getenv("TTS_AUTOSTART") == "1"
	ttsLabel := "TTS（Kokoro-82M）"
	defaultTtsPort := "8102"
	if ttsScriptName != "tts_kokoro_server.py" {
		ttsLabel = "TTS（" + strings.TrimSuffix(strings.TrimPrefix(ttsScriptName, "tts_"), "_server.py") + "）"
		defaultTtsPort = "8103"
	}
	// data/deps.json 的 tts.port 优先于按脚本名推的默认端口
	if p := strings.TrimSpace(conf.TTS.Port); p != "" {
		defaultTtsPort = p
	}
	ttsHint := ""
	if _, err := os.Stat(ttsScript); err != nil {
		ttsHint = "未找到 " + ttsScriptName + "：运行 FlowEdge.exe --export-services 导出脚本后安装依赖" +
			"（Kokoro: pip install kokoro；Audio8-ONNX: pip install -r onnx_runtime_0_1b_int8/requirements.txt；" +
			"Audio8-PyTorch: 需 CUDA torch 环境，用 TTS_PYTHON 指向共享底座）"
	}
	ttsEnv := append(os.Environ(),
		"TTS_PORT="+portFromURL(ttsBase, defaultTtsPort),
		"KOKORO_DIR="+firstNonEmpty(os.Getenv("KOKORO_DIR"), "D:/AI/Models/tts"),
		// Audio8-ONNX 专用；Kokoro/PyTorch 不读这几个变量，无条件传无害
		"ARKTTS_RUNTIME_DIR="+firstNonEmpty(os.Getenv("ARKTTS_RUNTIME_DIR"), "D:/AI/Audio8_TTS/onnx_runtime_0_1b_int8"),
		"ARKTTS_MODEL_DIR="+firstNonEmpty(os.Getenv("ARKTTS_MODEL_DIR"), "D:/AI/Models/audio8_tts_0_1b_int8"),
		"ARKTTS_VOICES_DIR="+firstNonEmpty(os.Getenv("ARKTTS_VOICES_DIR"), filepath.Join(ttsHome, "voices-audio8")),
		// Audio8-PyTorch 专用
		"AUDIO8_MODEL_DIR="+firstNonEmpty(os.Getenv("AUDIO8_MODEL_DIR"), "D:/AI/Models/audio8_tts_0_6b"),
		"AUDIO8_VOICES_DIR="+firstNonEmpty(os.Getenv("AUDIO8_VOICES_DIR"), filepath.Join(ttsHome, "voices-audio8-pt")),
		"AUDIO8_DTYPE="+firstNonEmpty(os.Getenv("AUDIO8_DTYPE"), "bfloat16"),
		// 空闲卸载：0 = 常驻不释放；>0 = 闲置这么久后自动把模型从显存卸掉（进程留着，
		// 下次合成再懒加载）。这是「取消常驻」的完整解法——不用人记得点停止。
		"AUDIO8_IDLE_UNLOAD="+firstNonEmpty(os.Getenv("AUDIO8_IDLE_UNLOAD"), strconv.Itoa(conf.TTS.IdleUnload)),
	)
	if os.Getenv("HF_ENDPOINT") == "" && os.Getenv("FLOWEDGE_HF_MIRROR") != "0" {
		ttsEnv = append(ttsEnv, "HF_ENDPOINT=https://hf-mirror.com")
	}
	s.specs["tts"] = &DepSpec{
		Name:       "tts",
		Label:      ttsLabel,
		Binary:     ttsPython,
		Args:       []string{ttsScript},
		WorkDir:    ttsHome,
		Env:        ttsEnv,
		Endpoint:   normalBase(ttsBase),
		HealthPath: "/health",
		Autostart:  ttsAutostart,
		Hint:       ttsHint,
	}

	return s
}

// AutoStartAll 拉起标记为 autostart 且具备启动条件的依赖
func (s *Supervisor) AutoStartAll() {
	for name, spec := range s.specs {
		if spec.Autostart && s.Startable(name) {
			go func(n string) {
				if err := s.Start(n); err != nil {
					fmt.Printf("[deps] %s 自动启动失败：%v\n", n, err)
				}
			}(name)
		}
	}
}

// Startable 是否具备启动条件（二进制与脚本都在）
func (s *Supervisor) Startable(name string) bool {
	spec, ok := s.specs[name]
	if !ok || spec.Binary == "" || len(spec.Args) == 0 {
		return false
	}
	if _, err := os.Stat(spec.Binary); err != nil {
		return false
	}
	if _, err := os.Stat(spec.Args[0]); err != nil {
		return false
	}
	return true
}

// Start 启动依赖（已就绪则直接返回）
func (s *Supervisor) Start(name string) error {
	spec, ok := s.specs[name]
	if !ok {
		return fmt.Errorf("未知依赖：%s", name)
	}
	if s.Healthy(name) {
		return nil
	}
	if !s.Startable(name) {
		hint := spec.Hint
		if hint == "" {
			hint = "启动条件不满足：找不到可执行文件或脚本"
		}
		return fmt.Errorf("%s", hint)
	}

	if spec.Exclusive != "" && s.Healthy(spec.Exclusive) {
		_ = s.Stop(spec.Exclusive) // 24GB 装不下两个实例：先让位再启动
		time.Sleep(2 * time.Second)
	}

	s.mu.Lock()
	if cmd, exists := s.procs[name]; exists && cmd.Process != nil {
		s.mu.Unlock()
		// 已经在启动中的进程，仍要按该依赖的冷启动上限等。
		// 这里原来写死 3s：H3 冷启要 ~50s，导致「先在环境面板点了启动、再跑工作流」
		// 这种最常见的路径必然报「3s 内未就绪」。
		return s.WaitReady(name, readyTimeout(name))
	}
	cmd := exec.Command(spec.Binary, spec.Args...)
	cmd.Dir = spec.WorkDir
	if len(spec.Env) > 0 {
		cmd.Env = spec.Env
	}
	cmd.SysProcAttr = hiddenWindowAttr()
	if err := cmd.Start(); err != nil {
		s.lastErr[name] = err.Error()
		s.mu.Unlock()
		return fmt.Errorf("启动 %s 失败：%w", spec.Label, err)
	}
	s.procs[name] = cmd
	s.started[name] = true
	s.lastErr[name] = ""
	s.mu.Unlock()

	// 回收僵尸进程（否则 Windows 上 cmd.ProcessState 永远不更新）
	go func() {
		_ = cmd.Wait()
		s.mu.Lock()
		if s.procs[name] == cmd {
			delete(s.procs, name)
		}
		s.mu.Unlock()
	}()

	timeout := readyTimeout(name)
	if err := s.WaitReady(name, timeout); err != nil {
		s.mu.Lock()
		s.lastErr[name] = err.Error()
		s.mu.Unlock()
		return err
	}
	return nil
}

// Stop 停止由本进程拉起的依赖（外部启动的进程不碰）
func (s *Supervisor) Stop(name string) error {
	s.mu.Lock()
	cmd, ok := s.procs[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%s 不是由本进程拉起的，未做任何操作", name)
	}
	delete(s.procs, name)
	s.mu.Unlock()
	if cmd.Process != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// Healthy 依赖是否已就绪（健康检查优先，失败再退化为端口探测）
func (s *Supervisor) Healthy(name string) bool {
	spec, ok := s.specs[name]
	if !ok || spec.Endpoint == "" {
		return false
	}
	if spec.HealthPath != "" {
		resp, err := s.client.Get(spec.Endpoint + spec.HealthPath)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
	}
	u, err := url.Parse(spec.Endpoint)
	if err != nil {
		return false
	}
	conn, err := net.DialTimeout("tcp", u.Host, 800*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// WaitReady 等到依赖可连通
func (s *Supervisor) WaitReady(name string, timeout time.Duration) error {
	spec := s.specs[name]
	label := name
	if spec != nil && spec.Label != "" {
		label = spec.Label
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.Healthy(name) {
			return nil
		}
		s.mu.Lock()
		_, alive := s.procs[name]
		wasStarted := s.started[name]
		s.mu.Unlock()
		if wasStarted && !alive {
			cmdLine := ""
			if spec != nil {
				cmdLine = spec.Binary + " " + strings.Join(spec.Args, " ")
			}
			return fmt.Errorf("%s 启动后立即退出，请检查日志（命令：%s）", label, cmdLine)
		}
		time.Sleep(700 * time.Millisecond)
	}
	return fmt.Errorf("%s 在 %s 内未就绪", label, timeout)
}

// Status 全部依赖状态
func (s *Supervisor) Status() []DepStatus {
	out := make([]DepStatus, 0, len(s.specs))
	names := make([]string, 0, len(s.specs))
	for n := range s.specs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		spec := s.specs[n]
		st := DepStatus{
			Name:      spec.Name,
			Label:     spec.Label,
			Endpoint:  spec.Endpoint,
			Autostart: spec.Autostart,
			Hint:      spec.Hint,
			Running:   s.Healthy(n),
		}
		s.mu.Lock()
		if cmd, ok := s.procs[n]; ok {
			st.Managed = true
			if cmd.Process != nil {
				st.PID = cmd.Process.Pid
			}
		}
		st.LastError = s.lastErr[n]
		s.mu.Unlock()
		if spec.Binary != "" {
			st.Command = spec.Binary + " " + strings.Join(spec.Args, " ")
		}
		out = append(out, st)
	}
	return out
}

func (s *Supervisor) Spec(name string) (DepSpec, bool) {
	sp, ok := s.specs[name]
	if !ok {
		return DepSpec{}, false
	}
	return *sp, true
}

// ttsEnvDigest 抽出与 TTS 相关的环境变量子集（用于比较差异）。
// idle_unload / dtype / 模型目录这些都是靠 Env 注入的，不比较 Env 就会漏报
// ——2026-10-23 实测：把 idle_unload 从 600 改成 30，Reload 返回的 changes 是空对象。
func ttsEnvDigest(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		for _, prefix := range []string{"TTS_", "AUDIO8_", "ARKTTS_", "KOKORO_"} {
			if strings.HasPrefix(k, prefix) {
				out[k] = v
				break
			}
		}
	}
	return out
}

// Reload 重新读 data/deps.json 重建启动规格，返回变更清单。
//
// 为什么需要它：autostart / idle_unload 这类字段只在 NewSupervisor 时读一次。
// 改完配置不重启引擎的话，面板会一直显示旧值 —— 改了没生效、也没人知道。
// 2026-10-23 就踩过：把 autostart 改成 false 后 /api/deps 还报 true，只能靠重启引擎兜回来。
//
// 边界：只重建「启动方式」类字段（脚本/解释器/端口/参数/autostart/idle_unload）。
// **base_url 改了仍需重启引擎**——TTSService 等是独立的对象，不归本结构管，
// 这里会在变更清单里把它标出来，避免「改了地址却连老地址」的静默错配。
// 已在跑的进程不受影响（procs/started/lastErr 都保留）。
func (s *Supervisor) Reload() map[string]string {
	s.mu.Lock()
	prev := make(map[string]DepSpec, len(s.specs))
	baseline := make(map[string]string, len(s.specs))
	for k, v := range s.specs {
		prev[k] = *v
		baseline[k] = v.Endpoint
	}
	s.mu.Unlock()

	fresh := NewSupervisor(baseline["comfyui"], baseline["asr"], baseline["tts"])

	changes := map[string]string{}
	s.mu.Lock()
	for k, v := range fresh.specs {
		if p, ok := prev[k]; ok {
			if p.Binary != v.Binary {
				changes[k+".binary"] = p.Binary + " -> " + v.Binary
			}
			if got, want := strings.Join(p.Args, " "), strings.Join(v.Args, " "); got != want {
				changes[k+".args"] = got + " -> " + want
			}
			if p.Autostart != v.Autostart {
				changes[k+".autostart"] = fmt.Sprintf("%v -> %v", p.Autostart, v.Autostart)
			}
			if p.Endpoint != v.Endpoint {
				changes[k+".endpoint"] = p.Endpoint + " -> " + v.Endpoint + "（需重启引擎才生效）"
			}
			// Env 差异：只看 TTS 相关前缀，避免把整份 os.Environ 的噪音算成变更
			pe, ve := ttsEnvDigest(p.Env), ttsEnvDigest(v.Env)
			for key, want := range ve {
				if got, ok := pe[key]; !ok || got != want {
					changes[k+".env."+key] = got + " -> " + want
				}
			}
		}
		s.specs[k] = v
	}
	s.mu.Unlock()
	return changes
}

// ===== 探测辅助 =====

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// firstExisting 返回第一个**文件真实存在**的路径。
//
// 与 firstNonEmpty 的区别是"配置了"与"装好了"的区别：
// 复用别人 venv 的场景下（MOSS 用 H3 的），写在前面那个路径本来就不该存在，
// 用 firstNonEmpty 会选中它，然后在 os.Stat 处报「缺失」——把能跑的配置说成缺件。
func firstExisting(paths ...string) string {
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func detectVenvPython(home string) string {
	if home == "" {
		return ""
	}
	cands := []string{
		filepath.Join(home, ".venv", "Scripts", "python.exe"),
		filepath.Join(home, "venv", "Scripts", "python.exe"),
		filepath.Join(home, "python_embeded", "python.exe"),
		filepath.Join(home, "python_embeded", "python.exe"),
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	if p, err := exec.LookPath("python"); err == nil {
		return p
	}
	return ""
}

// detectComfyHome 常见安装路径探测（不写死某一个盘，命中即用）
func detectComfyHome() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	cands := []string{
		`D:\AI\ComfyUI`, `C:\AI\ComfyUI`, `D:\ComfyUI`, `C:\ComfyUI`,
		`E:\AI\ComfyUI`, `D:\AI\ComfyUI_windows_portable\ComfyUI`,
	}
	for _, c := range cands {
		if st, err := os.Stat(filepath.Join(c, "main.py")); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

func detectASRHome() string {
	if base, err := os.Getwd(); err == nil {
		for _, c := range []string{
			filepath.Join(base, "local-services"),
			filepath.Join(base, "data", "local-services"),
		} {
			if st, err := os.Stat(filepath.Join(c, "asr_server.py")); err == nil && !st.IsDir() {
				return c
			}
		}
	}
	return ""
}

// DetectASRHome 对外暴露的 ASR 服务目录探测（main 用它决定默认 ASR 端点）
func DetectASRHome() string { return detectASRHome() }

// hasSageAttention 实测某个 python 能否 import sageattention（含 triton 依赖）
func hasSageAttention(python string) bool {
	if python == "" {
		return false
	}
	// 实测该 venv 里 `import torch + sageattention` 要 6.5s，超时给短了会误判成「没装」
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", "from sageattention import sageattn")
	cmd.SysProcAttr = hiddenWindowAttr()
	return cmd.Run() == nil
}

// DetectKokoroScript 探测本地是否已有 Kokoro TTS 脚本（有则默认启用该端点）
func DetectKokoroScript() string {
	home := detectASRHome()
	if home == "" {
		return ""
	}
	p := filepath.Join(home, "tts_kokoro_server.py")
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p
	}
	return ""
}

func portFromURL(raw, def string) string {
	if raw == "" {
		return def
	}
	u, err := url.Parse(raw)
	if err != nil || u.Port() == "" {
		return def
	}
	return u.Port()
}

func normalBase(raw string) string {
	if raw == "" {
		return ""
	}
	return strings.TrimRight(raw, "/")
}

// readyTimeout 各依赖的冷启动等待上限。
//
// 为什么不能统一给一个数：ASR 首次要预热模型（分钟级），H3 要载入 20GB 权重（~50s），
// 而主 ComfyUI 常在热态。之前"已在启动中"的分支写死 3s，等于把正常冷启判成失败。
func readyTimeout(name string) time.Duration {
	switch name {
	case "asr", "tts":
		return 300 * time.Second // 首次要下载/预热模型（Kokoro 还要载入 pipeline）
	case "comfy_h3":
		return 180 * time.Second // 独立 venv + 20GB 权重，实测冷启 ~50s
	case "comfy_moss":
		return 300 * time.Second // 实测冷启 ~130s（988 节点注册 + torch 2.14 导入），180s 会误判失败
	default:
		return 120 * time.Second
	}
}
