package service

// code_video：用代码逐帧渲染一段视频（知识卡/图表/章节卡），不调用任何生成模型。
//
// 渲染器是 lemo-opuscar 的 headless 渲染链（core/render/video.mjs）：
// 页面契约 window.READY / render(t) / DUR / TEXTS(t)，逐帧截图进 ffmpeg。
// 我们的模板（codetpl/blueprint）实现这个契约，视觉是「工程蓝图 + 手绘白线」。
//
// 为什么单独一个节点而不是塞进 text2video：
//   - 资源道不同：渲染吃 CPU、零显存，要与 H3 并行而不是抢显存仲裁（见 queue.go 白名单）；
//   - 语义不同：它是确定性渲染，同一 spec 永远同一帧，可反复改文案重渲；
//   - 产物不同：无音轨或只带 TTS 旁白，不参与 H3 的音频治理链。
//
// 设计文档：docs/混合出片-代码视频+模型视频+剪辑-方案-20261030.md

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"flowedge/backend/model"
)

//go:embed codetpl
var codeTplFS embed.FS

// codeVideoStyles 可用的视觉风格 → 内嵌模板目录。
// 每套风格只实现同一份 CodeSpec，靠视觉签名区分：
//   blueprint = 工程蓝图（自研初版）
//   darkcard  = 暗色 3D 卡片世界 + 准一镜到底（对标抖音「基地」）
//   atlas     = DK 图鉴 / 剖面图解（对标小红书爆款 3D 剖面图）
//   studio    = 黑画布 MG 信息图 + 章节 HUD（对标 anything2explainer）
// 共用 codetpl/_shared/kit.js。加风格 = 这里加一行 + 建目录，无需改执行器逻辑。
var codeVideoStyles = map[string]string{
	"blueprint": "codetpl/blueprint",
	"darkcard":  "codetpl/darkcard",
	"atlas":     "codetpl/atlas",
	"studio":    "codetpl/studio",
}

// codeVideoStyleNames 给报错信息用的稳定顺序。
var codeVideoStyleNames = []string{"blueprint", "darkcard", "atlas", "studio"}

// CodeCard 一张知识卡。
type CodeCard struct {
	Kicker  string   `json:"kicker,omitempty"`
	Heading string   `json:"heading"`
	Points  []string `json:"points,omitempty"`
	Figure  string   `json:"figure,omitempty"` // none | bars | flow
	Labels  []string `json:"labels,omitempty"`
	Values  []int    `json:"values,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// CodeSpec 整段视频的渲染规格。
type CodeSpec struct {
	Title       string     `json:"title"`
	TitleEn     string     `json:"title_en,omitempty"`
	Kicker      string     `json:"kicker,omitempty"`
	Number      string     `json:"number,omitempty"`
	Rev         string     `json:"rev,omitempty"`
	Scale       string     `json:"scale,omitempty"`
	Date        string     `json:"date,omitempty"`
	Duration    float64    `json:"duration,omitempty"`
	FPS         int        `json:"fps,omitempty"`
	CardSeconds float64    `json:"card_seconds,omitempty"`
	Cards       []CodeCard `json:"cards"`
}

// execCodeVideoNode 渲染一段代码视频。
func (e *EngineV2) execCodeVideoNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	style := "blueprint"
	if v, ok := node.Data["style"].(string); ok && strings.TrimSpace(v) != "" {
		style = strings.TrimSpace(v)
	}
	if _, ok := codeVideoStyles[style]; !ok {
		return nil, fmt.Errorf("code_video: 未知 style=%q；可用：%s", style, strings.Join(codeVideoStyleNames, ", "))
	}

	spec, err := buildCodeSpec(node, upstream)
	if err != nil {
		return nil, err
	}

	root := codeVideoRoot()
	if root == "" {
		return nil, fmt.Errorf("code_video: 找不到 lemo-opuscar 渲染库；设置环境变量 LEMO_HOME=<库根>（默认尝试 D:/AI/lemo-opuscar 与 %%USERPROFILE%%/lemo-opuscar）")
	}

	// 模板 → 临时 demo 目录（lemo 的静态服务会把库外目录挂到 /@film/）
	demoDir, err := os.MkdirTemp("", "flowedge-code-*")
	if err != nil {
		return nil, fmt.Errorf("code_video: 建临时目录失败：%w", err)
	}
	defer os.RemoveAll(demoDir)

	if err := materializeCodeTemplate(demoDir, style); err != nil {
		return nil, err
	}
	specJSON, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("code_video: spec 序列化失败：%w", err)
	}
	if err := os.WriteFile(filepath.Join(demoDir, "spec.json"), specJSON, 0o644); err != nil {
		return nil, fmt.Errorf("code_video: 写 spec.json 失败：%w", err)
	}

	outDir := "data/generated"
	_ = os.MkdirAll(outDir, 0o755)
	outPath := filepath.Join(outDir, fmt.Sprintf("code-video-%d.mp4", time.Now().UnixNano()))
	if p, ok := node.Data["output_path"].(string); ok && strings.TrimSpace(p) != "" {
		outPath = p
		_ = os.MkdirAll(filepath.Dir(outPath), 0o755)
	}
	absOut, absErr := filepath.Abs(outPath)
	if absErr == nil {
		outPath = absOut
	}

	w, h := 1920, 1080
	if v, ok := node.Data["width"].(float64); ok && v >= 16 {
		w = int(v)
	}
	if v, ok := node.Data["height"].(float64); ok && v >= 16 {
		h = int(v)
	}
	if w%2 == 1 {
		w++
	}
	if h%2 == 1 {
		h++
	}
	fps := spec.FPS
	if fps <= 0 {
		fps = 24
	}
	workers := 2
	if v, ok := node.Data["workers"].(float64); ok && v >= 1 {
		workers = int(v)
	}
	if workers > 6 {
		workers = 6
	}

	args := []string{
		filepath.Join("core", "render", "video.mjs"),
		demoDir,
		"--fps", strconv.Itoa(fps),
		"--workers", strconv.Itoa(workers),
		"--out", outPath,
		"--size", fmt.Sprintf("%dx%d", w, h),
	}
	nodeBin := strings.TrimSpace(os.Getenv("FLOWEDGE_NODE"))
	if nodeBin == "" {
		nodeBin = "node"
	}
	cmd := exec.CommandContext(ctx, nodeBin, args...)
	cmd.Dir = root
	// 子进程输出只在失败时进错误信息：成功时每 60 帧一行进度，刷日志没意义
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		tail := string(output)
		if len(tail) > 1200 {
			tail = tail[len(tail)-1200:]
		}
		return nil, fmt.Errorf("code_video: 渲染失败（%v）：%s", runErr, strings.TrimSpace(tail))
	}
	if st, statErr := os.Stat(outPath); statErr != nil || st.Size() == 0 {
		return nil, fmt.Errorf("code_video: 渲染进程正常退出但产物不存在或为空：%s", outPath)
	}

	// 可选旁白：复用 FE 的 TTS（Kokoro），合成后与画面合流。
	// 只在显式给了 tts_text 时才做——知识卡默认无声，混排时由流水线统一配音。
	if ttsText := strings.TrimSpace(dataString(node, "tts_text")); ttsText != "" {
		if e.tts == nil {
			return nil, fmt.Errorf("code_video: 指定了 tts_text 但 TTS 服务未装配")
		}
		wavPath := strings.TrimSuffix(outPath, ".mp4") + ".wav"
		voice := dataString(node, "voice")
		if _, terr := e.tts.SynthesizeWith(ttsText, wavPath, TTSOptions{Voice: voice}); terr != nil {
			return nil, fmt.Errorf("code_video: 旁白合成失败：%w", terr)
		}
		defer os.Remove(wavPath)
		mixed := strings.TrimSuffix(outPath, ".mp4") + "-vo.mp4"
		muxArgs := []string{"-y", "-hide_banner", "-loglevel", "error",
			"-i", outPath, "-i", wavPath,
			"-c:v", "copy", "-c:a", "aac", "-b:a", "192k",
			"-map", "0:v:0", "-map", "1:a:0", "-shortest", mixed}
		if b, merr := exec.CommandContext(ctx, ffmpegBin(), muxArgs...).CombinedOutput(); merr != nil {
			return nil, fmt.Errorf("code_video: 旁白合流失败（%v）：%s", merr, strings.TrimSpace(string(b)))
		}
		_ = os.Remove(outPath)
		outPath = mixed
	}

	if abs, aerr := filepath.Abs(outPath); aerr == nil {
		outPath = abs
	}
	log.Printf("[code_video] style=%s cards=%d %.1fs %dx%d@%dfps → %s", style, len(spec.Cards), spec.Duration, w, h, fps, outPath)
	return map[string]interface{}{
		"video":    outPath,
		"style":    style,
		"duration": spec.Duration,
		"cards":    len(spec.Cards),
		"fps":      fps,
		"width":    w,
		"height":   h,
	}, nil
}

// materializeCodeTemplate 把内嵌模板写到 demo 目录。
// 布局约定：风格目录的内容落到 demo 根（index.html / render.js），
// 共用工具落在 demo/shared/（render.js 用 './shared/kit.js' 引入）。
//
// 注意：共用目录叫 shared 不叫 _shared —— go:embed 的目录模式会跳过以 '_' 或 '.'
// 开头的文件与目录，叫 _shared 会编译通过但运行时文件根本不存在（踩过）。
func materializeCodeTemplate(demoDir, style string) error {
	root, ok := codeVideoStyles[style]
	if !ok {
		return fmt.Errorf("code_video: 未知 style=%q；可用：%s", style, strings.Join(codeVideoStyleNames, ", "))
	}
	const shared = "codetpl/shared"

	return fs.WalkDir(codeTplFS, "codetpl", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		var dst string
		switch {
		case strings.HasPrefix(p, shared+"/"):
			// shared 原样保留路径，供各风格 import
			dst = filepath.Join(demoDir, "shared", filepath.FromSlash(strings.TrimPrefix(p, shared+"/")))
		case strings.HasPrefix(p, root+"/"):
			rel := strings.TrimPrefix(p, root+"/")
			// spec.json 由调用方按参数生成，跳过模板里的示例
			if rel == "spec.json" {
				return nil
			}
			dst = filepath.Join(demoDir, filepath.FromSlash(rel))
		default:
			return nil
		}
		b, rerr := fs.ReadFile(codeTplFS, p)
		if rerr != nil {
			return rerr
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
}

// codeVideoRoot 找 lemo 渲染库根目录。
func codeVideoRoot() string {
	cands := []string{}
	if v := strings.TrimSpace(os.Getenv("LEMO_HOME")); v != "" {
		cands = append(cands, v)
	}
	cands = append(cands,
		filepath.Join("D:"+string(os.PathSeparator), "AI", "lemo-opuscar"),
		filepath.Join(os.Getenv("USERPROFILE"), "lemo-opuscar"),
	)
	for _, c := range cands {
		if c == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(c, "core", "render", "video.mjs")); err == nil {
			return c
		}
	}
	return ""
}

func dataString(node *model.NodeInstance, key string) string {
	if v, ok := node.Data[key].(string); ok {
		return v
	}
	return ""
}

// codeVideoTimeout 按帧数估：1080p/24fps 单 worker 约 0.1s/帧
// （实测 still 200ms 含冷启动，整片按 worker 切分后更快），
// 保守取 0.25s/帧 + 60s 固定（浏览器启动 + ffmpeg 收尾）。
// spec JSON 里的 duration 节点上看不到，这里只按节点字段估，硬上限 30 分钟兜底。
func codeVideoTimeout(n *model.NodeInstance) time.Duration {
	dur := 6.0
	if v, ok := n.Data["duration"].(float64); ok && v > 0 {
		dur = v
	}
	fps := 24.0
	if v, ok := n.Data["fps"].(float64); ok && v > 0 {
		fps = v
	}
	return 60*time.Second + time.Duration(dur*fps*0.25*float64(time.Second))
}

// buildCodeSpec 从节点参数/上游文本组装渲染规格。
// 优先级：显式 spec JSON > 单卡字段（title/points/…）> 上游 text 作为标题。
func buildCodeSpec(node *model.NodeInstance, upstream map[string]interface{}) (*CodeSpec, error) {
	if raw := strings.TrimSpace(dataString(node, "spec")); raw != "" {
		var sp CodeSpec
		if err := json.Unmarshal([]byte(raw), &sp); err != nil {
			return nil, fmt.Errorf("code_video: spec 不是合法 JSON：%w", err)
		}
		return normalizeCodeSpec(&sp), nil
	}

	title := strings.TrimSpace(dataString(node, "title"))
	if title == "" {
		title = strings.TrimSpace(upstreamText(upstream))
	}
	if title == "" {
		return nil, fmt.Errorf("code_video: 缺少内容；填 title/points，或给 spec（JSON），或让上游文本流入")
	}

	points := splitLoose(dataString(node, "points"))
	card := CodeCard{
		Kicker:  strings.TrimSpace(dataString(node, "kicker")),
		Heading: title,
		Points:  points,
		Figure:  strings.TrimSpace(dataString(node, "figure")),
		Labels:  splitLoose(dataString(node, "labels")),
		Note:    strings.TrimSpace(dataString(node, "note")),
	}
	for _, v := range splitLoose(dataString(node, "values")) {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			card.Values = append(card.Values, n)
		}
	}
	sp := CodeSpec{
		Title:  title,
		Kicker: strings.TrimSpace(dataString(node, "kicker")),
		Cards:  []CodeCard{card},
	}
	if v, ok := node.Data["duration"].(float64); ok && v > 0 {
		sp.Duration = v
	}
	if v, ok := node.Data["fps"].(float64); ok && v > 0 {
		sp.FPS = int(v)
	}
	return normalizeCodeSpec(&sp), nil
}

func normalizeCodeSpec(sp *CodeSpec) *CodeSpec {
	if sp.FPS <= 0 {
		sp.FPS = 24
	}
	if sp.CardSeconds <= 0 {
		sp.CardSeconds = 6
	}
	if len(sp.Cards) == 0 {
		sp.Cards = []CodeCard{{Heading: sp.Title}}
	}
	if sp.Duration <= 0 {
		sp.Duration = float64(len(sp.Cards)) * sp.CardSeconds
	}
	if sp.Title == "" {
		sp.Title = sp.Cards[0].Heading
	}
	if sp.TitleEn == "" {
		sp.TitleEn = "FLOWEDGE · CODE VIDEO"
	}
	return sp
}

// splitLoose 按换行/中文逗号/英文逗号切分，去空白与空项。
func splitLoose(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	f := func(r rune) bool { return r == '\n' || r == ',' || r == '，' || r == ';' || r == '；' }
	out := []string{}
	for _, p := range strings.FieldsFunc(s, f) {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
