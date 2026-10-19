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

	var conf depsFile
	if raw, err := os.ReadFile(filepath.Join("data", "deps.json")); err == nil {
		_ = json.Unmarshal(raw, &conf)
	}

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

	// ---- TTS（Kokoro-82M 本地神经语音）----
	ttsHome := firstNonEmpty(os.Getenv("TTS_HOME"), asrHome)
	ttsScript := filepath.Join(ttsHome, "tts_kokoro_server.py")
	ttsPython := firstNonEmpty(os.Getenv("TTS_PYTHON"), detectVenvPython(ttsHome))
	ttsAutostart := os.Getenv("TTS_AUTOSTART") == "1"
	ttsHint := ""
	if _, err := os.Stat(ttsScript); err != nil {
		ttsHint = "未找到 tts_kokoro_server.py：运行 FlowEdge.exe --export-services 导出脚本后安装依赖（pip install kokoro soundfile）"
	}
	ttsEnv := append(os.Environ(),
		"TTS_PORT="+portFromURL(ttsBase, "8102"),
		"KOKORO_DIR="+firstNonEmpty(os.Getenv("KOKORO_DIR"), "D:/AI/Models/tts"),
	)
	if os.Getenv("HF_ENDPOINT") == "" && os.Getenv("FLOWEDGE_HF_MIRROR") != "0" {
		ttsEnv = append(ttsEnv, "HF_ENDPOINT=https://hf-mirror.com")
	}
	s.specs["tts"] = &DepSpec{
		Name:       "tts",
		Label:      "TTS（Kokoro-82M）",
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

// ===== 探测辅助 =====

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
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
	default:
		return 120 * time.Second
	}
}
