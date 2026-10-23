package service

import (
	"fmt"
	"sort"
)

// 回归监测：一个变体晋升之后，后面跑出来的成绩有没有退回去。
//
// 为什么必须有这一步
//
//	配对判定只用**一批**证据下结论。但真实的退化不是"这一批就差了"，
//	而是"这一批恰好都是它擅长的 case，后来那批不擅长"。没有晋升后的监测，
//	一次错误的晋升会一直生效，直到有人偶然发现成片变差 —— 那时已经没人
//	记得是哪一次改动引入的。
//
// 为什么不做成"自动回滚"
//
//	判定基于证据，而证据可能缺（服务没起、门崩了）。自动回滚一个因为
//	**采集失败**而看起来变差的版本，比不滚更糟。所以这里只给判定与目标版本，
//	真正的回滚是一个显式动作（POST /api/evolution/rollback），
//	且回滚前还会再快照一次 —— 滚错了还能滚回来。

// RegressionReport 晋升后的回归体检
type RegressionReport struct {
	Verdict string `json:"verdict"` // healthy / regressed / insufficient

	BaselineRuns  int `json:"baselineRuns"`
	CandidateRuns int `json:"candidateRuns"`
	// BaselineVersion / CandidateVersion 两侧证据主要落在哪个图版本上
	BaselineVersion  int `json:"baselineVersion"`
	CandidateVersion int `json:"candidateVersion"`

	// 复用的配对判定：回归本来就是"候选这批是不是还不如基线"
	Decision Decision `json:"decision"`

	// RollbackTo 建议回滚到哪个版本（0 = 没有可回滚的版本）
	RollbackTo int      `json:"rollbackTo"`
	Reasons    []string `json:"reasons"`
}

// MonitorRegression 读一批证据，判断当前版本相对基线是否退化。
//
// 分组口径（顺序很重要，先按证据自己声明的身份，再按版本号兜底）：
//
//	① evidence.role = baseline / candidate —— 评测层显式标注，最可信
//	② role 缺失时按 workflowVersion：版本号小的算基线，最大的算当前
//
// ②只作为兜底，且**不猜版本区间**：拿"最大的和最小的"比，中间版本一律不参与。
func MonitorRegression(evs []Evidence, p EvoPolicy) RegressionReport {
	rep := RegressionReport{Reasons: []string{}}
	if p.MinPairs <= 0 {
		p = DefaultEvoPolicy()
	}

	var base, cand []Evidence
	for _, ev := range evs {
		switch ev.Role {
		case "baseline":
			base = append(base, ev)
		case "candidate":
			cand = append(cand, ev)
		}
	}

	// role 一侧缺失 → 用版本号兜底（只兜底缺的那一侧，不覆盖显式标注）
	if len(base) == 0 || len(cand) == 0 {
		versions := map[int]bool{}
		for _, ev := range evs {
			if ev.WorkflowVersion > 0 && (ev.Result == EvidencePassed || ev.Result == EvidenceFailed) {
				versions[ev.WorkflowVersion] = true
			}
		}
		var vs []int
		for v := range versions {
			vs = append(vs, v)
		}
		sort.Ints(vs)
		if len(vs) >= 2 {
			lo, hi := vs[0], vs[len(vs)-1]
			if len(base) == 0 {
				for _, ev := range evs {
					if ev.Role == "" && ev.WorkflowVersion == lo && isScored(ev) {
						base = append(base, ev)
					}
				}
				rep.Reasons = append(rep.Reasons, fmt.Sprintf(
					"基线侧没有 role 标注，按最小版本号 v%d 兜底（%d 条）", lo, len(base)))
			}
			if len(cand) == 0 {
				for _, ev := range evs {
					if ev.Role == "" && ev.WorkflowVersion == hi && isScored(ev) {
						cand = append(cand, ev)
					}
				}
				rep.Reasons = append(rep.Reasons, fmt.Sprintf(
					"候选侧没有 role 标注，按最大版本号 v%d 兜底（%d 条）", hi, len(cand)))
			}
		}
	}

	rep.BaselineRuns, rep.CandidateRuns = len(base), len(cand)
	rep.BaselineVersion = dominantVersion(base)
	rep.CandidateVersion = dominantVersion(cand)

	if len(base) == 0 || len(cand) == 0 {
		rep.Verdict = "insufficient"
		rep.Reasons = append(rep.Reasons, fmt.Sprintf(
			"两侧证据不齐（baseline=%d / candidate=%d）：判断回归需要同一个 case 在改前改后都跑过，缺一侧就是没测",
			len(base), len(cand)))
		return rep
	}

	d := Decide(base, cand, p)
	rep.Decision = d

	switch {
	case d.Verdict == "inconclusive":
		rep.Verdict = "insufficient"
		rep.Reasons = append(rep.Reasons, d.Reasons...)
		return rep
	case d.Verdict == "keep_baseline":
		rep.Verdict = "regressed"
		rep.Reasons = append(rep.Reasons,
			fmt.Sprintf("配对结论是 keep_baseline（%d 胜 / %d 负 / 门分差 %+.3f）—— 当前版本没有比基线好，等于退化",
				d.Better, d.Worse, d.GateScoreDelta))
		// 回滚目标是基线证据实际跑过的那个版本，不是"最大版本-1"
		rep.RollbackTo = rep.BaselineVersion
		if rep.RollbackTo == 0 {
			rep.Reasons = append(rep.Reasons,
				"基线证据没记 workflowVersion（旧信封），无法确定该滚回哪一版：请手工指定 toVersion")
		}
		return rep
	}

	rep.Verdict = "healthy"
	rep.Reasons = append(rep.Reasons,
		fmt.Sprintf("配对结论 promote 仍成立（%d 胜 / %d 负 / 门分差 %+.3f）",
			d.Better, d.Worse, d.GateScoreDelta))
	return rep
}

func isScored(ev Evidence) bool {
	return ev.Result == EvidencePassed || ev.Result == EvidenceFailed
}

// dominantVersion 一组证据里出现最多的版本号（0 表示都没记）
func dominantVersion(evs []Evidence) int {
	count := map[int]int{}
	for _, ev := range evs {
		if ev.WorkflowVersion > 0 {
			count[ev.WorkflowVersion]++
		}
	}
	best, bestN := 0, 0
	for v, n := range count {
		if n > bestN || (n == bestN && v > best) {
			best, bestN = v, n
		}
	}
	return best
}
