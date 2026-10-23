package service

import "testing"

// 内置卡片必须开箱就有：卡片系统是空壳的话，用户点开面板什么都贴不了。
func TestBuiltinCardsExist(t *testing.T) {
	s := LoadCards(t.TempDir() + "/cards.json")
	if len(s.Agents()) == 0 {
		t.Fatal("没有内置智能体卡")
	}
	if len(s.Skills()) == 0 {
		t.Fatal("没有内置技能卡")
	}
	for _, c := range s.Agents() {
		if c.ID == "" || c.Name == "" || c.System == "" {
			t.Errorf("智能体卡字段不全：%+v", c)
		}
	}
	for _, c := range s.Skills() {
		if c.ID == "" || c.Name == "" || c.System == "" {
			t.Errorf("技能卡字段不全：%+v", c)
		}
	}
}

// 文件里的同 ID 卡片要覆盖内置，新 ID 追加——"改内置卡"比"再建一张"更符合直觉。
func TestCardsMergeOverride(t *testing.T) {
	s := &CardStore{
		agents: mergeAgents(builtinAgents, []AgentCard{
			{ID: builtinAgents[0].ID, Name: "改过的名字", System: "x"},
			{ID: "custom-agent", Name: "新卡", System: "y"},
		}),
		skills: mergeSkills(builtinSkills, nil),
	}
	if got, _ := s.Agent(builtinAgents[0].ID); got.Name != "改过的名字" {
		t.Errorf("同 ID 应覆盖内置，实际 %q", got.Name)
	}
	if _, ok := s.Agent("custom-agent"); !ok {
		t.Error("新 ID 应追加")
	}
	if len(s.Agents()) != len(builtinAgents)+1 {
		t.Errorf("追加后数量不对：%d", len(s.Agents()))
	}
}

// 绑定解析：kind 必须是三类之一，缺字段一律当没绑。
func TestBindingOf(t *testing.T) {
	cases := []struct {
		name string
		data map[string]interface{}
		want Binding
	}{
		{"none", map[string]interface{}{}, Binding{}},
		{"agent", map[string]interface{}{"binding": map[string]interface{}{"kind": "agent", "id": "story-director"}},
			Binding{Kind: CardAgent, ID: "story-director"}},
		{"bad kind", map[string]interface{}{"binding": map[string]interface{}{"kind": "nope", "id": "x"}}, Binding{}},
		{"missing id", map[string]interface{}{"binding": map[string]interface{}{"kind": "skill"}}, Binding{}},
		{"not a map", map[string]interface{}{"binding": "agent:x"}, Binding{}},
	}
	for _, c := range cases {
		if got := BindingOf(c.data); got != c.want {
			t.Errorf("%s: 期望 %+v，实际 %+v", c.name, c.want, got)
		}
	}
}

// 工作流卡片的 id 是数字字符串；解析失败要让调用方知道（不能默默当 0 号工作流）。
func TestWorkflowIDOfBinding(t *testing.T) {
	if id, ok := WorkflowIDOfBinding(Binding{Kind: CardWorkflow, ID: "42"}); !ok || id != 42 {
		t.Fatalf("应解析出 42，实际 %d/%v", id, ok)
	}
	for _, b := range []Binding{
		{Kind: CardWorkflow, ID: "abc"},
		{Kind: CardWorkflow, ID: "0"},
		{Kind: CardAgent, ID: "42"},
	} {
		if _, ok := WorkflowIDOfBinding(b); ok {
			t.Errorf("%+v 不该解析成功", b)
		}
	}
}

// 删掉一张内置卡必须真的删掉：内置卡在代码里，文件里没有它们的记录，
// 不显式记 hidden 就会"删了又冒出来"。
func TestSaveCardsHiddenPersists(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/cards.json"
	st := LoadCards(path)

	// 只留第一张智能体 + 一张新技能，其余内置全部删除
	keep := st.Agents()[0]
	if err := st.SaveCards(
		[]AgentCard{keep},
		[]SkillCard{{ID: "my-skill", Name: "我的技能", System: "只输出 OK", Category: "自定义"}},
	); err != nil {
		t.Fatalf("保存失败：%v", err)
	}

	// 重新从磁盘加载（模拟重启），删掉的不能复活
	again := LoadCards(path)
	if len(again.Agents()) != 1 || again.Agents()[0].ID != keep.ID {
		t.Fatalf("删除的内置卡复活了：%+v", again.Agents())
	}
	if len(again.Skills()) != 1 || again.Skills()[0].ID != "my-skill" {
		t.Fatalf("新技能没保存住：%+v", again.Skills())
	}
}

// 保存期校验：缺提示词的卡贴到节点上会什么都不做，必须拦住。
func TestSaveCardsRejectsEmptySystem(t *testing.T) {
	st := LoadCards(t.TempDir() + "/cards.json")
	if err := st.SaveCards([]AgentCard{{ID: "a", Name: "空卡"}}, nil); err == nil {
		t.Fatal("没有 system 的智能体卡应被拒")
	}
	if err := st.SaveCards(nil, []SkillCard{{ID: "a", Name: "空技能"}}); err == nil {
		t.Fatal("没有 system 的技能卡应被拒")
	}
	// 同名 id 跨类型也要拒——引用时无法区分
	ok := []AgentCard{{ID: "x", Name: "A", System: "s"}}
	dup := []SkillCard{{ID: "x", Name: "B", System: "s"}}
	if err := st.SaveCards(ok, dup); err == nil {
		t.Fatal("跨类型同 id 应被拒")
	}
}
