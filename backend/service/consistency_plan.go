package service

import (
	"fmt"
	"strings"
)

// 分镜表级「一致性门」——固定流水线版的一致性保险。
//
// 为什么不是画布时代那个 consistency_gate 节点：那个节点吃的是**已生成的多段视频**
// （抽帧算签名比对），跑完 40 分钟才告诉你前面全白跑；而且它只能人工接到画布上，
// 我们删掉画布后它就没有入口了。
//
// 这里改成在**开机之前**审分镜表本身：轴线、光比、场景锁这三样一旦在 plan 层就漂了，
// 后面每一镜都会忠实执行这个错误的 plan——不拦就是花几十分钟拍一条接缝换场地的片。
//
// 判定分两级：
//   - FAIL：continue 衔接却换了轴线/光比（连续镜头跳轴），或锁句字节不全等（换场地）。
//   - WARN：环境声描述在镜头间变化（可能是合理的换声景，只提示不拦）。

// ShotConsistencyIssue 一条一致性判定。
type ShotConsistencyIssue struct {
	Level  string `json:"level"` // FAIL / WARN
	Code   string `json:"code"`
	ShotID int    `json:"shot_id,omitempty"`
	Msg    string `json:"msg"`
}

// AuditPlanConsistency 审分镜表的一致性。plan 为 nil 或镜头不足 2 个时无判定。
func AuditPlanConsistency(plan *ShotPlan) []ShotConsistencyIssue {
	var issues []ShotConsistencyIssue
	if plan == nil || len(plan.Shots) < 2 {
		return issues
	}

	// 1) continue 衔接 = 同一个连续镜头：轴线与光比必须延续。
	//    cut 换轴是合法的（反打），所以只在 continue 上判。
	for i := 0; i+1 < len(plan.Shots); i++ {
		a, b := plan.Shots[i], plan.Shots[i+1]
		if strings.TrimSpace(a.Transition) != "continue" {
			continue
		}
		if a.Axis != "" && b.Axis != "" && a.Axis != b.Axis {
			issues = append(issues, ShotConsistencyIssue{
				Level: "FAIL", Code: "plan.axis_flip", ShotID: b.ID,
				Msg: fmt.Sprintf("第 %d 镜声明 continue 续接，但轴线从 %q 变成 %q（跳轴：观众会瞬间丢失方位）",
					b.ID, a.Axis, b.Axis),
			})
		}
		if a.Light != "" && b.Light != "" && a.Light != b.Light {
			issues = append(issues, ShotConsistencyIssue{
				Level: "FAIL", Code: "plan.light_jump", ShotID: b.ID,
				Msg: fmt.Sprintf("第 %d 镜声明 continue 续接，但光比从 %q 变成 %q（同一镜头里换光=穿帮）",
					b.ID, a.Light, b.Light),
			})
		}
	}

	// 2) 场景锁：只在该片真的用了锁（有任一段带锁句）时才判，
	//    否则「没写锁」是另一件事，由 text2video 的提示词体检去提示。
	prompts := make([]string, 0, len(plan.Shots))
	anyLock := false
	for _, sh := range plan.Shots {
		prompts = append(prompts, sh.Prompt)
		if SceneLockOf(sh.Prompt) != "" {
			anyLock = true
		}
	}
	if anyLock {
		for _, it := range auditSceneConsistency(prompts) {
			issues = append(issues, ShotConsistencyIssue{
				Level: "FAIL", Code: it.Code, Msg: it.Msg,
			})
		}
	}

	// 3) 环境声：多镜共用一句「持续」描述才不重启音景；不同就提示（不拦）。
	if len(plan.Shots) >= 3 {
		seen := map[string]int{}
		for _, sh := range plan.Shots {
			if s := strings.TrimSpace(sh.Ambience); s != "" {
				seen[s]++
			}
		}
		if len(seen) > 1 {
			var uniq []string
			for k := range seen {
				uniq = append(uniq, k)
			}
			issues = append(issues, ShotConsistencyIssue{
				Level: "WARN", Code: "plan.ambience_drift",
				Msg: fmt.Sprintf("环境声描述有 %d 种写法：每镜换一句会让音景反复重启（建议全片共用一句「持续…」）：%s",
					len(uniq), strings.Join(uniq, " | ")),
			})
		}
	}

	// 4) 镜头语言枚举：angle 只能是机位角度。
	//    2026-10-25 实测：LLM 把 angle 填成「近景」——景别混进角度，
	//    生成端只会自行脑补，画面就不可控了。
	shotSizeWords := []string{"大特写", "特写", "近景", "中近景", "中景", "中远景", "全景", "远景"}
	legalAngles := map[string]bool{
		"平视": true, "平角": true, "仰拍": true, "仰角": true,
		"俯拍": true, "俯角": true, "顶拍": true, "荷兰角": true, "斜角": true,
	}
	for _, sh := range plan.Shots {
		a := strings.TrimSpace(sh.Angle)
		if a == "" {
			continue
		}
		if hasAnyWord(a, shotSizeWords) {
			issues = append(issues, ShotConsistencyIssue{
				Level: "FAIL", Code: "plan.angle_is_shot_size", ShotID: sh.ID,
				Msg: fmt.Sprintf("第 %d 镜 angle=%q 填的是景别，不是机位角度（仰拍/平视/俯拍/荷兰角）；字段串位会让生成端自行脑补", sh.ID, a),
			})
			continue
		}
		if !legalAngles[a] {
			issues = append(issues, ShotConsistencyIssue{
				Level: "WARN", Code: "plan.angle_unknown", ShotID: sh.ID,
				Msg: fmt.Sprintf("第 %d 镜 angle=%q 不在已知机位枚举里（仰拍/平视/俯拍/顶拍/荷兰角）", sh.ID, a),
			})
		}
	}

	// 5) 运镜单调：≥4 镜却有 80% 用同一个运镜（实测 6 镜里 5 镜「固定」）。
	//    只告警不拦：安静的戏确实可以全固定，但它必须是**选择**而不是默认值。
	if len(plan.Shots) >= 4 {
		count := map[string]int{}
		for _, sh := range plan.Shots {
			if c := strings.TrimSpace(sh.Camera); c != "" {
				count[c]++
			}
		}
		worst, worstN := "", 0
		for c, n := range count {
			if n > worstN {
				worst, worstN = c, n
			}
		}
		if worstN >= 3 && worstN*4 >= len(plan.Shots)*3 {
			issues = append(issues, ShotConsistencyIssue{
				Level: "WARN", Code: "plan.camera_monotone",
				Msg: fmt.Sprintf("%d 镜里 %d 镜都是「%s」：全片运镜单一，成片会像幻灯片（关键镜至少给一个推/移/摇）",
					len(plan.Shots), worstN, worst),
			})
		}
	}

	// 6) 景别重复：相邻两镜都在特写区间 → 观众没有呼吸空间。
	for i := 0; i+1 < len(plan.Shots); i++ {
		a, b := strings.TrimSpace(plan.Shots[i].ShotSize), strings.TrimSpace(plan.Shots[i+1].ShotSize)
		if strings.Contains(a, "特写") && strings.Contains(b, "特写") {
			issues = append(issues, ShotConsistencyIssue{
				Level: "WARN", Code: "plan.shot_size_repeat", ShotID: plan.Shots[i+1].ID,
				Msg: fmt.Sprintf("第 %d/%d 镜连着两记特写（%s → %s）：中间缺一个过渡景别",
					plan.Shots[i].ID, plan.Shots[i+1].ID, a, b),
			})
		}
	}

	return issues
}

// hasAnyWord 判断 s 是否包含词表里任一关键词。
func hasAnyWord(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// consistencyFails 取出 FAIL 级问题（调用方据此停机）。
func consistencyFails(issues []ShotConsistencyIssue) []ShotConsistencyIssue {
	out := make([]ShotConsistencyIssue, 0, len(issues))
	for _, it := range issues {
		if it.Level == "FAIL" {
			out = append(out, it)
		}
	}
	return out
}

// consistencyFailMessage 把 FAIL 项压成一段能照着改的话。
func consistencyFailMessage(issues []ShotConsistencyIssue) string {
	lines := make([]string, 0, len(issues))
	for _, it := range issues {
		lines = append(lines, fmt.Sprintf("· [%s] %s", it.Code, it.Msg))
	}
	return strings.Join(lines, "\n")
}
