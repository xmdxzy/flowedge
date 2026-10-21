package service

import (
	"testing"

	"flowedge/backend/model"
)

// CJK 检测：中文/日文假名/韩文算，纯英文不算
func TestHasCJK(t *testing.T) {
	cases := map[string]bool{
		"雪山顶的白鹿":                 true,
		"a deer on a snowy peak": false,
		"猫と犬":                    true, // 日文假名
		"한국어":                    true, // 韩文
		"":                       false,
		"1080p, cinematic, 4k":   false,
		"混合 mixed 中english":      true,
	}
	for in, want := range cases {
		if got := hasCJK(in); got != want {
			t.Errorf("hasCJK(%q) = %v, want %v", in, got, want)
		}
	}
}

// auto_translate 默认关时不得调用 LLM（llm 为 nil 也不能 panic）；
// 带一个 BaseURL 为空的 t2i，让链路走到「comfyui not configured」即证明
// 翻译钩子没有短路也没有 panic
func TestText2ImageNode_NoAutoTranslateByDefault(t *testing.T) {
	e := &EngineV2{t2i: &Text2ImageService{}} // llm 为 nil，BaseURL 为空
	node := &model.NodeInstance{
		ID: "t", Type: "text2image",
		Data: map[string]interface{}{"prompt": "雪山顶的白鹿，远景"},
	}
	_, err := e.execText2ImageNode(node, map[string]interface{}{})
	if err == nil {
		t.Fatal("无 ComfyUI 配置应显式失败")
	}
	if !containsFold(err.Error(), "comfyui not configured") {
		t.Fatalf("应走到 ComfyUI 未配置的显式失败，got %v", err)
	}
}
