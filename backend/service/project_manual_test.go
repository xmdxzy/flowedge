package service

import (
	"fmt"
	"strings"
	"testing"
)

// 内置项目手册是知识库的种子内容：空库时自动灌入，之后会被 RAG 检索命中、
// 直接喂给 LLM 节点。它写错了不是"文档瑕疵"，而是**引擎在给出错误答案**。
//
// 曾经的真实漂移：手册写「支持 13 种节点类型」，而 Modules 已经是 29 条。
// 这类漂移靠人记不住，只能靠测试盯 —— 加节点的人不会想起去改一段散文常量。
func TestProjectManualCoversAllModules(t *testing.T) {
	for _, typ := range ModuleTypes() {
		if !strings.Contains(projectManual, "（"+typ+"）") {
			t.Errorf("项目手册没提到模块 %q —— 加了节点要同步改 projectManual，否则知识库会给出过时答案", typ)
		}
	}
	// 反方向：手册里声明的节点数必须与契约表一致，防止"数字"先漂。
	//
	// 数字从 Modules 派生，不写字面量 —— 写死的话每加一个节点都要同时改手册和测试，
	// 而人只会记得改一处（这行以前就是硬编码 30，加两个节点后自己先漂了）。
	want := fmt.Sprintf("内置 %d 种节点类型", len(ModuleTypes()))
	if !strings.Contains(projectManual, want) {
		t.Errorf("手册里的节点数量与 Modules（%d 条）不一致：应在手册里写 %q", len(ModuleTypes()), want)
	}
}
