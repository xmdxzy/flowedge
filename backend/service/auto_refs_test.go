package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseAutoRefConfig(t *testing.T) {
	// 默认关（开启会多一次抽帧 + 改变参考图，不能悄悄打开）
	if cfg := ParseAutoRefConfig(nil); cfg.Enabled || cfg.Max != 3 {
		t.Errorf("默认应关闭且 Max=3，实际 %+v", cfg)
	}
	cfg := ParseAutoRefConfig(map[string]interface{}{"auto_ref_frames": true, "auto_ref_max": float64(2)})
	if !cfg.Enabled || cfg.Max != 2 {
		t.Errorf("解析错误：%+v", cfg)
	}
	// 上限 6：再多对 Ref2VA 没收益，只拖慢上传
	if cfg := ParseAutoRefConfig(map[string]interface{}{"auto_ref_frames": true, "auto_ref_max": float64(99)}); cfg.Max != 6 {
		t.Errorf("Max 应封顶 6，实际 %d", cfg.Max)
	}
}

// 排序决定"从哪一镜抽帧"，是这套机制的核心判断，必须钉住。
func TestRankAutoRefShots(t *testing.T) {
	shots := []AutoRefShot{
		{Path: "d.mp4", HasDialogue: true, Transition: "cut", Seconds: 8},       // 台词镜：最后
		{Path: "a.mp4", HasDialogue: false, Transition: "cut", Seconds: 3},      // 无台词短
		{Path: "b.mp4", HasDialogue: false, Transition: "cut", Seconds: 6},      // 无台词长 ← 首选
		{Path: "c.mp4", HasDialogue: false, Transition: "continue", Seconds: 9}, // continue：参考价值低
	}
	got := rankAutoRefShots(shots)
	if got[0].Path != "b.mp4" {
		t.Errorf("首选应为无台词+非 continue+最长（b.mp4），实际 %s", got[0].Path)
	}
	if got[len(got)-1].Path != "d.mp4" {
		t.Errorf("台词镜应排最后，实际 %s", got[len(got)-1].Path)
	}
	// continue 镜即使更长也应排在同条件非 continue 之后
	var bIdx, cIdx int
	for i, s := range got {
		switch s.Path {
		case "b.mp4":
			bIdx = i
		case "c.mp4":
			cIdx = i
		}
	}
	if cIdx < bIdx {
		t.Errorf("continue 镜不应排在同条件非 continue 之前：b=%d c=%d", bIdx, cIdx)
	}
}

func TestCollectAutoRefs_Disabled(t *testing.T) {
	refs, err := CollectAutoRefs(context.Background(), []AutoRefShot{{Path: "x.mp4"}}, t.TempDir(), AutoRefConfig{})
	if err != nil || refs != nil {
		t.Errorf("未启用时应返回空且无错误，实际 refs=%v err=%v", refs, err)
	}
}

// 一个可用候选都没有时必须报错，不能返回空列表冒充「有参考」。
func TestCollectAutoRefs_NoUsableShot(t *testing.T) {
	dir := t.TempDir()
	_, err := CollectAutoRefs(context.Background(),
		[]AutoRefShot{{Path: filepath.Join(dir, "missing.mp4")}},
		dir, AutoRefConfig{Enabled: true, Max: 3, FramesPerShot: 1})
	if err == nil {
		t.Fatal("没有任何可用候选时应报错")
	}
}

// 真抽帧：用 ffmpeg 造一段 1 秒彩条视频，验证「能抽出真 PNG 且数量受 Max 约束」。
// 依赖本机 ffmpeg（项目本来就强依赖它），跳过而不是假通过。
func TestCollectAutoRefs_RealFrames(t *testing.T) {
	dir := t.TempDir()
	src := makeTestClip(t, dir, "clip.mp4", 2.0, 2.0)
	if src == "" {
		t.Skip("本机 ffmpeg 不可用，跳过")
	}
	refs, err := CollectAutoRefs(context.Background(),
		[]AutoRefShot{{Path: src, HasDialogue: false, Seconds: 2.0}},
		filepath.Join(dir, "refs"), AutoRefConfig{Enabled: true, Max: 2, FramesPerShot: 2})
	if err != nil {
		t.Fatalf("抽帧失败：%v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("应抽到 2 张（受 Max 约束），实际 %d", len(refs))
	}
	for _, p := range refs {
		st, err := os.Stat(p)
		if err != nil || st.Size() == 0 {
			t.Errorf("产物不存在或为空：%s", p)
		}
	}
}
