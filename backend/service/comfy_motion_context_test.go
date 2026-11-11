package service

import (
	"os"
	"path/filepath"
	"testing"
)

// 段间引导的图接线。
//
// 为什么用 mock 也要钉这个：接错线的代价是**烧完一整条 GPU 任务才发现**。
// 这里锁死三件事：
//  1. 三个新节点 class_type 正确（VHS_LoadVideo / LoadAudio / MiniMaxH3AddGuide）；
//  2. guider 的 conditioning 必须来自 72（否则锚定根本没进采样）；
//  3. 有片段时**不许**再出现单帧 AddGuide(21)（同一帧挂两个 keyframe 会闪）。
func TestBuildMediaGraph_MotionContext(t *testing.T) {
	m := newMockComfyServer(t)
	out := t.TempDir()
	svc := newTestT2I(m.srv.URL, out)

	clip := filepath.Join(out, "tail.mp4")
	aud := filepath.Join(out, "tail.wav")
	for _, p := range []string{clip, aud} {
		if err := os.WriteFile(p, []byte("fake"), 0o644); err != nil {
			t.Fatalf("造素材失败：%v", err)
		}
	}

	g, err := svc.BuildMediaGraph(MediaRequest{
		Kind: "h3", Prompt: "p", Width: 608, Height: 352, Frames: 158, Steps: 12,
		GuideClip: clip, GuideFrames: 22, GuideAudio: aud, TrimHead: 22,
	})
	if err != nil {
		t.Fatalf("构图失败：%v", err)
	}

	want := map[string]string{"70": "VHS_LoadVideo", "71": "LoadAudio", "72": "MiniMaxH3AddGuide"}
	for id, cls := range want {
		node, ok := g[id].(map[string]interface{})
		if !ok {
			t.Fatalf("缺少段间引导节点 %s", id)
		}
		if got := node["class_type"]; got != cls {
			t.Fatalf("节点 %s 应为 %s，实际 %v", id, cls, got)
		}
	}

	// conditioning 必须接到 72，否则等于没开引导
	if got := nodeInputs(g, "7")["conditioning"].([]interface{})[0]; got != "72" {
		t.Fatalf("guider 的 conditioning 应来自引导节点 72，实际 %v", got)
	}

	ag := nodeInputs(g, "72")
	if got := ag["image"].([]interface{})[0]; got != "70" {
		t.Fatalf("AddGuide.image 应接 70（尾部片段），实际 %v", got)
	}
	if got := ag["audio"].([]interface{})[0]; got != "71" {
		t.Fatalf("AddGuide.audio 应接 71（尾音轨），实际 %v", got)
	}
	if got := ag["vae"].([]interface{})[0]; got != "3" {
		t.Fatalf("AddGuide.vae 应接视频 VAE 节点 3，实际 %v", got)
	}
	if got := ag["audio_vae"].([]interface{})[0]; got != "4" {
		t.Fatalf("AddGuide.audio_vae 应接音频 VAE 节点 4，实际 %v", got)
	}
	if got := ag["latent"].([]interface{})[0]; got != "5" {
		t.Fatalf("AddGuide.latent 应接 5 号 AV latent，实际 %v", got)
	}
	if got := ag["frame_idx"]; got != 0 {
		t.Fatalf("AddGuide.frame_idx 应为 0（钉在片头），实际 %v", got)
	}
	if _, exists := g["21"]; exists {
		t.Fatal("已有 22 帧片段锚定，不该再叠单帧 AddGuide（21）")
	}

	// 帧数必须原样传进图：158 = align17(124+22)
	if got := nodeInputs(g, "5")["length"]; got != 158 {
		t.Fatalf("ImageToVideo.length 应为 158，实际 %v", got)
	}
}

// 没有音轨时只锚画面：audio 口不能出现，audio_vae 也不该被连。
func TestBuildMediaGraph_MotionContextVideoOnly(t *testing.T) {
	m := newMockComfyServer(t)
	out := t.TempDir()
	svc := newTestT2I(m.srv.URL, out)
	clip := filepath.Join(out, "tail.mp4")
	if err := os.WriteFile(clip, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := svc.BuildMediaGraph(MediaRequest{
		Kind: "h3", Prompt: "p", Width: 608, Height: 352, Frames: 158,
		GuideClip: clip, GuideFrames: 22, TrimHead: 22,
	})
	if err != nil {
		t.Fatalf("构图失败：%v", err)
	}
	ag := nodeInputs(g, "72")
	if _, has := ag["audio"]; has {
		t.Fatal("没有尾音轨时不该连 AddGuide.audio")
	}
	if _, has := g["71"]; has {
		t.Fatal("没有尾音轨时不该建 LoadAudio 节点")
	}
	if _, has := ag["image"]; !has {
		t.Fatal("画面锚定仍应存在")
	}
}

// ref2va + 段间引导：身份锁与运动续接要能同时挂在图上。
// （真机是否两个条件同时生效另说，这里先钉住「图没有把它们互斥掉」。）
func TestBuildMediaGraph_MotionContextWithRef2VA(t *testing.T) {
	m := newMockComfyServer(t)
	out := t.TempDir()
	svc := newTestT2I(m.srv.URL, out)
	clip := filepath.Join(out, "tail.mp4")
	ref := filepath.Join(out, "ref.png")
	for _, p := range []string{clip, ref} {
		if err := os.WriteFile(p, []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g, err := svc.BuildMediaGraph(MediaRequest{
		Kind: "h3_r2v", Prompt: "p", Width: 608, Height: 352, Frames: 158,
		RefImages: []string{ref}, GuideClip: clip, GuideFrames: 22, TrimHead: 22,
	})
	if err != nil {
		t.Fatalf("构图失败：%v", err)
	}
	if got := g["5"].(map[string]interface{})["class_type"]; got != "MiniMaxH3ReferenceToVideo" {
		t.Fatalf("应走 ref2va，实际 %v", got)
	}
	// 参考图仍要在图上
	if got := nodeInputs(g, "5")["ref_images"]; got == nil {
		t.Fatal("ref_images 不能丢")
	}
	// 锚定仍要挂在最外层 conditioning 上
	if got := nodeInputs(g, "7")["conditioning"].([]interface{})[0]; got != "72" {
		t.Fatalf("guider 应接引导节点 72，实际 %v", got)
	}
}
