package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"flowedge/backend/model"
)

// ===== P2 壁垒：场景锁 =====

// TestSceneLockAppliedOnce 锁只能注入一次。
// 门禁按字节全等校验锁句，重复注入会让锁句叠两次——保护措施反成故障源。
func TestSceneLockAppliedOnce(t *testing.T) {
	const lock = "SCENE LOCK: a woman in a red coat, wooden pier, overcast sky."
	p := "cinematic wide shot, shallow depth"
	got, injected := applySceneLock(p, lock)
	if !injected {
		t.Fatalf("首次注入应生效，实际未注入：%s", got)
	}
	if !strings.Contains(got, "SCENE LOCK:") {
		t.Fatalf("注入结果里没有锁句：%s", got)
	}
	again, injected2 := applySceneLock(got, lock)
	if injected2 {
		t.Fatalf("已有锁句时不得重复注入（会让锁不再字节全等）：%s", again)
	}
	if again != got {
		t.Fatalf("直通语义破坏：\n一次=%q\n两次=%q", got, again)
	}
}

// TestSceneLockEmptyIsNoop 没有锁时不许改动提示词（避免给每个节点硬塞空串）
func TestSceneLockEmptyIsNoop(t *testing.T) {
	const p = "a cat on a roof"
	got, injected := applySceneLock(p, "   ")
	if injected || got != p {
		t.Fatalf("空锁应原样返回，实际 got=%q injected=%v", got, injected)
	}
}

// TestSceneLockResolvesFromUpstream 上游 scene_grid 节点的锁能被图像节点取到。
// 这条守的是 P2 的接线：此前只有文生视频取锁，图像节点取不到 = 一致性只做了一半。
func TestSceneLockResolvesFromUpstream(t *testing.T) {
	upstream := map[string]interface{}{
		"grid1": map[string]interface{}{"scene_lock": "SCENE LOCK: same actor, same coat."},
	}
	lock, err := resolveSceneLock(&model.NodeInstance{Data: map[string]interface{}{}}, upstream)
	if err != nil {
		t.Fatalf("取锁失败：%v", err)
	}
	if !strings.Contains(lock, "SCENE LOCK:") {
		t.Fatalf("上游锁没取到，得到 %q", lock)
	}
	// 没有锁的上游必须返回空而不是报错（不许把「没锁」变成运行失败）
	empty, err := resolveSceneLock(&model.NodeInstance{Data: map[string]interface{}{}}, map[string]interface{}{})
	if err != nil || empty != "" {
		t.Fatalf("无锁时应返回空串无错误，实际 lock=%q err=%v", empty, err)
	}
}

// ===== P3 稳定性：显存降级 =====

func TestIsOOMErrorRecognizesVRAMFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"torch OOM", errors.New("torch.cuda.OutOfMemoryError: CUDA out of memory"), true},
		{"中文显存不足", errors.New("生成失败：显存不足"), true},
		{"ComfyUI allocation", errors.New("Allocation on device 0 would exceed total GPU memory"), false}, // 不含既定特征词，不应误判
		{"普通网络错误", errors.New("dial tcp 127.0.0.1:8188: connect: connection refused"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := IsOOMError(c.err); got != c.want {
			t.Fatalf("%s：IsOOMError = %v，应为 %v", c.name, got, c.want)
		}
	}
}

// TestDegradeMediaRequestPrefersResolution 降级顺序必须是「分辨率 → 帧数 → 步数」：
// 分辨率对观感影响最小、对显存最有效，步数代价最大留最后。
func TestDegradeMediaRequestPrefersResolution(t *testing.T) {
	req := MediaRequest{Kind: "t2v", Width: 608, Height: 352, Frames: 124, Steps: 20}
	d1, ok := degradeMediaRequest(req)
	if !ok {
		t.Fatal("第一档应能降")
	}
	if d1.Width >= req.Width || d1.Height >= req.Height {
		t.Fatalf("第一档没降分辨率：%d×%d", d1.Width, d1.Height)
	}
	if d1.Width%8 != 0 || d1.Height%8 != 0 {
		t.Fatalf("分辨率必须对齐 8（VAE 要求）：%d×%d", d1.Width, d1.Height)
	}
	if d1.Frames != req.Frames || d1.Steps != req.Steps {
		t.Fatalf("第一档不该动帧数/步数（一次只降一个变量）：frames=%d steps=%d", d1.Frames, d1.Steps)
	}

	// 分辨率降到底之后才动帧数
	small := MediaRequest{Kind: "t2v", Width: 16, Height: 16, Frames: 124, Steps: 20}
	d2, ok2 := degradeMediaRequest(small)
	if !ok2 || d2.Frames >= small.Frames {
		t.Fatalf("分辨率不可降时应降帧数，实际 frames=%d ok=%v", d2.Frames, ok2)
	}

	// 全都降到底：必须返回 false，让调用方如实报错而不是空转重试
	bottom := MediaRequest{Kind: "t2v", Width: 16, Height: 16, Frames: 17, Steps: 6}
	if _, ok3 := degradeMediaRequest(bottom); ok3 {
		t.Fatal("降到底后必须返回 false（否则会无限重试）")
	}
}

func TestAlign8(t *testing.T) {
	if got := align8(608 * 3 / 4); got%8 != 0 || got <= 0 {
		t.Fatalf("align8 结果不合法：%d", got)
	}
	if got := align8(3); got != 8 {
		t.Fatalf("align8(3) 应兜底到 8，实际 %d", got)
	}
}

// ===== P2 质检门：修复建议 =====

// TestGateFixesAreActionable 每项 FAIL 都必须给出能照做的修复动作。
// 门只报码等于把排查成本原样退给用户——有建议和没建议是两回事。
func TestGateFixesAreActionable(t *testing.T) {
	info := &gateMediaInfo{
		kind: "video", width: 640, height: 360, frames: 100,
		videoSecond: 4.0, audioSecond: 5.0, hasVideo: true, hasAudio: true,
	}
	r := gateRules{
		minWidth:    1280,
		minFrames:   124,
		durationMax: 3.0,
		durationTol: 0.15,
	}
	checks := runGateChecks(context.Background(), "in.mp4", info, r)
	annotateFixes(checks, info, r)

	failed := 0
	for _, c := range checks {
		if c.OK {
			if c.Fix != "" {
				t.Fatalf("通过的项不该带修复建议（噪音）：%s", c.Name)
			}
			continue
		}
		failed++
		if strings.TrimSpace(c.Fix) == "" {
			t.Fatalf("FAIL 项 %s 没有修复建议", c.Name)
		}
	}
	if failed == 0 {
		t.Fatal("这组规则本应产生失败项，实际全通过——测试本身失效了")
	}
}
