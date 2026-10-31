package service

import "flowedge/backend/model"

// filmFinalRules 把 film 门声明（gates.technical / gates.audio）翻译成终检规则。
//
// 设计意图：预检门审的是「plan 层声明」（还没开机），终检门审的是「成片实体」。
// 两者必须共用同一套阈值来源，否则会出现「plan 说 av_drift 要 ≤0.2s，
// 成片 0.21s 却一路绿灯」——2026-10-25 实测就是这么漏的。
//
// 顺序：先取节点级覆盖（parseGateRules），再用 film gates 填，
// 因为 gates 是这部片子自己的技术契约，比节点默认值更具体。
func filmFinalRules(gates map[string]interface{}, node *model.NodeInstance, expectedFrames int) gateRules {
	r := parseGateRules(node)

	if v := gateMapGet(gates, "technical", "must_have_audio"); v != nil {
		r.requireAudio = gateBool(v)
	}
	if n := gateInt(gateMapGet(gates, "technical", "min_width"), 0); n > 0 {
		r.minWidth = n
	}
	if n := gateInt(gateMapGet(gates, "technical", "min_height"), 0); n > 0 {
		r.minHeight = n
	}
	// av_drift_max 与终检的 durationTol 是同一个量：音画时长差的上限
	if f := gateFloat(gateMapGet(gates, "technical", "av_drift_max"), 0); f > 0 {
		r.durationTol = f
	}
	if f := gateFloat(gateMapGet(gates, "audio", "max_silence_gap"), 0); f > 0 {
		r.maxSilenceGap = f
	}
	// loudness_db 是 [下限, 上限] 两元组，都天然为负
	if arr, ok := gateMapGet(gates, "audio", "loudness_db").([]interface{}); ok && len(arr) == 2 {
		if lo := gateFloat(arr[0], 0); lo < 0 {
			r.loudnessMinDB = lo
		}
		if hi := gateFloat(arr[1], 0); hi < 0 {
			r.loudnessMaxDB = hi
		}
	}
	// VLM 内容判定（V3）：gateMapGet 支持 gates.vlm（true 才开）。
	// 为什么放在 film gates 而不是节点默认值：它改变的是这部片子的交付标准（多花 10–30 秒），
	// 应该由片子自己声明，而不是靠节点上次留下的开关。
	if v := gateMapGet(gates, "vlm", "enabled"); v != nil {
		r.vlmEnabled = gateBool(v)
	}
	if v := gateMapGet(gates, "vlm", "topic"); v != nil {
		if s, ok := v.(string); ok {
			r.vlmTopic = s
		}
	}
	if r.durationTol <= 0 {
		r.durationTol = 0.15
	}
	// 帧数下限 = 各镜产出帧数的 90%：整段丢失会让帧数腰斩，这一条是「丢段」的哨兵。
	// 之所以不要求 100%，是因为 xfade 重叠会合法地吃掉几个接缝帧。
	if expectedFrames > 0 {
		r.minFrames = int(float64(expectedFrames) * 0.9)
	}
	return r
}
