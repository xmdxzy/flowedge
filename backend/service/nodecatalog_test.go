package service

import (
	"sort"
	"testing"

	"flowedge/backend/model"
)

// NodeSpecs（调度元数据）与 nodeContracts（画布契约）必须一一对应。
//
// 这条测试就是「单一来源」的守门人：加节点时漏登记任何一边都会红，
// 而不是等到画布上点了节点没表单、或者估算把它当 0 秒。
func TestContractCoversEveryNodeSpec(t *testing.T) {
	specKeys := make([]string, 0, len(NodeSpecs))
	for k := range NodeSpecs {
		specKeys = append(specKeys, k)
	}
	contractKeys := make([]string, 0, len(nodeContracts))
	for k := range nodeContracts {
		contractKeys = append(contractKeys, k)
	}
	sort.Strings(specKeys)
	sort.Strings(contractKeys)

	if len(specKeys) != len(contractKeys) {
		t.Fatalf("节点数不一致：NodeSpecs=%d，nodeContracts=%d\nspecs=%v\ncontracts=%v",
			len(specKeys), len(contractKeys), specKeys, contractKeys)
	}
	for i := range specKeys {
		if specKeys[i] != contractKeys[i] {
			t.Fatalf("节点集合不一致：specs=%v contracts=%v", specKeys, contractKeys)
		}
	}
}

// 除了控制节点，每个节点都必须声明输出端口 —— 否则画布上它连不出去，
// 用户只会看到"拖上来了但拉不出线"。
func TestEveryWorkingNodeDeclaresPorts(t *testing.T) {
	for typ, c := range nodeContracts {
		if typ == "start" {
			if len(c.Out) == 0 {
				t.Errorf("start 必须有输出端口")
			}
			continue
		}
		if typ == "end" {
			if len(c.In) == 0 {
				t.Errorf("end 必须有输入端口")
			}
			continue
		}
		if len(c.In) == 0 {
			t.Errorf("%s 没有输入端口", typ)
		}
		if len(c.Out) == 0 {
			t.Errorf("%s 没有输出端口", typ)
		}
	}
}

func TestNodeCatalogIsSortedAndComplete(t *testing.T) {
	cat := NodeCatalog()
	if len(cat) != len(NodeSpecs) {
		t.Fatalf("目录条目 %d，期望 %d", len(cat), len(NodeSpecs))
	}
	for i := 1; i < len(cat); i++ {
		if cat[i-1].Type >= cat[i].Type {
			t.Fatalf("目录必须按类型名升序，%q >= %q", cat[i-1].Type, cat[i].Type)
		}
	}
	// 成本模型缺失会让估算把节点当 0 秒，等于估算在撒谎
	for _, e := range cat {
		if e.Cost.Unit == "" {
			t.Errorf("%s 没有成本单位", e.Type)
		}
	}
}

// AGENTS §0.3：不跨参数外推、不报单值。
// 结构性保证：四档存在且单调；没有单一 seconds 字段可被误用。
func TestEstimateReturnsRangeNotSingleValue(t *testing.T) {
	nodes := []model.NodeInstance{
		nd("s", "start", map[string]interface{}{"prompt": "雨夜客栈"}),
		nd("t2v", "text2video", map[string]interface{}{"width": 608, "height": 352, "frames": 124}),
		nd("e", "end", nil),
	}
	est := EstimateGraph(nodes, nil)

	if est.OptimisticSec <= 0 || est.P50Sec <= 0 || est.P90Sec <= 0 || est.WorstSec <= 0 {
		t.Fatalf("四档估算都必须为正：%+v", est)
	}
	if !(est.OptimisticSec < est.P50Sec && est.P50Sec < est.P90Sec && est.P90Sec < est.WorstSec) {
		t.Fatalf("估算必须单调递增（乐观 < P50 < P90 < 硬上限）：%+v", est)
	}
	// 124 帧 × 0.7s + 20s ≈ 107s，给宽区间（这条不是精确断言，是防"返回 0"）
	if est.P50Sec < 60 || est.P50Sec > 240 {
		t.Errorf("124 帧单镜 P50 应在 60–240s，实际 %.1f", est.P50Sec)
	}
	if est.VramPeakMB != 23800 {
		t.Errorf("H3 峰值显存应为 23800MB，实际 %d", est.VramPeakMB)
	}
}

// 分辨率按面积缩放，且夹在 [0.25, 6]：736×416 应比 608×352 明显更贵。
func TestEstimateScalesByArea(t *testing.T) {
	base := EstimateGraph([]model.NodeInstance{
		nd("t2v", "text2video", map[string]interface{}{"width": 608, "height": 352, "frames": 124}),
	}, nil)
	big := EstimateGraph([]model.NodeInstance{
		nd("t2v", "text2video", map[string]interface{}{"width": 736, "height": 416, "frames": 124}),
	}, nil)
	if big.P50Sec <= base.P50Sec {
		t.Fatalf("更大分辨率必须更贵：base=%.1f big=%.1f", base.P50Sec, big.P50Sec)
	}
}

// quality_loop 的成本取决于被循环的图，静态不可知 —— 必须老实说不知道，不许编数字。
func TestEstimateMarksUnknownCostAsUncertain(t *testing.T) {
	est := EstimateGraph([]model.NodeInstance{
		nd("q", "quality_loop", map[string]interface{}{"workflow_id": 7, "max_rounds": 3}),
	}, nil)
	if len(est.Uncertain) == 0 {
		t.Fatalf("quality_loop 的未知成本必须进 Uncertain，实际 %+v", est)
	}
}

// 有内联分镜表时按真实镜数/帧数估；没有时进 Uncertain 而不是硬编一个数。
func TestEstimateShotlistInlinePlan(t *testing.T) {
	plan := `{"shots":[{"id":1,"frames":124},{"id":2,"frames":124}]}`
	est := EstimateGraph([]model.NodeInstance{
		nd("sl", "shotlist", map[string]interface{}{"plan": plan, "width": 608, "height": 352}),
	}, nil)
	if len(est.Lines) != 1 {
		t.Fatalf("应有一行估算，实际 %+v", est.Lines)
	}
	if est.Lines[0].Count != 248 {
		t.Errorf("应按 2 镜共 248 帧计数，实际 %.0f", est.Lines[0].Count)
	}
}
