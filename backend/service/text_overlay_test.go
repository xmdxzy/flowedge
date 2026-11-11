package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 滤镜里交给 libass 的路径必须是「正斜杠 + 冒号转义 + 整体单引号」。
// 实测（ffmpeg 8.x）：只转义冒号不加引号，过滤器会把路径当成下一个选项名。
// 冒号只出现在 Windows 盘符上，所以转义规则本身用固定路径验，与平台无关。
func TestBuildOverlayFilter_UsesAssAndEscapesPath(t *testing.T) {
	vf, cleanup, err := buildOverlayFilter(OverlayOptions{Text: "山海茶肆"}, 608, 352, 9.4, t.TempDir())
	if err != nil {
		t.Fatalf("生成滤镜失败：%v", err)
	}
	defer cleanup()
	if !strings.HasPrefix(vf, "ass=") {
		t.Fatalf("文字模式应当走 libass（ass=），得到 %q", vf)
	}
	if !strings.HasPrefix(vf, "ass='") || !strings.Contains(vf, ".ass'") {
		t.Fatalf("ASS 路径没被单引号包住：%s", vf)
	}
	if got := ffFilterPath("C:\\Users\\you\\out.ass"); got != "'C\\:/Users/you/out.ass'" {
		t.Fatalf("路径转义不对：%s", got)
	}
}

func TestBuildOverlayFilter_RejectsBadInput(t *testing.T) {
	if _, _, err := buildOverlayFilter(OverlayOptions{Text: "  "}, 608, 352, 5, t.TempDir()); err == nil {
		t.Fatal("空文字必须报错")
	}
	if _, _, err := buildOverlayFilter(OverlayOptions{Text: "a", Style: "霓虹"}, 608, 352, 5, t.TempDir()); err == nil {
		t.Fatal("非法样式必须报错")
	}
	if _, _, err := buildOverlayFilter(OverlayOptions{SRT: filepath.Join(t.TempDir(), "nope.srt")}, 608, 352, 5, t.TempDir()); err == nil {
		t.Fatal("字幕文件不存在必须报错")
	}
}

// ASS 里的关键项：画布尺寸、淡入淡出、换行、时间窗、样式差异。
func TestBuildASS_Content(t *testing.T) {
	base := assSpec{text: "山海茶肆\n今夜开张", font: "Microsoft YaHei", width: 608, height: 352, duration: 9.4, from: 1, to: 4}
	cap := buildASS(base)
	if !strings.Contains(cap, "PlayResX: 608") || !strings.Contains(cap, "PlayResY: 352") {
		t.Fatalf("画布尺寸没写进 ASS：%s", cap)
	}
	if !strings.Contains(cap, `山海茶肆\N今夜开张`) {
		t.Fatalf("换行没转成 \\N：%s", cap)
	}
	if !strings.Contains(cap, "{\\fad(120,120)}") {
		t.Fatalf("字幕应当有 120ms 淡入淡出：%s", cap)
	}
	if !strings.Contains(cap, "Dialogue: 0,0:00:01.00,0:00:04.00") {
		t.Fatalf("时间窗不对：%s", cap)
	}

	title := buildASS(assSpec{text: "标题", style: StyleTitle, font: "Microsoft YaHei", width: 608, height: 352, duration: 9})
	if !strings.Contains(title, "{\\fad(300,360)}") {
		t.Fatalf("标题应当淡得更长：%s", title)
	}
}

// 新字段必须真的进到 OverlayOptions：样式、上采样高度。
// 这条是「接口收了字段但代码没读」的钉子（AGENTS §0.4：开关上线必须验生效）。
func TestOverlayOptionsFromParams_StyleAndScale(t *testing.T) {
	opt, want, err := overlayOptionsFromParams(map[string]interface{}{
		"text_overlay":  "山海茶肆",
		"overlay_style": "title",
		"overlay_scale": float64(720),
		"overlay_from":  float64(0.6),
		"overlay_to":    float64(4),
		"overlay_size":  float64(0),
	})
	if err != nil || !want {
		t.Fatalf("应当触发贴字：want=%v err=%v", want, err)
	}
	if opt.Style != "title" || opt.Scale != 720 {
		t.Fatalf("样式/上采样没解析进来：%+v", opt)
	}
	if _, _, err := overlayOptionsFromParams(map[string]interface{}{
		"text_overlay":  "x",
		"overlay_scale": float64(100), // 低于下限
	}); err == nil {
		t.Fatal("非法上采样高度必须报错")
	}
	if _, _, err := overlayOptionsFromParams(map[string]interface{}{
		"text_overlay":  "x",
		"overlay_style": "霓虹灯", // 非法样式要在参数层就拦下
	}); err != nil {
		t.Fatalf("样式合法性应在渲染层校验，参数层不该报错：%v", err)
	}
}

func TestOverlaySizeFor_TitleBiggerThanCaption(t *testing.T) {
	if overlaySizeFor(StyleTitle, 0, 352) <= overlaySizeFor(StyleCaption, 0, 352) {
		t.Fatal("标题字号必须大于字幕")
	}
	if overlaySizeFor(StyleCaption, 40, 352) != 40 {
		t.Fatal("显式字号必须优先")
	}
}

func TestAssColor(t *testing.T) {
	cases := map[string]string{
		"white":    "&H00FFFFFF",
		"":         "&H00FFFFFF",
		"gold":     "&H006AC4E8", // RGB E8C46A → BGR 6AC4E8
		"#FFCC00":  "&H0000CCFF",
		"&H00AABB": "&H00AABB",
	}
	for in, want := range cases {
		if got := assColor(in, "00FFFFFF"); got != want {
			t.Fatalf("assColor(%q) = %s，期望 %s", in, got, want)
		}
	}
}

func TestOverlayWindow(t *testing.T) {
	if from, to := overlayWindow(1, 0, 9.4); from != 1 || to != 9.4 {
		t.Fatalf("to<=0 应当到片尾：%v %v", from, to)
	}
	if from, to := overlayWindow(1, 99, 9.4); from != 1 || to != 9.4 {
		t.Fatalf("超出片长应当夹紧：%v %v", from, to)
	}
	if _, to := overlayWindow(5, 2, 9.4); to <= 5 {
		t.Fatalf("终点早于起点时必须抬到起点之后：%v", to)
	}
}

// 真跑一遍：贴完之后帧数、时长、音频流都不许变；上采样只改分辨率。
func TestApplyTextOverlay_PreservesFramesAndAudio(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("未安装 ffmpeg，跳过")
	}
	svc := NewVideoService()
	if strings.TrimSpace(svc.FFmpegPath) == "" {
		t.Skip("VideoService 没解析到 ffmpeg，跳过")
	}
	probe := ffprobeBeside(svc.FFmpegPath)
	if probe == "" {
		t.Skip("未安装 ffprobe，跳过")
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mp4")
	gen := exec.Command(svc.FFmpegPath, "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=0x203040:s=320x180:d=1.5:r=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1.5",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("造测试片失败：%v（%s）", err, string(out))
	}
	before, err := overlayProbe(probe, src)
	if err != nil {
		t.Fatalf("读输入失败：%v", err)
	}

	dst, err := svc.ApplyTextOverlay(src, OverlayOptions{Text: "山海茶肆", Style: StyleTitle, Scale: 720})
	if err != nil {
		t.Fatalf("贴字失败：%v", err)
	}
	after, err := overlayProbe(probe, dst)
	if err != nil {
		t.Fatalf("读产物失败：%v", err)
	}
	if problems := compareOverlayIO(before, after); len(problems) > 0 {
		t.Fatalf("产物不合格：%s", strings.Join(problems, "；"))
	}
	if after.Height != 720 {
		t.Fatalf("上采样后高度应为 720，得到 %d", after.Height)
	}
	if after.AudioStreams != before.AudioStreams || after.Frames != before.Frames {
		t.Fatalf("帧数/音频流必须一致：%d/%d -> %d/%d",
			before.Frames, before.AudioStreams, after.Frames, after.AudioStreams)
	}
	if info, err := os.Stat(dst); err != nil || info.Size() == 0 {
		t.Fatal("产物为空或不存在")
	}
}
