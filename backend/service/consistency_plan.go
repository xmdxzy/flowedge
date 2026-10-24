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

	return issues
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
