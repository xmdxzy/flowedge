package service

import (
	"strings"
	"testing"
)

func TestDiagnosePrompt_Empty(t *testing.T) {
	d := DiagnosePrompt("   ", true)
	if d.Severity != SeverityFail {
		t.Errorf("空提示词应 fail，实际 %s", d.Severity)
	}
	if len(d.Defects) != 1 || d.Defects[0].Operator != "require_nonempty" {
		t.Errorf("空提示词应给 require_nonempty，实际 %+v", d.Defects)
	}
}

// 要素齐全的视频提示词必须判 ok —— 误判会让人去改一个本来没问题的提示词。
func TestDiagnosePrompt_Complete(t *testing.T) {
	text := "蓝发少女在海边码头转身，逆光，中景跟拍，海浪拍岸，无配乐"
	d := DiagnosePrompt(text, true)
	if d.Severity != SeverityOK {
		t.Fatalf("要素齐全应 ok，实际 %s：%+v", d.Severity, d.Defects)
	}
	if len(d.Defects) != 0 {
		t.Errorf("不应有缺陷：%+v", d.Defects)
	}
}

// 图片不要求镜头语言与音频段（图片没有运镜、没有音轨）。
func TestDiagnosePrompt_ImageSkipsCameraAndAudio(t *testing.T) {
	text := "蓝发少女在海边转身"
	d := DiagnosePrompt(text, false)
	for _, x := range d.Defects {
		if x.Type == DefectMissingCamera || x.Type == DefectMissingAudio {
			t.Errorf("图片提示词不应要求 %s", x.Type)
		}
	}
}

func TestDiagnosePrompt_MissingCameraAndAudio(t *testing.T) {
	text := "少女在海边转身"
	d := DiagnosePrompt(text, true)
	got := map[string]bool{}
	for _, x := range d.Defects {
		got[x.Type] = true
	}
	if !got[DefectMissingCamera] {
		t.Error("缺镜头语言应被报出")
	}
	if !got[DefectMissingAudio] {
		t.Error("缺音频描述应被报出")
	}
	if d.Severity != SeverityWarn {
		t.Errorf("缺镜头/音频应为 warn（能跑但不理想），实际 %s", d.Severity)
	}
}

// 缺动作/场景属 fail：这两项缺了基本拍不出想要的东西。
func TestDiagnosePrompt_MissingCoreIsFail(t *testing.T) {
	d := DiagnosePrompt("一个画面", true)
	if d.Severity != SeverityFail {
		t.Errorf("缺核心要素应 fail，实际 %s：%+v", d.Severity, d.Defects)
	}
}

// 模糊修饰只报一次：同类问题报 5 条会淹没真正的缺口。
func TestDiagnosePrompt_AmbiguousOnce(t *testing.T) {
	text := "少女在海边转身，画面很美很震撼，中景跟拍，海浪声"
	d := DiagnosePrompt(text, true)
	n := 0
	for _, x := range d.Defects {
		if x.Type == DefectAmbiguous {
			n++
		}
	}
	if n != 1 {
		t.Errorf("模糊修饰应只报 1 条，实际 %d 条", n)
	}
}

func TestDiagnosePrompt_Overlong(t *testing.T) {
	long := strings.Repeat("少女在海边转身，中景跟拍，海浪声。", 60)
	if len([]rune(long)) <= promptMaxRunes {
		t.Skip("测试文本不够长")
	}
	d := DiagnosePrompt(long, true)
	found := false
	for _, x := range d.Defects {
		if x.Type == DefectOverlong {
			found = true
		}
	}
	if !found {
		t.Error("超长提示词应报 overlong")
	}
}

func TestApplyOperator_Trim(t *testing.T) {
	long := strings.Repeat("第一句。", 300)
	out, note, err := ApplyOperator(long, "trim", "")
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(out)) > promptMaxRunes {
		t.Errorf("裁剪后仍超长：%d", len([]rune(out)))
	}
	if !strings.HasSuffix(out, "。") {
		tail := out
		if len(out) > 10 {
			tail = out[len(out)-10:]
		}
		t.Errorf("应在句子边界裁剪，实际结尾：%q", tail)
	}
	if note == "" {
		t.Error("裁剪应给说明")
	}

	// 未超长时不动
	short := "少女在海边转身。"
	if got, _, err := ApplyOperator(short, "trim", ""); err != nil || got != short {
		t.Errorf("未超长不应改动：got=%q err=%v", got, err)
	}
}

func TestApplyOperator_ReplaceAmbiguous(t *testing.T) {
	out, note, err := ApplyOperator("画面很美，少女转身", "replace_ambiguous", "暖色逆光、浅景深")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "很美") {
		t.Errorf("模糊词未被替换：%q", out)
	}
	if !strings.Contains(out, "暖色逆光") {
		t.Errorf("替换文本未出现：%q", out)
	}
	if note == "" {
		t.Error("替换应给说明")
	}

	// 空替换文本必须报错（不能把模糊词删掉就算了）
	if _, _, err := ApplyOperator("画面很美", "replace_ambiguous", "  "); err == nil {
		t.Error("空替换文本应报错")
	}
}

// 创作性算子必须拒绝：它们要 LLM 生成内容，纯规则硬拼等于编造。
func TestApplyOperator_CreativeOpsRejected(t *testing.T) {
	for _, op := range []string{"add_scene", "add_action", "add_camera", "add_audio"} {
		if _, _, err := ApplyOperator("少女", op, ""); err == nil {
			t.Errorf("%s 应报错（需 LLM），实际通过", op)
		}
	}
	if _, _, err := ApplyOperator("少女", "不存在的算子", ""); err == nil {
		t.Error("未知算子应报错")
	}
}

func TestApplyOperator_NormalizeSpace(t *testing.T) {
	out, _, err := ApplyOperator("少女   在海边    转身", "normalize_space", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "  ") {
		t.Errorf("多余空格未合并：%q", out)
	}
}
