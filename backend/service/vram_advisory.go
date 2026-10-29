package service

import (
	"fmt"

	"flowedge/backend/model"
)

// 出片前的显存前瞻：这条片子会不会被迫走到换页（慢一个数量级）。
//
// 由来（2026-10-26 现场实录）：
//
//	一次 video 跑到 23574 / 24576 MiB —— 只剩 752MB，
//	而 H3 的启动参数是 --reserve-vram 4（ComfyUI 会努力保 4GB 空余）。
//	24GB 卡上的算术：H3 峰值 ≈ 21.8GB + 桌面/浏览器 ≈ 3.1GB = 23.6GB，
//	空闲 0.9GB < 4GB 预留 ⇒ ComfyUI 只能把权重在显存/内存之间反复搬。
//	实测同一镜：84.9s → 2269.8s（27 倍），然后撞上 15 分钟节点超时，整支片子白跑。
//
// 这个判断是**纯算术**，不需要先烧 15 分钟才知道。放在出片前说清楚，
// 比事后从 sqlite 里挖 elapsed_ms 便宜得多（§0.14：证据优先，别用 GPU 探路）。
//
// 为什么不做成「预检失败就拦住」：换页跑得慢但**能出片**，
// 拦住等于把「慢」升级成「不可用」。说清楚、让用户决定，是这里的分寸。

// VRAMLevel 前瞻结论
const (
	VRAMLevelOK       = "ok"        // 装得下，还够预留
	VRAMLevelReserve  = "reserve"   // 装得下，但吃不满预留 ⇒ 会开始换页
	VRAMLevelTooSmall = "too_small" // 装不下 ⇒ OOM 或极端换页
)

// VRAMAdvisory 一条可读的前瞻结论
type VRAMAdvisory struct {
	Level     string `json:"level"`
	FreeMB    int    `json:"free_mb"`
	NeedMB    int    `json:"need_mb"`
	ReserveMB int    `json:"reserve_mb"`
	Message   string `json:"message"`
}

// JudgeVRAM 纯函数判档，便于测试与复用（不碰网络、不读配置）。
//
// 三档的边界都写在这里，别再散到调用方去各自拍：
//   - free ≥ need + reserve：ok
//   - need ≤ free < need + reserve：reserve（能装下，但保不住预留 → 换页）
//   - free < need：too_small
func JudgeVRAM(freeMB, needMB, reserveMB int) VRAMAdvisory {
	if needMB < 0 {
		needMB = 0
	}
	if reserveMB < 0 {
		reserveMB = 0
	}
	a := VRAMAdvisory{FreeMB: freeMB, NeedMB: needMB, ReserveMB: reserveMB}
	switch {
	case needMB == 0:
		a.Level = VRAMLevelOK
		a.Message = "该流水线不吃扩散显存"
	case freeMB >= needMB+reserveMB:
		a.Level = VRAMLevelOK
		a.Message = fmt.Sprintf("显存充足：空闲 %dMB ≥ 需要 %dMB + 预留 %dMB", freeMB, needMB, reserveMB)
	case freeMB >= needMB:
		a.Level = VRAMLevelReserve
		// 措辞纪律：这一档是**风险提示**，不是判死刑。
		// 实测（2026-10-26 run 208/209）：空闲 19GB、预留保不住，但两镜仍是 82–112s
		// （基准 87s 的 0.9–1.3 倍），照常出片。真正崩成 27 倍的那几晚，
		// 原因是同卡上还跑着**第二个** ComfyUI-H3 抢显存，不是"预留不足"本身。
		// 把风险写成确定性，用户下次看到告警就会不再信它。
		//
		// 机制（读 ComfyUI 源码确认，不是推测）：
		//   comfy/model_management.py:944
		//     minimum_memory_required = memory_required + extra_reserved_memory()
		//   comfy/model_management.py:883-884
		//     EXTRA_RESERVED_VRAM = args.reserve_vram * 1GB   → H3 是 4GB
		// 也就是说「全量加载」要求「模型 + 4GB」同时空闲。差额越大，
		// ComfyUI 越只能部分加载：权重留在内存里、按步流式换进显存。
		gap := needMB + reserveMB - freeMB
		a.Message = fmt.Sprintf(
			"显存偏紧：空闲 %dMB，模型要 %dMB，而 ComfyUI 全量加载要求「模型 + 预留 %dMB」"+
				"同时空闲（model_management.py:944），当前还差 %dMB。"+
				"这种状态下通常仍能出片（实测 1.3 倍以内），但会退化成**部分加载**——"+
				"权重按步从内存换进显存；同卡上只要再有别的进程抢显存（重复实例、浏览器），"+
				"单镜就会从 87s 退化到 10–27 倍并撞超时。先确认 8188/8189 上各只有一份实例",
			freeMB, needMB, reserveMB, gap)
	default:
		a.Level = VRAMLevelTooSmall
		// 同样是风险提示，不是判死刑——2026-10-26 实测：空闲 19028MB < 模型 20480MB，
		// 走的就是这一档，片子照样 82–112s 出来了（约 1.3 倍基准）。
		// 所以这里说清「会部分加载、余量为负」，而不是断言 OOM。
		a.Message = fmt.Sprintf(
			"显存不够装全：空闲 %dMB < 模型 %dMB（差 %dMB）。ComfyUI 会按部分加载跑——"+
				"一部分权重留在内存、按步换进显存。实测这种状态通常仍能出片（1.3 倍以内），"+
				"但余量已经是负的：同卡上任何别的进程（重复实例、浏览器）都会把它推成 10–27 倍慢。"+
				"要稳就先腾显存；要快就把分辨率/帧数降一档",
			freeMB, needMB, needMB-freeMB)
	}
	return a
}

// diffusionNeedMB 按节点类型估算扩散侧显存需求（取该图里最重的那个）。
//
// 数值来自 data/model_residency.json 的实测画像（不是权重文件体积）。
// 宁可高估：高估只会多一句提醒，低估会让用户以为没问题然后白等半小时。
func diffusionNeedMB(nodes []model.NodeInstance) int {
	need := 0
	for _, n := range nodes {
		var mb int
		switch n.Type {
		case "shotlist", "text2video":
			mb = 20480 // H3：608×352×124 帧实测峰值 21.8GB，留一点余量
		case "t2i", "i2i", "inpaint", "upscale":
			mb = 17408 // flux fp8 实测 ~17GB
		}
		if mb > need {
			need = mb
		}
	}
	return need
}

// VRAMAdvisoryForNodes 对一组节点给出显存前瞻（freeMB 由调用方现场探测）。
func VRAMAdvisoryForNodes(nodes []model.NodeInstance, freeMB int) VRAMAdvisory {
	return JudgeVRAM(freeMB, diffusionNeedMB(nodes), vramAdvisoryReserveMBFor(nodes))
}

// vramAdvisoryReserveMBFor 这组节点对应的实例实际会预留多少显存。
//
// 必须与各实例的**启动参数**对齐，否则前瞻会给出与实际行为不符的结论
// （两处不一致时，引擎会一直报「还差 XXXX MB」，而 ComfyUI 那边其实装得下）：
//   - 图像实例（8188）：`--reserve-vram 1` → 1024MB
//   - H3 实例（8189）：2026-10-26 起 `--reserve-vram 0` → 0MB
//
// 为什么 H3 必须是 0：24GB 卡上 20480（H3 权重）+ 4096（旧预留）= 24576MB，
// 恰好等于整张卡；叠加桌面基线 ~1.3GB 后**永远差 1.3GB**，于是每一镜都退化成
// 「权重按步换进显存」的部分加载（实测 87s → 2269s，27 倍）。
// 改成 0 之后 20480 + 0 < 23335（实测空闲）才装得下全量。
func vramAdvisoryReserveMBFor(nodes []model.NodeInstance) int {
	if usesH3(nodes) {
		return vramAdvisoryReserveH3MB
	}
	return vramAdvisoryReserveImageMB
}

const (
	// vramAdvisoryReserveImageMB 必须**逐字对齐** `运行/data/deps.json` 里
	// comfyui.args 的 `--reserve-vram`（当前为 1 → 1024MB）。
	// 2026-10-27 复盘：这条注释原来写"对齐 2"，而 deps.json 早已是 1——
	// 前瞻按 2048 算，于是本该报 reserve 档的情形被算成 ok（差 1GB 的假绿灯）。
	// 改 args 时必须同步这里；两者不一致时，前瞻等于在猜。
	vramAdvisoryReserveImageMB = 1024
	vramAdvisoryReserveH3MB    = 0 // 对齐 8189 的 --reserve-vram 0
)

// vramAdvisoryReserveMB 留给不区分实例的调用方（测试与默认档），与图像档一致。
const vramAdvisoryReserveMB = vramAdvisoryReserveImageMB

// ShortVRAMAdvisory 一行版本，用于日志（不换行、不刷屏）。
// 完整人话在 Message 里，给界面/接口用；日志只要档位与三个数。
func ShortVRAMAdvisory(a VRAMAdvisory) string {
	if a.Level == VRAMLevelOK {
		return ""
	}
	return fmt.Sprintf("[vram] %s（空闲 %dMB / 需要 %dMB / 预留 %dMB）",
		a.Level, a.FreeMB, a.NeedMB, a.ReserveMB)
}

// ProbeFreeVRAMMB 现场探一次设备级空闲显存（探不到返回 0）。
//
// 收口在这里是因为 handler 要用，而 comfy_evict.go 里的探针是包内私有的。
// 探不到时返回 0 而不是报错：前瞻是**建议**，不能因为它拿不到数据就挡住出片。
func ProbeFreeVRAMMB(base string) int {
	free, err := probeVRAMFreeMB(base)
	if err != nil || free <= 0 {
		return 0
	}
	return free
}

// usesH3 这组节点是否走 H3 实例（决定去探哪个端口）。
func usesH3(nodes []model.NodeInstance) bool {
	for _, n := range nodes {
		if n.Type == "shotlist" || n.Type == "text2video" {
			return true
		}
	}
	return false
}

// UsesH3 这组节点是否走 H3 实例（决定去探哪个端口 / 查哪个依赖的托管关系）。
func UsesH3(nodes []model.NodeInstance) bool { return usesH3(nodes) }

// AlreadyResidentForNodes 目标实例是否**已经把模型装进显存了**。
//
// 由来（2026-10-26 实测 run 247）：预检门只看"空闲显存 < 模型体积"，
// 于是热实例（模型已在显存里、空闲只剩 5GB）被判成"装不下"直接 409 拦住——
// 而它连一次加载都不需要，属于纯误伤。"空闲少"和"装不下"是两件事。
//
// 判据：已用显存 ≥ 模型需求的 60% 就认为模型已驻留。
// 取 60% 而不是 90%：ComfyUI 全量驻留时还有 VAE/激活的额外占用，
// 而冷实例的已用通常只有桌面的 1–3GB，两档不会混淆。
func AlreadyResidentForNodes(nodes []model.NodeInstance, comfyBase, h3Base string, needMB int) bool {
	if needMB <= 0 {
		return false
	}
	base := comfyBase
	if usesH3(nodes) {
		base = h3Base
	}
	if base == "" {
		return false
	}
	free, total, err := probeVRAMFreeAndTotalMB(base)
	if err != nil || total <= 0 {
		return false
	}
	used := total - free
	return used >= needMB*3/5
}

// FreeVRAMForNodes 按这组节点该看的实例探一次空闲显存。
// 探不到（服务没起）返回 0，调用方据此跳过前瞻——依赖预检已经会报「没起」。
func FreeVRAMForNodes(nodes []model.NodeInstance, comfyBase, h3Base string) int {
	base := comfyBase
	if usesH3(nodes) {
		base = h3Base
	}
	if base == "" {
		return 0
	}
	return ProbeFreeVRAMMB(base)
}
