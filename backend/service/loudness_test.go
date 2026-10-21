package service

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const sampleLoudnorm = `[Parsed_loudnorm_0 @ 000001] 
{
	"input_i" : "-27.61",
	"input_tp" : "-18.06",
	"input_lra" : "3.10",
	"input_thresh" : "-37.83",
	"output_i" : "-24.05",
	"output_tp" : "-14.50",
	"output_lra" : "3.10",
	"output_thresh" : "-34.26",
	"normalization_type" : "dynamic",
	"target_offset" : "0.05"
}`

func TestParseLoudnormJSONFromNoisyStderr(t *testing.T) {
	st, ok := parseLoudnormJSON(sampleLoudnorm)
	if !ok {
		t.Fatal("应从带日志的 stderr 里解析出 JSON")
	}
	if math.Abs(st.I+27.61) > 0.01 || math.Abs(st.TP+18.06) > 0.01 {
		t.Fatalf("解析值不对：%+v", st)
	}
	if math.Abs(st.LRA-3.10) > 0.01 || math.Abs(st.Threshold+37.83) > 0.01 {
		t.Fatalf("LRA/阈值解析不对：%+v", st)
	}
}

func TestParseLoudnormJSONRejectsGarbage(t *testing.T) {
	if _, ok := parseLoudnormJSON("no json here"); ok {
		t.Fatal("没有 input_i 时不该报成功——否则会拿 0 当实测值去归一")
	}
}

func TestLoudnormFilterUsesMeasuredValues(t *testing.T) {
	st := LoudnessStats{I: -27.6, TP: -18.1, LRA: 3.1, Threshold: -37.8, Valid: true}
	f := loudnormFilter(st, -14, -1.5)
	for _, want := range []string{
		"I=-14.0", "TP=-1.5", "LRA=11",
		"measured_I=-27.60", "measured_TP=-18.10", "measured_LRA=3.10", "measured_thresh=-37.80",
		"linear=true", // 两遍法的关键：恒定增益，而不是动态压缩
		"alimiter",
	} {
		if !strings.Contains(f, want) {
			t.Fatalf("滤镜缺 %q：%s", want, f)
		}
	}
}

func TestLoudnormFilterFallsBackWhenUnmeasured(t *testing.T) {
	f := loudnormFilter(LoudnessStats{}, -16, -2)
	if strings.Contains(f, "measured_I") {
		t.Fatalf("没测到值就不该写 measured_*（喂 0 会让归一算歪）：%s", f)
	}
	if !strings.Contains(f, "alimiter") {
		t.Fatal("即使没有实测值也要有真峰限幅")
	}
}

// 真机：把一条偏响的片子归一到 -14 LUFS，再用 ebur128 复测。
// 手动跑：FLOWEDGE_REAL_LOUD=1 go test ./service/ -run TestRealLoudnessNormalize -v
func TestRealLoudnessNormalize(t *testing.T) {
	if os.Getenv("FLOWEDGE_REAL_LOUD") != "1" {
		t.Skip("需要真实 ffmpeg，设 FLOWEDGE_REAL_LOUD=1 才跑")
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "loud.mp4")
	mk := exec.Command(ffmpegBin(), "-y",
		"-f", "lavfi", "-i", "color=c=black:s=160x120:d=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-af", "volume=0dB", "-shortest", "-pix_fmt", "yuv420p", in)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("造素材失败：%v\n%s", err, out)
	}

	out := filepath.Join(dir, "norm.mp4")
	svc := NewVideoService()
	if err := svc.PostProcess(in, out, PostSpec{LoudnessTarget: -14, TruePeak: -1.5}); err != nil {
		t.Fatalf("响度归一失败：%v", err)
	}

	measure := func(p string) float64 {
		c := exec.Command(ffmpegBin(), "-hide_banner", "-nostats", "-i", p,
			"-af", "ebur128", "-f", "null", "-")
		raw, _ := c.CombinedOutput()
		var last float64
		for _, ln := range strings.Split(string(raw), "\n") {
			if strings.Contains(ln, "I:") && strings.Contains(ln, "LUFS") {
				f := strings.Fields(ln)
				for i, tok := range f {
					if tok == "I:" && i+1 < len(f) {
						if v, err := strconv.ParseFloat(f[i+1], 64); err == nil {
							last = v
						}
					}
				}
			}
		}
		return last
	}
	before, after := measure(in), measure(out)
	if math.Abs(after-(-14)) > 1.5 {
		t.Fatalf("归一后应在 -14±1.5 LUFS，实际 %.1f（原片 %.1f）", after, before)
	}
	t.Logf("响度归一：原片 %.1f LUFS → 产物 %.1f LUFS（目标 -14）", before, after)
}
