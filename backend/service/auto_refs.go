package service

// 参考帧自动收集（V3 P0 ①，GroundShot arXiv 2606.20799 的工程化）
//
// 问题：多镜身份一致性现在靠人肉挑 3 张定妆图（`plan.Characters` / `ref_images`）。
// AGENTS 已记：「只给 characters + 普通提示，第 2 镜会漂成黑发女仆；Ref2VA 六段式 + 3 张参考图才锁住」。
// 也就是说，一致性天花板取决于**参考图从哪来**，而不是模型强不强。
//
// GroundShot 的观察：角色"首次清晰出现"的那一镜决定后续一致性的上限。
// 所以正确顺序是：先拍最容易拍清的一镜 → 从它抽帧 → 当后续镜的参考。
//
// 本文件只做「从已完成的镜里挑帧」，不改生成链路；是否启用由调用方开关决定（默认关）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// AutoRefConfig 自动参考帧配置。
type AutoRefConfig struct {
	Enabled       bool
	Max           int // 最多收集几张（默认 3：Ref2VA 六段式吃 3 张）
	FramesPerShot int // 每镜抽几张（默认 1）
}

// ParseAutoRefConfig 从节点 data 读开关（`auto_ref_frames` / `auto_ref_max`）。
func ParseAutoRefConfig(nodeData map[string]interface{}) AutoRefConfig {
	cfg := AutoRefConfig{Max: 3, FramesPerShot: 1}
	if nodeData == nil {
		return cfg
	}
	if v, ok := nodeData["auto_ref_frames"].(bool); ok {
		cfg.Enabled = v
	}
	if v, ok := nodeData["auto_ref_max"].(float64); ok && v > 0 {
		cfg.Max = int(v)
		if cfg.Max > 6 {
			cfg.Max = 6 // 超过 6 张对 Ref2VA 没有额外收益，只会拖慢上传
		}
	}
	return cfg
}

// AutoRefShot 一镜的可用于选帧的元信息。
type AutoRefShot struct {
	Path        string
	HasDialogue bool   // 台词镜：口型/表情在动，抽到的帧容易糊
	Transition  string // continue 镜与上一镜高度相似，参考价值低
	Seconds     float64
}

// rankAutoRefShots 排序：无台词 > 有台词，非 continue > continue，时长长者优先。
//
// 为什么时长优先：短镜（<2s）画面信息少，抽出来的帧常是过渡态（运动模糊）。
func rankAutoRefShots(shots []AutoRefShot) []AutoRefShot {
	out := append([]AutoRefShot{}, shots...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.HasDialogue != b.HasDialogue {
			return !a.HasDialogue
		}
		ac := a.Transition == "continue"
		bc := b.Transition == "continue"
		if ac != bc {
			return !ac
		}
		return a.Seconds > b.Seconds
	})
	return out
}

// CollectAutoRefs 从已完成的镜里抽帧，凑够 cfg.Max 张。
//
// 返回的路径是**新抽出来的 PNG**（放在 outDir），不是原视频 —— 参考图必须能直接喂给模型。
// 抽不出来的镜跳过（不报错），全抽不出来才返回错误：一条参考都没有时，
// 调用方应当退回"人肉挑图"或直接不带参考跑，而不是拿空列表冒充"有参考"。
func CollectAutoRefs(ctx context.Context, shots []AutoRefShot, outDir string, cfg AutoRefConfig) ([]string, error) {
	if !cfg.Enabled || cfg.Max <= 0 {
		return nil, nil
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	var refs []string
	var failures []string
	for idx, sh := range rankAutoRefShots(shots) {
		if len(refs) >= cfg.Max {
			break
		}
		if sh.Path == "" {
			continue
		}
		if _, err := os.Stat(sh.Path); err != nil {
			failures = append(failures, filepath.Base(sh.Path)+": 文件不存在")
			continue
		}
		perShot := cfg.FramesPerShot
		if perShot <= 0 {
			perShot = 1
		}
		if need := cfg.Max - len(refs); perShot > need {
			perShot = need
		}
		// 每镜一个子目录：ExtractFramesUniform 的产物名是固定的 vlmf_%02d.png，
		// 同一个目录连抽两镜会互相覆盖（第二镜只剩 1 张，且第一镜的丢了）。
		shotDir := filepath.Join(outDir, fmt.Sprintf("shot-%02d", idx))
		frames, err := ExtractFramesUniform(ctx, sh.Path, shotDir, perShot, sh.Seconds)
		if err != nil {
			failures = append(failures, filepath.Base(sh.Path)+": "+err.Error())
			continue
		}
		refs = append(refs, frames...)
	}
	if len(refs) == 0 {
		if len(failures) > 0 {
			return nil, fmt.Errorf("自动参考帧：所有候选镜都抽帧失败（%v）", failures)
		}
		return nil, fmt.Errorf("自动参考帧：没有可用的候选镜（还没有任何一镜生成成功）")
	}
	return refs, nil
}
