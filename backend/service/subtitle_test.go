package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatSRTTime(t *testing.T) {
	cases := map[float64]string{
		0:      "00:00:00,000",
		1.5:    "00:00:01,500",
		61.25:  "00:01:01,250",
		3661.5: "01:01:01,500",
		-3:     "00:00:00,000",
	}
	for in, want := range cases {
		if got := FormatSRTTime(in); got != want {
			t.Errorf("FormatSRTTime(%v)=%q，期望 %q", in, got, want)
		}
	}
}

func TestBuildSRTSkipsEmptyAndPreventsOverlap(t *testing.T) {
	out := BuildSRT([]SubtitleLine{
		{Start: 0, End: 1, Text: "第一句"},
		{Start: 1.5, End: 2, Text: "   "},   // 空行必须丢掉，否则播放器显示一个空条
		{Start: 2.0, End: 2.1, Text: "第二句"}, // 0.1s 太短 → 拉到 0.6s
		{Start: 2.2, End: 3.0, Text: "第三句"}, // 与上一条重叠 → 起点顺延
	})
	if strings.Count(out, "-->") != 3 {
		t.Fatalf("应输出 3 条字幕，实际：\n%s", out)
	}
	if !strings.Contains(out, "00:00:02,000 --> 00:00:02,600") {
		t.Fatalf("过短字幕应被拉到 0.6s：\n%s", out)
	}
	if strings.Contains(out, "00:00:02,200 -->") {
		t.Fatalf("重叠字幕起点应顺延（2.2 → 2.6），不该出现 2.2 起点：\n%s", out)
	}
}

func TestCleanDialogueStripsSpeaker(t *testing.T) {
	cases := map[string]string{
		"少女：这件事我其实早就知道了": "这件事我其实早就知道了",
		"老人：「你来晚了」":      "你来晚了",
		"无对白":            "",
		"静默":             "",
		"（环境声）":          "（环境声）",
	}
	for in, want := range cases {
		if got := cleanDialogue(in); got != want {
			t.Errorf("cleanDialogue(%q)=%q，期望 %q", in, got, want)
		}
	}
}

func TestSubtitlesFromPlanUsesShotTimeline(t *testing.T) {
	plan := ShotPlan{Shots: []Shot{
		{Seconds: 5, Dialogue: "少女：第一句话"},
		{Seconds: 4, Dialogue: "无对白"},
		{Seconds: 3, Dialogue: "少年：第二句话"},
	}}
	subs := SubtitlesFromPlan(plan)
	if len(subs) != 2 {
		t.Fatalf("只有两镜有台词，实际 %d 条", len(subs))
	}
	if subs[0].Start != 0.15 || subs[0].End != 4.85 {
		t.Fatalf("第一镜时间轴错了：%+v", subs[0])
	}
	// 第二句必须落在 5+4=9s 之后，而不是紧接第一句
	if subs[1].Start < 9.0 {
		t.Fatalf("无对白那一镜也要占时间轴，第二句不该早于 9s：%+v", subs[1])
	}
	if subs[1].Text != "第二句话" {
		t.Fatalf("台词应去掉「身份：」前缀，实际 %q", subs[1].Text)
	}
}

func TestSubtitlesFromLinesEvenSplit(t *testing.T) {
	subs := SubtitlesFromLines([]string{"a", "", "b"}, 10)
	if len(subs) != 2 {
		t.Fatalf("空行应被忽略，实际 %d 条", len(subs))
	}
	if subs[1].Start < 4.9 || subs[1].Start > 5.2 {
		t.Fatalf("两句均分 10s，第二句应从约 5s 开始：%+v", subs[1])
	}
}

// 真机烧录验证：默认跳过（依赖本机 ffmpeg 是否编译了 libass）。
// 手动跑：FLOWEDGE_REAL_SUB=1 go test ./service/ -run TestRealBurnSubtitles -v
func TestRealBurnSubtitles(t *testing.T) {
	if os.Getenv("FLOWEDGE_REAL_SUB") != "1" {
		t.Skip("需要真实 ffmpeg，设 FLOWEDGE_REAL_SUB=1 才跑")
	}
	dir := t.TempDir()
	video := filepath.Join(dir, "in.mp4")
	mk := exec.Command(ffmpegBin(), "-y",
		"-f", "lavfi", "-i", "color=c=navy:s=320x240:d=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-shortest", "-pix_fmt", "yuv420p", video)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("造测试视频失败：%v\n%s", err, out)
	}

	srt := filepath.Join(dir, "in.srt")
	if err := os.WriteFile(srt, []byte(BuildSRT([]SubtitleLine{
		{Start: 0.2, End: 1.8, Text: "中文测试字幕"},
	})), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.mp4")
	svc := NewVideoService()
	if err := svc.BurnSubtitles(video, srt, out, SubtitleStyle{FontSize: 18}); err != nil {
		t.Fatalf("烧字幕失败：%v", err)
	}
	info, err := probeMedia(out)
	if err != nil {
		t.Fatalf("产物读不出来：%v", err)
	}
	if !info.hasAudio {
		t.Fatal("音频流被丢了：-c:a copy 应该保住音轨")
	}
	if info.dur < 1.8 || info.dur > 2.4 {
		t.Fatalf("产物时长异常：%.2fs", info.dur)
	}

	// 「跑通了」不等于「字幕真的画上去了」：时长和音轨都正常、画面却没字，是最典型的假成功。
	// 用 PSNR 对账——两段画面的差异必须来自像素变化；inf 说明产物与原片逐像素相同。
	psnr := exec.Command(ffmpegBin(), "-hide_banner",
		"-i", video, "-i", out, "-lavfi", "psnr", "-f", "null", "-")
	raw, _ := psnr.CombinedOutput()
	txt := string(raw)
	i := strings.Index(txt, "average:")
	if i < 0 {
		t.Fatalf("拿不到 PSNR，无法证明字幕烧上去了：\n%s", txt)
	}
	val := strings.Fields(txt[i+len("average:"):])[0]
	if strings.HasPrefix(val, "inf") {
		t.Fatal("原片与产物逐像素相同——滤镜跑了但没有画上字幕")
	}
	t.Logf("烧字幕成功：%s 时长 %.2fs，与原片 PSNR=%s（画面确有变化）", filepath.Base(out), info.dur, val)
}

// 富模式台词表（lines）也要能排字幕——dialogue 字段是「无对白」占位，
// 真正的话在 lines 里。旧实现只读 dialogue，富模式分镜表会被判「没有台词」。
func TestSubtitlesFromPlanRichLines(t *testing.T) {
	plan := ShotPlan{Shots: []Shot{
		{ID: 1, Frames: 192, Lead: 0.25,
			Lines: []PlanLine{{Text: "你输了"}, {Text: "还有谁"}}},
		{ID: 2, Frames: 124, Dialogue: "无对白"},
	}}
	subs := SubtitlesFromPlan(plan)
	if len(subs) != 2 {
		t.Fatalf("应排出 2 条字幕，got %d：%+v", len(subs), subs)
	}
	// 首句落在 lead 之后（0.25s），两条都在第一镜 8s 内，且不重叠
	if subs[0].Start < 0.25 || subs[0].End <= subs[0].Start {
		t.Fatalf("首条字幕时点不对：%+v", subs[0])
	}
	if subs[1].Start <= subs[0].Start || subs[1].Start >= 8.0 {
		t.Fatalf("第二条字幕时点不对：%+v", subs[1])
	}
	if subs[0].Text != "你输了" || subs[1].Text != "还有谁" {
		t.Fatalf("字幕文本错：%+v", subs)
	}
}
