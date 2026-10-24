package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 真文件就是契约：data/pipelines.json 是产品形态本身，
// 写错一个字段名/节点 id 的表现是"用户点了出片然后失败"，
// 所以这里直接加载真文件，逐个流水线跑一次组装。
func loadRealPipelines(t *testing.T) *PipelineStore {
	t.Helper()
	path := filepath.Join("..", "..", "data", "pipelines.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("找不到流水线定义 %s：它现在就是产品本身，丢了等于没产品", path)
	}
	s, err := LoadPipelines(path)
	if err != nil {
		t.Fatalf("加载流水线定义失败：%v", err)
	}
	return s
}

// requiredDefaults 给必填字段填一个占位值，让"默认参数也能组装"成立。
func requiredDefaults(p *Pipeline) map[string]interface{} {
	out := map[string]interface{}{}
	for _, f := range p.Fields {
		if f.Required && f.Type != "bool" && f.Type != "int" && f.Type != "float" && f.Type != "enum" {
			out[f.Key] = "占位值"
		}
	}
	return out
}

func TestPipelinesLoadAndBuild(t *testing.T) {
	s := loadRealPipelines(t)
	list := s.List()
	if len(list) < 5 {
		t.Fatalf("流水线太少（%d）：出片台至少要覆盖 图/视频/长片/编辑/配音", len(list))
	}
	for _, p := range list {
		p := p
		t.Run(p.ID, func(t *testing.T) {
			built, err := s.Build(p.ID, requiredDefaults(&p))
			if err != nil {
				t.Fatalf("默认参数组装失败：%v", err)
			}
			if len(built.Nodes) == 0 {
				t.Fatal("组装后没有节点")
			}
			// 每个节点都必须有登记的类型（validatePipelineShape 已查，这里查组装结果）
			for _, n := range built.Nodes {
				if _, ok := NodeSpecs[n.Type]; !ok {
					t.Errorf("节点 %s 的类型 %q 未登记", n.ID, n.Type)
				}
			}
			// 连线必须两端都存在
			ids := map[string]bool{}
			for _, n := range built.Nodes {
				ids[n.ID] = true
			}
			for _, e := range built.Edges {
				if !ids[e.Source] || !ids[e.Target] {
					t.Errorf("边 %s 指向不存在的节点（%s → %s）", e.ID, e.Source, e.Target)
				}
			}
			// 至少一个 end 节点（出片必须有终点，不然产物没被取走）
			if !ids["end-1"] {
				t.Errorf("组装结果缺少 end-1")
			}
			// 保存期校验必须通过（否则"能组装但保存就报错"自相矛盾）
			if issues := ValidateNodeParams(built.Nodes, InboundSet(built.Edges)); len(issues) > 0 {
				t.Errorf("默认参数应能通过保存期校验，实际 %+v", issues)
			}
		})
	}
}

// 字段 bind 必须真的写到节点上：填了 width=768 而节点还是 1024，
// 是"我明明填了却没生效"这类最伤人的静默错误。
func TestPipelineBindWritesNodeData(t *testing.T) {
	s := loadRealPipelines(t)
	built, err := s.Build("image", map[string]interface{}{
		"prompt": "测试", "width": 768, "height": 1344, "batch": 2,
	})
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	var t2i map[string]interface{}
	for _, n := range built.Nodes {
		if n.Type == "text2image" {
			t2i = n.Data
		}
		if n.Type == "start" {
			if got, _ := n.Data["prompt"].(string); got != "测试" {
				t.Errorf("start.prompt = %q，期望 测试", got)
			}
		}
	}
	if t2i == nil {
		t.Fatal("没有 text2image 节点")
	}
	if got, _ := t2i["width"].(float64); got != 768 {
		t.Errorf("width = %v，期望 768", t2i["width"])
	}
	if got, _ := t2i["height"].(float64); got != 1344 {
		t.Errorf("height = %v，期望 1344", t2i["height"])
	}
	if got, _ := t2i["batch"].(float64); got != 2 {
		t.Errorf("batch = %v，期望 2", t2i["batch"])
	}
}

// when 条件：image_edit 三种动作互斥，选一个只能出现一个节点。
func TestPipelineWhenFiltersNodes(t *testing.T) {
	s := loadRealPipelines(t)
	cases := map[string]string{
		"restyle": "image2image",
		"inpaint": "inpaint",
		"upscale": "upscale",
	}
	for mode, wantType := range cases {
		mode, wantType := mode, wantType
		t.Run(mode, func(t *testing.T) {
			params := map[string]interface{}{"mode": mode, "image_path": "C:/tmp/a.png"}
			if mode != "upscale" {
				params["prompt"] = "改风格"
			}
			if mode == "inpaint" {
				params["mask_path"] = "C:/tmp/mask.png"
			}
			built, err := s.Build("image_edit", params)
			if err != nil {
				t.Fatalf("组装失败：%v", err)
			}
			types := map[string]bool{}
			for _, n := range built.Nodes {
				types[n.Type] = true
			}
			if !types[wantType] {
				t.Errorf("mode=%s 应包含 %s，实际节点 %v", mode, wantType, types)
			}
			if len(types) != 3 { // start + 目标节点 + end
				t.Errorf("mode=%s 应只有 3 种节点（start/目标/end），实际 %v", mode, types)
			}
		})
	}
}

func TestPipelineRejectsUnknownField(t *testing.T) {
	s := loadRealPipelines(t)
	_, err := s.Build("image", map[string]interface{}{"prompt": "x", "wdith": 768})
	if err == nil {
		t.Fatal("拼错的字段名必须被拒绝（静默忽略会变成\"我填了却没生效\"）")
	}
	if !containsStr(err.Error(), "wdith") {
		t.Errorf("错误信息应点名拼错的字段，实际：%v", err)
	}
}

func TestPipelineRejectsBadValues(t *testing.T) {
	s := loadRealPipelines(t)
	cases := []struct {
		name   string
		params map[string]interface{}
		want   string
	}{
		{"缺必填", map[string]interface{}{}, "画面描述"},
		{"超范围", map[string]interface{}{"prompt": "x", "width": 4000}, "不能大于"},
		{"非整数", map[string]interface{}{"prompt": "x", "batch": 1.5}, "整数"},
		{"枚举非法", map[string]interface{}{"mode": "rotate", "image_path": "a.png"}, "只能是"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			pipeline := "image"
			if c.name == "枚举非法" {
				pipeline = "image_edit"
			}
			_, err := s.Build(pipeline, c.params)
			if err == nil {
				t.Fatalf("%s 应被拒绝", c.name)
			}
			if !containsStr(err.Error(), c.want) {
				t.Errorf("错误信息应包含 %q，实际：%v", c.want, err)
			}
		})
	}
}

// 节点模板是共享的：并发组装两次，改一次不能污染另一次（深拷贝纪律）。
func TestPipelineBuildIsolation(t *testing.T) {
	s := loadRealPipelines(t)
	a, err := s.Build("video", map[string]interface{}{"prompt": "甲", "width": 512})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Build("video", map[string]interface{}{"prompt": "乙", "width": 1024})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range a.Nodes {
		if n.Type == "text2video" {
			if got, _ := n.Data["width"].(float64); got != 512 {
				t.Errorf("甲 width = %v，期望 512（说明模板被乙污染了）", got)
			}
		}
	}
	for _, n := range b.Nodes {
		if n.Type == "text2video" {
			if got, _ := n.Data["width"].(float64); got != 1024 {
				t.Errorf("乙 width = %v，期望 1024", got)
			}
		}
	}
}

// 定义文件本身坏掉时必须报错，而不是给一个空目录（空目录=界面空白，看起来像产品坏了）。
func TestPipelineStoreRejectsBrokenFile(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "pipelines.json")
	if err := os.WriteFile(bad, []byte(`{"pipelines":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPipelines(bad); err == nil {
		t.Fatal("空的 pipelines 必须报错")
	}

	missingNode := map[string]interface{}{
		"version": 1,
		"pipelines": []map[string]interface{}{{
			"id": "x", "name": "x", "output": "image",
			"fields": []map[string]interface{}{{"key": "prompt", "type": "text"}},
			"nodes":  []map[string]interface{}{{"id": "s", "type": "start", "data": map[string]interface{}{}}, {"id": "e", "type": "end", "data": map[string]interface{}{}}},
			"edges":  []map[string]interface{}{{"id": "1", "source": "s", "target": "nope"}},
		}},
	}
	raw, _ := json.Marshal(missingNode)
	if err := os.WriteFile(bad, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPipelines(bad); err == nil {
		t.Fatal("边指向不存在的节点必须报错")
	}
}

func containsStr(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
