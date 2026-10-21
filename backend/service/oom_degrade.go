package service

import (
	"fmt"
	"strings"
)

// 显存降级重试：OOM 不是「这次运气不好」，而是参数超出了当前显卡的容量。
//
// 由来：H3（34GB 权重）在 24GB 卡上是踩着线跑的，608×352×124 帧实测峰值 21.8GB，
// 分辨率或帧数稍微一顶就 OOM。以前的做法是直接失败，用户改参数重跑——
// 而重跑要重新加载权重（分钟级），代价远大于「降一档把这一条跑出来」。
//
// 纪律（与项目其余部分一致）：
//   - 只认真正的 OOM 特征，其他错误一律不吞（不能把参数错当成显存错）
//   - 降级必须留痕：返回降了几档、降成了什么，节点输出里带 degraded 字段
//   - 降到底还失败就如实报原始错误，不伪装成成功

// maxOOMDegrade 最多降三档（精度 / 分辨率 / 帧数或步数 各一档）。
// 再低（352×208·9帧）已经没有交付价值，
// 继续降只会把「出不了片」伪装成「出了个不能用的片」。
const maxOOMDegrade = 3

// roleForKind 媒体类型 → bundle 角色（精度阶梯按角色取）
func roleForKind(kind string) string {
	switch kind {
	case "h3", "h3_i2v":
		return RoleVideoH3Fl2va
	case "h3_r2v":
		return RoleVideoH3Ref2va
	case "t2v":
		return RoleVideoWan
	case "upscale":
		return RoleUpscale
	case "inpaint":
		return RoleInpaint
	case "img2img":
		return RoleImg2Img
	}
	return RoleT2IPrimary
}

// nextPrecision 精度降一档（换更省显存的权重文件）。
//
// 为什么排在最前面：换精度只损失细节保真，**不改变构图与时长**；
// 降分辨率会改构图，减帧数会改时长——用户对后两者的容忍度明显更低。
//
// 阶梯为空或长度 1 时直接跳过：本机权重多为 fp8/int8，没有更低档可换。
// 当前权重不在阶梯里（用户自己指定了别的文件）也不动——猜错的降级
// 会悄悄换成另一个模型，比 OOM 更难排查。
func nextPrecision(req MediaRequest, ladder []string) (MediaRequest, bool) {
	if len(ladder) < 2 {
		return req, false
	}
	cur := req.Precision
	if cur == "" {
		cur = ladder[0]
	}
	for i, name := range ladder {
		if name != cur {
			continue
		}
		if i+1 < len(ladder) {
			out := req
			out.Precision = ladder[i+1]
			return out, true
		}
		return req, false
	}
	return req, false
}

// oomMarkers ComfyUI / PyTorch 抛 OOM 时的实际文本特征。
// 全部小写匹配；只收明确指向显存耗尽的说法，避免误伤普通报错。
var oomMarkers = []string{
	"out of memory",
	"outofmemoryerror",
	"cuda oom",
	"cuda error: out of memory",
	"not enough memory",
	"显存不足",
	"显存不够",
	"vram",
}

// IsOOMError 是否为显存耗尽类错误（导出以便测试直验）。
func IsOOMError(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	for _, m := range oomMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// degradeMediaRequest 降一档：精度 → 分辨率 ×0.75（对齐 8，VAE 要求）→ 帧数 ×0.7 → 步数 ×0.6。
// 返回是否真的降了——降不动时必须让调用方报原始错误，不能空转重试。
func degradeMediaRequest(req MediaRequest, ladder []string) (MediaRequest, bool) {
	// 1) 精度：最无损的一档，先试
	if next, ok := nextPrecision(req, ladder); ok {
		return next, true
	}
	out := req
	// 分辨率优先：显存占用与像素数近似线性，且对观感影响最小
	if out.Width > 256 || out.Height > 256 {
		if out.Width > 0 {
			out.Width = align8(int(float64(out.Width) * 0.75))
		}
		if out.Height > 0 {
			out.Height = align8(int(float64(out.Height) * 0.75))
		}
		return out, out.Width != req.Width || out.Height != req.Height
	}
	// 分辨率降不动了再降帧数（视频）：17k+5 是 H3 的帧数吸附规则
	if out.Frames > 17 {
		f := int(float64(out.Frames) * 0.7)
		if f < 17 {
			f = 17
		}
		out.Frames = f
		return out, out.Frames != req.Frames
	}
	// 最后降步数：质量代价最大的一档，留到最后
	if out.Steps > 6 {
		s := int(float64(out.Steps) * 0.6)
		if s < 6 {
			s = 6
		}
		out.Steps = s
		return out, out.Steps != req.Steps
	}
	return out, false
}

// align8 对齐到 8 的倍数（VAE 要求），最小值兜底 8
func align8(n int) int {
	n = (n / 8) * 8
	if n < 8 {
		return 8
	}
	return n
}

// degradeNote 生成人话降级说明，写进节点输出供用户判断是否接受这一版
func degradeNote(before, after MediaRequest) string {
	parts := []string{}
	if after.Precision != before.Precision && after.Precision != "" {
		parts = append(parts, fmt.Sprintf("精度档 → %s", after.Precision))
	}
	if after.Width != before.Width || after.Height != before.Height {
		parts = append(parts, fmt.Sprintf("分辨率 %d×%d → %d×%d", before.Width, before.Height, after.Width, after.Height))
	}
	if after.Frames != before.Frames && before.Frames > 0 {
		parts = append(parts, fmt.Sprintf("帧数 %d → %d", before.Frames, after.Frames))
	}
	if after.Steps != before.Steps && before.Steps > 0 {
		parts = append(parts, fmt.Sprintf("步数 %d → %d", before.Steps, after.Steps))
	}
	return strings.Join(parts, "，")
}

// generateMediaWithDegrade 带显存降级重试的生成入口。
//
// 返回（路径, kind, 降了几档, 降级说明, 错误）。降级说明非空即表示这次是靠降档跑出来的，
// 调用方必须把它写进节点输出——用户有权知道自己拿到的是降档版。
func (e *EngineV2) generateMediaWithDegrade(req MediaRequest) (string, string, int, string, error) {
	level := 0
	note := ""
	cur := req
	lastErr := error(nil)
	// 精度阶梯按角色取（t2i 主模型 / H3 / Wan 各自的降级链）
	var ladder []string
	if e.t2i != nil {
		ladder = e.t2i.Bundles.Ladder(roleForKind(req.Kind))
	}
	for attempt := 0; attempt <= maxOOMDegrade; attempt++ {
		path, kind, err := e.t2i.GenerateMedia(cur)
		if err == nil {
			return path, kind, level, note, nil
		}
		lastErr = err
		if !IsOOMError(err) || e.t2i == nil {
			return "", "", level, note, err
		}
		// CUDA 显存释放是异步的：不等就「卸载了但没释放」，紧接着重试必然再 OOM
		e.releaseDiffusionAndWait()
		// 已经用掉全部重试次数：再降一档也没机会执行，不能把这一档算进 level
		// （否则返回的「降了几档」比实际生效的多一档，用户按它判断画质会判断错）
		if attempt >= maxOOMDegrade {
			break
		}
		next, changed := degradeMediaRequest(cur, ladder)
		if !changed {
			break // 降不动了，如实报原始错误
		}
		note = degradeNote(cur, next)
		cur = next
		level++
	}
	return "", "", level, note, lastErr
}
