package service

import (
	"strings"
	"testing"

	"flowedge/backend/model"
)

// 文本梯度的测试重点不是"能不能解析 JSON"，而是**模型说了不算**：
// from 由引擎校正、改判据的编辑被摘掉、触到防作弊线的整条变体被拒。

func variantGraph() []model.NodeInstance {
	return []model.NodeInstance{
		cnode("pp1", "post_production", map[string]interface{}{
			"loudness": float64(-16), "max_silence_gap": float64(2.0),
		}),
		cnode("qg1", "quality_gate", map[string]interface{}{
			"loudness_min_db": float64(-20), "min_frames": float64(124),
		}),
		cnode("e1", "end", map[string]interface{}{}),
	}
}

func TestBuildVariantPrompt_HidesGateJudgements(t *testing.T) {
	p := BuildVariantPrompt(VariantRequest{
		Nodes: variantGraph(),
		Attrs: []Attribution{{
			GateCheck: "loudness_min", NodeID: "qg1", Failed: 3, Total: 4, LossShare: 1,
			Knobs: TrainableSurface(variantGraph()),
		}},
	})
	// 「可调面」一节是模型照抄的候选池：里面不许出现门上的判据，
	// 否则等于把作弊选项摆到它面前（真正的防线仍是 FilterGateJudgementEdits + 审计）。
	head, tail, ok := strings.Cut(p, "## 可调面")
	if !ok {
		t.Fatal("提示词缺少可调面一节")
	}
	surfacePart := tail
	if i := strings.Index(tail, "## 门在漏什么"); i >= 0 {
		surfacePart = tail[:i]
	}
	_ = head
	if strings.Contains(surfacePart, "loudness_min_db") || strings.Contains(surfacePart, "min_frames") {
		t.Fatalf("可调面里出现了门判据：\n%s", surfacePart)
	}
	if !strings.Contains(p, "pp1") || !strings.Contains(p, "loudness") {
		t.Fatal("产出侧参数必须列进可调面，否则模型无从下手")
	}
}

func TestParseVariants_HandlesCodeFence(t *testing.T) {
	raw := "好的，以下是变体：\n```json\n{\"variants\":[{\"title\":\"抬响度\",\"hypothesis\":\"h\",\"edits\":[{\"nodeId\":\"pp1\",\"key\":\"loudness\",\"to\":-14,\"why\":\"对齐 -14 LUFS\"}]}]}\n```"
	vs, err := ParseVariants(raw)
	if err != nil {
		t.Fatalf("带代码围栏应当能解析：%v", err)
	}
	if len(vs) != 1 || vs[0].Edits[0].NodeID != "pp1" {
		t.Fatalf("解析结果不对：%+v", vs)
	}
}

func TestParseVariants_EmptyListIsNotSuccess(t *testing.T) {
	// 「模型认为不该改」与「模型没听懂」是两件不同的事，都不能当成"无需改动"
	if _, err := ParseVariants(`{"variants":[]}`); err == nil {
		t.Fatal("空 variants 必须报错，不许冒充「无需改动」")
	}
	if _, err := ParseVariants("抱歉，我无法完成"); err == nil {
		t.Fatal("没有 JSON 时必须报错")
	}
}

func TestProposeTextualVariants_CorrectsFromAndStripsJudgement(t *testing.T) {
	llm := func(prompt string) (string, error) {
		return `{"variants":[
			{"title":"抬响度","hypothesis":"响度不够","edits":[
				{"nodeId":"pp1","key":"loudness","to":-14,"why":"对齐 -14 LUFS"},
				{"nodeId":"qg1","key":"loudness_min_db","to":-40,"why":"放宽验收"}]},
			{"title":"只改判据","hypothesis":"作弊","edits":[
				{"nodeId":"qg1","key":"min_frames","to":1,"why":"降门槛"}]}
		]}`, nil
	}
	vs, err := ProposeTextualVariants(VariantRequest{Nodes: variantGraph()}, llm)
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	if len(vs) != 2 {
		t.Fatalf("应得到 2 个变体，实得 %d", len(vs))
	}

	first := vs[0]
	if first.Verdict != "accepted" {
		t.Fatalf("摘掉判据编辑后应变 accepted，实得 %s（%s）", first.Verdict, first.Reason)
	}
	if len(first.Stripped) != 1 {
		t.Fatalf("应摘掉 1 条判据编辑，实得 %v", first.Stripped)
	}
	if len(first.EditsAsCandidate) != 1 || first.EditsAsCandidate[0].NodeID != "pp1" {
		t.Fatalf("候选里只应剩产出侧编辑，实得 %+v", first.EditsAsCandidate)
	}
	// from 必须是图上的真实值（-16），不是模型说的（模型没给 from 也要补上）
	if got, _ := first.EditsAsCandidate[0].From.(float64); got != -16 {
		t.Fatalf("from 应由引擎从图上读为 -16，实得 %v", first.EditsAsCandidate[0].From)
	}

	second := vs[1]
	if second.Verdict != "rejected" {
		t.Fatalf("只改判据的变体必须整条拒绝，实得 %s", second.Verdict)
	}
	if !strings.Contains(second.Reason, "改评分标准") {
		t.Fatalf("拒绝理由要说清是改评分标准，实得 %s", second.Reason)
	}
}

func TestProposeTextualVariants_RejectsUnknownParam(t *testing.T) {
	llm := func(prompt string) (string, error) {
		return `{"variants":[{"title":"加个新参数","hypothesis":"x","edits":[
			{"nodeId":"pp1","key":"magic_gain","to":3,"why":"灵感"}]}]}`, nil
	}
	vs, err := ProposeTextualVariants(VariantRequest{Nodes: variantGraph()}, llm)
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	if vs[0].Verdict != "rejected" {
		t.Fatalf("图上没有的参数不许凭空加，实得 %s", vs[0].Verdict)
	}
}

func TestProposeTextualVariants_NilLLMIsError(t *testing.T) {
	// 不许退化成「返回空清单」
	if _, err := ProposeTextualVariants(VariantRequest{Nodes: variantGraph()}, nil); err == nil {
		t.Fatal("模型不可用必须报错，不能返回空变体清单")
	}
}

// 键名容错：模型常把节点类型当成键名的一部分（"post_production.loudness"）。
// 容错的是**写法**，不是"图上没有的参数也能加"——剥完仍必须真实存在。
func TestProposeTextualVariants_ToleratesPrefixedKey(t *testing.T) {
	llm := func(prompt string) (string, error) {
		return `{"variants":[{"title":"抬响度","hypothesis":"h","edits":[
			{"nodeId":"pp1","key":"post_production.loudness","to":-14,"why":"对齐 -14 LUFS"}]}]}`, nil
	}
	vs, err := ProposeTextualVariants(VariantRequest{Nodes: variantGraph()}, llm)
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	if vs[0].Verdict != "accepted" {
		t.Fatalf("带前缀的键应被归一后接受，实得 %s（%s）", vs[0].Verdict, vs[0].Reason)
	}
	if got := vs[0].EditsAsCandidate[0].Path; got != "data.loudness" {
		t.Fatalf("路径应归一为 data.loudness，实得 %s", got)
	}
	if vs[0].Edits[0].Key != "loudness" {
		t.Fatalf("回显的键名也应归一，实得 %s", vs[0].Edits[0].Key)
	}
}
