package service

import (
	"strings"
	"testing"
)

// fakeStoryboardLLM 按调用次序吐预设回复，记录每次 prompt 供断言。
// 第 i 次调用返回 resps[i]；越界返回空串。
type fakeStoryboardLLM struct {
	resps []fakeResp
	calls []string
}

type fakeResp struct {
	text string
	err  error
}

func (f *fakeStoryboardLLM) ChatWithModel(prompt, model string) (string, error) {
	i := len(f.calls)
	f.calls = append(f.calls, prompt)
	if i >= len(f.resps) {
		return "", nil
	}
	return f.resps[i].text, f.resps[i].err
}

const beatPlanJSON = `{"title":"雾中信号","logline":"悬疑反转：守塔人收到三十年前自己发出的求救信号",` +
	`"style":"渤海雾夜，青蓝冷调，塔灯旋转的光斑",` +
	`"characters":[{"name":"老崔","look":"灰白短发，深蓝粗呢大衣，左腰挂一只黄铜罗盘"}],` +
	`"beats":[` +
	`{"beat":"hook","purpose":"观众立刻想知道信号是谁发的","summary":"雾夜里灯塔光束扫过，海面浮着亮红灯的旧浮标"},` +
	`{"beat":"setup","purpose":"建立守塔人与灯塔的日常","summary":"老崔在塔顶检修灯器，墙上挂着泛黄的航海图"},` +
	`{"beat":"clash","purpose":"信号逼近，威胁升级","summary":"无线电杂音里人声渐清晰，老崔僵住"},` +
	`{"beat":"turn","purpose":"翻案：信号是三十年前的他自己","summary":"磁带倒转，标签上是年轻老崔的笔迹"},` +
	`{"beat":"finish","purpose":"收束：他关掉灯，雾吞没塔","summary":"老崔拉下总闸，塔灯熄灭，雾漫进镜头"}]}`

const shotPlanJSON = `{"title":"雾中信号","style":"渤海雾夜，青蓝冷调，塔灯旋转的光斑",` +
	`"character":"老崔：灰白短发，深蓝粗呢大衣，左腰挂一只黄铜罗盘",` +
	`"ambience":"持续的雾夜海浪与风声",` +
	`"shots":[` +
	`{"id":1,"seconds":5.0,"beat":"hook","shot_size":"远景","angle":"平视","axis":"A侧",` +
	`"camera":"缓慢推近，小幅，慢速","action":"灯塔光束扫过，海面浮标红灯明灭","light":"蓝调时刻，冷调低对比",` +
	`"sfx":"浮标铃铛轻响","music":"无音乐","dialogue":"无对白","transition":"cut"},` +
	`{"id":2,"seconds":4.0,"beat":"setup","shot_size":"中景","angle":"平视","axis":"A侧",` +
	`"camera":"固定","action":"老崔检修塔顶灯器","light":"塔灯暖光，侧逆光",` +
	`"sfx":"金属扳手轻磕","music":"无音乐","dialogue":"无对白","transition":"cut"}]}`

func TestPlanShotCreatively_TwoStage(t *testing.T) {
	llm := &fakeStoryboardLLM{resps: []fakeResp{{text: beatPlanJSON}, {text: shotPlanJSON}}}
	plan, bp, err := planShotCreatively(llm, "test-model", "守塔人的悬疑故事", "", "", 30, 6)
	if err != nil {
		t.Fatalf("两段式生成失败：%v", err)
	}
	if bp == nil {
		t.Fatal("结构层产物为 nil：应当用上结构层")
	}
	if len(llm.calls) != 2 {
		t.Fatalf("应调用两次 LLM（结构层+分镜层），实际 %d 次", len(llm.calls))
	}
	// 结构层的产物必须进到分镜层的提示词里（级联：上层作下层上下文）
	second := llm.calls[1]
	for _, want := range []string{"故事一句话", "节拍表", "守塔人收到三十年前", "老崔：灰白短发"} {
		if !strings.Contains(second, want) {
			t.Errorf("分镜层提示词缺结构层产物 %q", want)
		}
	}
	if plan.Shots[0].Beat != "hook" {
		t.Errorf("首镜 beat=%q，应为 hook（分镜表要携带节拍）", plan.Shots[0].Beat)
	}
}

func TestPlanShotCreatively_UserStyleWins(t *testing.T) {
	llm := &fakeStoryboardLLM{resps: []fakeResp{{text: beatPlanJSON}, {text: shotPlanJSON}}}
	plan, _, err := planShotCreatively(llm, "m", "故事", "用户钦定的风格", "", 30, 6)
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	if plan.Style != "用户钦定的风格" {
		t.Errorf("plan.Style=%q：用户点名的风格必须原样落表，不许被模型转述漂掉", plan.Style)
	}
}

func TestPlanShotCreatively_FallbackOnStructureJunk(t *testing.T) {
	llm := &fakeStoryboardLLM{resps: []fakeResp{{text: "我无法输出 JSON"}, {text: shotPlanJSON}}}
	plan, bp, err := planShotCreatively(llm, "m", "故事", "", "", 30, 6)
	if err != nil {
		t.Fatalf("结构层坏了不应拖垮整个分镜：%v", err)
	}
	if bp != nil {
		t.Error("结构层输出是垃圾时 bp 应为 nil（回退单段式）")
	}
	if plan == nil || len(plan.Shots) != 2 {
		t.Fatal("回退后仍应产出合法分镜表")
	}
	if strings.Contains(llm.calls[1], "已确定的故事结构") {
		t.Error("回退路径的分镜层提示词不应包含故事结构块")
	}
}

func TestPlanShotCreatively_ShotLayerErrorPropagates(t *testing.T) {
	llm := &fakeStoryboardLLM{resps: []fakeResp{{text: beatPlanJSON}, {text: "完全不是 JSON"}}}
	_, _, err := planShotCreatively(llm, "m", "故事", "", "", 30, 6)
	if err == nil || !strings.Contains(err.Error(), "分镜 JSON 解析失败") {
		t.Fatalf("分镜层坏输出应报错，实际 %v", err)
	}
}

func TestParseBeatPlan(t *testing.T) {
	fenced := "```json\n" + beatPlanJSON + "\n```"
	bp, err := parseBeatPlan(fenced)
	if err != nil {
		t.Fatalf("围栏包裹的 JSON 应能解析：%v", err)
	}
	if bp.Title != "雾中信号" || len(bp.Beats) != 5 {
		t.Errorf("解析结果不对：title=%q beats=%d", bp.Title, len(bp.Beats))
	}
	if _, err := parseBeatPlan("no json here"); err == nil {
		t.Error("无 JSON 应报错")
	}
	if _, err := parseBeatPlan(`{"title":"x"}`); err == nil {
		t.Error("beats 为空应报错（没有节拍就等于没做结构层）")
	}
}

// beatTestPlan 造一个只带 beat 的最小 plan，供审计规则单测。
func beatTestPlan(beats ...string) *ShotPlan {
	shots := make([]Shot, len(beats))
	for i, b := range beats {
		shots[i] = Shot{ID: i + 1, Beat: b, Seconds: 4, Frames: 89}
	}
	return &ShotPlan{Shots: shots}
}

func findIssue(issues []ShotConsistencyIssue, code string) *ShotConsistencyIssue {
	for i := range issues {
		if issues[i].Code == code {
			return &issues[i]
		}
	}
	return nil
}

func TestAuditPlanConsistency_BeatRules(t *testing.T) {
	// ≥4 镜全无节拍 → WARN plan.beat_missing
	if got := findIssue(AuditPlanConsistency(beatTestPlan("", "", "", "")), "plan.beat_missing"); got == nil {
		t.Error("4 镜无节拍应报 plan.beat_missing")
	}
	// ≥3 镜同一节拍 → WARN plan.beat_monotone
	if got := findIssue(AuditPlanConsistency(beatTestPlan("setup", "setup", "setup")), "plan.beat_monotone"); got == nil {
		t.Error("3 镜同节拍应报 plan.beat_monotone")
	}
	// 有节拍但首镜不是钩子 → WARN plan.no_cold_open
	if got := findIssue(AuditPlanConsistency(beatTestPlan("setup", "clash", "turn")), "plan.no_cold_open"); got == nil {
		t.Error("首镜非钩子应报 plan.no_cold_open")
	}
	// 健康结构：hook 开头、多节拍 → 三条都不报
	plan := beatTestPlan("hook", "setup", "clash", "turn", "finish")
	for _, code := range []string{"plan.beat_missing", "plan.beat_monotone", "plan.no_cold_open"} {
		if got := findIssue(AuditPlanConsistency(plan), code); got != nil {
			t.Errorf("健康节拍结构不应报 %s：%s", code, got.Msg)
		}
	}
}

// TestStoryboardTemplatesContract 钉住两份模板的创意要素：
// 词表/节奏型/节拍是这套剧本链的承重墙，模板被改薄时这里先红。
func TestStoryboardTemplatesContract(t *testing.T) {
	for _, want := range []string{"shot_size：大远景", "angle：平视", "节奏型", "beat", "首镜必须是钩子", "荷兰角"} {
		if !strings.Contains(storyboardInstruction, want) {
			t.Errorf("分镜模板缺创意要素 %q", want)
		}
	}
	for _, want := range []string{"片种先行", "冷开场", "turn", "角色圣经", "logline"} {
		if !strings.Contains(storyBeatInstruction, want) {
			t.Errorf("结构层模板缺创意要素 %q", want)
		}
	}
}

// TestFilmPipelineCreativeFields 锁 JSON 契约：style/characters 字段必须 bind 到 sb-1
// （没有 bind 的字段参数收下但永远不会落到节点 data——AGENTS.md 里点过名的坑）。
func TestFilmPipelineCreativeFields(t *testing.T) {
	s := loadRealPipelines(t)
	for _, id := range []string{"film", "storyboard"} {
		p, ok := s.Get(id)
		if !ok {
			t.Fatalf("流水线 %s 不存在", id)
		}
		found := map[string]bool{}
		for _, f := range p.Fields {
			if f.Key != "style" && f.Key != "characters" {
				continue
			}
			found[f.Key] = true
			bound := false
			for _, b := range f.Bind {
				if b.Node == "sb-1" && b.Path == f.Key {
					bound = true
				}
			}
			if !bound {
				t.Errorf("%s 的字段 %s 未 bind 到 sb-1.%s", id, f.Key, f.Key)
			}
		}
		if !found["style"] || !found["characters"] {
			t.Errorf("%s 缺 style/characters 字段（found=%v）", id, found)
		}
	}
}
