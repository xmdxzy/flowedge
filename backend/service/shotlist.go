package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"flowedge/backend/model"
)

// 分镜规划节点（storyboard）：剧本/需求 → 结构化分镜表（JSON）
//
// 规则来源：开源短剧流水线（ai-short-film / aigc-production-flow / Moyin / Jellyfish / ArcReel）
// 的「一镜一任务 + 每镜带衔接类型」，以及视听语言的分镜方法：
// 景别=心理距离、角度=权力关系、轴线必须逐镜锁死、运镜一镜一动。
const storyboardInstruction = `你是短剧分镜师。把用户的内容需求拆成一份可直接执行的分镜表，只输出 JSON，不要解释。

分镜方法（必须遵守）：
1. 一镜一任务：每个镜头只承担一个叙事功能，只能有一个主导动作 + 可见收尾
2. 景别递进：情绪升级时由远到近（远景→中景→近景/特写）；相邻两镜不要「特写接特写」，也不要无理由「远景直接跳大特写」
3. 角度讲权力：强势方用仰拍，弱势/失败用俯拍，对峙不要全程平视；失序/疯狂才用荷兰角
4. 轴线：**同一场景内所有镜头的 axis 必须填同一个值**（全 A 侧或全 B 侧），不要逐镜换侧——
   换侧会越轴、观众会以为人物位置变了。确需换侧时，必须先插一个正面或空镜作为过渡，之后才换
5. 运镜一镜一动：固定 / 缓慢推近 / 缓慢拉远 / 横移 / 跟拍 / 摇，只能给一个，带幅度与速度
6. 时长按内容给，不必均分：3–8 秒，快的动作短、情绪/大全景长；总时长接近用户要求
7. 衔接逐镜声明 transition：
   - cut：换机位/换角度/多角度阐述（**默认用这个**）
   - continue：同一机位同一动作延续（只有真正连续才用，且时长一般 5 秒左右）
   - dissolve：时间跳跃或段落收束
8. 声音只做两层：一句「持续」的环境底噪（全片统一，写进 ambience）+ 与画面事件绑定的音效；音乐只在需要情绪推动的镜头出现，其余写「无音乐」
9. 台词默认「无对白」；只有用户明确给出台词时才写「身份：原话」，每句不超过 10 个字
10. character 字段写死角色外貌与衣着（H3 没有跨镜记忆，靠逐镜重复维持一致性）

输出 JSON 结构（严格照此，不要多字段）：
{
  "title": "片名",
  "style": "全局风格锚，一句话",
  "character": "角色锁定描述（外貌+衣着+道具，一句话写全）",
  "ambience": "全集统一的持续环境声，一句话",
  "shots": [
    {
      "id": 1,
      "seconds": 5.0,
      "shot_size": "中景",
      "angle": "平视",
      "axis": "A侧",
      "camera": "缓慢推近，小幅，慢速",
      "action": "某人做一件具体的事，直到某个可见结果",
      "light": "侧逆光，暖色，低对比",
      "ambience": "",
      "sfx": "与画面事件绑定的一个音效",
      "music": "无音乐",
      "dialogue": "无对白",
      "transition": "cut"
    }
  ]
}`

// execStoryboardNode 生成分镜表（走语言侧 LLM）
func (e *EngineV2) execStoryboardNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	brief, _ := node.Data["brief"].(string)
	if strings.TrimSpace(brief) == "" {
		brief = upstreamText(upstream)
	}
	if strings.TrimSpace(brief) == "" {
		return nil, fmt.Errorf("分镜规划缺少内容需求：连上游 text 或在 brief 里写清楚")
	}
	target := int(fieldFloat(node, "seconds", 30))
	maxShots := int(fieldFloat(node, "max_shots", 8))
	if maxShots < 1 {
		maxShots = 8
	}

	release := e.enterSide(sideLanguage)
	defer release()

	if e.llm == nil {
		return nil, fmt.Errorf("LLM 服务不可用，无法规划分镜")
	}
	user := fmt.Sprintf("目标总时长：%d 秒。镜头数量不超过 %d 个（时长按内容给，3–8 秒/镜）。\n内容需求：%s", target, maxShots, brief)
	raw, err := e.llm.ChatWithModel(storyboardInstruction+"\n\n"+user, e.llm.Model)
	if err != nil {
		return nil, err
	}
	plan, perr := ParseShotPlan(raw)
	if perr != nil {
		return nil, fmt.Errorf("%w（原始输出片段：%.200s）", perr, raw)
	}
	planJSON := toJSON(plan)
	return map[string]interface{}{
		"text":   planJSON, // 供下游 shotlist 直接吃
		"plan":   planJSON,
		"shots":  len(plan.Shots),
		"title":  plan.Title,
		"style":  plan.Style,
		"target": target,
	}, nil
}

// shotResult 单镜执行结果（写进入 run 的输出，便于回溯）
type shotResult struct {
	ID         int     `json:"id"`
	File       string  `json:"file"`
	Frames     int     `json:"frames"`
	Seconds    float64 `json:"seconds"`
	Transition string  `json:"transition"`
	Speech     bool    `json:"speech_detected"`
	Ducked     bool    `json:"audio_ducked"`
	Stripped   bool    `json:"center_stripped"`
	Verified   bool    `json:"audio_verified"`
	Reused     bool    `json:"reused,omitempty"` // 复用上次成功产物（提示词未变）
	Note       string  `json:"note,omitempty"`
}

// execShotListNode 分镜执行器：逐镜生成 → 音频治理 → 按 transition 装配
func (e *EngineV2) execShotListNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	rawPlan, _ := node.Data["plan"].(string)
	if strings.TrimSpace(rawPlan) == "" {
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if p, ok := m["plan"].(string); ok && strings.TrimSpace(p) != "" {
					rawPlan = p
					break
				}
				if t, ok := m["text"].(string); ok && strings.Contains(t, "\"shots\"") {
					rawPlan = t
					break
				}
			}
		}
	}
	if strings.TrimSpace(rawPlan) == "" {
		return nil, fmt.Errorf("分镜执行器缺少分镜表：接一个「分镜规划」节点，或在 plan 里贴 JSON")
	}
	plan, err := ParseShotPlan(rawPlan)
	if err != nil {
		return nil, err
	}

	backend, _ := node.Data["backend"].(string)
	if backend == "" {
		backend = "h3"
	}
	width := int(fieldFloat(node, "width", 608))
	height := int(fieldFloat(node, "height", 352))
	// 步数默认交给媒体层决定（turbo 下是 12）。这里不能写死 8——
	// 8 步是音频欠采样区间，正是「人声乱语」的成因之一（见 AGENTS §5.7）。
	steps := int(fieldFloat(node, "steps", 0))
	turbo := true
	if v, ok := node.Data["turbo"].(bool); ok {
		turbo = v
	}
	interp := false
	if v, ok := node.Data["interp"].(bool); ok {
		interp = v
	}
	audioCheck := true
	if v, ok := node.Data["audio_check"].(bool); ok {
		audioCheck = v
	}

	// 片长上限保护：8 镜 × 15s 已经很极端，再多应该拆成多集
	if len(plan.Shots) > 12 {
		plan.Shots = plan.Shots[:12]
	}

	release := e.enterSide(sideDiffusion)
	defer release()
	if backend == "h3" {
		if e.deps != nil {
			if err := e.deps.Start("comfy_h3"); err != nil {
				return nil, fmt.Errorf("H3 实例启动失败：%w", err)
			}
		}
	} else if err := e.ensureComfy(""); err != nil {
		return nil, err
	}

	clips := make([]string, 0, len(plan.Shots))
	transitions := make([]string, 0, len(plan.Shots))
	results := make([]shotResult, 0, len(plan.Shots))
	var prevLastFrame string

	// 镜头级作业：每镜成功即落盘，失败重跑时已完成的镜头直接复用（不重烧显存）。
	// runID=0 表示独立执行（没有 run），此时 store 不落盘，行为与旧版一致。
	runID := runIDFrom(ctx)
	reroll := parseReroll(node)
	reusedCount := 0

	for i := range plan.Shots {
		// 镜头之间响应取消。旧版循环体完全不看 ctx，点了停止也要等整片跑完。
		if cerr := ctx.Err(); cerr != nil {
			log.Printf("[shotlist] 第 %d/%d 镜前收到取消", i+1, len(plan.Shots))
			return nil, fmt.Errorf("已取消（完成 %d/%d 镜，已完成部分已落盘，重跑会复用）：%w",
				len(results), len(plan.Shots), cerr)
		}
		sh := plan.Shots[i]
		// 只有上一镜声明 continue 时才用末帧引导；其余一律独立生成（多角度才成立）
		continuity := i > 0 && plan.Shots[i-1].Transition == "continue" && prevLastFrame != ""
		prompt := CompileShotPrompt(plan, sh, continuity)
		hash := shotPromptHash(prompt, sh.Frames, width, height, steps, turbo)

		// 复用：同 run + 同节点 + 同镜号 + 同提示词哈希，且产物文件仍在盘上。
		if !reroll[i] {
			if job, ok := e.shotJobs.Lookup(runID, node.ID, i, hash); ok {
				clips = append(clips, job.File)
				transitions = append(transitions, sh.Transition)
				results = append(results, shotResult{
					ID: sh.ID, File: job.File, Frames: job.Frames, Seconds: job.Seconds,
					Transition: sh.Transition, Reused: true,
					Note: "复用上次成功产物（提示词与参数未变）",
				})
				reusedCount++
				e.emitShotProgress(runID, node.ID, i, len(plan.Shots), sh.ID, "reused", 0)
				if i < len(plan.Shots)-1 && sh.Transition == "continue" {
					tmp := filepath.Join(os.TempDir(), fmt.Sprintf("flowedge-shot-%d-%d.png", time.Now().UnixNano(), sh.ID))
					if err := extractLastFrame(job.File, tmp); err == nil {
						prevLastFrame = tmp
					}
				}
				continue
			}
		}

		req := MediaRequest{
			Kind: "h3", Prompt: prompt,
			Width: width, Height: height,
			Frames: sh.Frames, FPS: 24, Steps: steps,
			Turbo: turbo, LowVRAM: true,
		}
		if continuity {
			req.Kind = "h3_i2v"
			req.Image = prevLastFrame
		}

		e.emitShotProgress(runID, node.ID, i, len(plan.Shots), sh.ID, "generating", 0)
		genStart := time.Now()
		part, _, gerr := e.t2i.GenerateMedia(req)
		genElapsed := time.Since(genStart)
		if gerr != nil {
			e.shotJobs.Record(model.ShotJob{
				RunID: runID, NodeID: node.ID, ShotIndex: i, PromptHash: hash,
				ShotID: sh.ID, Prompt: prompt, Frames: sh.Frames, Transition: sh.Transition,
				Status: "failed", Error: gerr.Error(), ElapsedMS: genElapsed.Milliseconds(),
			})
			e.emitShotProgress(runID, node.ID, i, len(plan.Shots), sh.ID, "failed", genElapsed)
			// 不再 return nil 把前几镜一起丢掉：它们已经落盘，重跑会复用。
			// 报错里直接给出镜号与「只想重拍这一镜」的写法。
			return nil, fmt.Errorf("第 %d 镜生成失败（已完成 %d/%d 镜并已落盘，重跑会复用；只重拍这一镜请设 reroll=%d）：%w",
				sh.ID, len(results), len(plan.Shots), sh.ID, gerr)
		}

		res := shotResult{ID: sh.ID, File: part, Frames: sh.Frames, Seconds: float64(sh.Frames) / 24.0, Transition: sh.Transition}

		// 音频治理（三道防线里的第三道）：
		// 提示词层已经把声音字段填满，但模型仍有概率即兴说话。这里做客观抽检，
		// 检测到人声就不再只是"压低音量"，而是中置相消把它从信号里减掉。
		if audioCheck && backend == "h3" {
			if speech, checked := audioHasSpeech(e.asr, part); !checked {
				res.Note = "ASR 不可达，人声抽检未执行（不等于没人声）"
			} else if speech {
				res.Speech = true
				if cerr := stripSpeechCenter(part); cerr == nil {
					res.Stripped = true
					// 修完要复核：这是"验证修复"，不是"验证跑通"
					if again, ok := audioHasSpeech(e.asr, part); ok && !again {
						res.Verified = true
						res.Note = "检测到人声 → 中置相消完成，复核通过（人声消失、环境声保留）"
					} else {
						res.Note = "中置相消后仍检测到人声"
						if derr := duckAudio(part); derr == nil {
							res.Ducked = true
							res.Note += "，已追加压音兜底"
						}
					}
				} else if derr := duckAudio(part); derr == nil {
					res.Ducked = true
					res.Note = "中置相消失败（可能非立体声），已退回压音兜底"
				} else {
					res.Note = "中置相消与压音都失败，该镜音频需人工处理"
				}
			}
		}
		clips = append(clips, part)
		transitions = append(transitions, sh.Transition)
		results = append(results, res)

		// 立刻落盘：这是「第 5 镜失败不丢前 4 镜」的全部依据。
		e.shotJobs.Record(model.ShotJob{
			RunID: runID, NodeID: node.ID, ShotIndex: i, PromptHash: hash,
			ShotID: sh.ID, Prompt: prompt, File: part, Frames: sh.Frames,
			Seconds: res.Seconds, Transition: sh.Transition, Note: res.Note,
			Status: "success", ElapsedMS: genElapsed.Milliseconds(),
		})
		e.emitShotProgress(runID, node.ID, i, len(plan.Shots), sh.ID, "done", genElapsed)

		// 取末帧供下一镜续接（仅当下一镜需要 continue 时才真的会用到）
		if i < len(plan.Shots)-1 && sh.Transition == "continue" {
			tmp := filepath.Join(os.TempDir(), fmt.Sprintf("flowedge-shot-%d-%d.png", time.Now().UnixNano(), sh.ID))
			if err := extractLastFrame(part, tmp); err == nil {
				prevLastFrame = tmp
			}
		}
	}

	// 装配：cut=1 帧、continue=2 帧、dissolve=0.3 秒的交叉淡化
	outDir := e.t2i.OutputDir
	if outDir == "" {
		outDir = "data/generated"
	}
	_ = os.MkdirAll(outDir, 0755)
	final := filepath.Join(outDir, fmt.Sprintf("film-%d-shots.mp4", time.Now().UnixNano()))
	if err := assembleShots(clips, transitions, final); err != nil {
		return nil, err
	}
	if interp {
		f2 := strings.TrimSuffix(final, ".mp4") + "-48.mp4"
		if err := interpolateVideo(final, f2, 48); err == nil {
			final = f2
		}
	}
	if abs, aerr := filepath.Abs(final); aerr == nil {
		final = abs
	}
	// 成片自检：数帧 + 音画时长比对。肉眼看时长看不出「中间整段画面丢失」，
	// 但流水线必须自己知道。自检失败不影响产出，只把问题显式挂到输出上。
	check, _ := inspectMedia(final)
	if !check.OK {
		for _, is := range check.Issues {
			log.Printf("[shotlist] 成片自检未通过：%s", is)
		}
	}

	total := 0.0
	for _, r := range results {
		total += r.Seconds
	}
	planJSON := toJSON(results)
	// 按镜明细：界面据此渲染「第 3/6 镜 · 复用 / 新生成 · 耗时」，也是 reroll 的依据
	jobs := e.shotJobs.ListByNode(runID, node.ID)
	return map[string]interface{}{
		"video":         final,
		"kind":          "video",
		"text":          plan.Title,
		"shots":         len(results),
		"seconds":       total,
		"shots_reused":  reusedCount,
		"shot_jobs":     toJSON(jobs),
		"detail":        planJSON,
		"check":         toJSON(check),
		"check_ok":      check.OK,
	}, nil
}


// toJSON 序列化辅助（失败时给空对象，不让输出字段撑爆）
func toJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
