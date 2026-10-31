package service

import (
	"strings"
	"testing"
)

// 诊断指导是「约束」不是「重写」：必须列清楚只改哪几处，并要求逐字保留已有内容。
func TestDiagnoseGuide_Content(t *testing.T) {
	d := DiagnosePrompt("少女在海边转身", true) // 缺镜头 + 缺音频
	g := DiagnoseGuide(d)
	if g == "" {
		t.Fatal("有缺陷时应产出指导文本")
	}
	for _, must := range []string{"只改这些", "逐字保留", DefectMissingCamera, DefectMissingAudio} {
		if !strings.Contains(g, must) {
			t.Errorf("指导文本缺少 %q：\n%s", must, g)
		}
	}
	// 不能把「不要新增未要求的元素」漏掉：多段片里角色漂移常来自 LLM 顺手加人/加景
	if !strings.Contains(g, "不要新增未要求的元素") {
		t.Errorf("必须禁止新增元素，实际：\n%s", g)
	}
}

// 诊断 ok 时必须返回空串 —— 这是「旧行为逐字不变」的开关。
// 一旦这里返回非空，所有提示词都会被注入指导，等于偷偷改了全量出片。
func TestDiagnoseGuide_EmptyWhenOK(t *testing.T) {
	d := DiagnosePrompt("蓝发少女在海边码头转身，逆光，中景跟拍，海浪拍岸，无配乐", true)
	if d.Severity != SeverityOK {
		t.Fatalf("前置条件错：应判 ok，实际 %s %+v", d.Severity, d.Defects)
	}
	if g := DiagnoseGuide(d); g != "" {
		t.Errorf("诊断 ok 时不应产出指导（否则旧行为会变），实际：\n%s", g)
	}
}

// 空提示词也走 fail 分支，但指导文本仍要给出可执行动作。
func TestDiagnoseGuide_EmptyPrompt(t *testing.T) {
	d := DiagnosePrompt("", true)
	g := DiagnoseGuide(d)
	if g == "" {
		t.Fatal("空提示词也应给指导")
	}
	if !strings.Contains(g, "require_nonempty") {
		t.Errorf("应包含 require_nonempty 算子名便于追溯，实际：\n%s", g)
	}
}
