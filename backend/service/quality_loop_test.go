package service

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"flowedge/backend/model"
)

// 质量循环的闭环验证：第一轮不达标 → 诊断 → 按修复建议改参数 → 第二轮达标 → 取最好一轮。
//
// 这里用 ffmpeg 合成测试视频，所以不依赖 GPU / ComfyUI：
//   - 第 1 轮：内层工作流只给 320x240 / 16 帧 → min_width、min_frames 双双 FAIL
//   - 诊断：把 width 提到 ≥720、frames 提到 ≥48（derivePatches）
//   - 第 2 轮：内层工作流读到补丁后的参数，产出 720x240 / 48 帧 → 全绿
//   - 结论：取第 2 轮，分数 100，报告里两轮都在
func TestQualityLoop_RepairsAndConverges(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 不可用，跳过")
	}

	e := &EngineV2{}
	e.registerBuiltinNodes()

	// 内层工作流的执行体：按节点 data 里的宽高/帧数造一段视频。
	// 它读的是**被补丁改过之后**的 data，这正是「修复是否真的生效」的判据。
	var renders []string
	e.RegisterNodeType("test_render", func(_ context.Context, n *model.NodeInstance, _ map[string]interface{}) (interface{}, error) {
		width := dataInt(n.Data, "width", 320)
		height := dataInt(n.Data, "height", 240)
		frames := dataInt(n.Data, "frames", 16)
		path := filepath.Join(t.TempDir(), fmt.Sprintf("out_%d_%d_%d.mp4", width, height, frames))
		seconds := float64(frames) / 16.0
		cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i",
			fmt.Sprintf("testsrc=duration=%.3f:size=%dx%d:rate=16", seconds, width, height),
			"-an", "-pix_fmt", "yuv420p", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("测试视频合成失败：%v %s", err, out)
		}
		renders = append(renders, fmt.Sprintf("%dx%d/%d帧", width, height, frames))
		return map[string]interface{}{"video": path, "text": "rendered"}, nil
	})

	// 内层工作流：start → test_render → end
	inner := `[
	  {"id":"s1","type":"start","data":{"prompt":"a test clip"}},
	  {"id":"r1","type":"test_render","data":{"width":320,"height":240,"frames":16}},
	  {"id":"e1","type":"end","data":{}}
	]`
	innerEdges := `[{"id":"a","source":"s1","target":"r1"},{"id":"b","source":"r1","target":"e1"}]`
	e.SetResolver(func(id uint) ([]byte, []byte, error) {
		if id != 1 {
			return nil, nil, fmt.Errorf("unknown workflow %d", id)
		}
		return []byte(inner), []byte(innerEdges), nil
	})

	node := &model.NodeInstance{
		ID: "q1", Type: "quality_loop",
		Data: map[string]interface{}{
			"workflow_id": float64(1),
			"max_rounds":  float64(2),
			"min_width":   float64(720),
			"min_frames":  float64(48),
			"repair":      "auto",
			"target_node": "r1",
		},
	}

	out, err := e.execQualityLoopNode(context.Background(), node, map[string]interface{}{})
	if err != nil {
		t.Fatalf("第二轮应达标，got error: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("输出应为 map，got %T", out)
	}
	if m["gate"] != "pass" || m["score"] != 100 {
		t.Fatalf("应取到达标的一轮（gate=pass/score=100），got gate=%v score=%v", m["gate"], m["score"])
	}
	if m["best_round"] != 2 {
		t.Errorf("best_round = %v, want 2（第一轮不该通过）", m["best_round"])
	}
	if m["rounds"] != 2 {
		t.Errorf("rounds = %v, want 2", m["rounds"])
	}
	if len(renders) != 2 {
		t.Fatalf("内层工作流应执行 2 次，实际 %d 次：%v", len(renders), renders)
	}
	// 第二轮必须真的用了修好的参数，而不是"重跑一次碰巧过了"
	if renders[1] != "720x240/48帧" {
		t.Errorf("第二轮渲染参数 = %s，want 720x240/48帧（修复补丁没生效）", renders[1])
	}
	// 报告必须逐轮留下证据
	report, _ := m["report"].([]qualityRound)
	if len(report) != 2 {
		t.Fatalf("report 应含 2 轮，got %d", len(report))
	}
	if report[0].Score == 100 {
		t.Errorf("第一轮不该是 100 分：%+v", report[0])
	}
	if len(report[1].Patches) == 0 {
		t.Errorf("第二轮应记录上一轮下发的补丁：%+v", report[1])
	}
}

// 未达标时必须显式失败（不能把「跑了但不合格」报成 success）。
func TestQualityLoop_FailsWhenTargetUnreachable(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 不可用，跳过")
	}

	e := &EngineV2{}
	e.registerBuiltinNodes()
	e.RegisterNodeType("test_still", func(_ context.Context, n *model.NodeInstance, _ map[string]interface{}) (interface{}, error) {
		path := filepath.Join(t.TempDir(), "still.mp4")
		cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "testsrc=duration=1:size=160x120:rate=16",
			"-an", "-pix_fmt", "yuv420p", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("造视频失败：%v %s", err, out)
		}
		return map[string]interface{}{"video": path}, nil
	})
	e.SetResolver(func(id uint) ([]byte, []byte, error) {
		return []byte(`[
		  {"id":"s1","type":"start","data":{}},
		  {"id":"r1","type":"test_still","data":{}},
		  {"id":"e1","type":"end","data":{}}
		]`), []byte(`[{"id":"a","source":"s1","target":"r1"},{"id":"b","source":"r1","target":"e1"}]`), nil
	})

	node := &model.NodeInstance{
		ID: "q2", Type: "quality_loop",
		Data: map[string]interface{}{
			"workflow_id": float64(1),
			"max_rounds":  float64(2),
			"min_width":   float64(1920),
			"repair":      "auto",
		},
	}
	out, err := e.execQualityLoopNode(context.Background(), node, map[string]interface{}{})
	if err == nil {
		t.Fatal("修不动时必须显式失败，不能报成功")
	}
	m, ok := out.(map[string]interface{})
	if !ok || m["gate"] != "fail" {
		t.Fatalf("失败也要带报告，got %+v", out)
	}
	// 尺寸补丁会把 width 提到 1920——但内层是固定 160x120 的替身，所以两轮都不会过。
	// 这里断言的是「不合格被如实报出来」，不是「一定修得好」。
}
