package service

import (
	"strings"
	"testing"
)

// 模板是契约：这三条（先证据、带帧号、五档等级）缺一条，判词就退化成"猜一个分数"。
func TestBuildJudgePrompt_Contract(t *testing.T) {
	p := BuildJudgePrompt("人物身份稳定", []string{"帧1", "帧2", "帧3"})
	for _, must := range []string{
		"不要先给结论",                                   // 先证据后等级
		"[帧N]",                                     // 证据必须带帧号
		"等级：",                                      // 等级行格式
		"excellent", "good", "fair", "poor", "bad", // 五档齐全
		"修复：", // 必须给可执行动作
		"帧1、帧2、帧3",
	} {
		if !strings.Contains(p, must) {
			t.Errorf("模板缺少契约要素 %q\n---\n%s", must, p)
		}
	}
	// 禁止模糊措辞的要求必须在
	if !strings.Contains(p, "可能/似乎") {
		t.Error("模板必须显式禁止「可能/似乎」这类模糊措辞")
	}
	// 空 topic 要给默认目标，不能生成"判断目标："后空着
	if def := BuildJudgePrompt("", nil); !strings.Contains(def, "判断目标：画面内容") {
		t.Errorf("空 topic 应回落默认目标，实际：%s", def)
	}
}

func TestParseVLMVerdict_Standard(t *testing.T) {
	raw := `1) 证据：
- [帧1] 蓝发少女站在码头，穿鲸鱼围裙，逆光。
- [帧2] 同一角色正面，围裙图案清晰。
- [帧3] 角色转身，背景有渔船。
2) 缺陷：
- [帧3] 左手手指数量异常。
3) 等级：fair
4) 修复：重拍第 3 镜，或在提示词里加「双手自然下垂」`

	v := ParseVLMVerdict(raw)
	if !v.ParseOK || v.Grade != GradeFair {
		t.Fatalf("应解析出 fair，实际 %+v", v)
	}
	if len(v.Evidence) != 3 {
		t.Errorf("应有 3 条证据，实际 %d：%v", len(v.Evidence), v.Evidence)
	}
	if len(v.Defects) != 1 || !strings.Contains(v.Defects[0], "手指") {
		t.Errorf("缺陷解析错误：%v", v.Defects)
	}
	if !strings.Contains(v.Fix, "重拍第 3 镜") {
		t.Errorf("修复建议解析错误：%q", v.Fix)
	}
}

// LLM 会加粗、换冒号、写英文——解析器必须宽容，但**不能宽容到猜等级**。
func TestParseVLMVerdict_Tolerant(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"加粗", "**等级：good**", GradeGood},
		{"英文冒号", "等级: poor", GradePoor},
		{"英文键", "Grade: excellent", GradeExcellent},
		{"行首列表", "- 等级：bad", GradeBad},
		{"全角冒号+空格", "等  级 ： fair", GradeFair},
	}
	for _, c := range cases {
		v := ParseVLMVerdict(c.raw)
		if !v.ParseOK || v.Grade != c.want {
			t.Errorf("%s：应得 %s，实际 grade=%q ok=%v", c.name, c.want, v.Grade, v.ParseOK)
		}
	}
}

// 没有等级就必须 ParseOK=false —— 绝不猜默认值（猜成 good 等于把门关掉）。
func TestParseVLMVerdict_NoGrade(t *testing.T) {
	for _, raw := range []string{
		"",
		"画面看起来不错，没有问题。",
		"1) 证据：\n- [帧1] 一个人站着\n",
		"等级：还行", // 不在五档内
	} {
		v := ParseVLMVerdict(raw)
		if v.ParseOK || v.Grade != "" {
			t.Errorf("无合法等级时应 ParseOK=false，实际 grade=%q ok=%v（输入 %q）", v.Grade, v.ParseOK, raw)
		}
	}
}

// 门映射：这是整套协议的落点，四条分支都必须钉住。
func TestVLMVerdict_GateCheckMapping(t *testing.T) {
	cases := []struct {
		grade     string
		wantOK    bool
		wantLevel string
	}{
		{GradeExcellent, true, "warn"},
		{GradeGood, true, "warn"},
		{GradeFair, false, "warn"},
		{GradePoor, false, "fail"},
		{GradeBad, false, "fail"},
	}
	for _, c := range cases {
		got := VLMVerdict{Grade: c.grade, ParseOK: true}.GateCheck("vlm_content")
		if got.OK != c.wantOK || got.Level != c.wantLevel {
			t.Errorf("等级 %s 应映射 ok=%v level=%s，实际 ok=%v level=%s",
				c.grade, c.wantOK, c.wantLevel, got.OK, got.Level)
		}
		if got.Name != "vlm_content" {
			t.Errorf("检查项名应透传，实际 %q", got.Name)
		}
	}

	// 解析失败：不通过 + 明说不可解析（不能静默当通过）
	bad := VLMVerdict{}.GateCheck("vlm_content")
	if bad.OK {
		t.Error("解析失败不能算通过")
	}
	if !strings.Contains(bad.Detail, "不可解析") {
		t.Errorf("解析失败的说明必须点明原因，实际：%q", bad.Detail)
	}
	if bad.Fix == "" {
		t.Error("解析失败也必须给修复动作（检查模板/换模型）")
	}
}

func TestVLMGradeRank(t *testing.T) {
	if VLMGradeRank(GradeBad) >= VLMGradeRank(GradeGood) {
		t.Error("bad 应排在 good 之前（序号更小）")
	}
	if VLMGradeRank("") != -1 {
		t.Error("未识别等级应返回 -1（聚合时排除，而不是当最差）")
	}
	if VLMGradeRank("EXCELLENT") != 4 {
		t.Error("等级比较应大小写不敏感")
	}
}
