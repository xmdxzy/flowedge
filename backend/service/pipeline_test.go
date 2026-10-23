package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"flowedge/backend/model"
)

// 旧数据只有单选 binding：必须自动迁移到对应阶段，不能让老工作流"贴了卡突然不生效"。
func TestParsePipelineLegacyBinding(t *testing.T) {
	p := ParsePipeline(map[string]interface{}{
		"binding": map[string]interface{}{"kind": "agent", "id": "story-director"},
	})
	if p.Body == nil || p.Body.ID != "story-director" {
		t.Fatalf("agent 旧绑定应迁到本体段，实际 %+v", p.Body)
	}

	p = ParsePipeline(map[string]interface{}{
		"binding": map[string]interface{}{"kind": "workflow", "id": "12"},
	})
	if p.Replace == nil || p.Replace.ID != "12" {
		t.Fatalf("workflow 旧绑定应迁到替换段，实际 %+v", p.Replace)
	}
}

// 三类卡可以同时存在：输入 / 本体 / 输出 / 替换 各自独立归档，顺序按 order。
func TestParsePipelineStages(t *testing.T) {
	p := ParsePipeline(map[string]interface{}{
		"bindings": []interface{}{
			map[string]interface{}{"kind": "skill", "id": "zh2en", "stage": "input", "order": 2},
			map[string]interface{}{"kind": "skill", "id": "summarize", "stage": "input", "order": 1},
			map[string]interface{}{"kind": "agent", "id": "prompt-engineer", "stage": "body"},
			map[string]interface{}{"kind": "skill", "id": "json-extract", "stage": "output"},
			map[string]interface{}{"kind": "workflow", "id": "12", "stage": "replace"},
		},
	})
	if len(p.Input) != 2 || p.Input[0].ID != "summarize" || p.Input[1].ID != "zh2en" {
		t.Fatalf("输入段应按 order 排序，实际 %+v", p.Input)
	}
	if p.Body == nil || p.Body.ID != "prompt-engineer" {
		t.Fatalf("本体段解析错：%+v", p.Body)
	}
	if len(p.Output) != 1 || p.Output[0].ID != "json-extract" {
		t.Fatalf("输出段解析错：%+v", p.Output)
	}
	if p.Replace == nil || p.Replace.ID != "12" {
		t.Fatalf("替换段解析错：%+v", p.Replace)
	}
	if n := PipelineCallCount(p); n != 4 {
		t.Errorf("模型调用次数应为 4（2 输入 + 1 本体 + 1 输出），实际 %d", n)
	}
}

// 本体与替换各限 1 张：多出来的忽略（先声明者优先），并在校验里明确说错。
func TestPipelineLimitsAndValidation(t *testing.T) {
	data := map[string]interface{}{
		"bindings": []interface{}{
			map[string]interface{}{"kind": "agent", "id": "a1", "stage": "body"},
			map[string]interface{}{"kind": "agent", "id": "a2", "stage": "body"},
			map[string]interface{}{"kind": "workflow", "id": "1", "stage": "replace"},
			map[string]interface{}{"kind": "workflow", "id": "2", "stage": "replace"},
		},
	}
	p := ParsePipeline(data)
	if p.Body == nil || p.Body.ID != "a1" {
		t.Errorf("本体段应保留先声明的那张，实际 %+v", p.Body)
	}
	if p.Replace == nil || p.Replace.ID != "1" {
		t.Errorf("替换段应保留先声明的那张，实际 %+v", p.Replace)
	}

	issues := ValidatePipeline(data)
	if len(issues) < 3 {
		t.Fatalf("应报出重复本体/重复替换/本体与替换互斥，实际 %v", issues)
	}
}

// 只有工作流卡能做整节点替换；agent 放进去要被拒。
func TestValidatePipelineRejectsWrongKind(t *testing.T) {
	issues := ValidatePipeline(map[string]interface{}{
		"bindings": []interface{}{
			map[string]interface{}{"kind": "agent", "id": "x", "stage": "replace"},
			map[string]interface{}{"kind": "workflow", "id": "1", "stage": "body"},
		},
	})
	if len(issues) < 2 {
		t.Fatalf("两种错配都应报出，实际 %v", issues)
	}
}

// 输入处理段必须对**任何节点**生效，而不只是 LLM 节点。
// 这条测试用 data_transform（它自己完全不认识卡片）来证明：
// 预处理发生在派发之前，与执行体无关。
func TestApplyInputStageIsNodeAgnostic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"TRANSFORMED-BY-CARD"}}]}`))
	}))
	defer srv.Close()

	e := &EngineV2{cards: LoadCards(t.TempDir() + "/cards.json")}
	e.llm = NewLLMService(srv.URL, "unit-test-model")
	e.llm.Backend = "openai" // httptest 返回 OpenAI 形状

	node := &model.NodeInstance{
		ID: "n1", Type: "data_transform",
		Data: map[string]interface{}{
			"bindings": []interface{}{
				map[string]interface{}{"kind": "skill", "id": "zh2en", "stage": "input"},
			},
		},
	}
	upstream := map[string]interface{}{
		"s": map[string]interface{}{"text": "一段独一无二的中文-用于管线测试-甲"},
	}

	out := e.applyInputStage(node, upstream, &cardTrace{})
	got := out["s"].(map[string]interface{})["text"]
	if got != "TRANSFORMED-BY-CARD" {
		t.Fatalf("输入段没有改写文本，实际 %q", got)
	}

	// 原 map 不能被动过：同层并行时它可能还被别的节点读着
	if orig := upstream["s"].(map[string]interface{})["text"]; orig != "一段独一无二的中文-用于管线测试-甲" {
		t.Fatalf("原上游 map 被就地改写：%q", orig)
	}

	// 没有输入卡时必须是同一个 map（零拷贝，避免每次执行都复制一遍大对象）
	bare := &model.NodeInstance{ID: "n2", Type: "data_transform", Data: map[string]interface{}{}}
	if same := e.applyInputStage(bare, upstream, &cardTrace{}); len(same) != len(upstream) {
		t.Fatal("无卡时不该改动上下游结构")
	}
}

// 输出段与输入段对称：任何节点的 text 产出都会被处理，其它产物原样保留。
func TestApplyOutputStageOnlyTouchesText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"SHAPED"}}]}`))
	}))
	defer srv.Close()

	e := &EngineV2{cards: LoadCards(t.TempDir() + "/cards.json")}
	e.llm = NewLLMService(srv.URL, "unit-test-model-out")
	e.llm.Backend = "openai"

	node := &model.NodeInstance{ID: "n3", Type: "data_transform", Data: map[string]interface{}{
		"bindings": []interface{}{
			map[string]interface{}{"kind": "skill", "id": "json-extract", "stage": "output"},
		},
	}}
	refs := ParsePipeline(node.Data).Output

	got := e.applyOutputStage(node, map[string]interface{}{"text": "原始产出-乙", "rows": 3}, refs, &cardTrace{})
	m := got.(map[string]interface{})
	if m["text"] != "SHAPED" {
		t.Fatalf("输出段没整形：%v", m["text"])
	}
	if m["rows"] != 3 {
		t.Fatalf("非文本字段被动了：%v", m)
	}

	// 纯图片产出（没有 text 字段）不该被塞一个 text 进去
	img := e.applyOutputStage(node, map[string]interface{}{"image": "a.png"}, refs, &cardTrace{})
	if _, has := img.(map[string]interface{})["text"]; has {
		t.Fatal("图片产出被凭空加了 text 字段")
	}
}

// 卡片执行要留痕：贴了卡但没生效（模型报错 / 卡被删）时，
// 用户必须能从输出里看出是哪一张失败、为什么。
func TestOutputStageRecordsCardTrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}))
	defer srv.Close()

	e := &EngineV2{cards: LoadCards(t.TempDir() + "/cards.json")}
	e.llm = NewLLMService(srv.URL, "unit-test-model-trace")
	e.llm.Backend = "openai"

	node := &model.NodeInstance{ID: "n4", Type: "data_transform", Data: map[string]interface{}{
		"bindings": []interface{}{
			map[string]interface{}{"kind": "skill", "id": "summarize", "stage": "output"},
			map[string]interface{}{"kind": "skill", "id": "已删除的技能", "stage": "output", "order": 1},
		},
	}}
	ct := &cardTrace{}
	out := e.applyOutputStage(node, map[string]interface{}{"text": "输入-丙"}, ParsePipeline(node.Data).Output, ct)
	m := out.(map[string]interface{})

	tr, ok := m["_cards"].([]map[string]interface{})
	if !ok || len(tr) != 2 {
		t.Fatalf("应记录 2 条卡片执行痕迹，实际 %v", m["_cards"])
	}
	if tr[0]["ok"] != true {
		t.Errorf("第一张应成功：%v", tr[0])
	}
	if tr[1]["ok"] != false || tr[1]["error"] == "" {
		t.Errorf("不存在的卡应记为失败并带原因：%v", tr[1])
	}
}

// 媒体卡：图节点也能有输出段——走视觉模型，结论写回 review（无 text 时补 text）。
// 这条路径以前不存在，理由是"输出段只改 text"；实际上图片节点才是最需要审片的。
func TestOutputStageRunsMediaCard(t *testing.T) {
	var gotImages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/chat" {
			var body struct {
				Messages []struct {
					Images []string `json:"images"`
				} `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Messages) > 0 {
				gotImages = len(body.Messages[0].Images)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"REVIEW-OK"},"done":true}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	dir := t.TempDir()
	img := filepath.Join(dir, "a.png")
	if err := os.WriteFile(img, []byte("not-a-real-png-but-bytes-are-enough"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := &EngineV2{cards: LoadCards(dir + "/cards.json")}
	e.llm = NewLLMService(srv.URL, "vision-test")
	e.llm.Backend = "ollama"

	node := &model.NodeInstance{ID: "g1", Type: "text2image", Data: map[string]interface{}{
		"model": "vl-test",
		"bindings": []interface{}{
			map[string]interface{}{"kind": "skill", "id": "art-review", "stage": "output"},
		},
	}}
	out := e.applyOutputStage(node, map[string]interface{}{"image": img}, ParsePipeline(node.Data).Output, &cardTrace{})
	m := out.(map[string]interface{})

	if m["review"] != "REVIEW-OK" {
		t.Fatalf("审片结论没写回 review：%v", m)
	}
	// 图节点原本没有 text：补上，下游质检门/LLM 才能直接吃
	if m["text"] != "REVIEW-OK" {
		t.Fatalf("没有 text 时应补上审片结论：%v", m)
	}
	if gotImages != 1 {
		t.Fatalf("应把 1 张图 base64 传给视觉模型，实际 %d", gotImages)
	}
	tr, _ := m["_cards"].([]map[string]interface{})
	if len(tr) != 1 || tr[0]["ok"] != true {
		t.Fatalf("媒体卡应记为成功：%v", m["_cards"])
	}
}

// 没有视觉模型时必须"记失败但不出错"——出图成功就该保留，不能因为审片挂掉把整步判死。
func TestMediaCardWithoutVisionModelDegrades(t *testing.T) {
	e := &EngineV2{cards: LoadCards(t.TempDir() + "/cards.json")}
	e.llm = NewLLMService("http://127.0.0.1:1", "vision-test")
	e.llm.Backend = "ollama"

	node := &model.NodeInstance{ID: "g2", Type: "text2image", Data: map[string]interface{}{
		"bindings": []interface{}{
			map[string]interface{}{"kind": "skill", "id": "art-review", "stage": "output"},
		},
	}}
	out := e.applyOutputStage(node, map[string]interface{}{"image": "missing.png"}, ParsePipeline(node.Data).Output, &cardTrace{})
	m := out.(map[string]interface{})
	if _, has := m["review"]; has {
		t.Fatal("没有视觉模型时不该凭空写出 review")
	}
	tr, _ := m["_cards"].([]map[string]interface{})
	if len(tr) != 1 || tr[0]["ok"] != false {
		t.Fatalf("应留下失败痕迹：%v", m["_cards"])
	}
}
