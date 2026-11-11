package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v2v：源视频必须作为 ref_video_0 接进 ReferenceToVideo，且按镜切片。
//
// 判据三条：
//  1. 图上存在 VHS_LoadVideo(76)，且 skip_first_frames == SourceStartFrame、
//     frame_load_cap == 生成帧数（切错就是"每镜都从第 0 帧开始"，画面全一样）；
//  2. ReferenceToVideo 的 ref_videos 里有 ref_video_0 → 76；
//  3. 提示词里出现 <Video 1>（否则模型把源片当风格参考，改了个寂寞）。
func TestBuildMediaGraph_SourceVideo(t *testing.T) {
	m := newMockComfyServer(t)
	out := t.TempDir()
	svc := newTestT2I(m.srv.URL, out)
	src := filepath.Join(out, "source.mp4")
	if err := os.WriteFile(src, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := svc.BuildMediaGraph(MediaRequest{
		Kind: "h3_r2v", Prompt: "the same room, change the wall to red",
		Width: 608, Height: 352, Frames: 124, Steps: 12,
		SourceVideo: src, SourceStartFrame: 248,
	})
	if err != nil {
		t.Fatalf("构图失败：%v", err)
	}

	node76, ok := g["76"].(map[string]interface{})
	if !ok {
		t.Fatal("缺少源视频节点 76")
	}
	if got := node76["class_type"]; got != "VHS_LoadVideo" {
		t.Fatalf("76 应为 VHS_LoadVideo，实际 %v", got)
	}
	in := nodeInputs(g, "76")
	if got := in["skip_first_frames"]; got != 248 {
		t.Fatalf("应从源视频第 248 帧开始切，实际 %v", got)
	}
	if got := in["frame_load_cap"]; got != 124 {
		t.Fatalf("应取 124 帧，实际 %v", got)
	}

	refVids, ok := nodeInputs(g, "5")["ref_videos"].(map[string]interface{})
	if !ok || refVids["ref_video_0"] == nil {
		t.Fatalf("ref_videos 里缺 ref_video_0：%v", nodeInputs(g, "5")["ref_videos"])
	}
	if got := refVids["ref_video_0"].([]interface{})[0]; got != "76" {
		t.Fatalf("ref_video_0 应接 76，实际 %v", got)
	}

	prompt, _ := nodeInputs(g, "5")["prompt"].(string)
	if !strings.Contains(strings.ToLower(prompt), "<video 1>") {
		t.Fatalf("提示词里必须出现 <Video 1>，实际：%s", prompt)
	}
}

// 没有源视频时不该出现 76 / ref_videos 内容（不该给模型塞空参考）。
func TestBuildMediaGraph_NoSourceVideo(t *testing.T) {
	m := newMockComfyServer(t)
	svc := newTestT2I(m.srv.URL, t.TempDir())
	g, err := svc.BuildMediaGraph(MediaRequest{
		Kind: "h3_r2v", Prompt: "p", Width: 608, Height: 352, Frames: 124, Steps: 12,
		RefAudios: []string{},
		RefImages: nil,
	})
	// 没有任何参考时本来就该报错；这里给一张图让它成立
	if err == nil {
		t.Fatal("既没参考图也没参考音频和源视频，应该报错")
	}

	src := filepath.Join(t.TempDir(), "x.mp4")
	_ = os.WriteFile(src, []byte("x"), 0o644)
	g2, err2 := svc.BuildMediaGraph(MediaRequest{
		Kind: "h3_r2v", Prompt: "p", Width: 608, Height: 352, Frames: 124,
		RefImages: []string{src},
	})
	if err2 != nil {
		t.Fatalf("有参考图时应能构图：%v", err2)
	}
	if _, ok := g2["76"]; ok {
		t.Fatal("没给源视频时不该有 76 节点")
	}
	refVids := nodeInputs(g2, "5")["ref_videos"]
	if m, ok := refVids.(map[string]interface{}); ok && len(m) > 0 {
		t.Fatalf("没给源视频时 ref_videos 应为空，实际 %v", m)
	}
	_ = g
}

// 提示词补 <Video 1>：必须写进 retention_analysis 段，而不是甩在末尾。
func TestEnsureSourceVideoPrompt(t *testing.T) {
	withRetention := "subject_definitions:\n<Subject 1> is the man.\nretention_analysis:\n<Picture 1>: fully_preserved.\n"
	got := ensureSourceVideoPrompt(withRetention)
	if !strings.Contains(got, "<Video 1>") {
		t.Fatalf("应补上 <Video 1>：%s", got)
	}
	// 插入位置：在 retention_analysis 之后、原第一条参考之前
	iRet := strings.Index(got, "retention_analysis:")
	iVid := strings.Index(got, "<Video 1>")
	iPic := strings.Index(got, "<Picture 1>")
	if !(iRet < iVid && iVid < iPic) {
		t.Fatalf("位置不对：retention=%d video=%d picture=%d\n%s", iRet, iVid, iPic, got)
	}

	// 已经写了就不再重复
	already := "retention_analysis:\n<Video 1>: keep it.\n"
	if got := ensureSourceVideoPrompt(already); strings.Count(got, "<Video 1>") != 1 {
		t.Fatalf("已有 <Video 1> 时不该重复添加：%s", got)
	}

	// 完全没有六段式时也要能用
	if got := ensureSourceVideoPrompt("a room"); !strings.Contains(got, "<Video 1>") {
		t.Fatalf("纯文本也应补上：%s", got)
	}
}
