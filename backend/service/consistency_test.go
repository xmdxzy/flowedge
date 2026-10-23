package service

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flowedge/backend/model"
)

// 一致性门的测试
//
// 这文件里最重要的一条是 **TestConsistencyGateHasTeeth**：
// 一个默认阈值下永远判过的门等于没有门（AGENTS §0.5「门必须有牙齿」）。
// 所以这里必须同时存在「同场景判 PASS」与「换景判 FAIL」两个方向，
// 只测「能跑通」不算验证。
//
// 素材全部由代码合成（不依赖本机权重与 GPU），因此这些用例在任何机器上都能跑。

// csWritePNG 用像素函数生成一张 png，返回路径。
func csWritePNG(t *testing.T, dir, name string, w, h int, fn func(x, y int) color.RGBA) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, fn(x, y))
		}
	}
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("建测试图失败：%v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("编码测试图失败：%v", err)
	}
	return p
}

// csStoneHall 一间暖色石殿的抽象画面：三条竖立柱 + 暖棕渐变 + 顶部压暗。
// tint > 0 时整体偏色（模拟同场景不同采样），shift 控制结构偏移。
// csStoneHall 一间石殿的抽象画面。
//
// 为什么要画得"杂"（冷天花 + 暖墙 + 灰石地 + 暖灯光 + 斑驳纹理），
// 而不是一个纯渐变：巴氏系数对**分布宽度**极敏感。纯渐变的颜色全落在
// 少数几个直方图桶里，整体偏一点点色就是"整桶搬家"，相似度掉到 0.87；
// 真实画面的颜色是分散的（实测同场景抖动仍有 0.985）。
// 拿纯色素材调出来的阈值会逼着人把门调松——那是拿假素材改真判据，
// 门就再也对真素材没牙了。
func csStoneHall(tint float64, shift int) func(x, y int) color.RGBA {
	return func(x, y int) color.RGBA {
		row := y * 3 / 96 // 0=上排（天花）1=中排（墙/柱）2=下排（地面）
		tex := 8*math.Sin(float64(x)/6.0)*math.Cos(float64(y)/4.0) +
			5*math.Sin(float64(x+y)/9.0)
		var r, g, b float64
		switch row {
		case 0: // 冷灰天花：低饱和、暗
			r, g, b = 46+tex*0.5, 48+tex*0.5, 52+tex*0.5
		case 1: // 暖棕石墙 + 三条竖柱
			base := 78.0 + float64(y)*0.25
			r, g, b = base*1.28+tex, base*0.96+tex*0.8, base*0.68+tex*0.6
			if (x+shift)%32 < 3 {
				r, g, b = base*1.52+tex, base*1.18+tex*0.8, base*0.86+tex*0.6
			}
			// middle-left 一盏暖灯：高亮、高饱和
			if x > 8 && x < 26 && y > 34 && y < 58 {
				r, g, b = 168+tex, 132+tex*0.8, 74+tex*0.6
			}
		default: // 灰石地面：低饱和、偏亮
			r, g, b = 104+tex*0.7, 102+tex*0.7, 98+tex*0.7
		}
		// 暗角（真实画面四角都会掉光）
		dx, dy := float64(x)-48, float64(y)-48
		vig := 1.0 - math.Sqrt(dx*dx+dy*dy)/96.0
		r, g, b = r*vig, g*vig, b*vig
		return color.RGBA{
			R: uint8(clampF(r + tint*255)),
			G: uint8(clampF(g + tint*255*0.3)),
			B: uint8(clampF(b - tint*255*0.6)),
			A: 255,
		}
	}
}

// csOutdoor 一间完全不同的场地：冷青 + 横向条带（结构与色相双变）。
func csOutdoor(x, y int) color.RGBA {
	band := (y / 12) % 2 // 横向条带，与竖柱形成结构性差异
	v := 70.0
	if band == 0 {
		v = 140.0
	}
	return color.RGBA{
		R: uint8(clampF(v * 0.45)),
		G: uint8(clampF(v * 1.05)),
		B: uint8(clampF(v * 1.35)),
		A: 255,
	}
}

// hsvRGBA 色相角(0-360) + 饱和/明度(0-1) → RGBA。测试素材用它做可控的色相滑动。
func hsvRGBA(h, s, v float64) color.RGBA {
	c := v * s
	x := c * (1 - math.Abs(math.Mod(h/60.0, 2)-1))
	m := v - c
	var r, g, b float64
	switch int(h / 60) {
	case 0:
		r, g, b = c, x, 0
	case 1:
		r, g, b = x, c, 0
	case 2:
		r, g, b = 0, c, x
	case 3:
		r, g, b = 0, x, c
	case 4:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return color.RGBA{
		R: uint8(clampF((r + m) * 255)),
		G: uint8(clampF((g + m) * 255)),
		B: uint8(clampF((b + m) * 255)),
		A: 255,
	}
}

func clampF(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

func TestConsistencySignatureIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	p := csWritePNG(t, dir, "a.png", 96, 96, csStoneHall(0, 0))
	a, err := csSignature(p)
	if err != nil {
		t.Fatalf("签名失败：%v", err)
	}
	b, err := csSignature(p)
	if err != nil {
		t.Fatalf("签名失败：%v", err)
	}
	for i := range a.cellHue {
		for j := range a.cellHue[i] {
			if math.Abs(a.cellHue[i][j]-b.cellHue[i][j]) > 1e-12 {
				t.Fatalf("同一张图两次块色相不一致（块 %d 桶 %d）：签名必须确定性，否则门的结果不可复核", i, j)
			}
		}
	}
	for i := range a.hist {
		if math.Abs(a.hist[i]-b.hist[i]) > 1e-12 {
			t.Fatalf("直方图第 %d 桶不一致", i)
		}
	}
}

func TestConsistencyIdenticalFrameScoresOne(t *testing.T) {
	dir := t.TempDir()
	p := csWritePNG(t, dir, "a.png", 96, 96, csStoneHall(0, 0))
	a, _ := csSignature(p)
	b, _ := csSignature(p)
	m := csCompare(a, b)
	if math.Abs(m.Layout-1.0) > 1e-9 {
		t.Errorf("同图布局相似度应为 1，实际 %.4f", m.Layout)
	}
	if math.Abs(m.Palette-1.0) > 1e-9 {
		t.Errorf("同图配色相似度应为 1，实际 %.4f", m.Palette)
	}
	if m.LumaDelta > 1e-9 {
		t.Errorf("同图亮度差应为 0，实际 %.4f", m.LumaDelta)
	}
}

// TestConsistencyGateHasTeeth 门必须有牙齿：换景必须 FAIL，同场景必须 PASS。
//
// 这是这个文件存在的理由。默认阈值要是宽松到"什么都过"，
// 那这条门就是一块写着"已检查"的装饰板——比没有更危险，因为它给人虚假安全感。
func TestConsistencyGateHasTeeth(t *testing.T) {
	dir := t.TempDir()
	base := csWritePNG(t, dir, "base.png", 96, 96, csStoneHall(0, 0))
	// 同场景不同采样：轻微偏色 + 结构微移（模拟模型在同一句锁句下的正常抖动）
	near := csWritePNG(t, dir, "near.png", 96, 96, csStoneHall(0.012, 1))
	// 换场地：色相与结构全变
	far := csWritePNG(t, dir, "far.png", 96, 96, csOutdoor)

	sigBase, _ := csSignature(base)
	sigNear, _ := csSignature(near)
	sigFar, _ := csSignature(far)

	nearM := csCompare(sigBase, sigNear)
	farM := csCompare(sigBase, sigFar)

	// 同场景抖动与换景的**分离方向**必须正确。
	//
	// 这里不断言"同场景绝对 PASS"：合成图的颜色分布比真实帧集中得多，
	// 拿它卡绝对阈值会逼着人把门调松——那是拿假素材改真判据。
	// 绝对阈值由 TestConsistencyDiscriminationOnRealFootage 用真实成片守住。
	if nearM.Layout <= farM.Layout && nearM.Palette <= farM.Palette {
		t.Errorf("分离方向错了：同场景抖动（布局 %.3f / 配色 %.3f）没有全面优于换景（%.3f / %.3f）",
			nearM.Layout, nearM.Palette, farM.Layout, farM.Palette)
	}
	if nearM.LumaDelta >= farM.LumaDelta {
		t.Errorf("分离方向错了：同场景亮度差 %.3f 不该大于换景 %.3f", nearM.LumaDelta, farM.LumaDelta)
	}
	if farM.Layout >= csLayoutMin && farM.Palette >= csPaletteMin && farM.LumaDelta <= csLumaMax {
		t.Fatalf("换场地被判 PASS，门没有牙齿：布局 %.3f 配色 %.3f 亮度差 %.3f",
			farM.Layout, farM.Palette, farM.LumaDelta)
	}
	// 换景至少要有一项明确越界（不是"差一点点"）
	if farM.Layout >= csLayoutMin && farM.Palette >= csPaletteMin {
		t.Errorf("换场地既没在布局也没在配色上越界：%.3f / %.3f", farM.Layout, farM.Palette)
	}
}

// TestConsistencyWorstCellNamesTheDriftedCell 报错必须指名哪一格漂了。
//
// 只报"画面不一样"没有用——用户不知道改九宫格的哪一格。
// 格位名与 scene_grid.go 的 gridCellNames 共用，报错才能直接落到锁句上。
func TestConsistencyWorstCellNamesTheDriftedCell(t *testing.T) {
	dir := t.TempDir()
	base := csWritePNG(t, dir, "base.png", 96, 96, csStoneHall(0, 0))
	// 只改左上格（3×3 的第 0 格 = 32×32 的左上角）为亮红
	patched := csWritePNG(t, dir, "patch.png", 96, 96, func(x, y int) color.RGBA {
		if x < 32 && y < 32 {
			return color.RGBA{R: 240, G: 30, B: 30, A: 255}
		}
		return csStoneHall(0, 0)(x, y)
	})
	a, _ := csSignature(base)
	b, _ := csSignature(patched)
	m := csCompare(a, b)
	if m.WorstCell != "top-left" {
		t.Fatalf("最漂格位应为 top-left，实际 %q（格位名必须与九宫格锁一致）", m.WorstCell)
	}
	if m.WorstCellDelta < 0.2 {
		t.Errorf("左上格整块换成红色，色距应显著（≥0.2），实际 %.3f", m.WorstCellDelta)
	}
}

func TestConsistencyGridNineCells(t *testing.T) {
	dir := t.TempDir()
	p := csWritePNG(t, dir, "a.png", 96, 96, csStoneHall(0, 0))
	s, err := csSignature(p)
	if err != nil {
		t.Fatalf("签名失败：%v", err)
	}
	// 九格都必须有值（平均亮度应在 0..1 内且非全零）
	for i, c := range s.grid {
		if c.Y <= 0 || c.Y >= 1 {
			t.Errorf("第 %d 格亮度异常：%.4f", i, c.Y)
		}
	}
	// 顶部天花压暗 → 上排三格应明显暗于下排
	top := (s.grid[0].Y + s.grid[1].Y + s.grid[2].Y) / 3
	bottom := (s.grid[6].Y + s.grid[7].Y + s.grid[8].Y) / 3
	if top >= bottom {
		t.Errorf("上排（%.3f）应暗于下排（%.3f）：九宫格分块错位", top, bottom)
	}
}

// TestConsistencySegmentOrderFollowsParts 段顺序必须来自有序数组。
//
// 顺序错了照样能算出数字，结论却是假的——这条守的是"比对了正确的对象"。
func TestConsistencySegmentOrderFollowsParts(t *testing.T) {
	dir := t.TempDir()
	a := csWritePNG(t, dir, "a.png", 32, 32, csStoneHall(0, 0))
	b := csWritePNG(t, dir, "b.png", 32, 32, csOutdoor)
	c := csWritePNG(t, dir, "c.png", 32, 32, csStoneHall(0.05, 2))

	node := &model.NodeInstance{ID: "g", Type: "consistency_gate", Data: map[string]interface{}{}}
	up := map[string]interface{}{
		"shotlist": map[string]interface{}{"parts": []interface{}{c, b, a}},
	}
	segs, order := csSegments(node, up)
	if order != "upstream_array" {
		t.Fatalf("应识别为有序数组，实际 %q", order)
	}
	if len(segs) != 3 || segs[0] != c || segs[1] != b || segs[2] != a {
		t.Fatalf("段顺序未保序：%v", segs)
	}

	// 没有数组时按 key 收，必须标记为 unordered（结论不可信，要写进输出）
	up2 := map[string]interface{}{
		"n2": map[string]interface{}{"video": b},
		"n1": map[string]interface{}{"video": a},
	}
	_, order2 := csSegments(node, up2)
	if order2 != "unordered" {
		t.Fatalf("无数组来源应标记 unordered，实际 %q", order2)
	}
}

func TestConsistencyRejectsMissingInput(t *testing.T) {
	e := &EngineV2{}
	node := &model.NodeInstance{ID: "g", Type: "consistency_gate", Data: map[string]interface{}{}}
	_, err := e.execConsistencyGateNode(context.Background(), node, map[string]interface{}{})
	if err == nil {
		t.Fatal("无输入时必须报错，不能静默通过（不可达 ≠ 空结果）")
	}
}

// TestConsistencySingleSegmentNeedsAnchor 只有一段时没有"段间"可比。
// 静默放行会让用户以为做过一致性检查，所以要么有锚图，要么明确拒绝。
func TestConsistencySingleSegmentNeedsAnchor(t *testing.T) {
	dir := t.TempDir()
	one := csWritePNG(t, dir, "one.png", 32, 32, csStoneHall(0, 0))
	e := &EngineV2{}
	node := &model.NodeInstance{ID: "g", Type: "consistency_gate", Data: map[string]interface{}{}}
	up := map[string]interface{}{"n": map[string]interface{}{"parts": []interface{}{one}}}
	if _, err := e.execConsistencyGateNode(context.Background(), node, up); err == nil {
		t.Fatal("单段且无锚图时应报错")
	}
}

// TestConsistencyEndToEnd 端到端：三张静态图走完整节点路径。
//
// 两张同场景 + 一张换景 → 必须 FAIL，且失败项要能指名段号。
func TestConsistencyEndToEnd(t *testing.T) {
	dir := t.TempDir()
	s1 := csWritePNG(t, dir, "s1.png", 96, 96, csStoneHall(0, 0))
	s2 := csWritePNG(t, dir, "s2.png", 96, 96, csStoneHall(0.012, 1))
	s3 := csWritePNG(t, dir, "s3.png", 96, 96, csOutdoor)

	e := &EngineV2{}
	node := &model.NodeInstance{ID: "g", Type: "consistency_gate", Data: map[string]interface{}{}}
	up := map[string]interface{}{
		"shot": map[string]interface{}{"parts": []interface{}{s1, s2, s3}},
	}
	out, err := e.execConsistencyGateNode(context.Background(), node, up)
	if err == nil {
		t.Fatalf("第 2→3 段换了场地，一致性门必须 FAIL，实际通过：%v", out)
	}
	if !strings.Contains(err.Error(), "一致性门未通过") {
		t.Fatalf("错误信息不是一致性问题：%v", err)
	}
	// 失败应指向 2→3 那道接缝
	if !strings.Contains(err.Error(), "seam_2_3") {
		t.Errorf("失败项应指名 2→3 接缝，实际：%v", err)
	}
	if !strings.Contains(err.Error(), "修复") {
		t.Errorf("失败项必须带可执行修复建议，实际：%v", err)
	}

	// 三张内容完全相同的图 → 必须 PASS（度量恒为 1，这是机制性断言）。
	// 用副本而不是同一路径：段收集按路径去重，同一路径只会被算成 1 段。
	s1b := csWritePNG(t, dir, "s1b.png", 96, 96, csStoneHall(0, 0))
	s1c := csWritePNG(t, dir, "s1c.png", 96, 96, csStoneHall(0, 0))
	up2 := map[string]interface{}{
		"shot": map[string]interface{}{"parts": []interface{}{s1, s1b, s1c}},
	}
	out2, err2 := e.execConsistencyGateNode(context.Background(), node, up2)
	if err2 != nil {
		t.Fatalf("三张相同图应 PASS，实际 FAIL：%v", err2)
	}
	m, _ := out2.(map[string]interface{})
	if m["gate"] != "pass" {
		t.Errorf("gate 应为 pass，实际 %v", m["gate"])
	}
	if m["segments"] != 3 {
		t.Errorf("segments 应为 3，实际 %v", m["segments"])
	}
}

// TestConsistencyWarnOnlyDoesNotBlock 调参阶段允许只报不拦。
// 但输出里必须出现 failures，否则"没拦"会被读成"没查"。
func TestConsistencyWarnOnlyDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	s1 := csWritePNG(t, dir, "s1.png", 96, 96, csStoneHall(0, 0))
	s3 := csWritePNG(t, dir, "s3.png", 96, 96, csOutdoor)
	e := &EngineV2{}
	node := &model.NodeInstance{ID: "g", Type: "consistency_gate", Data: map[string]interface{}{
		"warn_only": true,
	}}
	up := map[string]interface{}{"s": map[string]interface{}{"parts": []interface{}{s1, s3}}}
	out, err := e.execConsistencyGateNode(context.Background(), node, up)
	if err != nil {
		t.Fatalf("warn_only 时不该阻断：%v", err)
	}
	m, _ := out.(map[string]interface{})
	if m["gate"] != "warn" {
		t.Errorf("gate 应为 warn，实际 %v", m["gate"])
	}
	fails, ok := m["failures"].([]string)
	if !ok || len(fails) == 0 {
		t.Fatal("warn_only 放行时也必须列出 failures")
	}
}

// TestConsistencyThresholdsEchoed 门必须回显实际生效的阈值。
// 说"判过了"却不给判据，等于无法复核（AGENTS §0.4「参数生效要打印实际值」）。
func TestConsistencyThresholdsEchoed(t *testing.T) {
	dir := t.TempDir()
	s1 := csWritePNG(t, dir, "s1.png", 96, 96, csStoneHall(0, 0))
	e := &EngineV2{}
	node := &model.NodeInstance{ID: "g", Type: "consistency_gate", Data: map[string]interface{}{
		"layout_min": 0.5,
	}}
	s1d := csWritePNG(t, dir, "s1d.png", 96, 96, csStoneHall(0, 0))
	up := map[string]interface{}{"s": map[string]interface{}{"parts": []interface{}{s1, s1d}}}
	out, err := e.execConsistencyGateNode(context.Background(), node, up)
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	m, _ := out.(map[string]interface{})
	th, _ := m["thresholds"].(map[string]float64)
	if th["layout_min"] != 0.5 {
		t.Fatalf("自定义阈值未生效：%v", th)
	}
	if th["palette_min"] != csPaletteMin {
		t.Fatalf("未指定的项应回落默认值：%v", th)
	}
}

// TestConsistencyDriftCatchesSlowSlide 累积漂移：两两接缝全过，整条跑偏。
//
// 这是长片最典型的失败态：每段只比上段差一点（都在阈值内），
// 十段之后已经完全是另一个场景。只查相邻接缝抓不到，必须查相对首段。
func TestConsistencyDriftCatchesSlowSlide(t *testing.T) {
	dir := t.TempDir()
	var paths []interface{}
	const step = 22.0 // 每段色相滑 22°：单步跨不到一整个色相桶（30°/桶）
	for i := 0; i < 5; i++ {
		hue := 30.0 + float64(i)*step
		p := csWritePNG(t, dir, fmt.Sprintf("slide%d.png", i), 96, 96,
			func(x, y int) color.RGBA {
				// 画面本身保持同一结构（上暗下亮），只让色相滑
				v := 0.45 + float64(y)/96.0*0.3
				return hsvRGBA(hue, 0.55, v)
			})
		paths = append(paths, p)
	}
	sigs := make([]frameSig, 0, len(paths))
	for _, p := range paths {
		s, err := csSignature(p.(string))
		if err != nil {
			t.Fatalf("签名失败：%v", err)
		}
		sigs = append(sigs, s)
	}
	// 末段相对首段必须显著漂移
	m := csCompare(sigs[0], sigs[len(sigs)-1])
	if m.Palette >= csDriftPaletteMin {
		t.Fatalf("五段缓慢滑动后首末段配色仍判相似（%.3f ≥ %.2f）：累积漂移没抓到",
			m.Palette, csDriftPaletteMin)
	}
	// 相邻单步必须"看起来还行"（≥ 接缝阈值），累积后才越界 ——
	// 这正是只查相邻接缝会漏掉的情形：每一步都合格，整条片已经跑偏。
	stepM := csCompare(sigs[0], sigs[1])
	if stepM.Palette < csPaletteMin {
		t.Fatalf("单步漂移 %.3f 低于接缝阈值 %.2f：测试素材单步太大，证明不了「每步都合格」",
			stepM.Palette, csPaletteMin)
	}
	if stepM.Palette <= m.Palette {
		t.Fatalf("单步漂移 %.3f 应优于累计漂移 %.3f", stepM.Palette, m.Palette)
	}
}

// TestConsistencyAnchorComparesEverySegment 锚图模式：每段都与锚比一次。
func TestConsistencyAnchorComparesEverySegment(t *testing.T) {
	dir := t.TempDir()
	anchor := csWritePNG(t, dir, "anchor.png", 96, 96, csStoneHall(0, 0))
	s1 := csWritePNG(t, dir, "s1.png", 96, 96, csStoneHall(0.008, 0))
	s2 := csWritePNG(t, dir, "s2.png", 96, 96, csOutdoor)
	e := &EngineV2{}
	node := &model.NodeInstance{ID: "g", Type: "consistency_gate", Data: map[string]interface{}{
		"anchor": anchor, "warn_only": true,
	}}
	up := map[string]interface{}{"s": map[string]interface{}{"parts": []interface{}{s1, s2}}}
	out, err := e.execConsistencyGateNode(context.Background(), node, up)
	if err != nil {
		t.Fatalf("锚图模式执行失败：%v", err)
	}
	m, _ := out.(map[string]interface{})
	anchors, _ := m["anchors"].([]seamMetric)
	if len(anchors) != 2 {
		t.Fatalf("锚图应对每段各比一次，实际 %d 次", len(anchors))
	}
	// 第 2 段（换景）必须比第 1 段差
	if anchors[1].Palette >= anchors[0].Palette {
		t.Errorf("换景段与锚图的配色相似度（%.3f）不应高于同场景段（%.3f）",
			anchors[1].Palette, anchors[0].Palette)
	}
}

// TestConsistencyOutputIsJSONSerializable 输出要能进 evidence/变量引用。
func TestConsistencyOutputIsJSONSerializable(t *testing.T) {
	m := seamMetric{From: 1, To: 2, Kind: "seam", Layout: 0.9, Palette: 0.8, LumaDelta: 0.02, WorstCell: "center"}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var back seamMetric
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if back.WorstCell != "center" || back.Kind != "seam" {
		t.Fatalf("往返丢失字段：%+v", back)
	}
}

// TestConsistencyBhattacharyyaRange 巴氏系数的值域与极值行为。
func TestConsistencyBhattacharyyaRange(t *testing.T) {
	var a, b [csHistBins]float64
	a[0] = 1
	b[0] = 1
	if v := csBhattacharyya(a, b); math.Abs(v-1) > 1e-9 {
		t.Errorf("相同直方图应为 1，实际 %v", v)
	}
	var c [csHistBins]float64
	c[csHistBins-1] = 1
	if v := csBhattacharyya(a, c); v > 1e-9 {
		t.Errorf("完全不重叠应为 0，实际 %v", v)
	}
	// 半重叠
	for i := range a {
		a[i], b[i] = 0, 0
	}
	a[0], a[1] = 0.5, 0.5
	b[1], b[2] = 0.5, 0.5
	v := csBhattacharyya(a, b)
	if v <= 0.4 || v >= 0.75 {
		t.Errorf("半重叠应落在 0.4~0.75，实际 %.3f", v)
	}
}

// TestConsistencyRegisteredEverywhere 三处注册必须齐（漏一处运行期才炸）。
func TestConsistencyRegisteredEverywhere(t *testing.T) {
	e := &EngineV2{}
	e.registerBuiltinNodes()
	if _, ok := e.nodes["consistency_gate"]; !ok {
		t.Error("未注册执行体")
	}
	if _, ok := NodeSpecs["consistency_gate"]; !ok {
		t.Error("未登记 NodeSpec（超时/侧别会退回默认，长片被误杀）")
	}
	c, ok := Modules["consistency_gate"]
	if !ok {
		t.Fatal("未登记 Modules（任务书会缺段，预检会跳过它）")
	}
	if c.Role != RoleGate {
		t.Errorf("角色应为门，实际 %q", c.Role)
	}
	hasIdentity := false
	for _, l := range c.GateLayers {
		if l == "identity" {
			hasIdentity = true
		}
	}
	if !hasIdentity {
		t.Error("一致性门必须挂在 identity 门层（五层门里这一层原本只有提示词侧检查）")
	}
	// 门不产媒体：它是判定不是变换，算成生产者会污染血缘
	if NodeSpecs["consistency_gate"].Media {
		t.Error("一致性门不应标记 Media=true")
	}
}

// ---------- 真实素材上的判别力回归 ----------
//
// 为什么判别力必须拿真实成片测，而不是合成图：
// 巴氏系数对**颜色分布宽度**极敏感。合成图（哪怕加了纹理）的颜色仍集中在
// 少数几个桶里，整体偏一点点色就是"整桶搬家"，相似度掉到 0.87；
// 真实画面的颜色是分散的，实测同场景抖动仍有 0.98+。
// 拿合成素材调阈值，等于拿假素材改真判据——调松了门就再也对真素材没牙。
// 所以：机制性断言（确定性/格位/顺序/报错）用合成图，
//       **判别力断言一律用真实素材**，没有素材就 skip（不猜）。

// csRealFootage 找一段本机真实成片；没有则返回 ""。
func csRealFootage() string {
	for _, p := range []string{
		filepath.Join("..", "..", "data", "film", "huanggui", "out", "seg1-v2.mp4"),
		filepath.Join("..", "..", "data", "generated", "film-1792387000922507500-shots.mp4"),
	} {
		if st, err := os.Stat(p); err == nil && st.Size() > 10240 {
			return p
		}
	}
	return ""
}

// TestConsistencyDiscriminationOnRealFootage 判别力回归：真值由构造保证。
//
// 同一帧做受控变换：前四个必须判 PASS（同场景的正常抖动），
// 后三个必须判 FAIL（真的换了场景/配色/曝光）。
// 这条用例是**默认阈值**的最后一道保险：谁改了度量或阈值让它变松，这里会红。
func TestConsistencyDiscriminationOnRealFootage(t *testing.T) {
	src := csRealFootage()
	if src == "" {
		t.Skip("没有本机真实成片素材，跳过判别力回归（判定看真实产物，不猜）")
	}
	dir := t.TempDir()
	pr, err := csExtractProbe(context.Background(), src, dir, "base")
	if err != nil {
		t.Skipf("真实素材抽帧失败，跳过：%v", err)
	}
	base := csLoadImg(t, pr.Mid)
	baseSig, err := csSignature(pr.Mid)
	if err != nil {
		t.Fatalf("签名失败：%v", err)
	}

	cases := []struct {
		name string
		img  image.Image
		same bool
	}{
		{"亮度+4%", csMapImg(base, func(r, g, b float64) (float64, float64, float64) {
			return r * 1.04, g * 1.04, b * 1.04
		}), true},
		{"色相+5°", csMapImg(base, func(r, g, b float64) (float64, float64, float64) {
			h, s, v := csRGB2HSV(r, g, b)
			return csHSV2RGB(h+5, s, v)
		}), true},
		{"噪声5%", csNoiseImg(base, 0.05, 7), true},
		{"裁切3%", csCropImg(base, 0.03), true},
		{"色相+120°", csMapImg(base, func(r, g, b float64) (float64, float64, float64) {
			h, s, v := csRGB2HSV(r, g, b)
			return csHSV2RGB(h+120, s, v)
		}), false},
		{"上半换色", csMapRegion(base, 0, 0.5, func(r, g, b float64) (float64, float64, float64) {
			h, s, v := csRGB2HSV(r, g, b)
			return csHSV2RGB(h+150, s, v*1.3)
		}), false},
		{"亮度-35%", csMapImg(base, func(r, g, b float64) (float64, float64, float64) {
			return r * 0.65, g * 0.65, b * 0.65
		}), false},
	}

	for _, c := range cases {
		p := filepath.Join(dir, "case.png")
		f, _ := os.Create(p)
		if err := png.Encode(f, c.img); err != nil {
			f.Close()
			t.Fatalf("编码变体失败：%v", err)
		}
		f.Close()
		sig, err := csSignature(p)
		if err != nil {
			t.Fatalf("%s 签名失败：%v", c.name, err)
		}
		m := csCompare(baseSig, sig)
		passed := m.Layout >= csLayoutMin && m.Palette >= csPaletteMin && m.LumaDelta <= csLumaMax
		if c.same && !passed {
			t.Errorf("%s（同场景）被判 FAIL，门太紧：布局 %.3f 配色 %.3f 亮度差 %.3f",
				c.name, m.Layout, m.Palette, m.LumaDelta)
		}
		if !c.same && passed {
			t.Errorf("%s（换场景）被判 PASS，门没有牙齿：布局 %.3f 配色 %.3f 亮度差 %.3f",
				c.name, m.Layout, m.Palette, m.LumaDelta)
		}
		t.Logf("%-10s 布局 %.3f / 配色 %.3f / 亮度差 %.3f → %s",
			c.name, m.Layout, m.Palette, m.LumaDelta,
			map[bool]string{true: "PASS", false: "FAIL"}[passed])
	}
}

// ---------- 合成素材辅助（仅用于构造受控变体） ----------

func csLoadImg(t *testing.T, p string) image.Image {
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("打开失败：%v", err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	return img
}

func csMapImg(src image.Image, fn func(r, g, b float64) (float64, float64, float64)) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := src.At(x, y).RGBA()
			nr, ng, nb := fn(float64(r)/65535, float64(g)/65535, float64(bl)/65535)
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(clampF(nr * 255)), G: uint8(clampF(ng * 255)),
				B: uint8(clampF(nb * 255)), A: 255,
			})
		}
	}
	return dst
}

func csMapRegion(src image.Image, y0, y1 float64, fn func(r, g, b float64) (float64, float64, float64)) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(b)
	a := b.Min.Y + int(y0*float64(b.Dy()))
	c := b.Min.Y + int(y1*float64(b.Dy()))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := src.At(x, y).RGBA()
			fr, fg, fb := float64(r)/65535, float64(g)/65535, float64(bl)/65535
			if y >= a && y < c {
				fr, fg, fb = fn(fr, fg, fb)
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(clampF(fr * 255)), G: uint8(clampF(fg * 255)),
				B: uint8(clampF(fb * 255)), A: 255,
			})
		}
	}
	return dst
}

func csCropImg(src image.Image, frac float64) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(b)
	dx := int(frac * float64(b.Dx()))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			sx := x + dx
			if sx >= b.Max.X {
				sx = b.Max.X - 1
			}
			dst.Set(x, y, src.At(sx, y))
		}
	}
	return dst
}

func csNoiseImg(src image.Image, amp float64, seed int64) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(b)
	s := seed
	next := func() float64 {
		s = s*1103515245 + 12345
		return float64((s/1024)%2000)/1000.0 - 1.0
	}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := src.At(x, y).RGBA()
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(clampF(float64(r)/65535+amp*next()) * 255),
				G: uint8(clampF(float64(g)/65535+amp*next()) * 255),
				B: uint8(clampF(float64(bl)/65535+amp*next()) * 255),
				A: 255,
			})
		}
	}
	return dst
}

func csHSV2RGB(h, s, v float64) (float64, float64, float64) {
	h = math.Mod(h, 360)
	if h < 0 {
		h += 360
	}
	c := v * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := v - c
	var r, g, b float64
	switch int(h / 60) {
	case 0:
		r, g, b = c, x, 0
	case 1:
		r, g, b = x, c, 0
	case 2:
		r, g, b = 0, c, x
	case 3:
		r, g, b = 0, x, c
	case 4:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return r + m, g + m, b + m
}
