package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		// 台词镜不再被拒（2026-10-30：每镜一个 LoadAudio 就够）；但每镜只放 1 段，
		// 且音频只能挂在 ref2va 上——多段/挂错 kind 仍必须拦住。
		{"多段参考音频", []MediaRequest{
			{Kind: "h3_r2v", Prompt: "p", Width: 608, Height: 352, Frames: 124, Steps: 12, RefAudios: []string{"a.wav"}},
			{Kind: "h3_r2v", Prompt: "p", Width: 608, Height: 352, Frames: 124, Steps: 12, RefAudios: []string{"a.wav", "b.wav"}}}},
		{"音频挂错 kind", []MediaRequest{
			{Kind: "h3", Prompt: "p", Width: 608, Height: 352, Frames: 124, Steps: 12, RefAudios: []string{"a.wav"}},
			{Kind: "h3", Prompt: "p", Width: 608, Height: 352, Frames: 124, Steps: 12}}},
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

// 台词镜合批的关键接线：每镜一个 LoadAudio(150+i)，且只接到自己的 ref_audio_1。
// 接错（共用一条音轨 / 踩到 140+i 的产物段）会静默串音——批量片里两镜说同一句话，
// 帧数时长全正常，只有人耳能发现。所以这里钉死 id 与指向。
func TestBuildH3BatchGraph_PerShotAudio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/upload/image" {
			t.Errorf("上传应走 /upload/image，实际 %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"up.wav","subfolder":"","type":"input"}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	mkAudio := func(i int) string {
		p := filepath.Join(dir, fmt.Sprintf("dub-%d.wav", i))
		if err := os.WriteFile(p, []byte("RIFF....WAVE"), 0o644); err != nil {
			t.Fatalf("造音轨失败：%v", err)
		}
		return p
	}
	s := &Text2ImageService{H3BaseURL: srv.URL}
	reqs := []MediaRequest{
		{Kind: "h3_r2v", Prompt: "p1", Width: 608, Height: 352, Frames: 124, Steps: 12, RefAudios: []string{mkAudio(0)}},
		{Kind: "h3_r2v", Prompt: "p2", Width: 608, Height: 352, Frames: 124, Steps: 12, RefAudios: []string{mkAudio(1)}},
	}
	graph, saveIDs, err := s.BuildH3BatchGraph(reqs)
	if err != nil {
		t.Fatalf("构图失败：%v", err)
	}
	if len(saveIDs) != 2 {
		t.Fatalf("应有 2 个产物节点，实际 %v", saveIDs)
	}
	for i := 0; i < 2; i++ {
		aid := strconv.Itoa(BatchAudioNodeBase + i)
		aud, ok := graph[aid].(map[string]interface{})
		if !ok {
			t.Fatalf("缺少第 %d 镜的 LoadAudio(%s)", i+1, aid)
		}
		if got := aud["class_type"]; got != "LoadAudio" {
			t.Fatalf("节点 %s 应为 LoadAudio，实际 %v", aid, got)
		}
		ref, ok := graph[strconv.Itoa(50+i)].(map[string]interface{})
		if !ok {
			t.Fatalf("缺少第 %d 镜的 ReferenceToVideo(50+%d)", i+1, i)
		}
		auds, ok := ref["inputs"].(map[string]interface{})["ref_audios"].(map[string]interface{})
		if !ok {
			t.Fatalf("第 %d 镜缺少 ref_audios 映射", i+1)
		}
		lnk, ok := auds["ref_audio_1"].([]interface{})
		if !ok || len(lnk) < 1 {
			t.Fatalf("第 %d 镜缺少 ref_audio_1 接线：%v", i+1, auds)
		}
		if got := lnk[0]; got != aid {
			t.Fatalf("第 %d 镜 ref_audio_1 必须接自己的 %s，实际 %v（串音）", i+1, aid, got)
		}
	}
	// 音轨节点不许踩产物映射段（140+i）：collectOutputsAt 靠那个段号回填镜头
	for i := 0; i < 2; i++ {
		if _, clash := graph[strconv.Itoa(BatchSaveNodeBase+i)]; !clash {
			t.Fatalf("产物节点 %d 丢了", BatchSaveNodeBase+i)
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
	allCan := func(int) bool { return true }
	allSame := func(int) bool { return true }

	shots := []Shot{mk(false, "cut"), mk(false, "cut"), mk(false, "dissolve")}
	if got := batchRunLen(shots, 0, 8, false, allCan, allSame); got != 3 {
		t.Fatalf("三镜全可批，应得 3，实际 %d", got)
	}
	if got := batchRunLen(shots, 0, 2, false, allCan, allSame); got != 2 {
		t.Fatalf("上限 2 应得 2，实际 %d", got)
	}
	// 台词镜现在可以同批（TTS 前置后一张图提交）；只有 continue 才是硬断点
	withDub := []Shot{mk(false, "cut"), mk(true, "cut"), mk(true, "cut")}
	if got := batchRunLen(withDub, 0, 8, false, allCan, allSame); got != 3 {
		t.Fatalf("台词镜应可同批，实际 %d", got)
	}
	// continue 依赖：下一镜要等上一镜末帧
	cont := []Shot{mk(false, "cut"), mk(false, "continue"), mk(false, "cut")}
	if got := batchRunLen(cont, 0, 8, false, allCan, allSame); got != 2 {
		t.Fatalf("continue 之后必须断组，实际 %d", got)
	}
	// start 自己是续接镜
	if got := batchRunLen(cont, 1, 8, true, allCan, allSame); got != 0 {
		t.Fatalf("续接镜不能当组头，实际 %d", got)
	}
	// 缓存命中直接剔除
	if got := batchRunLen(shots, 0, 8, false, func(j int) bool { return j != 1 }, allSame); got != 1 {
		t.Fatalf("第 2 镜命中缓存应断组，实际 %d", got)
	}
	// 同批签名不一致（kind / steps / 低显存档任一不同）
	if got := batchRunLen(shots, 0, 8, false, allCan, func(j int) bool { return j != 2 }); got != 2 {
		t.Fatalf("签名不一致应断组，实际 %d", got)
	}
}
