package service

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"flowedge/backend/model"
)

// 质检门拦截：require_audio=true 而产物无音轨 → 必须 FAIL（阻断）
func TestQualityGate_RequireAudioBlocksSilentVideo(t *testing.T) {
	e := &EngineV2{}
	node := &model.NodeInstance{
		ID: "g", Type: "quality_gate",
		Data: map[string]interface{}{"require_audio": true},
	}
	// 造一个只有视频流的 mp4（testsrc + -an）
	path := makeSilentVideo(t)
	upstream := map[string]interface{}{
		"v1": map[string]interface{}{"video": path},
	}
	_, err := e.execQualityGateNode(context.Background(), node, upstream)
	if err == nil {
		t.Fatal("无音轨视频在 require_audio 门下应 FAIL")
	}
	if !containsFold(err.Error(), "质检未通过") {
		t.Fatalf("错误信息应说明质检未通过，got %v", err)
	}
}

// 同一条产物在无 require_audio 规则时应放行
func TestQualityGate_PassesWithoutAudioRule(t *testing.T) {
	e := &EngineV2{}
	node := &model.NodeInstance{ID: "g", Type: "quality_gate", Data: map[string]interface{}{}}
	path := makeSilentVideo(t)
	upstream := map[string]interface{}{
		"v1": map[string]interface{}{"video": path},
	}
	out, err := e.execQualityGateNode(context.Background(), node, upstream)
	if err != nil {
		t.Fatalf("无额外规则应放行，got %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok || m["gate"] != "pass" {
		t.Fatalf("输出应带 gate=pass，got %+v", out)
	}
}

// 缺输入产物必须显式失败（不能伪造通过）
func TestQualityGate_NoArtifactFails(t *testing.T) {
	e := &EngineV2{}
	node := &model.NodeInstance{ID: "g", Type: "quality_gate", Data: map[string]interface{}{}}
	_, err := e.execQualityGateNode(context.Background(), node, map[string]interface{}{})
	if err == nil || !containsFold(err.Error(), "缺少输入产物") {
		t.Fatalf("缺产物应显式失败，got %v", err)
	}
}

// makeSilentVideo 造一条 2 秒无音轨测试视频
func makeSilentVideo(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg 不可用，跳过")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "silent.mp4")
	cmd := exec.Command(bin, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=16",
		"-an", "-pix_fmt", "yuv420p", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("造测试视频失败：%v %s", err, out)
	}
	return path
}

// 音画同步检查：0.1s drift 应在 0.15s 容差内（fail 级）
func TestGateCheck_AVDriftWithinTolerance(t *testing.T) {
	info := &gateMediaInfo{hasVideo: true, hasAudio: true, kind: "video", videoSecond: 5.0, audioSecond: 5.1}
	r := gateRules{durationTol: 0.15}
	checks := runGateChecks(context.Background(), "", info, r)
	found := false
	for _, c := range checks {
		if c.Name == "av_drift" {
			found = true
			if !c.OK {
				t.Fatalf("0.1s drift 应在 0.15s 容差内，got %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("应产出 av_drift 检查项")
	}
}

// 超差必须 FAIL 级阻断
func TestGateCheck_AVDriftExceedsTolerance(t *testing.T) {
	info := &gateMediaInfo{hasVideo: true, hasAudio: true, kind: "video", videoSecond: 5.0, audioSecond: 5.5}
	r := gateRules{durationTol: 0.15}
	checks := runGateChecks(context.Background(), "", info, r)
	for _, c := range checks {
		if c.Name == "av_drift" && (c.OK || c.Level != "fail") {
			t.Fatalf("0.5s drift 应 fail 级不过，got %+v", c)
		}
	}
}
