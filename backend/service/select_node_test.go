package service

import (
	"strings"
	"testing"

	"flowedge/backend/model"
)

// 择优闭环的纯逻辑测试（包 D）。
//
// 锁住三件容易悄悄坏掉的事：
//  1. 小模型输出不规范时（代码块 / 带闲话 / 只有一句话）仍能拿到分数；
//  2. 候选收集要吃得下 text2image 的 images 数组、聚合节点的 values、以及节点自填多行；
//  3. 没给参考主题时，提示词必须显式说明——否则模型会「按画面自评」，结论与主题无关。

func TestParseJudgeFormats(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		score  int
		ok     bool
		reason string
	}{
		{"clean json", `{"score": 82, "reason": "主体一致"}`, 82, true, "主体一致"},
		{"code fence", "```json\n{\"score\": 45, \"reason\": \"构图被裁\"}\n```", 45, true, "构图被裁"},
		{"chatter around", "我觉得这张图…\n{\"score\": 70, \"reason\": \"有微弱水印\"}\n以上。", 70, true, "有微弱水印"},
		{"plain sentence", "这张大概 65 分，人物偏小。", 65, true, "这张大概 65 分，人物偏小。"},
		{"clamp high", `{"score": 130, "reason": "越界"}`, 100, true, "越界"},
		{"clamp low", `{"score": -5, "reason": "负分"}`, 0, true, "负分"},
		{"garbage", "无法判断，这张图我不想评。", 0, false, "无法判断，这张图我不想评。"},
	}
	for _, c := range cases {
		score, reason, ok := parseJudge(c.in)
		if ok != c.ok {
			t.Fatalf("%s: ok=%v want %v (score=%d)", c.name, ok, c.ok, score)
		}
		if ok && (score != c.score || reason != c.reason) {
			t.Fatalf("%s: got score=%d reason=%q, want %d/%q", c.name, score, reason, c.score, c.reason)
		}
	}
}

func TestCollectCandidatesFromUpstreamShapes(t *testing.T) {
	node := &model.NodeInstance{Data: map[string]interface{}{
		"images": "/n/self1.png\n/n/self2.png\nnot-an-image.txt\n",
	}}
	upstream := map[string]interface{}{
		"t2i-1": map[string]interface{}{
			"image":  "/gen/a.png",
			"images": []interface{}{"/gen/a.png", "/gen/b.png"},
		},
		"agg-1": map[string]interface{}{
			"values": []interface{}{"/gen/c.png", "/gen/a.png"}, // 重复项要去掉
		},
	}
	got := collectCandidates(node, upstream)
	want := []string{"/n/self1.png", "/n/self2.png", "/gen/a.png", "/gen/b.png", "/gen/c.png"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order/dedup mismatch at %d: got %v, want %v", i, got, want)
		}
	}

	// 没有候选时必须返回空，而不是把非图片路径混进来
	if got := collectCandidates(&model.NodeInstance{Data: map[string]interface{}{}}, map[string]interface{}{}); len(got) != 0 {
		t.Fatalf("expected no candidates, got %v", got)
	}
}

func TestPickBest(t *testing.T) {
	if idx, best := pickBest([]int{10, 90, 40}); idx != 1 || best != 90 {
		t.Fatalf("got idx=%d best=%d", idx, best)
	}
	// 同分取靠前（顺序即生成顺序，先出来的优先）
	if idx, _ := pickBest([]int{70, 70}); idx != 0 {
		t.Fatalf("tie must keep first, got %d", idx)
	}
	if idx, best := pickBest([]int{0, 0}); idx != 0 || best != 0 {
		t.Fatalf("all-zero must not fail, got idx=%d best=%d", idx, best)
	}
}

func TestBuildJudgePromptStatesMissingReference(t *testing.T) {
	p := buildJudgePrompt("", "")
	if !strings.Contains(p, "未提供参考主题") {
		t.Fatal("empty reference must be stated explicitly, otherwise the model self-assesses")
	}
	p2 := buildJudgePrompt("青袍修士，白骨法杖，低机位对峙", "不要出现文字")
	if !strings.Contains(p2, "青袍修士") || !strings.Contains(p2, "不要出现文字") {
		t.Fatal("reference and criteria must both reach the prompt")
	}
	if !strings.Contains(p2, `"score"`) {
		t.Fatal("prompt must demand a JSON score field")
	}
}

func TestNoteForPickIsActionable(t *testing.T) {
	if got := noteForPick(80, 60, 2); !strings.Contains(got, "第 3 张") {
		t.Fatalf("passing note must name the picked take, got %q", got)
	}
	if got := noteForPick(40, 60, 0); !strings.Contains(got, "低于阈值") || !strings.Contains(got, "重跑") {
		t.Fatalf("failing note must tell the user what to do, got %q", got)
	}
}
