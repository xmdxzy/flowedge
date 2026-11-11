package service

import (
	"testing"
)

// 二采（同分辨率精修）的图接线。
//
// 关键点只有一个：解码必须改接第二遍采样器的输出（75），否则二采白算——
// 图能跑、产物正常、时间也花了，就是画面还是一采的。这种"静默无效"最难发现。
func TestBuildMediaGraph_Refine(t *testing.T) {
	m := newMockComfyServer(t)
	out := t.TempDir()
	svc := newTestT2I(m.srv.URL, out)

	g, err := svc.BuildMediaGraph(MediaRequest{
		Kind: "h3", Prompt: "p", Width: 608, Height: 352, Frames: 124, Steps: 12,
		RefineDenoise: 0.35, RefineSteps: 4,
	})
	if err != nil {
		t.Fatalf("构图失败：%v", err)
	}

	want := map[string]string{
		"73": "RandomNoise",
		"74": "BasicScheduler",
		"75": "SamplerCustomAdvanced",
	}
	for id, cls := range want {
		node, ok := g[id].(map[string]interface{})
		if !ok {
			t.Fatalf("缺少二采节点 %s", id)
		}
		if got := node["class_type"]; got != cls {
			t.Fatalf("节点 %s 应为 %s，实际 %v", id, cls, got)
		}
	}

	sched := nodeInputs(g, "74")
	if got := sched["denoise"]; got != 0.35 {
		t.Fatalf("二采 denoise 应为 0.35，实际 %v", got)
	}
	if got := sched["steps"]; got != 4 {
		t.Fatalf("二采 steps 应为 4，实际 %v", got)
	}

	// 二采必须吃一采的 latent（10），否则等于重新生成一遍
	if got := nodeInputs(g, "75")["latent_image"].([]interface{})[0]; got != "10" {
		t.Fatalf("二采 latent_image 应接一采输出 10，实际 %v", got)
	}

	// 解码必须改接 75
	if got := nodeInputs(g, "11")["samples"].([]interface{})[0]; got != "75" {
		t.Fatalf("VAEDecode 应接二采输出 75，实际 %v", got)
	}
	if got := nodeInputs(g, "12")["samples"].([]interface{})[0]; got != "75" {
		t.Fatalf("VAEDecodeAudio 应接二采输出 75，实际 %v", got)
	}
}

// 不开二采时，图里不该出现 73/74/75，解码仍接一采。
func TestBuildMediaGraph_NoRefine(t *testing.T) {
	m := newMockComfyServer(t)
	svc := newTestT2I(m.srv.URL, t.TempDir())
	g, err := svc.BuildMediaGraph(MediaRequest{
		Kind: "h3", Prompt: "p", Width: 608, Height: 352, Frames: 124, Steps: 12,
	})
	if err != nil {
		t.Fatalf("构图失败：%v", err)
	}
	for _, id := range []string{"73", "74", "75"} {
		if _, ok := g[id]; ok {
			t.Fatalf("没开二采不该有节点 %s", id)
		}
	}
	if got := nodeInputs(g, "11")["samples"].([]interface{})[0]; got != "10" {
		t.Fatalf("VAEDecode 应接一采输出 10，实际 %v", got)
	}
}

// nodeFloatLoose 要能吃 JSON 数字、整数与字符串，才能让 refine 字段在任何来源下都生效。
func TestNodeFloatLoose(t *testing.T) {
	if v, ok := nodeFloatLoose(float64(0.35)); !ok || v != 0.35 {
		t.Fatalf("float64 解析失败：%v %v", v, ok)
	}
	if v, ok := nodeFloatLoose("0.5"); !ok || v != 0.5 {
		t.Fatalf("字符串解析失败：%v %v", v, ok)
	}
	if _, ok := nodeFloatLoose(nil); ok {
		t.Fatal("nil 不该被当成 0")
	}
}
