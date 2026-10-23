package service

import (
	"context"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"flowedge/backend/model"
)

// 一致性门（consistency_gate）
//
// 要解决的问题
// ------------
// 引擎在「场景一致性」上已有的东西全部在**文本侧**：
//   - 九宫格锁（scene_grid.go）：把场景拆成 3×3 写死物理事实，编译成一句锁句逐段注入；
//   - 字节全等门（auditSceneConsistency）：各段锁句必须完全相同。
//
// 这两条守的是「承诺一致」——提示词里写的是同一句。它守不住「结果一致」：
// 模型收到同一句锁句，仍然会各自采样出不同的光位、道具位置与配色。
// 成片看起来换了场地，而时长、帧数、锁句三项检查**全部正常**。
// AGENTS §0.12② 记的就是这类错：验收维度选错，用「工具没报错」当「没问题」。
//
// 所以这里补的是**像素侧**的门：把每段画面算成可比的签名，逐对比较，
// 判据是数值而不是印象。
//
// 度量怎么选的（GA = 灰度可用性）
// ------------------------------
// 本机引擎是 Go，不引 torch，跑不了 CLIP / DINO 特征。可选的是 ffmpeg + 标准库，
// 于是选三项**互相不可替代**的度量，任何一项单独漂都能被抓到：
//
//   layout   布局/陈设：dHash 汉明距离 → 相似度。抓「道具挪了、墙换了、构图变了」。
//   palette  配色：HSV 联合直方图（12 色相 × 3 饱和 × 3 明度）的巴氏系数。
//            抓「色调漂了」——亮度直方图看不出红木变青灰。
//   luma     光比：平均亮度差。抓「换了个曝光」，这是接缝处最刺眼的一类跳变。
//
// 为什么不用一个总分：加权总分会让「布局全变但色调没变」被「色调没变」掩盖，
// 那是拿调参掩盖结构错误（AGENTS §0.12③）。**三项各自独立判，各自单独报**。
//
// 与九宫格锁的格位对齐（本文件最关键的设计）
// ------------------------------------------
// 度量侧把每帧划成同样的 3×3，格位名直接复用 scene_grid.go 的 gridCellNames。
// 于是报错可以精确说「第 3 段接缝：top-left 格漂了」（而不是「画面不太一样」），
// 用户改哪一格是明确的。文本锁的九格与度量的九格一一对应，
// 「写了锁」和「锁住了」之间才第一次有了闭环。
//
// 零新增依赖：抽帧用 ffmpeg，解码用标准库 image/png|jpeg，度量全部自己算。

// ---------- 度量常量 ----------

// csSize 帧签名的工作分辨率。
//
// 不用原图：不同段分辨率可能不同（OOM 降档时尤其），必须先归一才能比。
// 96 够填满直方图（9216 采样点 / 108 桶 ≈ 85 点每桶），又足够快。
const csSize = 96

// 直方图分桶：色相 12（30°/档）× 饱和 3 × 明度 3 = 108 桶。
const csHueBins, csSatBins, csValBins = 12, 3, 3
const csHistBins = csHueBins * csSatBins * csValBins // 108

// 判定阈值默认值。
//
// 全部**可判 FAIL**，且是**标定过的**，不是拍脑袋写的宽松值
// （AGENTS §0.5「门必须有牙齿」——默认就全过的门等于没有门，
// 默认就全拦的门同样等于没有门，两边都要靠实测数据顶住）。
//
// 标定方法：拿本机真实成片的一帧做受控变换（真值由构造保证），
// 量出「同场景」与「换场景」的数值区间，阈值取在两者之间：
//
//	变体（真值）        布局    配色   亮度差
//	亮度 +4%（同）      1.000  1.000  0.007
//	色相 +5°（同）      0.998  0.995  0.000
//	5% 噪声（同）       0.992  0.985  0.043
//	裁切 3%（同）       0.999  1.000  0.002
//	色相 +120°（异）    0.388  0.570  0.015
//	上半换色（异）      0.665  0.842  0.044
//	亮度 -35%（异）     1.000  0.965  0.081  ← 只有亮度差抓得到
//	换成另一部片（异）  0.393  0.767  0.069
//
// 三条判据各有分工，缺一不可：布局抓换色与局部换景，配色抓整体色调漂移，
// 亮度差抓换曝光（前两条对纯曝光变化几乎无感，实测 1.000 / 0.965）。
const (
	csLayoutMin       = 0.94 // 接缝布局相似度下限（同场景实测 ≥0.992）
	csPaletteMin      = 0.92 // 接缝配色相似度下限（同场景实测 ≥0.985）
	csLumaMax         = 0.07 // 接缝亮度差上限（同场景实测 ≤0.043）
	csDriftLayoutMin  = 0.92 // 各段相对首段的布局下限（防累积漂移）
	csDriftPaletteMin = 0.90 // 各段相对首段的配色下限
)

// ---------- 帧签名 ----------

// frameSig 一帧的可比签名。
type frameSig struct {
	// cellHue 九宫格各自的色相直方图（每块 csCellHueBins 桶，已归一）。
	//
	// **这是布局判据，dHash 被实测淘汰后的替代品**。淘汰理由写在这里，免得有人改回去：
	// 拿同一帧做受控变换测过（标定过程见 docs/多模态出片手册.md §一致性门）：
	//   - 色相旋转 120°（明显换色）  → dHash 0.984（完全没反应）
	//   - 亮度 -35%（换了个曝光）    → dHash 1.000（完全没反应）
	//   - 上半部换色（局部换景）     → dHash 0.984（完全没反应）
	// dHash 只比"相邻像素谁更亮"，是**相对关系**，任何全局色彩变换都伤不到它；
	// 而它唯一能抓的"内容位移"，在真实视频里和相机运动完全混在一起
	// （实测同一段视频首帧 vs 末帧 dHash 就有 0.61，换片对照才 0.52——信噪比 < 1）。
	// 块级色相直方图既带空间信息（哪块是什么颜色 = 陈设/构图），
	// 又是块内统计（对噪点与块内位移免疫），两头都要。
	cellHue [9][csCellHueBins]float64
	hist    [csHistBins]float64 // 全局 HSV 联合直方图（已归一，和为 1）
	grid    [9]csCell           // 九宫格：每格平均色与亮度
	luma    float64             // 整帧平均亮度 0..1
}

// csCellHueBins 每块的色相桶数。
//
// 8 桶（45°/档）：再细则块内像素不够分（每块仅约 1024 个采样点），
// 再粗则"红木变青灰"落在同一桶，门又没牙了。
const csCellHueBins = 8

// csCell 九宫格一格的统计量（数值全部归一到 0..1）。
type csCell struct {
	R, G, B, Y float64
}

// csSample 把任意尺寸图**面积平均**降采样到 n×n。
//
// 必须是面积平均而不是最近邻（这是实测踩出来的，不是理论洁癖）：
// 用最近邻时，1080p 帧缩到 96×96 等于从每 20×11 个像素里随机挑 1 个，
// 挑到的是噪点还是纹理全看运气。后果是——同一部片子的两个镜头，
// 布局相似度只有 0.53；换成完全不同的一部片子是 0.48。**两者几乎分不开**，
// 门在这组真实素材上既会乱判 FAIL 又抓不到真换景。
// 改成面积平均后，高频噪声被平均掉，度量才真正反映"块与块的明暗关系"。
func csSample(src image.Image, n int) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, n, n))
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return dst
	}
	for y := 0; y < n; y++ {
		y0 := b.Min.Y + (y*b.Dy())/n
		y1 := b.Min.Y + ((y+1)*b.Dy())/n
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < n; x++ {
			x0 := b.Min.X + (x*b.Dx())/n
			x1 := b.Min.X + ((x+1)*b.Dx())/n
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, cnt uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					pr, pg, pb, pa := src.At(sx, sy).RGBA()
					r += uint64(pr)
					g += uint64(pg)
					bl += uint64(pb)
					a += uint64(pa)
					cnt++
				}
			}
			if cnt == 0 {
				continue
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(r / cnt >> 8),
				G: uint8(g / cnt >> 8),
				B: uint8(bl / cnt >> 8),
				A: uint8(a / cnt >> 8),
			})
		}
	}
	return dst
}

// csGray 把任意尺寸图**面积平均**降采样成 w×h 灰度矩阵（用于 dHash）。
//
// 与 csSample 同理：dHash 比的是相邻像素大小关系，最近邻带来的高频抖动
// 会直接把比较结果变成抛硬币。平均之后比的才是"左半比右半亮"这类结构事实。
func csGray(src image.Image, w, h int) [][]float64 {
	b := src.Bounds()
	out := make([][]float64, h)
	for y := range out {
		out[y] = make([]float64, w)
	}
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return out
	}
	for y := 0; y < h; y++ {
		y0 := b.Min.Y + (y*b.Dy())/h
		y1 := b.Min.Y + ((y+1)*b.Dy())/h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < w; x++ {
			x0 := b.Min.X + (x*b.Dx())/w
			x1 := b.Min.X + ((x+1)*b.Dx())/w
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sum, cnt uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					r, g, bl, _ := src.At(sx, sy).RGBA()
					// RGBA() 返回 16 位，统一缩到 0..1 再走亮度公式
					sum += uint64(0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl))
					cnt++
				}
			}
			if cnt > 0 {
				out[y][x] = float64(sum) / float64(cnt) / 65535.0
			}
		}
	}
	return out
}

// csRGB2HSV 标准 RGB→HSV（输入 0..1）。
func csRGB2HSV(r, g, b float64) (h, s, v float64) {
	mx := math.Max(r, math.Max(g, b))
	mn := math.Min(r, math.Min(g, b))
	v = mx
	d := mx - mn
	if d <= 0 {
		return 0, 0, v
	}
	s = d / mx
	switch mx {
	case r:
		h = 60 * math.Mod((g-b)/d, 6)
	case g:
		h = 60 * ((b-r)/d + 2)
	default:
		h = 60 * ((r-g)/d + 4)
	}
	if h < 0 {
		h += 360
	}
	return h, s, v
}

// csSatBin 饱和度分桶：低饱和单独一档。
//
// 灰墙、金属、烟雾这些"没有色相"的东西占了画面很大比例，
// 和有色相的像素混在一档里会让配色判定被噪声主导。
func csSatBin(s float64) int {
	switch {
	case s < 0.15:
		return 0
	case s < 0.45:
		return 1
	default:
		return 2
	}
}

func csValBin(v float64) int {
	switch {
	case v < 0.25:
		return 0
	case v < 0.60:
		return 1
	default:
		return 2
	}
}

// csSignature 从图片文件算出签名。
//
// 解码失败必须报错，不许返回零值签名：零值签名之间的相似度是 1.0，
// 会把「根本没读到图」伪装成「完全一致」——这正是 §0.6 那条「不可达 ≠ 空结果」。
func csSignature(path string) (frameSig, error) {
	var sig frameSig
	f, err := os.Open(path)
	if err != nil {
		return sig, fmt.Errorf("打不开帧文件：%w", err)
	}
	defer f.Close()
	img, _, derr := image.Decode(f)
	if derr != nil {
		return sig, fmt.Errorf("帧解码失败（%s）：%w", filepath.Base(path), derr)
	}

	px := csSample(img, csSize)
	cellPx := csSize / 3      // 每格 32×32
	var cellAcc [9][4]float64 // r,g,b,y 累加
	var cellHue [9][csCellHueBins]float64
	var cellHueN [9]float64 // 每块计入色相统计的像素数
	var hist [csHistBins]float64
	total := 0.0
	lum := 0.0

	for y := 0; y < csSize; y++ {
		cy := y / cellPx
		for x := 0; x < csSize; x++ {
			r16, g16, b16, _ := px.RGBAAt(x, y).RGBA()
			r := float64(r16) / 65535.0
			g := float64(g16) / 65535.0
			b := float64(b16) / 65535.0
			yy := 0.299*r + 0.587*g + 0.114*b

			lum += yy
			cx := x / cellPx
			ci := cy*3 + cx
			cellAcc[ci][0] += r
			cellAcc[ci][1] += g
			cellAcc[ci][2] += b
			cellAcc[ci][3] += yy

			h, s, v := csRGB2HSV(r, g, b)
			hb := int(h / 360.0 * csHueBins)
			if hb >= csHueBins {
				hb = csHueBins - 1
			}
			// 低饱和时色相无意义，统一归到 0 号色相桶，避免噪声色相污染配色判定
			sb := csSatBin(s)
			if sb == 0 {
				hb = 0
			}
			hist[hb*csSatBins*csValBins+sb*csValBins+csValBin(v)]++
			total++

			// 块级色相：**按饱和度加权**，而不是"够不够饱和"的硬开关。
			//
			// 硬开关（s >= 0.12 才计数）实测会制造假阳性：一块刚好卡在阈值附近的墙，
			// 采样抖动让它的饱和度在 0.13 / 0.11 之间来回，整块直方图就从"有颜色"
			// 跳成"全空"，相似度直接归零——同场景抖动因此被判成换景（实测 0.333）。
			// 改成加权后，灰墙贡献接近 0（色相本就无意义），但不会突然消失；
			// 真正的彩色区域权重接近 1，判别力不丢。
			if s > 0.05 {
				cb := int(h / 360.0 * csCellHueBins)
				if cb >= csCellHueBins {
					cb = csCellHueBins - 1
				}
				cellHue[ci][cb] += s
				cellHueN[ci] += s
			}
		}
	}

	if total <= 0 {
		return sig, fmt.Errorf("帧为空（%s）", filepath.Base(path))
	}
	for i := range hist {
		hist[i] /= total
	}
	sig.hist = csSmoothHist(hist)
	for i := range cellHue {
		if cellHueN[i] > 0 {
			for j := range cellHue[i] {
				cellHue[i][j] /= cellHueN[i]
			}
		}
		sig.cellHue[i] = csSmoothHue(cellHue[i])
	}
	sig.luma = lum / total
	n := float64(cellPx * cellPx)
	for i := range cellAcc {
		sig.grid[i] = csCell{
			R: cellAcc[i][0] / n,
			G: cellAcc[i][1] / n,
			B: cellAcc[i][2] / n,
			Y: cellAcc[i][3] / n,
		}
	}
	return sig, nil
}

// csSmoothHue 对块色相直方图做环形平滑。
//
// 与 csSmoothHist 同一个理由（分桶边界是悬崖），但只需要一维：
// 块的采样点比全帧少一个数量级，边界效应更明显。
func csSmoothHue(in [csCellHueBins]float64) [csCellHueBins]float64 {
	var out [csCellHueBins]float64
	for i := range in {
		prev := in[(i+csCellHueBins-1)%csCellHueBins]
		next := in[(i+1)%csCellHueBins]
		out[i] = 0.5*in[i] + 0.25*prev + 0.25*next
	}
	return out
}

// csSmoothHist 对直方图做三维可分离平滑（色相环形、饱和与明度非环形）。
//
// 为什么必须平滑（这是踩出来的，不是理论推导）：
// 分桶边界是悬崖——画面整体色相偏 3°，大量像素**同时**跨过 0.45 那条饱和分界线，
// 直方图就整体搬了家，巴氏系数从 0.95 直接掉到 0.08。
// 也就是说：门会在"同场景、只是白平衡略变"时误判 FAIL。
// 平滑之后，跨一格只损失一半重叠而不是全部，误判消失，而"真换景"
// （色相/饱和/明度都离得远）仍然几乎零重叠——判别力没丢。
//
// 卷积核 [0.25, 0.5, 0.25]：中心保留一半，两侧各四分之一。
// 再宽（如等权 1/3）会把"换了相邻色调"也判成相似，门就又没牙了。
func csSmoothHist(in [csHistBins]float64) [csHistBins]float64 {
	idx := func(h, s, v int) int { return h*csSatBins*csValBins + s*csValBins + v }
	cur := in

	// 色相：环形（0 与 11 相邻），否则"红偏紫"会被判成最远
	{
		var out [csHistBins]float64
		for h := 0; h < csHueBins; h++ {
			for s := 0; s < csSatBins; s++ {
				for v := 0; v < csValBins; v++ {
					sum := 0.5*cur[idx(h, s, v)] +
						0.25*cur[idx((h+csHueBins-1)%csHueBins, s, v)] +
						0.25*cur[idx((h+1)%csHueBins, s, v)]
					out[idx(h, s, v)] = sum
				}
			}
		}
		cur = out
	}
	// 饱和与明度：非环形，越界部分并回自身（边缘桶不该被削掉权重）。
	// 两个维度写成两段而不是一股 dim 分支：分支嵌套三层已经出过一次错，
	// 而这里错一点点不会报错，只会让门悄悄变松。
	{
		var out [csHistBins]float64
		for h := 0; h < csHueBins; h++ {
			for s := 0; s < csSatBins; s++ {
				for v := 0; v < csValBins; v++ {
					c := cur[idx(h, s, v)]
					lo, hi := c, c
					if s-1 >= 0 {
						lo = cur[idx(h, s-1, v)]
					}
					if s+1 < csSatBins {
						hi = cur[idx(h, s+1, v)]
					}
					out[idx(h, s, v)] = 0.5*c + 0.25*lo + 0.25*hi
				}
			}
		}
		cur = out
	}
	{
		var out [csHistBins]float64
		for h := 0; h < csHueBins; h++ {
			for s := 0; s < csSatBins; s++ {
				for v := 0; v < csValBins; v++ {
					c := cur[idx(h, s, v)]
					lo, hi := c, c
					if v-1 >= 0 {
						lo = cur[idx(h, s, v-1)]
					}
					if v+1 < csValBins {
						hi = cur[idx(h, s, v+1)]
					}
					out[idx(h, s, v)] = 0.5*c + 0.25*lo + 0.25*hi
				}
			}
		}
		cur = out
	}
	return cur
}

// csBhattacharyya 直方图巴氏系数（0..1，1 = 完全重合）。
//
// 比欧氏距离稳：两个直方图只要**不重叠**就接近 0，欧氏距离对"整体平移一档"
// 和"完全不重叠"给的数值差别不大，判据因此没有牙齿。
func csBhattacharyya(a, b [csHistBins]float64) float64 {
	return csBC(a[:], b[:])
}

// csBC 巴氏系数（切片版，块级与全局直方图共用）。
func csBC(a, b []float64) float64 {
	sum := 0.0
	for i := range a {
		sum += math.Sqrt(a[i] * b[i])
	}
	if sum > 1 {
		sum = 1 // 浮点误差
	}
	return sum
}

// csCellDelta 一格的颜色距离（归一 RGB 欧氏 / √3 → 0..1）。
func csCellDelta(a, b csCell) float64 {
	dr, dg, db := a.R-b.R, a.G-b.G, a.B-b.B
	return math.Sqrt(dr*dr+dg*dg+db*db) / math.Sqrt(3.0)
}

// ---------- 段间比较 ----------

// seamMetric 一处接缝（或一次漂移比对）的度量。
type seamMetric struct {
	From int    `json:"from"` // 1-based 段号
	To   int    `json:"to"`
	Kind string `json:"kind"` // seam（相邻接缝）/ drift（相对首段）
	// Layout 布局相似度 0..1，越高越像
	Layout float64 `json:"layout"`
	// Palette 配色相似度 0..1
	Palette float64 `json:"palette"`
	// LumaDelta 亮度差 0..1
	LumaDelta float64 `json:"luma_delta"`
	// WorstCell 漂得最狠的格位名（与九宫格锁同一套名字）
	WorstCell string `json:"worst_cell"`
	// WorstCellDelta 该格的色距 0..1
	WorstCellDelta float64 `json:"worst_cell_delta"`
}

// csCompare 比较两帧签名。
func csCompare(a, b frameSig) seamMetric {
	// 布局 = 九块色相分布相似度的平均。
	//
	// 取平均而不是最小值：单个小块变化（前景走过一个人）不该判整段换景；
	// 但块数只有 9，换掉 3 块就已经把均值拉低到 0.77 上下，仍会被阈值抓住。
	// 最差的那一块另外单独报出来（WorstCell），定位与判定分开——
	// 判定用均值才稳，定位看最差块才准。
	sum := 0.0
	n := 0.0
	for i := range a.cellHue {
		ha, hb := sumFloats(a.cellHue[i][:]), sumFloats(b.cellHue[i][:])
		if ha <= 0 && hb <= 0 {
			// 两块都是灰的（没有有彩色的像素）：没有信息，不参与平均。
			//
			// 把它们记成 0 会让**同一张图跟自己比**都只有 0.667——
			// 实测踩到：夜景石殿的上排天花与下排地面都是低饱和灰，六块空直方图
			// 各记 0 分，均值被拉到 0.333，门对任何片子都判换景。
			// "两边都没有颜色"是**一致**，不是不一致。
			continue
		}
		sum += csBC(a.cellHue[i][:], b.cellHue[i][:])
		n++
	}
	layout := 1.0
	if n > 0 {
		layout = sum / n
	}
	// n == 0：整幅画面都没有彩色像素（纯灰/黑白）。无从比色，按一致处理——
	// 这种片子的差异由 palette 与 luma 两条判据兜住，不该由布局乱判。
	m := seamMetric{
		Layout:  layout,
		Palette: csBhattacharyya(a.hist, b.hist),
	}
	m.LumaDelta = math.Abs(a.luma - b.luma)
	worst, worstIdx := 0.0, 0
	for i := range a.grid {
		if d := csCellDelta(a.grid[i], b.grid[i]); d > worst {
			worst, worstIdx = d, i
		}
	}
	m.WorstCellDelta = worst
	if worstIdx < len(gridCellNames) {
		m.WorstCell = gridCellNames[worstIdx]
	}
	return m
}

// ---------- 抽帧 ----------

// csProbe 一段素材的三个探针帧（首 / 中 / 末）。
//
// 首末帧判**接缝**（上一段末帧 vs 下一段首帧——模型在这两帧之间重新采样，
// 漂移就发生在这里）；中帧判**累积漂移**（各段中段 vs 第一段中段，
// 抓"每一段都比上一段差一点点"这种逐段 slippery，两两接缝全过但整条片已经跑偏）。
type csProbe struct {
	First, Mid, Last string
}

// csExtractProbe 抽三个探针帧到临时目录。
//
// 三段独立命令而不是一条 select 滤镜：select 里拿不到总帧数（N 不是标准变量），
// 靠 fps 反推在可变帧率素材上会漂。三次调用对几秒的片段代价可以忽略。
func csExtractProbe(ctx context.Context, path, dir, tag string) (csProbe, error) {
	if ctx == nil {
		// nil ctx 会让 exec.CommandContext 直接 panic。调用方漏传时宁可用兜底
		// 也不要崩在抽帧这一步——崩了用户看到的是栈，不是"第几段不对"。
		ctx = context.Background()
	}
	var p csProbe
	if looksLikeImagePath(path) {
		// 图片：三个探针就是它自己，不用抽（也不该走 ffmpeg——静态图没有时长）
		p.First, p.Mid, p.Last = path, path, path
		return p, nil
	}
	bin := ffmpegBin()
	if bin == "" {
		return p, fmt.Errorf("找不到 ffmpeg：一致性门必须能抽帧才能判定，无法跳过")
	}
	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, bin, args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}

	p.First = filepath.Join(dir, tag+"-first.png")
	p.Mid = filepath.Join(dir, tag+"-mid.png")
	p.Last = filepath.Join(dir, tag+"-last.png")

	if err := run("-y", "-v", "error", "-i", path, "-frames:v", "1", "-update", "1", p.First); err != nil {
		return p, fmt.Errorf("抽首帧失败：%w", err)
	}
	// 中帧：先要时长。探测失败不退化为 0（那会抽到首帧，等于没测中段）
	info, ierr := probeMedia(path)
	if ierr != nil || info.dur <= 0 {
		return p, fmt.Errorf("探测时长失败，无法取中帧：%v", ierr)
	}
	mid := info.dur / 2.0
	if err := run("-y", "-v", "error", "-ss", strconv.FormatFloat(mid, 'f', 3, 64),
		"-i", path, "-frames:v", "1", "-update", "1", p.Mid); err != nil {
		return p, fmt.Errorf("抽中帧失败：%w", err)
	}
	// 末帧复用现成实现（shotlist 的首帧引导链就是用它，口径必须一致）
	if err := extractLastFrame(path, p.Last); err != nil {
		return p, fmt.Errorf("抽末帧失败：%w", err)
	}
	return p, nil
}

// ---------- 段收集 ----------

// csSegments 收集待比较的段（保序）。
//
// 顺序是这条门的命根子：段序错了，比的就是「段 3 的末尾 vs 段 1 的开头」，
// 数值照样算得出来，结论完全是假的。所以：
//   - 上游给了 parts / videos / files 数组 → **严格按数组顺序**（分镜节点产出就是有序的）
//   - 没有数组 → 退回按上游 key 排序，并在输出里**明确警告顺序不可信**
func csSegments(node *model.NodeInstance, upstream map[string]interface{}) ([]string, string) {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(strings.Trim(p, `"`))
		if p == "" || seen[p] {
			return
		}
		if _, err := os.Stat(p); err != nil {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	addAny := func(v interface{}) {
		switch t := v.(type) {
		case []string:
			for _, s := range t {
				add(s)
			}
		case []interface{}:
			for _, x := range t {
				if s, ok := x.(string); ok {
					add(s)
				}
			}
		}
	}

	// 1. 节点自己填的多行路径（最高优先级：用户显式指定的顺序）
	if node != nil {
		if s, ok := node.Data["inputs"].(string); ok {
			for _, ln := range strings.Split(s, "\n") {
				add(strings.TrimSpace(ln))
			}
		}
	}
	if len(out) > 0 {
		return out, "manual"
	}

	// 2. 上游数组（parts 是分镜节点的段序列，有序）
	keys := make([]string, 0, len(upstream))
	for k := range upstream {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m, ok := upstream[k].(map[string]interface{})
		if !ok {
			continue
		}
		for _, key := range []string{"parts", "videos", "files", "segments"} {
			if v, ok := m[key]; ok {
				addAny(v)
			}
		}
		if len(out) > 0 {
			return out, "upstream_array"
		}
	}

	// 3. 退回：单个 video/image，按 key 排。顺序不保证 = 结论不保证，必须写明
	for _, k := range keys {
		m, ok := upstream[k].(map[string]interface{})
		if !ok {
			continue
		}
		for _, key := range []string{"video", "image"} {
			if s, ok := m[key].(string); ok {
				add(s)
			}
		}
	}
	if len(out) > 0 {
		return out, "unordered"
	}
	return nil, ""
}

// ---------- 规则与执行 ----------

// csRules 一致性门的判定阈值（全部可选，未配用标定过的默认值）。
type csRules struct {
	layoutMin       float64
	paletteMin      float64
	lumaMax         float64
	driftLayoutMin  float64
	driftPaletteMin float64
	// checkDrift 是否查累积漂移（默认开）
	checkDrift bool
	// anchorPath 可选的锚图：所有段都额外与它比一次（角色/场景锚定）
	anchorPath string
	// warnOnly 只报不拦（调参阶段用；生产必须关）
	warnOnly bool
}

func csParseRules(node *model.NodeInstance) csRules {
	r := csRules{
		layoutMin:       csLayoutMin,
		paletteMin:      csPaletteMin,
		lumaMax:         csLumaMax,
		driftLayoutMin:  csDriftLayoutMin,
		driftPaletteMin: csDriftPaletteMin,
		checkDrift:      true,
	}
	if node == nil {
		return r
	}
	getF := func(key string, dst *float64) {
		if v, ok := node.Data[key].(float64); ok && v > 0 {
			*dst = v
		}
	}
	getF("layout_min", &r.layoutMin)
	getF("palette_min", &r.paletteMin)
	getF("luma_max", &r.lumaMax)
	getF("drift_layout_min", &r.driftLayoutMin)
	getF("drift_palette_min", &r.driftPaletteMin)
	if v, ok := node.Data["check_drift"].(bool); ok {
		r.checkDrift = v
	}
	if v, ok := node.Data["warn_only"].(bool); ok {
		r.warnOnly = v
	}
	if s, ok := node.Data["anchor"].(string); ok {
		r.anchorPath = strings.TrimSpace(s)
	}
	return r
}

// execConsistencyGateNode 一致性门执行器。
func (e *EngineV2) execConsistencyGateNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	segs, order := csSegments(node, upstream)
	if len(segs) == 0 {
		return nil, fmt.Errorf("一致性门没有可比的素材：连一条来自分镜/文生视频的线（parts 数组最佳），或在 inputs 里一行填一条路径")
	}
	if len(segs) == 1 {
		// 单段没有"段间"可比。锚图模式仍可与锚比一次，否则显式拒绝——
		//  silently pass 会让用户以为做过一致性检查。
		if csParseRules(node).anchorPath == "" {
			return nil, fmt.Errorf("一致性门只有 1 段素材：段间一致性需要 ≥2 段（或填 anchor 参考图做锚定比对）")
		}
	}

	tmp, err := os.MkdirTemp("", "flowedge-consistency-")
	if err != nil {
		return nil, fmt.Errorf("建临时目录失败：%w", err)
	}
	defer os.RemoveAll(tmp)

	rules := csParseRules(node)

	// 逐段抽帧 + 签名。抽帧失败 = 判不了，必须报错而不是跳过（§0.6）
	type probe struct {
		First, Mid, Last frameSig
	}
	sigs := make([]probe, 0, len(segs))
	for i, s := range segs {
		if cerr := ctx.Err(); cerr != nil {
			return nil, fmt.Errorf("已取消（已处理 %d/%d 段）：%w", i, len(segs), cerr)
		}
		p, perr := csExtractProbe(ctx, s, tmp, fmt.Sprintf("s%d", i+1))
		if perr != nil {
			return nil, fmt.Errorf("第 %d 段（%s）取帧失败：%w", i+1, filepath.Base(s), perr)
		}
		f, e1 := csSignature(p.First)
		m, e2 := csSignature(p.Mid)
		l, e3 := csSignature(p.Last)
		if e1 != nil || e2 != nil || e3 != nil {
			return nil, fmt.Errorf("第 %d 段签名失败：%v %v %v", i+1, e1, e2, e3)
		}
		sigs = append(sigs, probe{First: f, Mid: m, Last: l})
	}

	// 相邻接缝：上一段末帧 vs 下一段首帧
	seams := make([]seamMetric, 0, len(sigs)-1)
	for i := 0; i+1 < len(sigs); i++ {
		m := csCompare(sigs[i].Last, sigs[i+1].First)
		m.From, m.To, m.Kind = i+1, i+2, "seam"
		seams = append(seams, m)
	}

	// 累积漂移：各段中帧 vs 首段中帧。
	// 只有两两接缝会漏掉「每段差一点、整条跑偏」——这是长片最常见的失败态。
	drifts := make([]seamMetric, 0, len(sigs)-1)
	if rules.checkDrift && len(sigs) > 2 {
		for i := 1; i < len(sigs); i++ {
			m := csCompare(sigs[0].Mid, sigs[i].Mid)
			m.From, m.To, m.Kind = 1, i+1, "drift"
			drifts = append(drifts, m)
		}
	}

	// 锚定比对：每段中帧 vs 锚图（角色/场景锚定，ViMax Producer 那一路）
	anchors := make([]seamMetric, 0, len(sigs))
	if rules.anchorPath != "" {
		if _, serr := os.Stat(rules.anchorPath); serr != nil {
			return nil, fmt.Errorf("锚图不存在：%s", rules.anchorPath)
		}
		as, aerr := csSignature(rules.anchorPath)
		if aerr != nil {
			return nil, fmt.Errorf("锚图签名失败：%w", aerr)
		}
		for i := range sigs {
			m := csCompare(as, sigs[i].Mid)
			m.From, m.To, m.Kind = 0, i+1, "anchor"
			anchors = append(anchors, m)
		}
	}

	checks := csBuildChecks(seams, drifts, anchors, rules, order)
	csAnnotateFixes(checks)

	fails := make([]string, 0, 4)
	for _, c := range checks {
		if c.OK || c.Level != "fail" {
			continue
		}
		d := fmt.Sprintf("%s：%s", c.Name, c.Detail)
		if c.Fix != "" {
			d += " → 修复：" + c.Fix
		}
		fails = append(fails, d)
	}
	if len(fails) > 0 && !rules.warnOnly {
		// 判不过也要把逐项度量带出去（同 quality_gate 的理由）：
		// 证据层用「有没有 checks」区分「判不过」与「没测到」，返回 nil 会把
		// 真正的质量不合格记成 unscored，自进化就看不到一致性在漏。
		return csFailPayload(segs, order, seams, drifts, anchors, checks, fails, rules), fmt.Errorf(
			"一致性门未通过（%d 段）：%s", len(segs), strings.Join(fails, "；"))
	}

	worst := csWorst(append(append(append([]seamMetric{}, seams...), drifts...), anchors...))
	out := map[string]interface{}{
		"gate":       map[bool]string{true: "pass", false: "warn"}[len(fails) == 0],
		"segments":   len(segs),
		"order":      order,
		"seams":      seams,
		"drifts":     drifts,
		"anchors":    anchors,
		"worst":      worst,
		"checks":     checks,
		"text":       csSummary(seams, drifts, anchors, checks),
		"thresholds": csThresholds(rules),
	}
	if order == "unordered" {
		out["warning"] = "段顺序是按上游节点 id 排的，不是生成顺序：结论可能比错对象。" +
			"请连分镜节点的 parts（有序），或在 inputs 里按序填路径。"
	}
	if len(fails) > 0 {
		out["gate"] = "warn"
		out["failures"] = fails
	}
	return out, nil
}

// csFailPayload 一致性门判不过时的产物：逐项度量 + 失败原因，结构与通过时一致。
//
// 关键字段是 checks：证据层（evidence.go gateFacts）用「失败节点有没有 checks」
// 区分「判不过（blocked）」与「门自己崩了（crashed）」。少这一个字段，
// 一次真实的质量不合格就会被记成「没测到」，评测与自进化同时失去信号。
func csFailPayload(segs []string, order string, seams, drifts, anchors []seamMetric,
	checks []gateCheck, fails []string, r csRules) map[string]interface{} {
	worst := csWorst(append(append(append([]seamMetric{}, seams...), drifts...), anchors...))
	out := map[string]interface{}{
		"gate":       "fail",
		"segments":   len(segs),
		"order":      order,
		"seams":      seams,
		"drifts":     drifts,
		"anchors":    anchors,
		"worst":      worst,
		"checks":     checks,
		"failures":   fails,
		"text":       csSummary(seams, drifts, anchors, checks),
		"thresholds": csThresholds(r),
	}
	if order == "unordered" {
		out["warning"] = "段顺序是按上游节点 id 排的，不是生成顺序：结论可能比错对象。" +
			"请连分镜节点的 parts（有序），或在 inputs 里按序填路径。"
	}
	return out
}

// csThresholds 把实际生效的阈值写进输出。
//
// 理由与「参数生效要打印实际值」同一条纪律（AGENTS §0.4）：
// 门说"判过了"，就必须能看见它是按多少判的，否则无法复核。
func csThresholds(r csRules) map[string]float64 {
	return map[string]float64{
		"layout_min":        r.layoutMin,
		"palette_min":       r.paletteMin,
		"luma_max":          r.lumaMax,
		"drift_layout_min":  r.driftLayoutMin,
		"drift_palette_min": r.driftPaletteMin,
	}
}

// csBuildChecks 把度量落成可判定的检查项。
//
// 每项独立判、独立命名，命名里带段号——"第 2→3 段接缝布局"，
// 用户一眼知道重跑哪一段，不用自己对着数字猜。
func csBuildChecks(seams, drifts, anchors []seamMetric, r csRules, order string) []gateCheck {
	checks := make([]gateCheck, 0, 8)
	if order == "unordered" {
		checks = append(checks, gateCheck{
			Name: "segment_order", OK: false, Level: "warn",
			Detail: "段顺序不可信（按上游 id 排）",
		})
	}
	add := func(name string, ok bool, format string, args ...interface{}) {
		checks = append(checks, gateCheck{Name: name, OK: ok, Level: "fail", Detail: fmt.Sprintf(format, args...)})
	}
	for _, m := range seams {
		tag := fmt.Sprintf("seam_%d_%d", m.From, m.To)
		add(tag+"_layout", m.Layout >= r.layoutMin,
			"布局相似度 %.3f ≥ %.2f（最漂格位 %s %.3f）", m.Layout, r.layoutMin, m.WorstCell, m.WorstCellDelta)
		add(tag+"_palette", m.Palette >= r.paletteMin,
			"配色相似度 %.3f ≥ %.2f", m.Palette, r.paletteMin)
		add(tag+"_luma", m.LumaDelta <= r.lumaMax,
			"亮度差 %.3f ≤ %.2f", m.LumaDelta, r.lumaMax)
	}
	for _, m := range drifts {
		tag := fmt.Sprintf("drift_1_%d", m.To)
		add(tag+"_layout", m.Layout >= r.driftLayoutMin,
			"第 %d 段相对首段布局 %.3f ≥ %.2f（最漂格位 %s）", m.To, m.Layout, r.driftLayoutMin, m.WorstCell)
		add(tag+"_palette", m.Palette >= r.driftPaletteMin,
			"第 %d 段相对首段配色 %.3f ≥ %.2f", m.To, m.Palette, r.driftPaletteMin)
	}
	for _, m := range anchors {
		add(fmt.Sprintf("anchor_%d", m.To), m.Layout >= r.layoutMin && m.Palette >= r.paletteMin,
			"第 %d 段与锚图：布局 %.3f / 配色 %.3f", m.To, m.Layout, m.Palette)
	}
	return checks
}

// csAnnotateFixes 给每项失败填**能直接照做**的修复动作。
//
// 与 quality_gate 同一条纪律：门只报"不合格"等于把排查成本原样退回去。
// 这里每一条都指名改哪个节点的哪个字段。
func csAnnotateFixes(checks []gateCheck) {
	for i := range checks {
		c := &checks[i]
		if c.OK {
			continue
		}
		switch {
		case strings.Contains(c.Name, "_layout"):
			c.Fix = "陈设/构图变了：① 打开分镜的 continue 转场让下一段用上一段末帧开场；" +
				"② 把九宫格对应格位写死到材质与数量级别（如 three iron hooks on the left wall）"
		case strings.Contains(c.Name, "_palette"):
			c.Fix = "色调漂了：在场景九宫格节点填 palette 字段（主色关系），" +
				"并确认各段复用**同一份**锁句（锁句漂移时先查 h3.scene_lock_drift）"
		case strings.Contains(c.Name, "_luma"):
			c.Fix = "光比跳变：在场景九宫格节点填 light 字段（方向+色温+硬度），" +
				"并把光源写进具体格位而不是只写全局氛围"
		case strings.HasPrefix(c.Name, "anchor_"):
			c.Fix = "该段与锚图不一致：把这张锚图接到该段的参考图输入，或换一张同场景的锚图"
		case c.Name == "segment_order":
			c.Fix = "连分镜节点的 parts 输出（有序），或在 inputs 里按生成顺序一行一条填路径"
		}
	}
}

// csWorst 找最差的一次比对（供输出与人看）。
func csWorst(ms []seamMetric) *seamMetric {
	if len(ms) == 0 {
		return nil
	}
	worst := ms[0]
	wi := 0
	for i, m := range ms {
		// 以"布局偏离 + 配色偏离 + 亮度偏离"的合成距离挑最差的，
		// 仅用于**展示**优先级，不参与判定（判定永远按分项阈值）
		if csDist(m) > csDist(worst) {
			worst, wi = m, i
		}
	}
	return &ms[wi]
}

func csDist(m seamMetric) float64 {
	return (1 - m.Layout) + (1 - m.Palette) + m.LumaDelta
}

// csSummary 汇总文本（供变量引用 {{gate.text}} 与运行日志）。
func csSummary(seams, drifts, anchors []seamMetric, checks []gateCheck) string {
	pass, fail := 0, 0
	for _, c := range checks {
		if c.OK {
			pass++
		} else if c.Level == "fail" {
			fail++
		}
	}
	worst := csWorst(append(append(append([]seamMetric{}, seams...), drifts...), anchors...))
	s := fmt.Sprintf("一致性：%d 段，%d 项检查，通过 %d，失败 %d",
		len(seams)+1, len(checks), pass, fail)
	if worst != nil {
		s += fmt.Sprintf("；最差 %s %d→%d（布局 %.2f / 配色 %.2f / 亮度差 %.2f，最漂格位 %s）",
			worst.Kind, worst.From, worst.To, worst.Layout, worst.Palette, worst.LumaDelta, worst.WorstCell)
	}
	return s
}

// sumFloats 切片求和（用于判断块直方图是否为空）。
func sumFloats(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s
}
