package service

import (
	"strings"
	"testing"
	"unicode/utf8"

	"flowedge/backend/model"
)

// 文本拆分是 compose 标定里唯一剩下的失败点：模型编造不存在的 `split_text`。
// 下面盯的是"切得对不对"，尤其是中文——用 ASCII 的 .!? 切会把整段当成一句。

func splitNode(data map[string]interface{}) *model.NodeInstance {
	if data == nil {
		data = map[string]interface{}{}
	}
	return &model.NodeInstance{ID: "sp", Type: "text_split", Data: data}
}

func runSplit(t *testing.T, data map[string]interface{}, upstream map[string]interface{}) map[string]interface{} {
	t.Helper()
	out, err := (&EngineV2{}).execTextSplitNode(splitNode(data), upstream)
	if err != nil {
		t.Fatalf("拆分失败：%v", err)
	}
	return out.(map[string]interface{})
}

func partsOf(out map[string]interface{}) []string {
	list, _ := out["parts"].([]interface{})
	res := make([]string, 0, len(list))
	for _, v := range list {
		res = append(res, v.(string))
	}
	return res
}

// 中文句号必须被认成句尾。用 ASCII 的 .!? 判会把整段当成一句 —— 这是最常见的切分错误。
func TestTextSplit_ChineseSentence(t *testing.T) {
	out := runSplit(t, map[string]interface{}{
		"mode": "by_sentence",
		"text": "第一句。第二句！第三句？最后一句",
	}, nil)
	got := partsOf(out)
	if len(got) != 4 {
		t.Fatalf("应切出 4 句，实际 %d：%v", len(got), got)
	}
	// 标点必须留在句内：TTS 靠它判停顿，扔掉会让每段都像被突然掐断
	for _, want := range []string{"第一句。", "第二句！", "第三句？"} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("句末标点应保留，缺少 %q：%v", want, got)
		}
	}
}

// "…！" "？!" 这类连续标点算同一个句尾，不能切成两句
func TestTextSplit_ConsecutivePunctuationStaysTogether(t *testing.T) {
	got := partsOf(runSplit(t, map[string]interface{}{
		"mode": "by_sentence",
		"text": "怎么会这样……！我不信。",
	}, nil))
	if len(got) != 2 {
		t.Fatalf("连续标点应算同一句尾，实际 %d 段：%v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "怎么会这样") || !strings.Contains(got[0], "！") {
		t.Fatalf("第一段应把「……！」都带上：%q", got[0])
	}
}

// 右引号 / 右括号紧跟在句号后，属于本句
func TestTextSplit_ClosingQuoteStaysInSentence(t *testing.T) {
	got := partsOf(runSplit(t, map[string]interface{}{
		"mode": "by_sentence",
		"text": "他说：「走吧。」然后转身。",
	}, nil))
	if len(got) != 2 {
		t.Fatalf("右引号应在句内，实际 %d 段：%v", len(got), got)
	}
	if !strings.Contains(got[0], "」") {
		t.Fatalf("第一段应含右引号：%q", got[0])
	}
}

// 按空行切段；没有空行时退化为按行（剧本常见"一句一行"）
func TestTextSplit_ParagraphThenLine(t *testing.T) {
	got := partsOf(runSplit(t, map[string]interface{}{
		"mode": "by_paragraph",
		"text": "第一段第一句。第一段第二句。\n\n第二段。\n\n第三段。",
	}, nil))
	if len(got) != 3 {
		t.Fatalf("空行应切出 3 段，实际 %d：%v", len(got), got)
	}

	got = partsOf(runSplit(t, map[string]interface{}{
		"mode": "by_paragraph",
		"text": "第一行\n第二行\n第三行",
	}, nil))
	if len(got) != 3 {
		t.Fatalf("无空行时应按行切出 3 段，实际 %d：%v", len(got), got)
	}
}

// 合并成 N 段：切点必须落在句末，且尽量均匀
func TestTextSplit_ByCountBalancedAtSentenceEnds(t *testing.T) {
	text := "甲甲甲甲甲甲甲甲甲甲。乙乙乙乙乙乙乙乙乙乙。丙丙丙丙丙丙丙丙丙丙。丁丁丁丁丁丁丁丁丁丁。戊戊戊戊戊戊戊戊戊戊。己己己己己己己己己己。"
	out := runSplit(t, map[string]interface{}{"mode": "by_count", "count": 3, "text": text}, nil)
	got := partsOf(out)
	if len(got) != 3 {
		t.Fatalf("应切成 3 段，实际 %d：%v", len(got), got)
	}
	for i, g := range got {
		if !strings.HasSuffix(g, "。") {
			t.Fatalf("第 %d 段应以句号结尾（切点不能在句中间）：%q", i+1, g)
		}
	}
	// 六句分三段 → 每段两句，字数应完全均衡
	for i, g := range got {
		if n := utf8.RuneCountInString(g); n != 22 {
			t.Fatalf("第 %d 段应 2 句共 22 字，实际 %d：%q", i+1, n, g)
		}
	}
	// 不能丢字
	var joined strings.Builder
	for _, g := range got {
		joined.WriteString(g)
	}
	if joined.String() != text {
		t.Fatalf("拆分后拼回应与原文完全一致（不许丢字）")
	}
}

// 要的段数比句子还多 → 按句子数给，不硬造空段
func TestTextSplit_CountMoreThanSentences(t *testing.T) {
	got := partsOf(runSplit(t, map[string]interface{}{
		"mode": "by_count", "count": 5, "text": "只有一句。",
	}, nil))
	if len(got) != 1 {
		t.Fatalf("只有一句时应返回 1 段，实际 %d：%v", len(got), got)
	}
}

// 每段字数上限
func TestTextSplit_ByLength(t *testing.T) {
	text := "一二三四五。六七八九十。十一十二十三。十四十五十六。"
	got := partsOf(runSplit(t, map[string]interface{}{
		"mode": "by_length", "max_chars": 12, "text": text,
	}, nil))
	if len(got) < 2 {
		t.Fatalf("应被切成多段，实际 %d：%v", len(got), got)
	}
	for i, g := range got {
		// 单句超长时允许超（不硬切词），但这里每句 6 字，都在限内
		if n := utf8.RuneCountInString(g); n > 12 {
			t.Fatalf("第 %d 段 %d 字超过上限 12：%q", i+1, n, g)
		}
	}
}

// 空文本必须当场失败，不能返回空数组往下跑
// （下游拿到空文本会：TTS 合成 0 秒 → 成片某段没声音，而根因在这里）
func TestTextSplit_EmptyFailsLoudly(t *testing.T) {
	_, err := (&EngineV2{}).execTextSplitNode(splitNode(map[string]interface{}{"text": "   "}), nil)
	if err == nil {
		t.Fatal("空文本应报错")
	}
	if !strings.Contains(err.Error(), "为空") {
		t.Fatalf("错误要说明是文本为空：%v", err)
	}
}

// 上游给的文本也能拆（参数留空时）
func TestTextSplit_UsesUpstreamText(t *testing.T) {
	out := runSplit(t, map[string]interface{}{"mode": "by_sentence"},
		map[string]interface{}{"prev": map[string]interface{}{"text": "上游第一句。上游第二句。"}})
	if n := len(partsOf(out)); n != 2 {
		t.Fatalf("应拆上游文本为 2 段，实际 %d", n)
	}
}

// 非法 mode 要列出可用值，而不是静默退回默认
func TestTextSplit_BadModeRejected(t *testing.T) {
	_, err := (&EngineV2{}).execTextSplitNode(splitNode(map[string]interface{}{
		"mode": "by_magic", "text": "一句话。",
	}), nil)
	if err == nil || !strings.Contains(err.Error(), "by_sentence") {
		t.Fatalf("非法 mode 应报错并列出可用值：%v", err)
	}
}

// parts 与 text 必须一致（text === join(parts, separator)），否则下游接哪个端口结果不同
func TestTextSplit_TextIsJoinOfParts(t *testing.T) {
	out := runSplit(t, map[string]interface{}{
		"mode": "by_sentence", "separator": "|",
		"text": "甲。乙。丙。",
	}, nil)
	got := partsOf(out)
	if want := strings.Join(got, "|"); out["text"] != want {
		t.Fatalf("text 应是 parts 用分隔符拼起来：%q != %q", out["text"], want)
	}
	if out["count"].(int) != len(got) {
		t.Fatalf("count 与 parts 长度不一致")
	}
}

// 超长文本要合并而不是撑爆 parts（但不许丢内容）
func TestTextSplit_OverflowMergedNotDropped(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 260; i++ {
		b.WriteString("句")
		b.WriteString("。")
	}
	text := b.String()
	out := runSplit(t, map[string]interface{}{"mode": "by_sentence", "text": text}, nil)
	got := partsOf(out)
	if len(got) > 200 {
		t.Fatalf("parts 应被限制在 200 段内，实际 %d", len(got))
	}
	var joined strings.Builder
	for _, g := range got {
		joined.WriteString(g)
	}
	if joined.String() != text {
		t.Fatal("合并溢出段时不许丢内容")
	}
	if out["merged_overflow"] == nil {
		t.Fatal("发生过合并时应标出，别让人以为本来就是这么多段")
	}
}

// 契约：不吃显存、不产媒体、三个输出都非必填
func TestTextSplitContract(t *testing.T) {
	c, ok := ModuleContractOf("text_split")
	if !ok {
		t.Fatal("text_split 没有契约")
	}
	if c.Role != RoleTransform {
		t.Fatalf("角色应为 transform，实际 %v", c.Role)
	}
	// 注意：**不断言输出端口的 Required**。Port.Required 的语义是「必须有的来源」，
	// 那是输入端口的属性；现有契约里输出端口 80 处写 true、33 处写 false，
	// 没有统一含义。对着它断言等于虚构一条不存在的规则（第一版就这么写错过一次）。
	// 有意义的断言是输入端：它必须声明"文本必须来自某处"。
	if len(c.Inputs) != 1 || !c.Inputs[0].Required || c.Inputs[0].Kind != PortText {
		t.Fatalf("应有一个必填的 text 输入：%+v", c.Inputs)
	}
	if len(c.Outputs) != 3 {
		t.Fatalf("应有 text/parts/count 三个输出，实际 %d", len(c.Outputs))
	}
	spec := NodeSpecs["text_split"]
	if spec.Side != "" || spec.Heavy || spec.Media {
		t.Fatalf("文本拆分不该吃显存/算重节点/进媒体血缘：%+v", spec)
	}
}
