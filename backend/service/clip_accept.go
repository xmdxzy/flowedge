package service

import (
	"fmt"
	"log"
	"math"
	"strings"
)

// 单镜验收：这一镜自己能不能用。
//
// 为什么要在装配之前逐镜判一次（2026-10-25）：
// 成片终检只能告诉你「整条片坏了」，定位不到是哪一镜——6 镜 13 分钟的产出，
// 最后只报一句 av_drift 0.21s，人得手工一镜一镜查。
// 逐镜验收把定位做在生成侧：坏镜当场记号，定点重拍，好镜不动。
//
// 判据只认「客观可测且与观感相关」的三项，不做审美判断：
//  1. 有音轨（H3 的产物必带音频；没有就是管线出口错了）
//  2. 帧数不少于计划的 95%（xfade 接缝会合法吃掉几帧，所以留 5%）
//  3. 音画时长差 ≤ 0.2s（单镜内的漂移，成片级另有终检门）
func clipAcceptable(path string, sh Shot, driftTol float64) (bool, []string) {
	if driftTol <= 0 {
		driftTol = 0.2
	}
	info, err := inspectMedia(path)
	if err != nil {
		return false, []string{"读不到媒体信息：" + err.Error()}
	}
	issues := make([]string, 0, 3)
	if info.VSeconds <= 0 {
		issues = append(issues, "视频时长为 0（容器坏了或没有视频流）")
	}
	if !info.HasAudio {
		issues = append(issues, "没有音轨（H3 产物应带立体声）")
	}
	if sh.Frames > 0 && info.Frames > 0 {
		want := int(float64(sh.Frames) * 0.95)
		if info.Frames < want {
			issues = append(issues, fmt.Sprintf("帧数 %d < 计划 %d 的 95%%（%d）", info.Frames, sh.Frames, want))
		}
	}
	if info.HasAudio && info.VSeconds > 0 && info.ASeconds > 0 {
		if d := math.Abs(info.VSeconds - info.ASeconds); d > driftTol {
			issues = append(issues, fmt.Sprintf("音画时长差 %.3fs > %.2fs", d, driftTol))
		}
	}
	return len(issues) == 0, issues
}

// markNeedsReshoot 逐镜验收，返回需要重拍的镜位（下标）。
// 抽成纯函数是为了能单测「谁该重拍」这个决策本身——
// 它决定了会多烧几分钟显存，判错一次就是白花钱或少花钱买坏片。
func markNeedsReshoot(clips []string, shots []Shot, driftTol float64) []int {
	bad := make([]int, 0, len(clips))
	for i := range clips {
		if i >= len(shots) {
			break
		}
		if ok, issues := clipAcceptable(clips[i], shots[i], driftTol); !ok {
			bad = append(bad, i)
			log.Printf("[shotlist] 第 %d 镜验收不过（%s），标记重拍", shots[i].ID, strings.Join(issues, "；"))
		}
	}
	return bad
}
