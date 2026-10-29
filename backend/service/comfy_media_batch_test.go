package service

import (
	"strconv"
	"testing"
)

func batchReq(kind string, frames int) MediaRequest {
	return MediaRequest{Kind: kind, Prompt: "p", Width: 608, Height: 352, Frames: frames, Steps: 12}
}

// 屏障两阶段是批量构图的存在理由：guider 必须全部接在 cond 屏障上、
// 解码必须全部接在 latent 屏障上，否则 ComfyUI 还是逐镜换权重。
func TestBuildH3BatchGraph_Barriers(t *testing.T) {
	s := &Text2ImageService{}
	reqs := []MediaRequest{batchReq("h3", 124), batchReq("h3", 124), batchReq("h3", 141)}
	graph, saveIDs, err := s.BuildH3BatchGraph(reqs)
	if err != nil {
		t.Fatalf("构图失败：%v", err)
	}
	if len(saveIDs) != 3 || saveIDs[0] != "140" || saveIDs[2] != "142" {
		t.Fatalf("SaveVideo 节点 id 应为 140/141/142，实际 %v", saveIDs)
	}
	for _, id := range []string{"90", "91"} {
		if _, ok := graph[id]; !ok {
			t.Fatalf("缺少屏障节点 %s", id)
		}
	}
	if got := graph["90"].(map[string]interface{})["class_type"]; got != "FlowEdgeCondBarrier" {
		t.Fatalf("节点 90 应为条件屏障，实际 %v", got)
	}
	if got := graph["91"].(map[string]interface{})["class_type"]; got != "FlowEdgeLatentBarrier" {
		t.Fatalf("节点 91 应为 latent 屏障，实际 %v", got)
	}
	// 每个采样器的 latent 都要来自自己的编码节点（50+i），conditioning 来自屏障
	for i := 0; i < 3; i++ {
		samp, ok := graph[strconv.Itoa(100+i)].(map[string]interface{})
		if !ok {
			t.Fatalf("缺少采样器 %d", 100+i)
		}
		inputs := samp["inputs"].(map[string]interface{})
		if got := inputs["latent_image"].([]interface{})[0]; got != strconv.Itoa(50+i) {
			t.Fatalf("第 %d 镜 latent 应来自编码节点 %d，实际 %v", i+1, 50+i, got)
		}
		guider, ok := graph[strconv.Itoa(70+i)].(map[string]interface{})
		if !ok {
			t.Fatalf("缺少 guider %d", 70+i)
		}
		if got := guider["inputs"].(map[string]interface{})["conditioning"].([]interface{})[0]; got != "90" {
			t.Fatalf("第 %d 镜 guider 必须接屏障 90，实际 %v", i+1, got)
		}
	}
}

func TestBuildH3BatchGraph_Rejects(t *testing.T) {
	s := &Text2ImageService{}
	cases := []struct {
		name string
		reqs []MediaRequest
	}{
		{"单镜不成批", []MediaRequest{batchReq("h3", 124)}},
		{"kind 混批", []MediaRequest{batchReq("h3", 124), batchReq("h3_r2v", 124)}},
		{"帧数非法", []MediaRequest{batchReq("h3", 124), batchReq("h3", 100)}},
		{"续接镜", []MediaRequest{batchReq("h3", 124), {Kind: "h3", Prompt: "p", Width: 608, Height: 352, Frames: 124, Steps: 12, Image: "a.png"}}},
		{"台词镜", []MediaRequest{batchReq("h3", 124), {Kind: "h3_r2v", Prompt: "p", Width: 608, Height: 352, Frames: 124, Steps: 12, RefAudios: []string{"a.wav"}}}},
		{"参考图不一致", []MediaRequest{
			{Kind: "h3_r2v", Prompt: "p", Width: 608, Height: 352, Frames: 124, Steps: 12, RefImages: []string{"a.png"}},
			{Kind: "h3_r2v", Prompt: "p", Width: 608, Height: 352, Frames: 124, Steps: 12, RefImages: []string{"b.png"}}}},
		{"步数不一致", []MediaRequest{batchReq("h3", 124), {Kind: "h3", Prompt: "p", Width: 608, Height: 352, Frames: 124, Steps: 8}}},
	}
	for _, c := range cases {
		if _, _, err := s.BuildH3BatchGraph(c.reqs); err == nil {
			t.Errorf("%s：应当拒绝，却构图成功", c.name)
		}
	}
}

func TestBatchRunLen(t *testing.T) {
	mk := func(dialogue bool, transition string) Shot {
		sh := Shot{Transition: transition, Frames: 124}
		if dialogue {
			sh.Lines = []PlanLine{{Speaker: "S1", Text: "你好"}}
		}
		return sh
	}
	allLow := func(int) bool { return false }
	allMiss := func(int) bool { return true }

	shots := []Shot{mk(false, "cut"), mk(false, "cut"), mk(false, "dissolve")}
	if got := batchRunLen(shots, 0, 8, false, allLow, allMiss); got != 3 {
		t.Fatalf("三镜全可批，应得 3，实际 %d", got)
	}
	if got := batchRunLen(shots, 0, 2, false, allLow, allMiss); got != 2 {
		t.Fatalf("上限 2 应得 2，实际 %d", got)
	}
	// 台词镜打断
	withDub := []Shot{mk(false, "cut"), mk(true, "cut"), mk(false, "cut")}
	if got := batchRunLen(withDub, 0, 8, false, allLow, allMiss); got != 1 {
		t.Fatalf("台词镜应打断分组，实际 %d", got)
	}
	// continue 依赖：下一镜要等上一镜末帧
	cont := []Shot{mk(false, "cut"), mk(false, "continue"), mk(false, "cut")}
	if got := batchRunLen(cont, 0, 8, false, allLow, allMiss); got != 2 {
		t.Fatalf("continue 之后必须断组，实际 %d", got)
	}
	// start 自己是续接镜
	if got := batchRunLen(cont, 1, 8, true, allLow, allMiss); got != 0 {
		t.Fatalf("续接镜不能当组头，实际 %d", got)
	}
	// 缓存命中直接剔除
	if got := batchRunLen(shots, 0, 8, false, allLow, func(j int) bool { return j != 1 }); got != 1 {
		t.Fatalf("第 2 镜命中缓存应断组，实际 %d", got)
	}
	// lowVRAM 判档不一致
	if got := batchRunLen(shots, 0, 8, false, func(j int) bool { return j == 2 }, allMiss); got != 2 {
		t.Fatalf("判档不一致应断组，实际 %d", got)
	}
}
