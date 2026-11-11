package service

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"flowedge/backend/model"
)

// 确定性贴字（标题 / 字幕 / 信息条），出片成功后由 produce 调用。
//
// 为什么需要它（2026-10-26 实测）：
//
//	H3 在「少字 + 大字 + 静止镜头」下确实能把中文写对（「山海茶肆」四字，
//	608×352×226 帧全片不走形），但**密集小字、长句、多行一定糊成伪字**。
//	需要精确文案时，唯一可靠的办法是出片后把字排上去：字型、位置、时点全部可控，
//	重跑调参不用重新出片（零 GPU）。
//
// 为什么不用 drawtext（第一版就是它，效果被判定"像贴纸"）：
//
//	drawtext 只能给「字 + 硬底色块」，没有字重/字间距/描边层次/淡入淡出，
//	在 352p 上直接烧还会糊边。改成 libass（ASS 字幕）后：Bold、Spacing、
//	Outline+Shadow 双层、\fad 淡入淡出都是原生能力，这才是「排过版」的样子；
//	再把成片按需上采样到 720p 再烧字，字才不会像素化。
//
// 纪律：
//   - 贴完必须 ffprobe 复核「帧数 / 时长 / 音频流」，任一不符就报错，
//     不许返回一个"看起来成功"的坏文件（AGENTS §0.5 / §0.12 ②）。
//   - 参数一律显式，不许魔法常数：字号/边距都由画面高度推导，其余来自请求。
const (
	overlayKeyText     = "text_overlay"
	overlayKeySRT      = "overlay_srt"
	overlayKeyPosition = "overlay_position"
	overlayKeyStyle    = "overlay_style"
	overlayKeySize     = "overlay_size"
	overlayKeyScale    = "overlay_scale"
	overlayKeyBox      = "overlay_box"
	overlayKeyFrom     = "overlay_from"
	overlayKeyTo       = "overlay_to"
)

// 样式预设。名字进 API，别随手改。
const (
	// StyleCaption 常规字幕：底部居中，白字 + 描边阴影。
	StyleCaption = "caption"
	// StyleTitle 片名/大标题：更大、加字间距、淡入淡出、下半部压暗带。
	StyleTitle = "title"
	// StylePlate 底板条：实底块（信息卡、告示），适合花背景。
	StylePlate = "plate"
	// StyleLowerThird 左下角信息条：新闻/访谈式，带一条细金线。
	StyleLowerThird = "lower_third"
)

// OverlayOptions 是一次贴字的完整参数（零值 = 全默认）。
type OverlayOptions struct {
	Text     string  // 要贴的文字，换行用 \n
	SRT      string  // 字幕文件（.srt/.ass）；给了就按时间轴烧，忽略 Text
	Position string  // top | center | bottom（默认按样式）
	Style    string  // caption | title | plate | lower_third
	Size     int     // 字号 px；0 = 按样式与画面高度推导
	Scale    int     // 上采样目标高度（720/1080）；0 = 保持原分辨率
	Font     string  // 字体文件；空 = 按 msyhbd → msyh → simhei 顺序找
	Color    string  // 字色（ASS &HBBGGRR 或 white/#RRGGBB/gold）
	Border   int     // 描边宽度 px（0 = 按字号推导）
	Box      bool    // 兼容旧字段：等价于 Style=plate
	From     float64 // 出现时间（秒）
	To       float64 // 消失时间（秒；<=0 = 到片尾）
}

// videoExts 是"能贴字"的产物后缀。贴字对音频/图片没有意义，遇到就直接报错，
// 而不是硬跑一遍 ffmpeg 得出个空文件。
var videoExts = map[string]bool{".mp4": true, ".mov": true, ".mkv": true, ".webm": true, ".m4v": true}

// ApplyTextOverlay 按本次请求的参数给运行产物贴字，返回新产物路径。
//
// 没请求贴字（两个内容键都空）时返回 ("", nil)，调用方据此区分"没做"与"做失败"。
// 返回错误时调用方必须把这次运行判成 failed —— 交付物没达标就不许报成功。
func (e *EngineV2) ApplyTextOverlay(states []model.NodeInstance, params map[string]interface{}) (string, error) {
	if e == nil || e.video == nil {
		return "", nil
	}
	opt, want, err := overlayOptionsFromParams(params)
	if err != nil {
		return "", err
	}
	// 自动字幕（2026-10-30）：subtitles=true 且没手填 overlay_srt 时，用节点
	// 产出的 SRT（配音实测时间戳排轴，shotlist 写到成片同名 .srt）。
	// 显式给的 overlay_text/overlay_srt 永远优先——参数化不覆盖显式指定。
	if !want && paramBool(params, "subtitles") {
		if srt := pickSubtitlePathFromStates(states); srt != "" {
			opt.SRT = srt
			want = true
		}
	}
	if !want {
		return "", nil
	}

	src := pickArtifactPathFromStates(states)
	if src == "" {
		return "", fmt.Errorf("贴字失败：这次运行没有产出可贴字的视频文件")
	}
	if ext := strings.ToLower(filepath.Ext(src)); !videoExts[ext] {
		return "", fmt.Errorf("贴字只支持视频产物，当前是 %q（%s）", ext, src)
	}

	dst, err := e.video.ApplyTextOverlay(src, opt)
	if err != nil {
		return "", err
	}
	// 把节点 Output 指向新文件：产物清单、前端预览、后续门禁都以它为准。
	// 原文件保留（便于对比「贴字前/后」），不删。
	rewriteArtifactPath(states, src, dst)
	return dst, nil
}

// paramBool 读 produce 参数里的布尔值（JSON bool；字符串 "true" 也认——
// 前端某些路径会把 bool 序列化成字符串）。
func paramBool(params map[string]interface{}, key string) bool {
	switch v := params[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

// pickSubtitlePathFromStates 找节点产出的字幕文件：shotlist 在有台词时把
// 成片同名 .srt 的路径写进 Output["subtitles"]。只认磁盘上真实存在的文件。
func pickSubtitlePathFromStates(states []model.NodeInstance) string {
	for i := len(states) - 1; i >= 0; i-- {
		if states[i].Status != "success" {
			continue
		}
		m, ok := states[i].Output.(map[string]interface{})
		if !ok {
			continue
		}
		if p, ok := m["subtitles"].(string); ok && p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// ApplyTextOverlay 对单个视频文件贴字，写到同目录 <名字>-text.mp4。
func (s *VideoService) ApplyTextOverlay(src string, opt OverlayOptions) (string, error) {
	if s == nil || strings.TrimSpace(s.FFmpegPath) == "" {
		return "", fmt.Errorf("贴字失败：没有可用的 ffmpeg")
	}
	if _, err := os.Stat(src); err != nil {
		return "", fmt.Errorf("贴字失败：输入不存在（%s）：%w", src, err)
	}
	probe := ffprobeBeside(s.FFmpegPath)
	if probe == "" {
		return "", fmt.Errorf("贴字失败：找不到 ffprobe（贴完要核帧数与流列表，不能跳过）")
	}

	in, err := overlayProbe(probe, src)
	if err != nil {
		return "", fmt.Errorf("贴字失败：读不了输入：%w", err)
	}
	if in.Width <= 0 || in.Height <= 0 {
		return "", fmt.Errorf("贴字失败：输入没有视频流（%s）", src)
	}

	// 输出分辨率：按需上采样（只上不下）。上采样后再烧字，字才是按目标尺寸排的，
	// 不会被"先烧小再放大"糊掉。
	outW, outH := in.Width, in.Height
	if opt.Scale > in.Height {
		ratio := float64(opt.Scale) / float64(in.Height)
		outH = opt.Scale
		outW = int(float64(in.Width)*ratio/2+0.5) * 2 // 偶数宽度，编码器友好
	}

	vf, cleanup, err := buildOverlayFilter(opt, outW, outH, in.Duration, filepath.Dir(src))
	if err != nil {
		return "", err
	}
	defer cleanup()

	dst := strings.TrimSuffix(src, filepath.Ext(src)) + "-text.mp4"
	args := []string{"-y", "-hide_banner", "-loglevel", "error", "-i", src}
	if outW != in.Width || outH != in.Height {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d:flags=lanczos,%s", outW, outH, vf))
	} else {
		args = append(args, "-vf", vf)
	}
	args = append(args, "-c:v", "libx264", "-preset", "medium", "-crf", "18", "-pix_fmt", "yuv420p")
	if in.FPS > 0 {
		// 显式锁帧率：烧字不该改变时基，重编码后帧数必须与输入一致。
		args = append(args, "-r", strconv.FormatFloat(in.FPS, 'f', 3, 64))
	}
	if in.AudioStreams > 0 {
		args = append(args, "-c:a", "copy")
	}
	args = append(args, "-movflags", "+faststart", dst)

	if out, err := hiddenExec(s.FFmpegPath, args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("贴字失败：ffmpeg 报错：%w（%.400s）", err, string(out))
	}

	got, err := overlayProbe(probe, dst)
	if err != nil {
		return "", fmt.Errorf("贴字失败：输出读不回来：%w", err)
	}
	if problems := compareOverlayIO(in, got); len(problems) > 0 {
		_ = os.Remove(dst)
		return "", fmt.Errorf("贴字产物不合格：%s", strings.Join(problems, "；"))
	}
	return dst, nil
}

// buildOverlayFilter 生成 -vf 字符串，并返回一个必须在 ffmpeg 跑完之后调用的
// cleanup（libass 要在运行时读这份 ASS，提前删就等于让它读空气）。
//
// 两条路径：
//   - SRT 模式：把用户字幕直接交给 libass（保留原时间轴），只强制字体与描边。
//   - 文字模式：按预设生成一份 ASS，再交给 libass 渲染。
//
// tmpDir 必须是**确定可写**的目录（调用方传产物所在目录），不能用系统 Temp：
// 实测引擎进程对 `%LOCALAPPDATA%\Temp` 是 Access denied（2026-10-26 现场：
// "临时文件创建不了：open ...Temp\flowedge-overlay-xxx.txt: Access is denied"，
// 同一个错误也让引擎派生的 H3 起不来——torch 启动时要写 Temp）。产物目录能写出
// 视频就必然可写，把临时文件放那儿，这条失败路径就结构性地不存在了。
func buildOverlayFilter(opt OverlayOptions, width, height int, duration float64, tmpDir string) (string, func(), error) {
	noop := func() {}

	if opt.SRT != "" {
		if _, err := os.Stat(opt.SRT); err != nil {
			return "", noop, fmt.Errorf("贴字失败：字幕文件不存在（%s）：%w", opt.SRT, err)
		}
		size := opt.Size
		if size <= 0 {
			size = overlayFontSize(height)
		}
		style := fmt.Sprintf("FontName=Microsoft YaHei,FontSize=%d,PrimaryColour=&H00FFFFFF,Outline=%d,Shadow=1,BorderStyle=1",
			size, maxIntOr(2, size/12))
		return "subtitles=" + ffFilterPath(opt.SRT) + ":force_style='" + style + "'", noop, nil
	}

	text := strings.ReplaceAll(opt.Text, `\n`, "\n")
	if strings.TrimSpace(text) == "" {
		return "", noop, fmt.Errorf("贴字失败：文字为空")
	}

	style, err := normalizeOverlayStyle(opt.Style, opt.Box)
	if err != nil {
		return "", noop, err
	}
	font, err := pickOverlayFont(opt.Font)
	if err != nil {
		return "", noop, err
	}

	ass := buildASS(assSpec{
		text:     text,
		style:    style,
		font:     fontFileName(font),
		color:    opt.Color,
		size:     opt.Size,
		border:   opt.Border,
		position: opt.Position,
		width:    width,
		height:   height,
		from:     opt.From,
		to:       opt.To,
		duration: duration,
	})

	tmp, err := os.CreateTemp(tmpDir, "flowedge-overlay-*.ass")
	if err != nil {
		return "", noop, fmt.Errorf("贴字失败：临时文件创建不了（%s）：%w", tmpDir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.WriteString(ass); err != nil {
		tmp.Close()
		cleanup()
		return "", noop, fmt.Errorf("贴字失败：写 ASS 失败：%w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("贴字失败：关闭 ASS 失败：%w", err)
	}

	filters := []string{"ass=" + ffFilterPath(tmpPath)}
	// lower_third 的金线是"设计"的一部分：一条竖条压在文字左侧，
	// 让信息条看起来是排出来的，而不是随便飘在角上。
	if style == StyleLowerThird {
		size := overlaySizeFor(style, opt.Size, height)
		lines := 1 + strings.Count(text, "\n")
		lineH := int(float64(size)*1.25) * lines
		marginL := int(float64(width) * 0.055)
		marginV := int(float64(height) * 0.115)
		barY := height - marginV - lineH + int(float64(size)*0.15)
		filters = append(filters, fmt.Sprintf(
			"drawbox=x=%d:y=%d:w=%d:h=%d:color=0xE8C46A@0.95:t=fill",
			maxIntOr(4, marginL-8), maxIntOr(0, barY), maxIntOr(3, height/180), lineH))
	}
	// title 的压暗带：只压下半部分，字从画面里"浮出来"，不靠色块垫着。
	if style == StyleTitle {
		filters = append(filters, fmt.Sprintf(
			"drawbox=x=0:y=%d:w=%d:h=%d:color=black@0.30:t=fill",
			int(float64(height)*0.60), width, int(float64(height)*0.40)))
	}
	return strings.Join(filters, ","), cleanup, nil
}

// assSpec 是生成一份 ASS 所需的全部输入。
type assSpec struct {
	text     string
	style    string
	font     string
	color    string
	size     int
	border   int
	position string
	width    int
	height   int
	from     float64
	to       float64
	duration float64
}

// buildASS 拼出完整的 .ass 文本。
//
// 所有尺寸都由画面高度推导，不写死：同一份配置在 352p 和 720p 上得到相同的相对观感。
func buildASS(sp assSpec) string {
	var b strings.Builder
	size := overlaySizeFor(sp.style, sp.size, sp.height)
	border := sp.border
	if border <= 0 {
		border = defaultOutline(sp.style, size)
	}
	shadow := defaultShadow(sp.style, size)
	spacing := defaultSpacing(sp.style, size)
	marginV := defaultMarginV(sp.style, sp.height)
	align := defaultAlign(sp.style, sp.position)

	primary := assColor(sp.color, "00FFFFFF")
	// plate 用实底块：BorderStyle=4 时 BackColour 是块色，Outline 变成内边距。
	borderStyle := 1
	back := "&H80000000"
	outlineColour := "&H00000000"
	if sp.style == StylePlate {
		borderStyle = 4
		back = "&HA6000000" // ~65% 黑
		border = int(float64(size) * 0.55)
		shadow = 0
	}

	b.WriteString("[Script Info]\n")
	b.WriteString("ScriptType: v4.00+\n")
	b.WriteString(fmt.Sprintf("PlayResX: %d\n", sp.width))
	b.WriteString(fmt.Sprintf("PlayResY: %d\n", sp.height))
	b.WriteString("WrapStyle: 0\n")
	b.WriteString("ScaledBorderAndShadow: yes\n")
	b.WriteString("YCbCr Matrix: TV.709\n\n")

	b.WriteString("[V4+ Styles]\n")
	b.WriteString("Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, " +
		"Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, " +
		"Alignment, MarginL, MarginR, MarginV, Encoding\n")
	marginL := 40
	if sp.style == StyleLowerThird {
		marginL = int(float64(sp.width) * 0.055)
	}
	b.WriteString(fmt.Sprintf(
		"Style: Overlay,%s,%d,%s,%s,%s,%s,-1,0,0,0,100,100,%d,0,%d,%d,%d,%d,%d,%d,%d,1\n\n",
		sp.font, size, primary, primary, outlineColour, back,
		spacing, borderStyle, border, shadow, align, marginL, marginL, marginV))

	from, to := overlayWindow(sp.from, sp.to, sp.duration)
	fadeIn, fadeOut := defaultFade(sp.style)
	b.WriteString("[Events]\n")
	b.WriteString("Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n")
	b.WriteString(fmt.Sprintf("Dialogue: 0,%s,%s,Overlay,,0,0,0,,{\\fad(%d,%d)}%s\n",
		assTime(from), assTime(to), fadeIn, fadeOut, assEscape(sp.text)))
	return b.String()
}

// assEscape 把换行变成 ASS 的硬换行。
func assEscape(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", `\N`)
	return s
}

// assTime 输出 H:MM:SS.cc（ASS 用厘秒）。
func assTime(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	cs := int(sec*100 + 0.5)
	h := cs / 360000
	cs -= h * 360000
	m := cs / 6000
	cs -= m * 6000
	s := cs / 100
	cs -= s * 100
	return fmt.Sprintf("%d:%02d:%02d.%02d", h, m, s, cs)
}

// assColor 把 "white" / "#RRGGBB" / "&HBBGGRR" 统一成 ASS 的 &HAABBGGRR。
func assColor(v, fallback string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "&H" + fallback
	}
	if len(v) > 2 && (strings.HasPrefix(v, "&H") || strings.HasPrefix(v, "&h")) {
		return "&H" + strings.ToUpper(v[2:])
	}
	named := map[string]string{
		"white": "FFFFFF", "black": "000000", "gold": "E8C46A",
		"warm": "F2E6D0", "yellow": "FFE066",
	}
	if hex, ok := named[strings.ToLower(v)]; ok {
		return "&H00" + hex[4:6] + hex[2:4] + hex[0:2] // RGB → BGR
	}
	if strings.HasPrefix(v, "#") && len(v) == 7 {
		return "&H00" + strings.ToUpper(v[5:7]+v[3:5]+v[1:3])
	}
	return "&H" + fallback
}

// overlayWindow 计算显示区间：to<=0 表示到片尾；非法区间夹紧而不是报错，
// 因为这一步在渲染路径上，报错只会在 ffmpeg 跑完才被看见。
func overlayWindow(from, to, duration float64) (float64, float64) {
	if from < 0 {
		from = 0
	}
	if duration > 0 && from > duration {
		from = 0
	}
	if to <= 0 || (duration > 0 && to > duration) {
		to = duration
	}
	if duration <= 0 {
		to = from + 3
	}
	if to <= from {
		to = from + 0.5
	}
	return from, to
}

func normalizeOverlayStyle(style string, box bool) (string, error) {
	s := strings.ToLower(strings.TrimSpace(style))
	if s == "" {
		if box {
			return StylePlate, nil
		}
		return StyleCaption, nil
	}
	switch s {
	case StyleCaption, StyleTitle, StylePlate, StyleLowerThird:
		return s, nil
	case "subtitle", "sub":
		return StyleCaption, nil
	case "lower-third", "lowerthird", "info":
		return StyleLowerThird, nil
	}
	return "", fmt.Errorf("贴字失败：样式只能是 caption/title/plate/lower_third，收到 %q", style)
}

// overlaySizeFor 按样式推导字号：同一画面高度下，标题必然比字幕大。
func overlaySizeFor(style string, explicit, height int) int {
	if explicit > 0 {
		return explicit
	}
	div := 16.0
	switch style {
	case StyleTitle:
		div = 8.5
	case StylePlate:
		div = 17.0
	case StyleLowerThird:
		div = 19.0
	}
	s := int(float64(height)/div + 0.5)
	if s < 14 {
		s = 14
	}
	return s
}

func defaultOutline(style string, size int) int {
	switch style {
	case StyleTitle:
		return maxIntOr(2, size/18)
	case StylePlate:
		return maxIntOr(2, size/20)
	default:
		return maxIntOr(2, size/11)
	}
}

func defaultShadow(style string, size int) int {
	switch style {
	case StyleTitle:
		return maxIntOr(2, size/10)
	case StylePlate:
		return 0
	default:
		return maxIntOr(1, size/16)
	}
}

func defaultSpacing(style string, size int) int {
	if style == StyleTitle {
		if s := size / 10; s > 0 {
			return s
		}
		return 1
	}
	return 0
}

func defaultMarginV(style string, height int) int {
	switch style {
	case StyleTitle:
		return int(float64(height) * 0.10)
	case StyleLowerThird:
		return int(float64(height) * 0.115)
	case StylePlate:
		return int(float64(height) * 0.08)
	default:
		return int(float64(height) * 0.07)
	}
}

func defaultAlign(style, position string) int {
	// ASS 对齐：1=左下 2=中下 3=右下 4=左中 5=正中 6=右中 7=左上 8=中上 9=右上
	if style == StyleLowerThird {
		return 1
	}
	switch strings.ToLower(strings.TrimSpace(position)) {
	case "top":
		return 8
	case "center", "middle":
		return 5
	default:
		return 2
	}
}

// defaultFade 淡入淡出（毫秒）。标题给足，字幕给短——字幕淡太久会读不清。
func defaultFade(style string) (int, int) {
	if style == StyleTitle {
		return 300, 360
	}
	return 120, 120
}

// overlayOptionsFromParams 从流水线参数里读出贴字配置。
// 返回值 want 表示「用户确实要贴字」；只看两个内容键，其余是修饰项。
func overlayOptionsFromParams(params map[string]interface{}) (OverlayOptions, bool, error) {
	get := func(k string) string {
		if v, ok := params[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	num := func(k string) float64 {
		switch v := params[k].(type) {
		case float64:
			return v
		case int:
			return float64(v)
		}
		return 0
	}
	boolean := func(k string) bool {
		if v, ok := params[k].(bool); ok {
			return v
		}
		return false
	}

	opt := OverlayOptions{
		Text:     get(overlayKeyText),
		SRT:      get(overlayKeySRT),
		Position: get(overlayKeyPosition),
		Style:    get(overlayKeyStyle),
		Size:     int(num(overlayKeySize)),
		Scale:    int(num(overlayKeyScale)),
		Box:      boolean(overlayKeyBox),
		From:     num(overlayKeyFrom),
		To:       -1,
	}
	if v, ok := params[overlayKeyTo].(float64); ok {
		opt.To = v
	}
	if opt.Size < 0 {
		return opt, false, fmt.Errorf("贴字失败：字号不能为负（%d）", opt.Size)
	}
	if opt.From < 0 {
		return opt, false, fmt.Errorf("贴字失败：出现时间不能为负（%.2f）", opt.From)
	}
	if opt.Scale < 0 || (opt.Scale > 0 && opt.Scale < 240) {
		return opt, false, fmt.Errorf("贴字失败：上采样高度不合法（%d）", opt.Scale)
	}
	if opt.Text == "" && opt.SRT == "" {
		return opt, false, nil
	}
	return opt, true, nil
}

// rewriteArtifactPath 把节点 Output 里指向旧文件的值改成新文件。
// 不改变量结构，只换字符串值，避免影响下游对 Output 形状的假设。
func rewriteArtifactPath(states []model.NodeInstance, from, to string) {
	for i := range states {
		m, ok := states[i].Output.(map[string]interface{})
		if !ok {
			continue
		}
		for _, k := range []string{"video", "image", "audio", "path", "file"} {
			if v, ok := m[k].(string); ok && v == from {
				m[k] = to
			}
		}
	}
}

// ffFilterPath 把本地路径变成 ffmpeg 滤镜参数里可用的形式。
func ffFilterPath(p string) string {
	q := strings.ReplaceAll(p, "\\", "/")
	q = strings.ReplaceAll(q, ":", `\:`)
	return "'" + q + "'"
}

// fontFileName 只取字体名：libass 按**字体名**匹配，不认路径，
// 所以字体必须装在系统里（本机 msyh/simhei 都在 C:\Windows\Fonts）。
func fontFileName(p string) string {
	base := filepath.Base(p)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	switch strings.ToLower(name) {
	case "msyh", "msyhbd":
		return "Microsoft YaHei"
	case "msyhl":
		return "Microsoft YaHei Light"
	case "simhei":
		return "SimHei"
	case "simsun":
		return "SimSun"
	}
	return name
}

func overlayFontSize(height int) int {
	if height <= 0 {
		return 28
	}
	if s := height / 16; s > 16 {
		return s
	}
	return 16
}

// pickOverlayFont 按顺序找本机可用的中文字体（Windows 常见三档）。
func pickOverlayFont(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("贴字失败：指定的字体不存在（%s）", explicit)
		}
		return explicit, nil
	}
	for _, f := range []string{
		`C:\Windows\Fonts\msyhbd.ttc`,
		`C:\Windows\Fonts\msyh.ttc`,
		`C:\Windows\Fonts\simhei.ttf`,
	} {
		if _, err := os.Stat(f); err == nil {
			return f, nil
		}
	}
	return "", fmt.Errorf("贴字失败：没找到中文字体（试过 msyhbd / msyh / simhei），可用 overlay_font 指定")
}

func ffprobeBeside(ffmpegPath string) string {
	dir := filepath.Dir(ffmpegPath)
	name := "ffprobe"
	if strings.EqualFold(filepath.Ext(ffmpegPath), ".exe") {
		name = "ffprobe.exe"
	}
	cand := filepath.Join(dir, name)
	if _, err := os.Stat(cand); err == nil {
		return cand
	}
	if p, err := exec.LookPath("ffprobe"); err == nil {
		return p
	}
	return ""
}

// overlayMedia 是复核需要的几个数（只取用得上的，不做通用封装）。
type overlayMedia struct {
	Width, Height int
	Duration      float64
	Frames        int
	AudioStreams  int
	VideoStreams  int
	FPS           float64
}

func overlayProbe(ffprobe, path string) (overlayMedia, error) {
	var p overlayMedia
	cmd := hiddenExec(ffprobe, "-v", "error", "-show_streams", "-show_format", "-of", "json", path)
	out, err := cmd.Output()
	if err != nil {
		return p, fmt.Errorf("ffprobe: %w", err)
	}
	var doc struct {
		Streams []struct {
			CodecType    string `json:"codec_type"`
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			NbFrames     string `json:"nb_frames"`
			AvgFrameRate string `json:"avg_frame_rate"`
			Duration     string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return p, fmt.Errorf("ffprobe JSON: %w", err)
	}
	p.Duration, _ = strconv.ParseFloat(doc.Format.Duration, 64)
	for _, st := range doc.Streams {
		switch st.CodecType {
		case "video":
			p.VideoStreams++
			if p.Width == 0 {
				p.Width, p.Height = st.Width, st.Height
			}
			if n, err := strconv.Atoi(st.NbFrames); err == nil && n > 0 && p.Frames == 0 {
				p.Frames = n
			}
			if p.FPS == 0 {
				if num, den, ok := splitRate(st.AvgFrameRate); ok && den > 0 {
					p.FPS = float64(num) / float64(den)
				}
			}
		case "audio":
			p.AudioStreams++
		}
	}
	if p.Frames == 0 {
		p.Frames = countOverlayFrames(ffprobe, path)
	}
	return p, nil
}

func splitRate(s string) (int, int, bool) {
	a, b, ok := strings.Cut(s, "/")
	if !ok {
		return 0, 0, false
	}
	n, err1 := strconv.Atoi(a)
	d, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return n, d, true
}

// countOverlayFrames 精确数帧：有些封装根本不写 nb_frames（H3 的 mp4 就是），
// 只信容器头的数字会让"帧数变了"这类事故漏检（AGENTS §0.12 ⑤）。
func countOverlayFrames(ffprobe, path string) int {
	out, err := hiddenExec(ffprobe, "-v", "error", "-select_streams", "v:0",
		"-count_frames", "-show_entries", "stream=nb_read_frames", "-of", "json", path).Output()
	if err != nil {
		return 0
	}
	var doc struct {
		Streams []struct {
			NbReadFrames string `json:"nb_read_frames"`
		} `json:"streams"`
	}
	if json.Unmarshal(out, &doc) != nil || len(doc.Streams) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(doc.Streams[0].NbReadFrames)
	return n
}

// compareOverlayIO 返回不合格项；空 = 通过。
//
// 判据只用用户能感知的东西：帧数、时长、音频流。
// **分辨率不在此列** —— overlay_scale 会主动上采样，那是允许的改动。
func compareOverlayIO(in, got overlayMedia) []string {
	var problems []string
	if in.Frames > 0 && got.Frames > 0 && in.Frames != got.Frames {
		problems = append(problems, fmt.Sprintf("帧数变了 %d → %d", in.Frames, got.Frames))
	}
	if in.Duration > 0 && got.Duration > 0 {
		if diff := in.Duration - got.Duration; diff > 0.15 || diff < -0.15 {
			problems = append(problems, fmt.Sprintf("时长漂移 %.2fs → %.2fs", in.Duration, got.Duration))
		}
	}
	if in.AudioStreams > 0 && got.AudioStreams == 0 {
		problems = append(problems, "音频流丢了")
	}
	if got.VideoStreams == 0 {
		problems = append(problems, "没有视频流")
	}
	return problems
}

func maxIntOr(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// PreviewOverlay 是给命令行预览用的薄封装：用默认 VideoService 跑一次贴字。
// 存在的意义是「先看效果再出片」——排版迭代不该每次都花几分钟重新生成。
func PreviewOverlay(src string, opt OverlayOptions) (string, error) {
	return NewVideoService().ApplyTextOverlay(src, opt)
}
