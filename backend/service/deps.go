package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	Running   bool   `json:"running"`   // 端口/健康检查通过，**或**进程活着只是忙
	Managed   bool   `json:"managed"`   // 由本进程拉起（可停止）
	Autostart bool   `json:"autostart"` // 启动时自动拉起
	// Busy 进程明明活着，但 HTTP 忙到不回健康检查。
	//
	// 由来（2026-10-26 实测）：H3 在换页状态里 GPU 100%、占 23.8GB，
	// 但 /system_stats 与 /queue 全都超时不回 → 旧逻辑判 Running=false。
	// 面板于是显示「未启动」，用户照着点「启动」→ **又派生一份实例**，
	// 两个进程抢一张卡 —— 这正是那晚 27 倍慢的起点。
	// 「忙」和「死」必须分开：忙的时候该禁用启动按钮，而不是鼓励再起一个。
	Busy      bool   `json:"busy,omitempty"`
	State     string `json:"state,omitempty"` // ok / busy / stopped
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

	// logDir 托管进程的 stdout/stderr 落盘目录（<部署目录>/logs）。
	// 空字符串 = 丢弃输出，与改造前行为一致（测试里就不必造目录）。
	logDir   string
	procLogs map[string][]*os.File

	// startLocks 每个依赖一把「单飞」启动锁：Start 全程持有，
	// 防止冷启动窗口内被第二次调用再派生一个实例（见 Start 注释）。
	startLocks map[string]*sync.Mutex
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
	// Ollama 段：视频提示词优化、拆镜、分镜规划都要它。
	// 不给配置时按常见安装位置自动探测（见 defaultOllamaBinary）。
	Ollama struct {
		Binary    string   `json:"binary"`
		Args      []string `json:"args"`
		BaseURL   string   `json:"base_url"`
		Autostart bool     `json:"autostart"`
	} `json:"ollama"`
	// H3 段：ComfyUI-H3 独立工作树的启动参数。
	//
	// ReserveVRAM 单独拎出来是因为它是最常被调的那个：24GB 卡上 4GB 预留属于
	// 「算术不可满足」（见 applyH3Config 注释），做 A/B 时要能一键改、一键回滚。
	// Args 是整体覆盖口子（等价于环境变量 H3_ARGS，但能写进配置、可 reload）。
	H3 struct {
		Args        []string `json:"args"`
		ReserveVRAM *int     `json:"reserve_vram"`
		// CacheNone 控制 ComfyUI 的 --cache-none（缺省 true，保持既有行为）。
		//
		// 设成 false 会保留缓存。依据（2026-10-26 实测，ComfyUI 自己的日志）：
		//   `Model MiniMaxH3 prepared for dynamic VRAM loading. 19995MB Staged`
		//   → `Model Initializing ...` 4 分 14 秒才 complete，之后 8 步采样只用 45 秒
		//   （1.6 it/s）；而同一进程里已初始化过的提交，12 步只要 29 秒（2.17 s/it）。
		// 也就是说"每轮 4 分钟初始化"很可能就是 --cache-none 每次重做 staging 造成的。
		// 这是待 A/B 的假设，不是已证结论——所以做成配置，不直接改默认。
		CacheNone *bool `json:"cache_none"`
	} `json:"h3"`
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
		// Device TTS 后端的运行设备（cpu / cuda / 留空 = 服务自己探测）。
		//
		// 2026-10-27 起 Go 侧真的用它做**调度决策**：device=cpu 的 TTS 不占显存，
		// voice 才能进 CPU 道与出片并行（见 queue.go JobClassForNodes）。
		// 之前这个键只给 Python 服务自己读，Go 侧解析不到 ⇒ 队列只能一刀切按 GPU 串行。
		Device string `json:"device"`
	} `json:"tts"`
	// Acestep 段：ACE-Step 1.5 音乐生成服务（本地 GPU，XL 权重约 9GB）。
	//
	// 与 TTS 段的差别（别照抄 TTS 的字段就不看差异）：
	//   - 没有 idle_unload：上游服务不提供 /unload，释放显存只能停进程，
	//     所以「空闲卸载」由引擎侧的 idle_release 统一管（它会 Stop 本服务）；
	//   - model/lm_model/lm_backend 是**模型选择**：XL turbo 8 步是日常档，
	//     xl-sft 质量更高但要 50 步（慢一个数量级）。
	Acestep struct {
		Home      string `json:"home"`
		Script    string `json:"script"`
		Python    string `json:"python"`
		Port      string `json:"port"`
		BaseURL   string `json:"base_url"`
		Autostart bool   `json:"autostart"`
		Model     string `json:"model"`
		LMModel   string `json:"lm_model"`
		LMBackend string `json:"lm_backend"`
		Device    string `json:"device"`
		// InitLLM 是否在服务启动时就初始化 5Hz LM（auto/true/false）。
		//
		// 默认 false：2026-11-03 实测，预载 LM 要多付 3.4GB 权重 + 49s tokenizer，
		// 而**纯音乐（BGM）根本不用它**——请求里已经把 use_cot_caption /
		// use_cot_language 关掉了。需要「写歌 / 带歌词」时改成 auto 或 true。
		InitLLM string `json:"init_llm"`
		// CheckpointsDir 权重目录（默认 D:/AI/Models/ace-step）。
		// 项目规则：权重只放 D:/AI/Models，仓库目录不存模型。
		CheckpointsDir string `json:"checkpoints_dir"`
	} `json:"acestep"`

	// IdleRelease 空闲显存回收（2026-10-28）。	//
	// 用户的原话是「无任务时释放我的显存啊，flowedge 白白占用了那么多」。
	// 这一段的语义：**连续 after_seconds 秒没有任何 GPU 活 → 把托管模型的权重
	// 还回显存**（/unload、/free 都保留进程，下次只付权重加载）。
	// 判定与执行都在 service/vram_idle.go，不依赖任何服务自己的空闲超时
	// （那个看不见引擎队列，会在下一镜配音前 5 秒把模型卸掉）。
	//
	// 为什么 AfterSeconds 是 *int：
	//   不写这一段 = 用默认 10 分钟；写 0 = **明确关闭**。
	//   用普通 int 的话两者都是 0，分不出「没配」和「关掉」。
	IdleRelease struct {
		AfterSeconds *int     `json:"after_seconds"`
		Targets      []string `json:"targets"`
		TTSMinFreeMB int      `json:"tts_min_free_mb"`
		TTSGuard     *bool    `json:"tts_guard"`
	} `json:"idle_release"`
}

// IdleReclaimPolicy 把 deps.json 的 idle_release 段翻成策略。
//
// 缺省即安全：没写这一段时返回 DefaultIdleReclaimPolicy()（10 分钟 + TTS 守卫）。
// 显式 after_seconds=0 才会关闭——「关掉」必须是有人明确写下的决定，
// 不能是「配置解析失败」的副作用。
func (f depsFile) IdleReclaimPolicy() IdleReclaimPolicy {
	p := DefaultIdleReclaimPolicy()
	if f.IdleRelease.AfterSeconds != nil {
		p.After = time.Duration(*f.IdleRelease.AfterSeconds) * time.Second
	}
	if len(f.IdleRelease.Targets) > 0 {
		p.Targets = f.IdleRelease.Targets
	}
	if f.IdleRelease.TTSMinFreeMB > 0 {
		p.TTSMinFreeMB = f.IdleRelease.TTSMinFreeMB
	}
	if f.IdleRelease.TTSGuard != nil {
		p.TTSGuard = *f.IdleRelease.TTSGuard
	}
	return p
}

// LoadIdleReclaimPolicy 读配置并给出空闲回收策略（main 启动时调一次）。
//
// 包一层是为了别让调用方去碰未导出的 depsFile 类型；也保证「配置文件不存在」
// 和「解析失败」都走同一条路径——回落默认策略（10 分钟 + 守卫），而不是静默关闭。
func LoadIdleReclaimPolicy() IdleReclaimPolicy {
	return LoadDepsFile().IdleReclaimPolicy()
}

// depsJSONPath 依赖配置文件的相对路径（引擎启动后会 chdir 到部署目录）
const depsJSONPath = "data/deps.json"

// LoadDepsFile 读 data/deps.json（不存在时返回零值，不报错）
//
// 必须剥掉 UTF-8 BOM：Windows 上某些编辑器、以及 Python 的 `encoding="utf-8-sig"`，
// 都会写出开头的 EF BB BF。Go 的 json.Unmarshal 见到它直接失败
// （`invalid character 'ï' looking for beginning of value`），**整个文件解析不出来**。
//
// 2026-10-26 实测的代价：原来这里写的是 `_ = json.Unmarshal(...)`，错误被静默吞掉，
// 于是 h3.reserve_vram / h3.args / tts.idle_unload / ollama.autostart 全部静默失效，
// 引擎一直吃代码内置默认的 `--reserve-vram 4`，而 deps.json 里明明写着 0。
// 表现是「配置改了、reload 了、重启了，argv 还是 4」，排查了一整天。
// 现在的契约：剥 BOM；解析失败必须留痕，不许再静默。
func LoadDepsFile() depsFile {
	var conf depsFile
	raw, err := os.ReadFile(depsJSONPath)
	if err != nil {
		return conf // 文件不存在是正常情况，回落代码内置默认
	}
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		raw = raw[3:]
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		log.Printf("[deps] %s 解析失败（%v）——本次所有配置项回落为代码内置默认值", depsJSONPath, err)
	}
	return conf
}

// TTSBaseURL 从配置里取 TTS 服务地址。
// 供 main.go 解析 TTS_BASE_URL 用：只靠环境变量的话，切换后端就必须重启外壳
// （外壳 spawn 引擎时不传 env，纯 os.Getenv 也没有 .env 支持），太脆。
func TTSBaseURL() string {
	return strings.TrimSpace(LoadDepsFile().TTS.BaseURL)
}

// MusicBaseURL 从配置里取音乐服务地址（缺省端口 8104）。
func MusicBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("MUSIC_BASE_URL")); v != "" {
		return v
	}
	return strings.TrimSpace(LoadDepsFile().Acestep.BaseURL)
}

// acestepWorkRoot 给 ACE-Step 一个「派生进程写得进去」的项目根。
//
// 不能直接用 D:/AI/ACE-Step-1.5：它的 .cache/acestep/local_redis 是 sqlite，
// 引擎派生进程写进去会报 readonly（实测），和 H3 的 output 目录同一个坑。
// 放在 <cwd>/data/acestep 下（cwd = 部署目录），随部署目录一起可写。
func acestepWorkRoot() string {
	if v := strings.TrimSpace(os.Getenv("ACESTEP_PROJECT_ROOT")); v != "" {
		return v
	}
	wd, err := os.Getwd()
	if err != nil {
		wd = "."
	}
	root := filepath.Join(wd, "data", "acestep")
	_ = os.MkdirAll(filepath.Join(root, ".cache", "acestep"), 0o755)
	return filepath.ToSlash(root)
}

// DefaultMusicBaseURL 音乐服务的缺省地址（与 deps.go 里推导出的端口一致）。
const DefaultMusicBaseURL = "http://127.0.0.1:8104"

// NewSupervisor 依据配置构造依赖规格；comfyBase/asrBase 用于推导端口
func NewSupervisor(comfyBase, asrBase, ttsBase string) *Supervisor {
	s := &Supervisor{
		specs:      map[string]*DepSpec{},
		procs:      map[string]*exec.Cmd{},
		started:    map[string]bool{},
		lastErr:    map[string]string{},
		procLogs:   map[string][]*os.File{},
		startLocks: map[string]*sync.Mutex{},
		client:     &http.Client{Timeout: 1500 * time.Millisecond},
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
	// 配置层覆盖（data/deps.json 的 "h3" 段）：先于环境变量，便于「改配置 → reload → 重启依赖」。
	//
	// 加这一段的原因（2026-10-26）：`--reserve-vram 4` 在 24GB 卡上是**算术不可满足**的
	// —— ComfyUI 全量加载要求「模型 + 4GB」同时空闲（model_management.py:944），
	// 而 H3 权重约 20GB、桌面 3.1GB，空闲只剩 0.3–0.9GB。
	// 想验证「降到 1 会不会显著变快」时，原来只能改环境变量并重启**引擎**（因为
	// H3_ARGS 只在 NewSupervisor 里读一次）。做成配置后，A/B 只需 reload + 重启该依赖。
	h3Args = applyH3Config(h3Args, conf.H3.Args, conf.H3.ReserveVRAM)
	// cache_none=false 时去掉 --cache-none（A/B 用；默认仍带着，见 depsFile.H3 注释）
	if conf.H3.CacheNone != nil && !*conf.H3.CacheNone {
		h3Args = dropArg(h3Args, "--cache-none")
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

	// ---- ACE-Step 1.5（音乐生成，GPU）----
	//
	// 与 TTS 的托管契约几乎一样：一个 python 脚本 + HTTP /health + 固定端口。
	// 三个**必须知道**的差异：
	//   1. 冷加载更贵（9GB XL 权重 + LM，实测 3–5 分钟）⇒ readyTimeout 单独给 600s；
	//   2. 没有 /unload ⇒ 换侧只能停进程（见 vram.go releaseMusicSide）；
	//   3. 权重目录必须指到 D:/AI/Models（ACESTEP_CHECKPOINTS_DIR），
	//      否则它会往仓库目录里下 10GB+ —— 项目规则禁止在项目目录存权重。
	acestepHome := firstNonEmpty(os.Getenv("ACESTEP_HOME"), conf.Acestep.Home, "D:/AI/ACE-Step-1.5")
	acestepScriptName := firstNonEmpty(os.Getenv("ACESTEP_SCRIPT"), conf.Acestep.Script, "acestep/api_server.py")
	acestepScript := acestepScriptName
	if !filepath.IsAbs(acestepScriptName) {
		acestepScript = filepath.Join(acestepHome, acestepScriptName)
	}
	acestepPython := firstNonEmpty(os.Getenv("ACESTEP_PYTHON"), conf.Acestep.Python, detectVenvPython(acestepHome))
	musicBase := firstNonEmpty(os.Getenv("MUSIC_BASE_URL"), conf.Acestep.BaseURL, DefaultMusicBaseURL)
	acestepPort := portFromURL(musicBase, "8104")
	if p := strings.TrimSpace(conf.Acestep.Port); p != "" {
		acestepPort = p
	}
	acestepHint := ""
	if _, err := os.Stat(acestepScript); err != nil {
		acestepHint = "未找到 ACE-Step（需 " + acestepHome + " 下有 acestep/api_server.py）：" +
			"克隆 https://github.com/ace-step/ACE-Step-1.5 并在该目录 uv sync 后重试"
	}
	acestepModel := firstNonEmpty(os.Getenv("ACESTEP_CONFIG_PATH"), conf.Acestep.Model, "acestep-v15-xl-turbo")
	acestepLM := firstNonEmpty(os.Getenv("ACESTEP_LM_MODEL_PATH"), conf.Acestep.LMModel, "acestep-5Hz-lm-1.7B")
	acestepLMBackend := firstNonEmpty(os.Getenv("ACESTEP_LM_BACKEND"), conf.Acestep.LMBackend, "vllm")
	acestepCkpt := firstNonEmpty(os.Getenv("ACESTEP_CHECKPOINTS_DIR"), conf.Acestep.CheckpointsDir, "D:/AI/Models/ace-step")
	acestepEnv := append(os.Environ(),
		"ACESTEP_API_PORT="+acestepPort,
		"ACESTEP_API_HOST=127.0.0.1",
		"ACESTEP_CHECKPOINTS_DIR="+acestepCkpt,
		// 项目根指向**工作区**：ACE-Step 会在 <project_root>/.cache/acestep 下建
		// diskcache（sqlite）+ tmp。2026-11-03 实测：引擎派生进程对 D:/AI 写不了
		// （sqlite 报 `attempt to write a readonly database`，最后 `Application
		// startup failed`），而同一个命令在 agent 的 job 里跑就正常——和 H3
		// SaveVideo 写 D:/AI/ComfyUI/output 报 PermissionError 是同一类问题
		// （AGENTS §0.16 那条 2026-10-27）。权重目录仍指 D:/AI/Models（只读，没问题）。
		"ACESTEP_PROJECT_ROOT="+acestepWorkRoot(),
		"ACESTEP_CONFIG_PATH="+acestepModel,
		"ACESTEP_LM_MODEL_PATH="+acestepLM,
		"ACESTEP_LM_BACKEND="+acestepLMBackend,
		"ACESTEP_DEVICE="+firstNonEmpty(os.Getenv("ACESTEP_DEVICE"), conf.Acestep.Device, "auto"),
		// 默认 false：BGM 不需要 LM，预载它只会让冷启动多等 4GB + 49s
		"ACESTEP_INIT_LLM="+firstNonEmpty(os.Getenv("ACESTEP_INIT_LLM"), conf.Acestep.InitLLM, "false"),
		// 权重已在本机时别再联网查更新
		"HF_HUB_OFFLINE="+firstNonEmpty(os.Getenv("HF_HUB_OFFLINE"), "1"),
		// HF_HOME 必须指到**工作区内**：引擎派生进程对工作区外（D:/AI、E:/caches）没有写权限，
		// 而 transformers 加载 sft 权重的 trust_remote_code 动态模块时要往
		// <HF_HOME>/modules/transformers_modules 写文件。2026-11-04 实测：
		// 沿用全局 HF_HOME=E:/caches/huggingface 会 WinError 5 直接初始化失败，
		// 换到工作区后正常（turbo 能跑只是因为它那份模块早在 11-03 就缓存好了、只读命中）。
		"HF_HOME="+filepath.ToSlash(filepath.Join(acestepWorkRoot(), "hf-home")),
		"TRANSFORMERS_CACHE="+filepath.ToSlash(filepath.Join(acestepWorkRoot(), "hf-home", "transformers")),
		"PYTHONIOENCODING=utf-8",
		"PYTHONUTF8=1",
	)
	s.specs["acestep"] = &DepSpec{
		Name:       "acestep",
		Label:      "音乐生成（ACE-Step 1.5 XL）",
		Binary:     acestepPython,
		Args:       []string{acestepScript},
		WorkDir:    acestepHome,
		Env:        acestepEnv,
		Endpoint:   normalBase(musicBase),
		HealthPath: "/health",
		// 默认不常驻：9GB 权重挂在卡上会挤掉 H3 的 20.5GB。用到再起，
		// 用完由引擎侧 idle_release / 换侧停进程回收。
		Autostart: conf.Acestep.Autostart,
		Hint:      acestepHint,
	}

	// ---- Ollama（语言侧）----
	//
	// 为什么把它也收进托管：video 的提示词优化、image_batch 的拆镜、film 的分镜规划
	// 都要语言模型。此前引擎不管它，用户看到的是"依赖未就绪 → 另开终端跑 ollama serve"，
	// 在"双击 exe"的交付形态里等于让用户去开终端——三步里最容易卡住的一步。
	// 已在外跑（桌面版/系统服务）时只探活复用，不会拉起第二个实例。
	if ollamaBin := firstNonEmpty(os.Getenv("OLLAMA_BINARY"), conf.Ollama.Binary, defaultOllamaBinary()); ollamaBin != "" {
		args := conf.Ollama.Args
		if len(args) == 0 {
			args = []string{"serve"}
		}
		s.specs["ollama"] = &DepSpec{
			Name:       "ollama",
			Label:      "语言模型（Ollama）",
			Binary:     ollamaBin,
			Args:       args,
			Endpoint:   normalBase(firstNonEmpty(os.Getenv("OLLAMA_BASE_URL"), conf.Ollama.BaseURL, "http://127.0.0.1:11434")),
			HealthPath: "/api/tags",
			Autostart:  conf.Ollama.Autostart,
			Hint:       "托管 Ollama（ollama serve）；若要它开机自起，把 data/deps.json 的 ollama.autostart 设 true",
		}
	}

	return s
}

// defaultOllamaBinary 常见安装位置兜底（PATH 里没有也能找到）。
func defaultOllamaBinary() string {
	if p, err := exec.LookPath("ollama"); err == nil {
		return p
	}
	candidates := []string{
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Ollama", "ollama.exe"),
		`C:\Program Files\Ollama\ollama.exe`,
		"/usr/local/bin/ollama",
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
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
	return s.unstartableReason(name) == ""
}

// unstartableReason 说不清"缺什么"的 Startable 会让一键修复变成静默失败：
// 用户点了启动、界面 202、什么也没发生，也没有 last_error。
// 这里把每种不满足的原因写成人话，Start 会写进 last_error 并在响应里说清楚。
func (s *Supervisor) unstartableReason(name string) string {
	spec, ok := s.specs[name]
	if !ok {
		return "未知依赖：" + name
	}
	if spec.Binary == "" || len(spec.Args) == 0 {
		return "启动条件不满足：没有配置可执行文件或参数"
	}
	if _, err := os.Stat(spec.Binary); err != nil {
		return "启动条件不满足：找不到可执行文件 " + spec.Binary
	}
	// 参数不一定是脚本文件：ollama 的 `serve` 是子命令，不是路径。
	// 只有"看起来像路径"的那个参数才需要存在性检查——以前一律 os.Stat(Args[0])，
	// 结果 `ollama serve` 被判成"脚本不存在"，进程根本不派生，还一句错都不报。
	if arg0 := spec.Args[0]; looksLikeScriptPath(arg0) {
		cand := arg0
		if !filepath.IsAbs(cand) && spec.WorkDir != "" {
			cand = filepath.Join(spec.WorkDir, cand)
		}
		if _, err := os.Stat(cand); err != nil {
			return "启动条件不满足：找不到脚本 " + cand
		}
	}
	return ""
}

// looksLikeScriptPath 参数是不是一个文件路径（而不是子命令/开关）。
func looksLikeScriptPath(arg string) bool {
	if arg == "" {
		return false
	}
	if strings.HasPrefix(arg, "-") {
		return false // 开关
	}
	if strings.ContainsAny(arg, `/\\`) {
		return true // 带路径分隔符
	}
	for _, ext := range []string{".py", ".js", ".mjs", ".cjs", ".bat", ".cmd", ".ps1"} {
		if strings.HasSuffix(strings.ToLower(arg), ext) {
			return true
		}
	}
	return false
}

// ensureTempEnv 给托管进程指定一个**确定可写**的临时目录。
//
// 由来（2026-10-26 现场）：引擎拉起的 ComfyUI-H3 一起就退，日志里是
//
//	FileNotFoundError: No usable temporary directory found in
//	['<用户临时目录>', '<用户临时目录>\2']
//
// 而用同一个 venv 的 python 从交互 shell 跑完全正常（连 `import torch._dynamo` 都过）。
// 也就是说：不是 torch/权重坏了，是**引擎派生的这个子进程在 %TEMP% 里建不了文件**。
// 这类"父进程能写、子进程不能写"的差异不值得继续猜（job 对象 / 令牌 / 安全软件都可能），
// 直接把 TEMP/TMP 指到项目内目录——依赖自己的日志就写在这里，一定可写。
//
// 顺带的好处：临时文件不再散落在系统 Temp，出问题好找。
func ensureTempEnv(env []string) []string {
	if len(env) == 0 {
		env = os.Environ()
	}
	dir := ""
	if wd, err := os.Getwd(); err == nil {
		dir = filepath.Join(wd, "data", "tmp")
	}
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "flowedge")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[deps] 临时目录不可用（%v），子进程沿用原 TEMP", err)
		return env
	}
	out := make([]string, 0, len(env)+3)
	for _, kv := range env {
		u := strings.ToUpper(kv)
		if strings.HasPrefix(u, "TEMP=") || strings.HasPrefix(u, "TMP=") || strings.HasPrefix(u, "TMPDIR=") {
			continue // 旧值一律丢掉：留着就可能挑到那个"建不了文件"的目录
		}
		out = append(out, kv)
	}
	return append(out, "TEMP="+dir, "TMP="+dir, "TMPDIR="+dir)
}

// startLock 每个依赖一把单飞锁（懒建，受 s.mu 保护）。
func (s *Supervisor) startLock(name string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startLocks == nil {
		s.startLocks = map[string]*sync.Mutex{}
	}
	mu, ok := s.startLocks[name]
	if !ok {
		mu = &sync.Mutex{}
		s.startLocks[name] = mu
	}
	return mu
}

// Start 启动依赖（已就绪则直接返回）
//
// 并发安全：整个「探活 → 派生 → 等就绪」全程持每个依赖各自的单飞锁。
//
// 由来（2026-10-26 现场）：00:05:54 出现了**两个** ComfyUI-H3 进程
// （一个 25644 抢到 8189 并持有 20GB；一个 28124 卡在 3MB 活了一小时）。
// 那一晚的三次出片全部 15 分钟超时，单镜从 84.9s 退化到 2269.8s——
// 典型的「两个实例抢一张 24G 卡」。
// 原来只靠 s.mu 保护派生那几步，**WaitReady 之前就解锁了**：
// 冷启动 30–60 秒的窗口里，第二次 Start（双击两次 / 预检自动拉起 + 手动点）
// 完全可能再派生一个。单飞锁把整个窗口关掉。
func (s *Supervisor) Start(name string) error {
	spec, ok := s.specs[name]
	if !ok {
		return fmt.Errorf("未知依赖：%s", name)
	}
	lock := s.startLock(name)
	lock.Lock()
	defer lock.Unlock()

	if s.Healthy(name) {
		return nil
	}
	if reason := s.unstartableReason(name); reason != "" {
		s.mu.Lock()
		s.lastErr[name] = reason
		s.mu.Unlock()
		return fmt.Errorf("%s", reason)
	}

	if spec.Exclusive != "" && s.Healthy(spec.Exclusive) {
		_ = s.Stop(spec.Exclusive) // 24GB 装不下两个实例：先让位再启动
		time.Sleep(2 * time.Second)
	}

	// 日志路径必须在**上锁之前**解析：depLogPath 自己会读 s.mu。
	logPath := s.depLogPath(name)

	s.mu.Lock()
	if cmd, exists := s.procs[name]; exists && cmd.Process != nil {
		s.mu.Unlock()
		// 已经在启动中的进程，仍要按该依赖的冷启动上限等。
		// 这里原来写死 3s：H3 冷启要 ~50s，导致「先在环境面板点了启动、再跑工作流」
		// 这种最常见的路径必然报「3s 内未就绪」。
		return s.WaitReady(name, readyTimeout(name))
	}
	cmd := hiddenExec(spec.Binary, spec.Args...)
	cmd.Dir = spec.WorkDir
	// 临时目录必须在**上锁之前**备好（ensureTempEnv 会建目录，可能慢一拍）。
	tmpEnv := ensureTempEnv(spec.Env)
	cmd.Env = tmpEnv
	cmd.SysProcAttr = hiddenWindowAttr()
	// 注意：日志接线走无锁的 depLogHandleAt，path 已在上面取好。
	// 这里正持有 s.mu，任何再抢 s.mu 的调用都会永久死锁（2026-10-26 现场）。
	logFile := depLogHandleAt(cmd, name, logPath)
	if logFile != nil {
		s.procLogs[name] = append(s.procLogs[name], logFile)
	}
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
		s.closeDepLog(name)
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
	err := error(nil)
	if cmd.Process != nil {
		err = cmd.Process.Kill()
	}
	s.closeDepLog(name)
	return err
}

// SetLogDir 指定托管进程日志目录（空 = 丢弃输出）。
//
// 由来（2026-10-25）：改造前 `cmd.Start()` 没接 Stdout/Stderr，Go 把子进程输出
// 直接丢到空设备。于是 H3/ASR/TTS 自己的日志（模型加载耗时、卸载、"loaded
// partially"、CUDA 报错细节）**一行都没留下**，`运行/logs/` 里只有 engine.log。
//
// 代价很实在：排查「H3 为什么跑完就冷」「ASR 到底是不可达还是空结果」
// 这类问题只能靠反复花 GPU/时间做实验；有了它，打开一条 log 就能读结论。
// 这也是 §0.6「服务没起 ≠ 没有结果」唯一能落地的前提——不可达要能被看见。
func (s *Supervisor) SetLogDir(dir string) {
	s.mu.Lock()
	s.logDir = dir
	s.mu.Unlock()
}

// depLogPath 某个依赖的日志文件路径（未配置目录时返回空）。
func (s *Supervisor) depLogPath(name string) string {
	s.mu.Lock()
	dir := s.logDir
	s.mu.Unlock()
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	return filepath.Join(dir, name+".log")
}

// attachDepLog 解析日志路径并把子进程输出接过去。
//
// **调用方不得持有 s.mu**（内部会读 logDir）。Start 的持锁路径用的是
// depLogHandleAt + 事先取好的 path，不走这里——原因见下面的死锁说明。
func (s *Supervisor) attachDepLog(cmd *exec.Cmd, name string) *os.File {
	return depLogHandleAt(cmd, name, s.depLogPath(name))
}

// depLogHandleAt 纯函数版：给定路径直接接线，**不碰任何锁**。
//
// 2026-10-26 现场（必须记住的教训）：Start 在 spawn 路径上持有 s.mu，
// 原来是 s.attachDepLog(cmd,name) → 内部 depLogPath() 又 s.mu.Lock()
// ⇒ Go 互斥锁不可重入 ⇒ **永久死锁**。症状极具误导性：
//   - /api/deps 全部挂住（20s+ 不返回）
//   - /api/deps/comfy_h3/start 返回 202，但再也不派生任何进程
//   - /api/health、/api/runs 却完全正常（它们不碰这把锁）
//
// 所以：**"服务起不来"先怀疑锁，别往模型/权重上找**。
// 这里留成无锁纯函数，是让 Start 能安全地在锁内调用。
func depLogHandleAt(cmd *exec.Cmd, name, path string) *os.File {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("[deps] %s 日志目录不可用（%v），该依赖输出丢弃", name, err)
		return nil
	}
	if st, err := os.Stat(path); err == nil && st.Size() > 8<<20 {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("[deps] %s 日志打开失败（%v），该依赖输出丢弃", name, err)
		return nil
	}
	// 启动分隔线：依赖被停掉再起时，同一份日志里能一眼看出「这是新的一次」。
	_, _ = fmt.Fprintf(f, "\n=== %s 启动 %s ===\n", name, time.Now().Format("2006-01-02 15:04:05"))
	cmd.Stdout = f
	cmd.Stderr = f
	return f
}

// closeDepLog 关闭某个依赖的日志句柄（Stop 与进程退出时都调，可重复）。
func (s *Supervisor) closeDepLog(name string) {
	s.mu.Lock()
	files := s.procLogs[name]
	delete(s.procLogs, name)
	s.mu.Unlock()
	for _, f := range files {
		_ = f.Close()
	}
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
			// 端口通了，但要分清「谁在提供服务」。
			// 我们自己派生的进程已经退出、端口却有人应答 ⇒ 是外部（用户手动起、
			// 别的工具拉起）实例。这件事必须留痕：否则「停止」点了没反应、
			// 「启动」看起来成功，人和 agent 都会以为是自己的进程在跑。
			s.mu.Lock()
			_, ours := s.procs[name]
			s.mu.Unlock()
			if spec.Exclusive != "" || name == "comfy_h3" || name == "comfyui" {
				if !ours {
					log.Printf("[deps] %s 的端口由**非本进程托管的实例**提供（我们派生的那个已退出）；"+
						"停止本服务不会影响它，显存仍被它占着", label)
				}
			}
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
		healthy := s.Healthy(n)
		st := DepStatus{
			Name:      spec.Name,
			Label:     spec.Label,
			Endpoint:  spec.Endpoint,
			Autostart: spec.Autostart,
			Hint:      spec.Hint,
			Running:   healthy,
		}
		s.mu.Lock()
		if cmd, ok := s.procs[n]; ok {
			st.Managed = true
			if cmd.Process != nil {
				st.PID = cmd.Process.Pid
			}
		}
		managedAlive := st.Managed && st.PID > 0
		st.LastError = s.lastErr[n]
		s.mu.Unlock()

		// 「忙」≠「没起」：进程活着但健康检查不回，绝不能报「未启动」。
		// 报成未启动会诱导用户点「启动」再派生一份，两个实例抢一张卡
		// （2026-10-26 那晚 27 倍慢的起点就是这么来的）。
		if healthy {
			st.State = "ok"
		} else if managedAlive {
			st.Busy = true
			st.Running = true
			st.State = "busy"
			if st.Hint == "" {
				st.Hint = "进程还活着，但忙到不回健康检查（生成中/换页中）。此时不要再点启动，" +
					"否则会多出一份实例抢显存"
			}
		} else {
			st.State = "stopped"
		}
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
	cmd := hiddenExecContext(ctx, python, "-c", "from sageattention import sageattn")
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
	case "asr":
		return 300 * time.Second // 首次要下载/预热模型
	case "tts":
		// Breeze TTS 2 的冷启动包含 CUDA Graph 预热：编译缓存热时 ~117s，冷编译实测
		// ~245s，而启动瞬间显存被别的实例占着时会拖到 300s 以上（图捕获要吃满 24GB 卡）。
		// 原来和 asr 共用 300s，只剩 55s 余量，2026-10-26 实测被卡过两次——表现为
		// 「服务其实起来了，但引擎记了一条启动失败」。
		// Audio8 / Kokoro 走同一分支：它们更快，放宽只会抬高「判定就绪」的上限，
		// 不影响就绪判定的正确性（探活一成功就返回，不会真等满）。
		return 600 * time.Second
	case "comfy_h3":
		return 180 * time.Second // 独立 venv + 20GB 权重，实测冷启 ~50s
	case "comfy_moss":
		return 300 * time.Second // 实测冷启 ~130s（988 节点注册 + torch 2.14 导入），180s 会误判失败
	case "acestep":
		// ACE-Step XL 的冷启动**比 README 说的 3–5 分钟长得多**。
		// 2026-11-03 本机实测（3090 24GB）：进程拉起 ~40s → 主模型 4 个 shard
		// 载入 ~3 分钟 → LM tokenizer 49s → LM 权重 → 首次生成，全程 **15 分钟以上**。
		// 其中很大一块是「它自己又去 ModelScope 校验/补下 29 个文件」。
		// 1200s 覆盖这条最坏路径；探活成功会立刻返回，放宽不等于真等满。
		return 1200 * time.Second
	default:
		return 120 * time.Second
	}
}
