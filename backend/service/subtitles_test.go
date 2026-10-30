package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"flowedge/backend/model"
)

func TestTransitionOverlapD(t *testing.T) {
	cases := map[string]float64{"cut": 0.08, "dissolve": 0.30, "continue": 0.12, "": 0.08, "乱写": 0.08}
	for trans, want := range cases {
		if got := transitionOverlapD(trans); got != want {
			t.Errorf("transitionOverlapD(%q)=%v，期望 %v（装配器与字幕排轴共用，改值必须一起改）", trans, got, want)
		}
	}
}

func TestFilmSubtitleLines_Timeline(t *testing.T) {
	dub := func(lines ...DubLineInfo) *DubTrackReport {
		return &DubTrackReport{Lines: lines}
	}
	dubs := []*DubTrackReport{
		dub(DubLineInfo{Speaker: "老崔", Text: "灯又灭了", Start: 0.25, End: 1.50}),
		nil, // 无台词镜
		dub(DubLineInfo{Text: "又是它", Start: 0.20, End: 0.90},
			DubLineInfo{Text: "三十年前的今天也是这盏灯", Start: 1.50, End: 3.00}),
	}
	// 三镜各 2s，衔接 cut（0.08）+ dissolve（0.30）：
	// 镜起点 = 0 / 1.92 / 3.62；镜2 的超长句尾必须夹回 5.62−0.05
	durations := []float64{2.0, 2.0, 2.0}
	transitions := []string{"cut", "dissolve"}
	lines := filmSubtitleLines(dubs, durations, transitions)
	if len(lines) != 3 {
		t.Fatalf("应排出 3 条字幕，实际 %d 条", len(lines))
	}
	want := []struct{ start, end float64 }{{0.25, 1.50}, {3.82, 4.52}, {5.12, 5.57}}
	for i, w := range want {
		// 容差比较：3.62+0.20 这类和的浮点表示与字面量差 1ulp，语义上相等
		if d0, d1 := lines[i].Start-w.start, lines[i].End-w.end; d0 > 1e-9 || d0 < -1e-9 || d1 > 1e-9 || d1 < -1e-9 {
			t.Errorf("第 %d 条 (%.4f–%.4f)，期望 (%.2f–%.2f)：接缝重叠值没扣或截断夹取不对",
				i+1, lines[i].Start, lines[i].End, w.start, w.end)
		}
	}
	if lines[0].Speaker != "老崔" {
		t.Errorf("说话人丢了：%q", lines[0].Speaker)
	}
}

func TestWriteShotSRT_Format(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.srt")
	lines := []srtLine{
		{Start: 0.25, End: 1.5, Speaker: "老崔", Text: "灯又灭了"},
		{Start: 3.82, End: 4.52, Text: "又是它"},
	}
	if err := writeShotSRT(path, lines); err != nil {
		t.Fatalf("写 SRT 失败：%v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	want := "1\n00:00:00,250 --> 00:00:01,500\n老崔：灯又灭了\n\n" +
		"2\n00:00:03,820 --> 00:00:04,520\n又是它\n\n"
	if got != want {
		t.Errorf("SRT 格式不对：\n%q\n期望：\n%q", got, want)
	}
}

func TestPickSubtitlePathFromStates(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "film.srt")
	if err := os.WriteFile(real, []byte("1\n00:00:00,000 --> 00:00:01,000\nx\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mk := func(status string, output interface{}) model.NodeInstance {
		return model.NodeInstance{Status: status, Output: output}
	}
	// 成功节点带真实文件 → 找到
	states := []model.NodeInstance{
		mk("success", map[string]interface{}{"video": "x.mp4"}),
		mk("success", map[string]interface{}{"subtitles": real}),
	}
	if got := pickSubtitlePathFromStates(states); got != real {
		t.Errorf("应找到 %s，实际 %q", real, got)
	}
	// 文件不存在 → 忽略（不许把空气当字幕烧）
	states = append(states, mk("success", map[string]interface{}{"subtitles": filepath.Join(dir, "ghost.srt")}))
	_ = states // pick 从后往前扫，ghost 在最后会被先扫到——单独验证：
	if got := pickSubtitlePathFromStates([]model.NodeInstance{
		mk("success", map[string]interface{}{"subtitles": filepath.Join(dir, "ghost.srt")}),
	}); got != "" {
		t.Errorf("不存在的字幕文件应被忽略，实际 %q", got)
	}
	// 失败节点不参与
	if got := pickSubtitlePathFromStates([]model.NodeInstance{
		mk("failed", map[string]interface{}{"subtitles": real}),
	}); got != "" {
		t.Errorf("失败节点的产物不该被采用，实际 %q", got)
	}
}

func TestParamBool(t *testing.T) {
	if !paramBool(map[string]interface{}{"subtitles": true}, "subtitles") {
		t.Error("bool true 应识别")
	}
	if !paramBool(map[string]interface{}{"subtitles": "true"}, "subtitles") {
		t.Error("字符串 \"true\" 应识别（前端某些路径序列化成字符串）")
	}
	if paramBool(map[string]interface{}{"subtitles": false}, "subtitles") {
		t.Error("bool false 不应识别为真")
	}
	if paramBool(map[string]interface{}{}, "subtitles") {
		t.Error("缺键应为假")
	}
}

// TestGeneratedSRTBurnsPreservingFrames 集成：自己生成的 SRT 必须能被烧录链
// （libass SRT 模式）消费，且帧数/时长/音轨不损。这是「自动字幕」闭环的产物级证明。
func TestGeneratedSRTBurnsPreservingFrames(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("未安装 ffmpeg，跳过")
	}
	svc := NewVideoService()
	if strings.TrimSpace(svc.FFmpegPath) == "" {
		t.Skip("VideoService 没解析到 ffmpeg，跳过")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mp4")
	gen := exec.Command(svc.FFmpegPath, "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=0x203040:s=320x180:d=2:r=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("造测试片失败：%v（%s）", err, string(out))
	}
	srt := filepath.Join(dir, "clip.srt")
	lines := []srtLine{
		{Start: 0.2, End: 0.9, Speaker: "老崔", Text: "灯又灭了"},
		{Start: 1.1, End: 1.8, Text: "又是它"},
	}
	if err := writeShotSRT(srt, lines); err != nil {
		t.Fatalf("生成 SRT 失败：%v", err)
	}
	dst, err := svc.ApplyTextOverlay(src, OverlayOptions{SRT: srt, Style: StyleCaption})
	if err != nil {
		t.Fatalf("烧录自动生成的 SRT 失败：%v", err)
	}
	info, err := inspectMedia(dst)
	if err != nil || !info.OK {
		t.Fatalf("烧录产物无效：%v %+v", err, info)
	}
	dur, _ := probeMedia(dst)
	if dur.dur < 1.9 || dur.dur > 2.1 {
		t.Errorf("烧录后时长 %.3f 偏离源 2s：字幕链改了时长", dur.dur)
	}
}
