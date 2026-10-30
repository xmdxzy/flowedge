package service

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// 声床（room tone）测试的三层判据：
//  1. 电平校准：静音输入 + 声床 ≈ floorDB（换算常数漂了这里先红）
//  2. 端到端：纯静音双镜装配 → 有中段静音；铺床后 → verify-media 同判据下归零
//  3. 滥用防护：正数/0 电平必须报错，不许静默当「关」

func makeSilentWav(t *testing.T, dir, name string, secs float64) string {
	t.Helper()
	p := filepath.Join(dir, name)
	args := []string{"-y", "-v", "error", "-f", "lavfi", "-i", "anullsrc=r=32000:cl=stereo",
		"-t", fmt.Sprintf("%.2f", secs), "-c:a", "pcm_s16le", p}
	if out, err := exec.Command(ffmpegBin(), args...).CombinedOutput(); err != nil {
		t.Fatalf("造静音样本失败：%v %s", err, out)
	}
	return p
}

// meanVolume 跑 volumedetect 拿 mean/max（与 verify-media 同一读法）
func meanVolume(t *testing.T, path string) (mean, max float64) {
	t.Helper()
	out, err := exec.Command(ffmpegBin(), "-hide_banner", "-nostats", "-i", path,
		"-af", "volumedetect", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("volumedetect 失败：%v %s", err, out)
	}
	re := regexp.MustCompile(`mean_volume:\s*(-?[\d.]+)`)
	if m := re.FindSubmatch(out); m != nil {
		mean, _ = strconv.ParseFloat(string(m[1]), 64)
	}
	reMax := regexp.MustCompile(`max_volume:\s*(-?[\d.]+)`)
	if m := reMax.FindSubmatch(out); m != nil {
		max, _ = strconv.ParseFloat(string(m[1]), 64)
	}
	return
}

func TestMixRoomTone_Level(t *testing.T) {
	dir := t.TempDir()
	sil := makeSilentWav(t, dir, "sil.wav", 3)
	out := filepath.Join(dir, "bed.m4a")
	if err := mixRoomTone(sil, out, -36); err != nil {
		t.Fatalf("铺声床失败：%v", err)
	}
	mean, max := meanVolume(t, out)
	if mean < -40 || mean > -32 {
		t.Errorf("声床电平 mean=%.1f dB，期望 ≈-36±4：换算常数 roomToneGeneratorMeanDB 漂了"+
			"（太高会漏掉静音缺口，太低会变成噪音）", mean)
	}
	if max > -18 {
		t.Errorf("声床峰值 %.1f dB 过高：底噪不该有可闻峰", max)
	}
}

func TestMixRoomTone_RejectsNonNegativeFloor(t *testing.T) {
	if err := mixRoomTone("in.mp4", "out.mp4", 0); err == nil {
		t.Error("floorDB=0 应报错（0 是「关闭」语义，不该走到铺底层）")
	}
	if err := mixRoomTone("in.mp4", "out.mp4", 12); err == nil {
		t.Error("正数 floorDB 应报错：底噪比节目声还响是配置事故，必须显式炸")
	}
}

// countMidSilence 用 verify-media 的同一判据数中段静音（-38dB / ≥minSec，
// 片头片尾各留 edgeSec 不算）。
func countMidSilence(t *testing.T, path string, minSec, edgeSec float64, total float64) int {
	t.Helper()
	out, err := exec.Command(ffmpegBin(), "-hide_banner", "-nostats", "-i", path,
		"-af", "silencedetect=noise=-38dB:d=0.1", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("silencedetect 失败：%v %s", err, out)
	}
	reS := regexp.MustCompile(`silence_start:\s*(-?[\d.]+)`)
	reE := regexp.MustCompile(`silence_end:\s*(-?[\d.]+)`)
	var starts, ends []float64
	for _, m := range reS.FindAllSubmatch(out, -1) {
		v, _ := strconv.ParseFloat(string(m[1]), 64)
		starts = append(starts, v)
	}
	for _, m := range reE.FindAllSubmatch(out, -1) {
		v, _ := strconv.ParseFloat(string(m[1]), 64)
		ends = append(ends, v)
	}
	n := 0
	for i, s := range starts {
		e := total
		if i < len(ends) {
			e = ends[i]
		}
		if s > edgeSec && e < total-edgeSec && e-s >= minSec {
			n++
		}
	}
	return n
}

// TestAssembleThenRoomTone_FillsSilenceGaps 端到端：装配一条「有声-静音-有声-静音-有声」
// 结构的片子（静音区间孤立在中段，verify-media 会报 silence_gap 的那种），铺床后按
// 同一判据归零。这就是「声床填住接缝断音」的产物级证明，不依赖真 GPU 出片。
//
// 注意对照设计：不能两段全静音——整片一个静音区间会被「片头片尾各留 0.6s 不算」
// 的中段过滤排除（verify-media 同款规则），对照就不成立了。
func TestAssembleThenRoomTone_FillsSilenceGaps(t *testing.T) {
	dir := t.TempDir()
	// aevalsrc 按时间表达式造「有声/静音相间」的立体声，电平 0.6（≈-4.5dB，远高于 -38dB 判据）
	mkClip := func(name, expr string) string {
		p := filepath.Join(dir, name)
		args := []string{"-y", "-v", "error",
			"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24:duration=2",
			"-f", "lavfi", "-i", fmt.Sprintf("aevalsrc='%s':s=32000:d=2:c=stereo", expr),
			"-t", "2", "-c:v", "libx264", "-preset", "ultrafast",
			"-pix_fmt", "yuv420p", "-c:a", "aac", "-ar", "32000", p}
		if out, err := exec.Command(ffmpegBin(), args...).CombinedOutput(); err != nil {
			t.Fatalf("造测试镜头失败：%v %s", err, out)
		}
		return p
	}
	// a：0–0.4s 有声，0.4–1.6 静音，1.6–2 有声（把一个静音区间顶到头，它会被
	// 片头边缘规则排除；真正要抓的是接缝后的那一段）
	// b：0–0.9 静音，0.9–1.1 有声（把第二段静音截在片尾边缘之前）
	a := mkClip("a.mp4", `0.6*sin(2*PI*440*t)*(lt(t,0.4)+gte(t,1.6))`)
	b := mkClip("b.mp4", `0.6*sin(2*PI*520*t)*between(t,0.9,1.1)`)
	raw := filepath.Join(dir, "raw.mp4")
	if err := assembleShots([]string{a, b}, []string{"cut"}, raw); err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	total := 2.0 + 2.0 - 0.08 // 两镜时长减 cut 融合窗口
	if gaps := countMidSilence(t, raw, 0.45, 0.6, total); gaps == 0 {
		t.Fatal("「有声-静音相间」装配居然没有中段静音：对照不成立，测试判据失效")
	}
	bedded := filepath.Join(dir, "bed.mp4")
	if err := mixRoomTone(raw, bedded, -36); err != nil {
		t.Fatalf("铺声床失败：%v", err)
	}
	if gaps := countMidSilence(t, bedded, 0.45, 0.6, total); gaps != 0 {
		t.Errorf("铺床后仍有 %d 处中段静音：声床电平不够或判据没对齐", gaps)
	}
}
