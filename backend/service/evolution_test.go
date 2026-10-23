package service

import (
	"strings"
	"testing"

	"flowedge/backend/model"
)

// 自进化内核的守卫测试。
//
// 重点不是"能不能跑通"，而是**四条防作弊与防自嗨的硬规则**有没有牙齿：
// 改判据必须被拦、样本不够必须不产结论、验收口径缩小必须拒绝、fail 级回退必须一票否决。
// 这四条一旦失效，自进化会稳定收敛到"最松的门 + 最少的样本"。

func strPtr(s string) *string { return &s }

func f64Ptr(v float64) *float64 { return &v }

// evoNode 造一个节点
func evoNode(id, typ string, data map[string]interface{}) model.NodeInstance {
	return model.NodeInstance{ID: id, Type: typ, Data: data}
}

// evoFixture 一张最小可判图：一个产出节点 + 一个门节点
func evoFixture() []model.NodeInstance {
	return []model.NodeInstance{
		evoNode("pp1", "post_production", map[string]interface{}{
			"loudness": float64(-18), "bgm_gain": float64(0.2), "aspect": "9:16",
		}),
		evoNode("qg1", "quality_gate", map[string]interface{}{
			"min_frames": float64(124), "loudness_min_db": float64(-20),
			"require_audio": true,
		}),
	}
}

// TestTrainableSurface_GateJudgementVsProduceParam 判据与产出参数必须分开
//
// gateParamKeys 是一张扁平表，loudness_min_db（门上的判据）和
// loudness（后期上的归一目标）都命中 "loudness"，但只有前者不能调。
// 混为一谈会把唯一有标准答案的改进（响度归一到 -14）自己拦掉。
func TestTrainableSurface_GateJudgementVsProduceParam(t *testing.T) {
	knobs := TrainableSurface(evoFixture())
	byKey := map[string]Knob{}
	for _, k := range knobs {
		byKey[k.NodeID+"."+k.Key] = k
	}

	cases := []struct {
		key        string
		gateLocked bool
		gateRel    bool
		why        string
	}{
		{"qg1.min_frames", true, false, "门节点上的判据：改了等于改评分标准"},
		{"qg1.loudness_min_db", true, false, "同上"},
		{"pp1.loudness", false, true, "产出节点上的归一目标：允许调，但要留名"},
		{"pp1.bgm_gain", false, false, "普通参数"},
		{"pp1.aspect", false, false, "枚举参数"},
	}
	for _, c := range cases {
		k, ok := byKey[c.key]
		if !ok {
			t.Fatalf("缺旋钮 %s", c.key)
		}
		if k.GateLocked != c.gateLocked || k.GateRelated != c.gateRel {
			t.Errorf("%s（%s）：GateLocked=%v GateRelated=%v，期望 %v/%v",
				c.key, c.why, k.GateLocked, k.GateRelated, c.gateLocked, c.gateRel)
		}
	}
	if len(knobs) != 6 {
		t.Errorf("应有 6 个旋钮（pp 3 + qg 3），实得 %d", len(knobs))
	}
}

// TestAuditVariant_HacksAreRejected 四类改判据行为必须被拦
func TestAuditVariant_HacksAreRejected(t *testing.T) {
	base := evoFixture()

	cases := []struct {
		name string
		mut  func([]model.NodeInstance) []model.NodeInstance
		want string
	}{
		{
			name: "放宽门阈值",
			mut: func(n []model.NodeInstance) []model.NodeInstance {
				n[1].Data["min_frames"] = float64(60)
				return n
			},
			want: "改评分标准",
		},
		{
			name: "删门节点",
			mut: func(n []model.NodeInstance) []model.NodeInstance {
				return n[:1]
			},
			want: "门节点",
		},
		{
			name: "删判据键",
			mut: func(n []model.NodeInstance) []model.NodeInstance {
				delete(n[1].Data, "require_audio")
				return n
			},
			want: "拆门",
		},
		{
			name: "关掉门开关",
			mut: func(n []model.NodeInstance) []model.NodeInstance {
				n[1].Data["require_audio"] = false
				return n
			},
			want: "改评分标准",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cand := c.mut(cloneNodes(base))
			hacks, _ := AuditVariant(base, cand)
			if len(hacks) == 0 {
				t.Fatalf("%s 没被拦下来", c.name)
			}
			joined := strings.Join(hacks, " | ")
			if !strings.Contains(joined, c.want) {
				t.Errorf("拦是拦了，但理由不含 %q：%s", c.want, joined)
			}
		})
	}
}

// TestAuditVariant_ProduceParamAllowed 改产出参数只给 warning，不拦
func TestAuditVariant_ProduceParamAllowed(t *testing.T) {
	base := evoFixture()
	cand := cloneNodes(base)
	cand[0].Data["loudness"] = float64(-14)
	cand[0].Data["bgm_gain"] = float64(0.3)

	hacks, warnings := AuditVariant(base, cand)
	if len(hacks) != 0 {
		t.Fatalf("改产出参数不该被拦，却报了：%v", hacks)
	}
	if len(warnings) != 1 {
		t.Fatalf("loudness 属门层词汇，应给 1 条 warning，实得 %d：%v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "loudness") {
		t.Errorf("warning 应指出具体键：%s", warnings[0])
	}
}

// TestAttributeEvidence_UnscoredIsNotFailure unscored 不进归因分母
//
// 这是三态纪律在自进化层的延续：服务没起导致"没测到"，
// 若算进失败率，引擎会去修一个不存在的质量问题。
func TestAttributeEvidence_UnscoredIsNotFailure(t *testing.T) {
	nodes := evoFixture()
	evs := []Evidence{
		{Result: EvidenceFailed, GateChecks: []GateCheckEvidence{
			{Name: "响度", OK: false, Level: "fail", NodeID: "qg1"},
			{Name: "帧数", OK: true, Level: "fail", NodeID: "qg1"},
		}},
		{Result: EvidencePassed, GateChecks: []GateCheckEvidence{
			{Name: "响度", OK: true, Level: "fail", NodeID: "qg1"},
			{Name: "帧数", OK: true, Level: "fail", NodeID: "qg1"},
		}},
		// 两条没测到：不能算响度失败，也不能算通过
		{Result: EvidenceUnscored, UnscoredReason: "execution_error"},
		{Result: EvidenceUnscored, UnscoredReason: "gate_error"},
	}

	attrs := AttributeEvidence(evs, nodes)
	if len(attrs) == 0 {
		t.Fatal("应有归因结果")
	}
	var loud *Attribution
	for i := range attrs {
		if attrs[i].GateCheck == "响度" {
			loud = &attrs[i]
		}
	}
	if loud == nil {
		t.Fatal("缺响度归因")
	}
	if loud.Total != 2 {
		t.Errorf("分母应只含真正跑过门的 2 条，实得 %d", loud.Total)
	}
	if loud.Failed != 1 || loud.FailRate != 0.5 {
		t.Errorf("失败率应为 1/2，实得 %d/%d", loud.Failed, loud.Total)
	}
	if loud.Verdict != "param_tunable" {
		t.Errorf("响度有标准值（-14 LUFS）且图上有 post_production.loudness，应判 param_tunable，实得 %q（%s）",
			loud.Verdict, loud.Reason)
	}
	if loud.NodeID != "qg1" {
		t.Errorf("归因应指到具体门节点 qg1，实得 %q", loud.NodeID)
	}
	found := false
	for _, k := range loud.Knobs {
		if k.NodeID == "pp1" && k.Key == "loudness" {
			found = true
		}
	}
	if !found {
		t.Errorf("应指出可调的 pp1.loudness 旋钮，实得 %+v", loud.Knobs)
	}
}

// TestAttributeEvidence_UnscoredMajorityBlocksAttribution unmeasured 过半时不归因
func TestAttributeEvidence_UnscoredMajorityBlocksAttribution(t *testing.T) {
	evs := []Evidence{
		{Result: EvidenceFailed, GateChecks: []GateCheckEvidence{{Name: "响度", OK: false, Level: "fail"}}},
		{Result: EvidenceUnscored}, {Result: EvidenceUnscored}, {Result: EvidenceUnscored},
	}
	attrs := AttributeEvidence(evs, evoFixture())
	if len(attrs) != 1 || attrs[0].Verdict != "unmeasured" {
		t.Fatalf("unscored 占 3/4 时应判 unmeasured，实得 %+v", attrs)
	}
	if !strings.Contains(attrs[0].Reason, "不可信") {
		t.Errorf("理由要说清为什么不可信：%s", attrs[0].Reason)
	}
}

// TestAttributeEvidence_NoGateAtAll 没接门时必须说清楚，不能返回空
func TestAttributeEvidence_NoGateAtAll(t *testing.T) {
	evs := []Evidence{{Result: EvidenceUnscored, UnscoredReason: "no_gate"}}
	attrs := AttributeEvidence(evs, evoFixture())
	if len(attrs) != 1 || attrs[0].Verdict != "unmeasured" {
		t.Fatalf("没接门应明确报 unmeasured，实得 %+v", attrs)
	}
	if !strings.Contains(attrs[0].Reason, "先把门接上") {
		t.Errorf("要给出下一步：%s", attrs[0].Reason)
	}
}

// TestInspectFuel_BlocksWhenNotEnoughData 样本不够时不许开始
func TestInspectFuel_BlocksWhenNotEnoughData(t *testing.T) {
	p := DefaultEvoPolicy()

	// 一条都没跑过门
	fs := InspectFuel([]Evidence{{Result: EvidenceUnscored, UnscoredReason: "no_gate"}}, p)
	if fs.Ready {
		t.Fatal("没有跑过门的数据却判 ready")
	}
	if len(fs.Blockers) < 2 {
		t.Errorf("应同时报「没跑过门」和「配对不足」，实得 %v", fs.Blockers)
	}

	// 10 条配对 case，两类别 → 应该 ready
	var evs []Evidence
	for i := 0; i < 10; i++ {
		id := string(rune('a' + i))
		evs = append(evs,
			Evidence{Result: EvidencePassed, CaseID: strPtr(id), Category: strPtr("short"), Role: "baseline"},
			Evidence{Result: EvidencePassed, CaseID: strPtr(id), Category: strPtr("long"), Role: "candidate"},
		)
	}
	fs = InspectFuel(evs, p)
	if !fs.Ready {
		t.Fatalf("10 对配对 + 2 类别应 ready，blockers=%v", fs.Blockers)
	}
	if fs.PairableCases != 10 {
		t.Errorf("可配对 case 应为 10，实得 %d", fs.PairableCases)
	}

	// unscored 超过 20% → 不 ready
	evs = append(evs, Evidence{Result: EvidenceUnscored}, Evidence{Result: EvidenceUnscored},
		Evidence{Result: EvidenceUnscored}, Evidence{Result: EvidenceUnscored},
		Evidence{Result: EvidenceUnscored}, Evidence{Result: EvidenceUnscored})
	if InspectFuel(evs, p).Ready {
		t.Fatal("unscored 占 6/26≈23%（>20%）仍判 ready")
	}
}

// evoPair 造一对配对证据
func evoPair(caseID string, baseScore, candScore float64, baseTok, candTok int) (Evidence, Evidence) {
	return Evidence{
			Result: EvidencePassed, CaseID: strPtr(caseID), Role: "baseline",
			GateScore: f64Ptr(baseScore), EffectiveTokens: baseTok, DurationMS: 1000,
			GateChecks: []GateCheckEvidence{{Name: "响度", OK: true, Level: "fail"}},
		}, Evidence{
			Result: EvidencePassed, CaseID: strPtr(caseID), Role: "candidate",
			GateScore: f64Ptr(candScore), EffectiveTokens: candTok, DurationMS: 1000,
			GateChecks: []GateCheckEvidence{{Name: "响度", OK: true, Level: "fail"}},
		}
}

// TestDecide_InconclusiveWhenTooFewPairs 样本不足只出 inconclusive
func TestDecide_InconclusiveWhenTooFewPairs(t *testing.T) {
	var base, cand []Evidence
	for i := 0; i < 3; i++ {
		b, c := evoPair(string(rune('a'+i)), 0.5, 0.9, 100, 100)
		base, cand = append(base, b), append(cand, c)
	}
	d := Decide(base, cand, DefaultEvoPolicy())
	if d.Verdict != "inconclusive" {
		t.Fatalf("3 对（要 8）应 inconclusive，实得 %s：%v", d.Verdict, d.Reasons)
	}
	if !strings.Contains(strings.Join(d.Reasons, " "), "样本不足") {
		t.Errorf("理由要说明是样本问题：%v", d.Reasons)
	}
}

// TestDecide_PromoteOnRealImprovement 真提升才 promote
func TestDecide_PromoteOnRealImprovement(t *testing.T) {
	var base, cand []Evidence
	for i := 0; i < 10; i++ {
		b, c := evoPair(string(rune('a'+i)), 0.6, 0.9, 1000, 950)
		base, cand = append(base, b), append(cand, c)
	}
	d := Decide(base, cand, DefaultEvoPolicy())
	if d.Verdict != "promote" {
		t.Fatalf("门分 +0.3、token -5%%、10 胜 0 负应 promote，实得 %s：%v", d.Verdict, d.Reasons)
	}
	if d.Better != 10 || d.Worse != 0 {
		t.Errorf("配对统计错：%d 胜 %d 负", d.Better, d.Worse)
	}
	if d.GateScoreDelta < 0.29 || d.GateScoreDelta > 0.31 {
		t.Errorf("门分差应约 0.3，实得 %.3f", d.GateScoreDelta)
	}
}

// TestDecide_TokenRegressionBlocksToken 涨了不许晋升
func TestDecide_TokenRegressionBlocksToken(t *testing.T) {
	var base, cand []Evidence
	for i := 0; i < 10; i++ {
		b, c := evoPair(string(rune('a'+i)), 0.6, 0.9, 1000, 1200)
		base, cand = append(base, b), append(cand, c)
	}
	d := Decide(base, cand, DefaultEvoPolicy())
	if d.Verdict != "keep_baseline" {
		t.Fatalf("token +20%% 应 keep_baseline，实得 %s", d.Verdict)
	}
	if !strings.Contains(strings.Join(d.Reasons, " "), "token") {
		t.Errorf("理由要点名 token：%v", d.Reasons)
	}
}

// TestDecide_ScopeReducedIsRejected 验收口径缩小一票否决
func TestDecide_ScopeReducedIsRejected(t *testing.T) {
	var base, cand []Evidence
	for i := 0; i < 10; i++ {
		b, c := evoPair(string(rune('a'+i)), 0.6, 0.9, 1000, 1000)
		// 候选侧少了 2 条检查：分数当然涨了
		c.GateChecks = []GateCheckEvidence{{Name: "响度", OK: true, Level: "fail"}}
		b.GateChecks = []GateCheckEvidence{
			{Name: "响度", OK: true, Level: "fail"},
			{Name: "帧数", OK: false, Level: "fail"},
			{Name: "音轨", OK: false, Level: "fail"},
		}
		base, cand = append(base, b), append(cand, c)
	}
	d := Decide(base, cand, DefaultEvoPolicy())
	if d.Verdict != "keep_baseline" {
		t.Fatalf("检查项变少应 keep_baseline，实得 %s", d.Verdict)
	}
	if !d.ScopeReduced {
		t.Error("ScopeReduced 标记没置位")
	}
}

// TestDecide_CriticalRegressionVetoes 基线过、候选不过一票否决
func TestDecide_CriticalRegressionVetoes(t *testing.T) {
	var base, cand []Evidence
	for i := 0; i < 10; i++ {
		b, c := evoPair(string(rune('a'+i)), 0.6, 0.9, 1000, 1000)
		if i == 0 {
			c.GateChecks = []GateCheckEvidence{
				{Name: "响度", OK: true, Level: "fail"},
				{Name: "人声", OK: false, Level: "fail"}, // 基线这过，候选不过
			}
			b.GateChecks = []GateCheckEvidence{
				{Name: "响度", OK: true, Level: "fail"},
				{Name: "人声", OK: true, Level: "fail"},
			}
		}
		base, cand = append(base, b), append(cand, c)
	}
	d := Decide(base, cand, DefaultEvoPolicy())
	if d.Verdict != "keep_baseline" {
		t.Fatalf("有 fail 级回退应 keep_baseline，实得 %s", d.Verdict)
	}
	if d.CriticalRegressions != 1 {
		t.Errorf("应记 1 条 critical regression，实得 %d", d.CriticalRegressions)
	}
}

// TestDecide_MajorityLossRejected 均分涨但赢少输多不算改善
func TestDecide_MajorityLossRejected(t *testing.T) {
	var base, cand []Evidence
	for i := 0; i < 12; i++ {
		bs, cs := 0.9, 0.8
		if i < 4 {
			// 4 个 case 大涨，8 个 case 小跌：均值仍可能上涨
			bs, cs = 0.2, 1.0
		}
		b, c := evoPair(string(rune('a'+i)), bs, cs, 1000, 1000)
		base, cand = append(base, b), append(cand, c)
	}
	d := Decide(base, cand, DefaultEvoPolicy())
	if d.Better >= d.Worse {
		t.Fatalf("构造失败：应赢少输多，实得 %d 胜 %d 负", d.Better, d.Worse)
	}
	if d.Verdict == "promote" {
		t.Fatalf("赢少输多仍判 promote：%v", d.Reasons)
	}
}

// TestDecide_NullGateScoreExcluded 一侧没测到的 case 整对作废
func TestDecide_NullGateScoreExcluded(t *testing.T) {
	var base, cand []Evidence
	for i := 0; i < 8; i++ {
		b, c := evoPair(string(rune('a'+i)), 0.6, 0.9, 1000, 1000)
		base, cand = append(base, b), append(cand, c)
	}
	// 把 4 条的候选侧改成"没测到"
	for i := 0; i < 4; i++ {
		cand[i].GateScore = nil
		cand[i].Result = EvidenceUnscored
	}
	d := Decide(base, cand, DefaultEvoPolicy())
	if d.MatchedCases != 4 {
		t.Fatalf("应剩 4 个能配上的 case，实得 %d", d.MatchedCases)
	}
	if d.Pairs != 0 {
		t.Errorf("两侧门分都不全，有效对应为 0，实得 %d", d.Pairs)
	}
	if d.Verdict != "inconclusive" {
		t.Errorf("有效对只剩 4 对（要 8）应 inconclusive，实得 %s", d.Verdict)
	}
}

// TestProjectionFor 会改渲染成本的参数必须真跑
func TestProjectionFor(t *testing.T) {
	nodes := evoFixture()
	cases := []struct {
		path string
		want string
	}{
		{"data.steps", ProjectionReal},
		{"data.seconds", ProjectionReal},
		{"data.loudness", ProjectionDry},
		{"data.aspect", ProjectionDry},
		{"data.prompt", ProjectionDry},
		{"data.model", ProjectionReal},
	}
	for _, c := range cases {
		got := ProjectionFor(nodes, []SpecEdit{{NodeID: "pp1", Path: c.path}})
		if got != c.want {
			t.Errorf("%s 应判 %s，实得 %s", c.path, c.want, got)
		}
	}
}

// cloneNodes 深拷贝节点（测试用，避免用例之间互相污染）
func cloneNodes(in []model.NodeInstance) []model.NodeInstance {
	out := make([]model.NodeInstance, len(in))
	for i := range in {
		d := make(map[string]interface{}, len(in[i].Data))
		for k, v := range in[i].Data {
			d[k] = v
		}
		out[i] = in[i]
		out[i].Data = d
	}
	return out
}
