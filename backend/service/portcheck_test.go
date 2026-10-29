package service

import (
	"strings"
	"testing"

	"flowedge/backend/model"
)

func nd(id, typ string, data map[string]interface{}) model.NodeInstance {
	return model.NodeInstance{ID: id, Type: typ, Data: data}
}

func ed(id, src, dst string) model.EdgeInstance {
	return model.EdgeInstance{ID: id, Source: src, Target: dst}
}

// 现网 image_edit 流水线里有一条 start-1(文本) → up-1(只收图片) 的线。
// 它是纯执行顺序依赖，运行期无害（节点从自身字段取图）。
// 所以类型不匹配必须是 warn：定成 error 会把自家产品拦下。
func TestValidatePorts_TypeMismatchIsWarnNotError(t *testing.T) {
	nodes := []model.NodeInstance{
		nd("s", "start", map[string]interface{}{"prompt": "x"}),
		nd("up", "upscale", map[string]interface{}{"image_path": "a.png"}),
	}
	issues := ValidatePorts(nodes, []model.EdgeInstance{ed("e1", "s", "up")})

	var mismatch *PortIssue
	for i := range issues {
		if issues[i].Code == IssueTypeMismatch {
			mismatch = &issues[i]
		}
	}
	if mismatch == nil {
		t.Fatalf("应报 type_mismatch，实际 %+v", issues)
	}
	if mismatch.Level != "warn" {
		t.Errorf("type_mismatch 必须是 warn（否则 image_edit 流水线被自己拦下），实际 %s", mismatch.Level)
	}
	for _, i := range issues {
		if i.Level == "error" {
			t.Errorf("这条线不该产生 error 级问题：%+v", i)
		}
	}
}

// v1 的真 bug：「我连了线，但它没生效」。
// 必填输入既没有兼容入边、节点自身也没值 —— 这种图一定跑不起来，必须 error。
func TestValidatePorts_InputUnreachableIsError(t *testing.T) {
	nodes := []model.NodeInstance{
		nd("s", "start", map[string]interface{}{"prompt": "x"}),
		nd("up", "upscale", nil), // 没有 image_path，也没有图片入边
	}
	issues := ValidatePorts(nodes, []model.EdgeInstance{ed("e1", "s", "up")})

	found := false
	for _, i := range issues {
		if i.Code == IssueInputUnreachable {
			found = true
			if i.Level != "error" {
				t.Errorf("input_unreachable 必须是 error，实际 %s", i.Level)
			}
			if len(i.Nodes) != 1 || i.Nodes[0] != "up" {
				t.Errorf("问题应指向节点 up，实际 %v", i.Nodes)
			}
		}
	}
	if !found {
		t.Fatalf("应报 input_unreachable，实际 %+v", issues)
	}
}

// 自己填了 image_path 就算有来源（不要求必须是连线）。
func TestValidatePorts_OwnFieldSatisfiesRequiredInput(t *testing.T) {
	nodes := []model.NodeInstance{nd("up", "upscale", map[string]interface{}{"image_path": "a.png"})}
	if issues := ValidatePorts(nodes, nil); len(issues) != 0 {
		t.Fatalf("自填 image_path 应满足必填输入，实际问题 %+v", issues)
	}
}

// v1 遗留图必须给出可读原因，而不是裸的 unknown node type 崩溃。
func TestValidatePorts_UnknownNodeTypeIsBlocking(t *testing.T) {
	nodes := []model.NodeInstance{
		nd("s", "start", map[string]interface{}{"prompt": "x"}),
		nd("if", "if_else", nil),
	}
	issues := ValidatePorts(nodes, nil)
	if len(issues) != 1 || issues[0].Code != IssueUnknownNodeType || issues[0].Level != "error" {
		t.Fatalf("应报一条 error 级 unknown_node_type，实际 %+v", issues)
	}
	if !strings.Contains(issues[0].Message, "种节点重建") {
		t.Errorf("错误信息应说明用现有节点重建，实际 %q", issues[0].Message)
	}
}

// handle 是 UI 细节，写错只提醒，不阻断保存。
func TestValidatePorts_UnknownHandleIsWarnOnly(t *testing.T) {
	nodes := []model.NodeInstance{
		nd("s", "start", map[string]interface{}{"prompt": "x"}),
		nd("t2i", "text2image", nil),
	}
	e := ed("e1", "s", "t2i")
	e.SourceHandle = "nope"
	issues := ValidatePorts(nodes, []model.EdgeInstance{e})
	if len(issues) != 1 || issues[0].Code != IssueUnknownPortKey || issues[0].Level != "warn" {
		t.Fatalf("应报一条 warn 级 unknown_port，实际 %+v", issues)
	}
}

// 正常图必须零问题（否则画布会出现"满屏黄色警告"，等于没有校验）。
func TestValidatePorts_CleanGraphIsSilent(t *testing.T) {
	nodes := []model.NodeInstance{
		nd("s", "start", map[string]interface{}{"prompt": "x"}),
		nd("t2i", "text2image", nil),
		nd("e", "end", nil),
	}
	edges := []model.EdgeInstance{ed("e1", "s", "t2i"), ed("e2", "t2i", "e")}
	if issues := ValidatePorts(nodes, edges); len(issues) != 0 {
		t.Fatalf("干净图不该有任何端口问题，实际 %+v", issues)
	}
}

