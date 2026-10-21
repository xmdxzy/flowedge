package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestBuildPostFilterNoop(t *testing.T) {
	f, v, a := buildPostFilter(PostSpec{}, true)
	if f != "" {
		t.Fatalf("什么都没有时不该生成滤镜（否则白付一次重编码）：%q", f)
	}
	if v != "0:v" || a != "" {
		t.Fatalf("标签不对：v=%q a=%q", v, a)
	}
}

func TestBuildPostFilterVerticalCrop(t *testing.T) {
	f, v, a := buildPostFilter(PostSpec{Width: 1080, Height: 1920}, true)
	if !strings.Contains(f, "scale=1080:1920:force_original_aspect_ratio=increase,crop=1080:1920") {
		t.Fatalf("中心裁切式子不对：%s", f)
	}
	if v != "vfit" || a != "" {
		t.Fatalf("标签不对：v=%q a=%q", v, a)
	}
}

func TestBuildPostFilterVerticalBlur(t *testing.T) {
	f, v, _ := buildPostFilter(PostSpec{Width: 1080, Height: 1920, Fit: "blur"}, true)
	for _, want := range []string{"split=2[bga][fga]", "boxblur=24:2", "overlay=(W-w)/2:(H-h)/2", "force_original_aspect_ratio=decrease"} {
		if !strings.Contains(f, want) {
			t.Fatalf("模糊垫底缺少 %q：%s", want, f)
		}
	}
	if v != "vfit" {
		t.Fatalf("视频标签应是 vfit，实际 %q", v)
	}
}

func TestBuildPostFilterBGMDuck(t *testing.T) {
	f, _, a := buildPostFilter(PostSpec{BGM: "bgm.wav", BGMGainDB: -12, Duck: true}, true)
	for _, want := range []string{"asplit=2[voice][sc]", "volume=-12.0dB", "sidechaincompress=threshold=0.05:ratio=8", "amix=inputs=2:duration=first:dropout_transition=0:normalize=0"} {
		if !strings.Contains(f, want) {
			t.Fatalf("侧链闪避缺少 %q：%s", want, f)
		}
	}
	if a != "aout" {
		t.Fatalf("音频标签应是 aout，实际 %q", a)
	}
}

func TestBuildPostFilterBGMWithoutDuck(t *testing.T) {
	f, _, _ := buildPostFilter(PostSpec{BGM: "bgm.wav", BGMGainDB: -6, Duck: false}, true)
	if strings.Contains(f, "sidechaincompress") {
		t.Fatalf("关掉 duck 就不该有侧链压缩：%s", f)
	}
	if !strings.Contains(f, "amix=inputs=2") || !strings.Contains(f, "normalize=0") {
		// normalize=0 是实测出来的：默认归一化会把 BGM 压到听不见
		t.Fatalf("仍然要混音，且必须 normalize=0：%s", f)
	}
}

func TestBuildPostFilterBGMOnSilentVideo(t *testing.T) {
	// 原片没音轨：不能引用 [0:a]，否则 ffmpeg 直接报 "Stream specifier ':a' ... matches no streams"
	f, _, a := buildPostFilter(PostSpec{BGM: "bgm.wav"}, false)
	if strings.Contains(f, "[0:a]") {
		t.Fatalf("无音轨的原片不该被引用音频流：%s", f)
	}
	if !strings.Contains(f, "[1:a]") || a != "aout" {
		t.Fatalf("BGM 应直接成为主音轨：%s a=%q", f, a)
	}
}

func TestBuildPostFilterSubtitlesInSamePass(t *testing.T) {
	f, v, _ := buildPostFilter(PostSpec{Width: 1080, Height: 1920, SRT: "x.srt", FontSize: 18, MarginV: 20}, true)
	if !strings.Contains(f, "subtitles=sub.srt:force_style='FontSize=18,MarginV=20,Outline=2,Shadow=0'") {
		t.Fatalf("字幕滤镜应与竖屏串在同一条链上：%s", f)
	}
	if v != "vsub" {
		t.Fatalf("字幕之后的视频标签应是 vsub，实际 %q", v)
	}
}

// 真机验证：横屏 → 竖屏 + BGM + 字幕一次通行，并 A/B 验证侧链闪避真的压了 BGM。
// 手动跑：FLOWEDGE_REAL_POST=1 go test ./service/ -run TestRealPostProcess -v
func TestRealPostProcess(t *testing.T) {
	if os.Getenv("FLOWEDGE_REAL_POST") != "1" {
		t.Skip("需要真实 ffmpeg，设 FLOWEDGE_REAL_POST=1 才跑")
	}
	dir := t.TempDir()
	svc := NewVideoService()

	mkAV := func(name, audioSrc string) string {
		p := filepath.Join(dir, name)
		cmd := exec.Command(ffmpegBin(), "-y",
			"-f", "lavfi", "-i", "testsrc=size=320x240:rate=24:duration=2",
			"-f", "lavfi", "-i", audioSrc,
			"-shortest", "-pix_fmt", "yuv420p", p)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("造 %s 失败：%v\n%s", name, err, out)
		}
		return p
	}
	// 660Hz 频段能量：母片里没有 660Hz，所以这个数只能来自 BGM
	band := func(p string) float64 {
		c := exec.Command(ffmpegBin(), "-hide_banner", "-i", p,
			"-af", "bandpass=f=660:width_type=h:w=30,volumedetect", "-f", "null", "-")
		o, _ := c.CombinedOutput()
		for _, ln := range strings.Split(string(o), "\n") {
			if strings.Contains(ln, "mean_volume:") {
				f := strings.Fields(ln)
				for i, tok := range f {
					if tok == "mean_volume:" && i+1 < len(f) {
						if v, perr := strconv.ParseFloat(f[i+1], 64); perr == nil {
							return v
						}
					}
				}
			}
		}
		return -100
	}

	bgm := filepath.Join(dir, "bgm.wav")
	if out, err := exec.Command(ffmpegBin(), "-y", "-f", "lavfi",
		"-i", "sine=frequency=660:duration=1", bgm).CombinedOutput(); err != nil {
		t.Fatalf("造 BGM 失败：%v\n%s", err, out)
	}
	srt := filepath.Join(dir, "in.srt")
	if err := os.WriteFile(srt, []byte(BuildSRT([]SubtitleLine{{Start: 0.2, End: 1.8, Text: "竖屏字幕"}})), 0o644); err != nil {
		t.Fatal(err)
	}

	// ---- 用例 1：静音原片 + 竖屏(模糊垫底) + BGM + 字幕 ----
	silent := mkAV("silent.mp4", "anullsrc=r=48000:cl=stereo")
	out1 := filepath.Join(dir, "out1.mp4")
	if err := svc.PostProcess(silent, out1, PostSpec{
		Width: 1080, Height: 1920, Fit: "blur",
		BGM: bgm, BGMGainDB: -12, Duck: true,
		SRT: srt, FontSize: 18, MarginV: 24,
	}); err != nil {
		t.Fatalf("后期处理失败：%v", err)
	}

	dims, err := exec.Command(ffprobeBin(), "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "csv=p=0", out1).CombinedOutput()
	if err != nil {
		t.Fatalf("读尺寸失败：%v\n%s", err, dims)
	}
	if got := strings.TrimSpace(string(dims)); got != "1080,1920" {
		t.Fatalf("产物尺寸应为 1080x1920，实际 %s", got)
	}
	info, err := probeMedia(out1)
	if err != nil {
		t.Fatalf("产物读不出来：%v", err)
	}
	if !info.hasAudio {
		t.Fatal("混了 BGM 却没有音轨")
	}
	if info.dur < 1.9 || info.dur > 2.2 {
		t.Fatalf("时长应仍为约 2s（BGM 只有 1s，必须循环铺满），实际 %.2fs", info.dur)
	}
	// 静音原片的 660Hz 能量应在底噪以下；混入 BGM 后必须显著抬起来
	silentBand, out1Band := band(silent), band(out1)
	if out1Band < -50 {
		t.Fatalf("BGM 没真的混进去：产物 660Hz 只有 %.1fdB（静音母片 %.1fdB）", out1Band, silentBand)
	}

	// ---- 用例 2：A/B —— 同样的片子，只切换 duck ----
	voice := mkAV("voice.mp4", "sine=frequency=440:duration=2")
	outNoDuck := filepath.Join(dir, "noduck.mp4")
	outDuck := filepath.Join(dir, "duck.mp4")
	base := PostSpec{Width: 1080, Height: 1920, BGM: bgm, BGMGainDB: -12}
	if err := svc.PostProcess(voice, outNoDuck, base); err != nil {
		t.Fatalf("无侧链渲染失败：%v", err)
	}
	base.Duck = true
	if err := svc.PostProcess(voice, outDuck, base); err != nil {
		t.Fatalf("侧链渲染失败：%v", err)
	}
	nd, dk := band(outNoDuck), band(outDuck)
	if dk > nd-3 {
		t.Fatalf("侧链没压住 BGM：duck=off %.1fdB，duck=on %.1fdB（应至少低 3dB）", nd, dk)
	}

	// ---- 用例 3：只混 BGM、不改构图 ----
	// 这条曾经直接失败：没有视频滤镜时 vLabel 是原始流 "0:v"，旧代码仍给它套方括号
	// 变成 -map "[0:v]"，ffmpeg 报 Invalid file index（EINVAL）。
	outBGMOnly := filepath.Join(dir, "bgmonly.mp4")
	if err := svc.PostProcess(voice, outBGMOnly, PostSpec{BGM: bgm, BGMGainDB: -12}); err != nil {
		t.Fatalf("只混 BGM（不改构图）失败：%v", err)
	}
	if info, err := probeMedia(outBGMOnly); err != nil || !info.hasAudio {
		t.Fatalf("只混 BGM 的产物异常：%v %+v", err, info)
	}
	t.Logf("竖屏 1080x1920 + BGM + 字幕一次通行；静音母片 660Hz %.1fdB → 产物 %.1fdB；"+
		"侧链 A/B：off %.1fdB / on %.1fdB", silentBand, out1Band, nd, dk)
}
