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

	release := e.enterLanguageSide("")
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
	Reused     bool    `json:"reused,omitempty"`   // 复用上次成功产物（提示词未变）
	Attempts   int     `json:"attempts,omitempty"` // 段级门重试轮数（含首次）
	Gate       string  `json:"gate,omitempty"`     // 段级门结论（台词验词 / 人声抽检）
	Note       string  `json:"note,omitempty"`
}

// execShotListNode 分镜执行器：五层门预检 → 逐镜生成 → 音频治理 → 按 transition 装配
//
// 相对旧版的三处升级（nodecontract 早就写着 shotlist「承载全部五层门」，这里兑现）：
//  1. 门检预检：plan.gates 声明过的门开机前逐条校验，硬违约直接拦（不烧显存），
//     WARN 显式进 gate_report——「声明有门」与「门真有牙齿」之间的缺口在这闭合
//  2. 富模式接入：characters 定妆图 → 每镜 h3_r2v（身份由图像锁，不靠文字描述）；
//     lines 台词表 → TTS 念稿音轨走 ref_audio（fully_copy，模型只对口型）
//  3. 段级音频门分流：台词段 ASR 验词（不过按 gates.audio.segment_retry 重跑），
//     氛围段保留中置相消三道防线——旧版把有台词的段也当氛围消人声，方向是反的
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

	// ---- 一致性门（开机前）：审分镜表的轴线/光比/场景锁 ----
	//
	// 放在这里而不是出片后：这个 plan 一旦漂了，后面每一镜都会忠实执行漂移，
	// 抽帧比对只能在 40 分钟后告诉你"前面白拍"。
	consistencyCheck := true
	if v, ok := node.Data["consistency_check"].(bool); ok {
		consistencyCheck = v
	}
	var consistencyIssues []ShotConsistencyIssue
	if consistencyCheck {
		consistencyIssues = AuditPlanConsistency(plan)
		if fails := consistencyFails(consistencyIssues); len(fails) > 0 {
			return nil, fmt.Errorf("一致性门未通过（未开机，未烧显存）：\n%s", consistencyFailMessage(fails))
		}
		for _, it := range consistencyIssues {
			log.Printf("[shotlist] 一致性告警 %s: %s", it.Code, it.Msg)
		}
	}

	backend, _ := node.Data["backend"].(string)
	if backend == "" {
		backend = "h3"
	}
	width := int(fieldFloat(node, "width", 608))
	height := int(fieldFloat(node, "height", 352))
	// 富模式 plan 可自带分辨率声明（节点参数优先还是 plan 优先？plan 优先——
	// 它是为本片设计的，节点默认值只是兜底）
	if plan.Width > 0 {
		width = plan.Width
	}
	if plan.Height > 0 {
		height = plan.Height
	}
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
	ambienceGain := fieldFloat(node, "ambience_gain", 0.85)
	requireGates := false
	if v, ok := node.Data["require_gates"].(bool); ok {
		requireGates = v
	}

	// 片长上限保护：8 镜 × 15s 已经很极端，再多应该拆成多集
	if len(plan.Shots) > 12 {
		plan.Shots = plan.Shots[:12]
	}

	// ---- 参考图解析：plan.characters（相对路径按 CWD 解析）+ 节点级覆盖 ----
	// 身份由图像锁：手册明言「不靠文字描述」。旧版 shotlist 不读 image 端口、
	// 不传 RefImages，定妆图根本进不去——这是与 text2video 的最大能力差。
	refImgs := append([]string{}, plan.Characters...)
	if ov := stringList(node.Data, "ref_images"); len(ov) > 0 {
		refImgs = ov
	}
	for i := range refImgs {
		if !filepath.IsAbs(refImgs[i]) {
			if abs, aerr := filepath.Abs(refImgs[i]); aerr == nil {
				refImgs[i] = abs
			}
		}
	}

	// ---- 五层门预检（开机前拦截）----
	verdict, gates, gateStats := CheckFilmGates(buildGatePlanMap(plan, refImgs, width, height), requireGates)
	if verdict.HasFail() {
		return nil, fmt.Errorf("五层门预检未通过（未开机，未烧显存）：\n%s", gateFailMessage(verdict))
	}
	for _, it := range verdict.Warns() {
		log.Printf("[shotlist] 门告警 %s: %s", it.Gate, it.Msg)
	}
	perSegCap := gateInt(gateMapGet(gates, "budget", "per_segment_wall_max"), 0)
	wallCap := gateInt(gateMapGet(gates, "budget", "wall_seconds_max"), 0)
	onExceed := gateStr(gateMapGet(gates, "budget", "on_exceed"))
	segRetry := gateInt(gateMapGet(gates, "audio", "segment_retry"), 1)
	onSegFail := gateStr(gateMapGet(gates, "audio", "on_segment_fail"))
	if onSegFail == "" {
		onSegFail = "retry"
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

	outDir := e.t2i.OutputDir
	if outDir == "" {
		outDir = "data/generated"
	}
	_ = os.MkdirAll(outDir, 0755)

	clips := make([]string, 0, len(plan.Shots))
	transitions := make([]string, 0, len(plan.Shots))
	results := make([]shotResult, 0, len(plan.Shots))
	var prevLastFrame string
	nodeStart := time.Now()

	// 镜头级作业：每镜成功即落盘，失败重跑时已完成的镜头直接复用（不重烧显存）。
	// runID=0 表示独立执行（没有 run），此时 store 不落盘，行为与旧版一致。
	runID := runIDFrom(ctx)
	reusedCount := 0

	for i := range plan.Shots {
		// 镜头之间响应取消。旧版循环体完全不看 ctx，点了停止也要等整片跑完。
		if cerr := ctx.Err(); cerr != nil {
			log.Printf("[shotlist] 第 %d/%d 镜前收到取消", i+1, len(plan.Shots))
			return nil, fmt.Errorf("已取消（完成 %d/%d 镜，已完成部分已落盘，重跑会复用）：%w",
				len(results), len(plan.Shots), cerr)
		}
		// 预算总闸：on_exceed=fail 时超了就地停机（已完成的镜已落盘，重跑复用）
		if wallCap > 0 && onExceed == "fail" {
			used := time.Since(nodeStart).Seconds()
			if used > float64(wallCap) {
				return nil, fmt.Errorf("预算门：已用 %.0fs 超过总预算 %ds（on_exceed=fail），停机；已完成 %d/%d 镜已落盘，重跑会复用",
					used, wallCap, len(results), len(plan.Shots))
			}
		}
		sh := plan.Shots[i]
		// 只有上一镜声明 continue 时才用末帧引导；其余一律独立生成（多角度才成立）
		continuity := i > 0 && plan.Shots[i-1].Transition == "continue" && prevLastFrame != ""
		// 提示词：富模式手写六段式优先；结构化模式字段编译
		prompt := sh.Prompt
		if strings.TrimSpace(prompt) == "" {
			prompt = CompileShotPrompt(plan, sh, continuity)
		}

		// 台词段：合成念稿音轨（TTS）。失败 = 该镜失败——不能拿假音频糊弄，
		// fully_copy 会把任何垃圾原样带进成片。
		dialogue := sh.HasDialogue()
		var dubPath string
		var dub *DubTrackReport
		if dialogue && backend == "h3" {
			lines := sh.Lines
			if len(lines) == 0 {
				// 结构化模式的台词在 dialogue 字段（「身份：原话」）
				if t := sh.DialogueText(); t != "" {
					lines = []PlanLine{{Speaker: "S1", Text: t}}
				}
			}
			dubPath = dubTrackTempPath(outDir, sh.ID)
			gain := sh.AmbienceGain
			if gain <= 0 {
				gain = ambienceGain // 节点级兜底（默认 0.85）
			}
			dub, err = SynthesizeDubTrack(e.tts, lines, dubPath,
				float64(sh.Frames)/24.0, sh.Lead, gain, plan.AmbienceFile(sh))
			if err != nil {
				return nil, fmt.Errorf("第 %d 镜念稿音轨合成失败：%w", sh.ID, err)
			}
		}

		req := MediaRequest{
			Kind: "h3", Prompt: prompt,
			Width: width, Height: height,
			Frames: sh.Frames, FPS: 24, Steps: steps,
			Turbo: turbo, LowVRAM: true,
		}
		switch {
		case dubPath != "":
			// 台词段必须走 ref2va：只有它吃 ref_audio（念稿 1:1 透传 + 口型同步）。
			// 与末帧续接互斥时音频确定性优先——i2v 根本没有音频输入口。
			req.Kind = "h3_r2v"
			req.RefImages = refImgs
			req.RefAudios = []string{dubPath}
		case len(refImgs) > 0 && !continuity:
			// 有定妆图且不是续接镜：身份交给参考图（ref2va 自己管身份，
			// 与 AddGuide 互斥，所以这里显式不让位首帧）
			req.Kind = "h3_r2v"
			req.RefImages = refImgs
		case continuity:
			req.Kind = "h3_i2v"
			req.Image = prevLastFrame
		}

		// 生成 + 段级门：台词段 ASR 验词，不过按 segment_retry 重跑
		// （采样有随机性，重跑比放过一镜乱语便宜得多）。
		var part string
		var gerr error
		var genElapsed time.Duration
		attempts := 0
		gateNote := ""
		for {
			attempts++
			e.emitShotProgress(runID, node.ID, i, len(plan.Shots), sh.ID, "generating", 0)
			genStart := time.Now()
			part, _, gerr = e.t2i.GenerateMedia(req)
			genElapsed = time.Since(genStart)
			if gerr != nil {
				e.emitShotProgress(runID, node.ID, i, len(plan.Shots), sh.ID, "failed", genElapsed)
				return nil, fmt.Errorf("第 %d 镜生成失败（已完成 %d/%d 镜并已落盘）：%w",
					sh.ID, len(results), len(plan.Shots), gerr)
			}
			if !dialogue {
				break
			}
			expect := sh.DialogueText()
			passed, checked := verifyDialogueLine(e.asr, part, expect)
			if !checked {
				// ASR 不可达 = 「没验」，不是「念对了」。显式记下，不冒充通过。
				gateNote = "台词校验未执行（ASR 不可达），不等于念对"
				break
			}
			if passed {
				gateNote = "ASR 验词通过"
				break
			}
			gateNote = fmt.Sprintf("台词「%s」验词未过（第 %d 次）", expect, attempts)
			if attempts > segRetry || onSegFail != "retry" {
				e.emitShotProgress(runID, node.ID, i, len(plan.Shots), sh.ID, "failed", genElapsed)
				return nil, fmt.Errorf("第 %d 镜段级门未通过：台词「%s」验词 %d 次未转出（已完成 %d/%d 镜已落盘）",
					sh.ID, expect, attempts, len(results), len(plan.Shots))
			}
			log.Printf("[shotlist] 第 %d 镜第 %d 次验词未过，重跑（采样有随机性）", sh.ID, attempts)
		}

		res := shotResult{ID: sh.ID, File: part, Frames: sh.Frames, Seconds: float64(sh.Frames) / 24.0,
			Transition: sh.Transition, Attempts: attempts, Gate: gateNote}
		if dub != nil {
			res.Note = fmt.Sprintf("念稿音轨 %.2fs（%s 后端，台词 %d 句）", dub.Seconds, dub.Backend, len(dub.Lines))
		}

		// 氛围段音频治理（三道防线里的第三道）：
		// 提示词层已经把声音字段填满，但模型仍有概率即兴说话。这里做客观抽检，
		// 检测到人声就不再只是"压低音量"，而是中置相消把它从信号里减掉。
		// 台词段**不能**走这条——中置相消会把台词本身消掉。
		if audioCheck && backend == "h3" && !dialogue {
			if speech, checked := audioHasSpeech(e.asr, part); !checked {
				res.Note = appendNote(res.Note, "ASR 不可达，人声抽检未执行（不等于没人声）")
			} else if speech {
				res.Speech = true
				if cerr := stripSpeechCenter(part); cerr == nil {
					res.Stripped = true
					// 修完要复核：这是"验证修复"，不是"验证跑通"
					if again, ok := audioHasSpeech(e.asr, part); ok && !again {
						res.Verified = true
						res.Note = appendNote(res.Note, "检测到人声 → 中置相消完成，复核通过（人声消失、环境声保留）")
					} else {
						res.Note = appendNote(res.Note, "中置相消后仍检测到人声")
						if derr := duckAudio(part); derr == nil {
							res.Ducked = true
							res.Note = appendNote(res.Note, "，已追加压音兜底")
						}
					}
				} else if derr := duckAudio(part); derr == nil {
					res.Ducked = true
					res.Note = appendNote(res.Note, "中置相消失败（可能非立体声），已退回压音兜底")
				} else {
					res.Note = appendNote(res.Note, "中置相消与压音都失败，该镜音频需人工处理")
				}
			}
		}
		clips = append(clips, part)
		transitions = append(transitions, sh.Transition)
		results = append(results, res)

		// 立刻落盘：这是「第 5 镜失败不丢前 4 镜」的全部依据。
		e.emitShotProgress(runID, node.ID, i, len(plan.Shots), sh.ID, "done", genElapsed)

		if perSegCap > 0 && genElapsed.Seconds() > float64(perSegCap) {
			log.Printf("[shotlist] 预算告警：第 %d 镜用了 %.0fs，超过单段上限 %ds",
				sh.ID, genElapsed.Seconds(), perSegCap)
		}

		// 取末帧供下一镜续接（仅当下一镜需要 continue 时才真的会用到）
		if i < len(plan.Shots)-1 && sh.Transition == "continue" {
			tmp := filepath.Join(os.TempDir(), fmt.Sprintf("flowedge-shot-%d-%d.png", time.Now().UnixNano(), sh.ID))
			if err := extractLastFrame(part, tmp); err == nil {
				prevLastFrame = tmp
			}
		}
	}

	// 装配：cut=1 帧、continue=2 帧、dissolve=0.3 秒的交叉淡化
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

	// 门检报告随成片落输出：预检结论 + 合并后的门 + 统计 + 预算实耗。
	// 证据信封的判据是「门有没有留下 checks」——没有报告等于没测。
	gateReport := map[string]interface{}{
		"verdict": verdict.Worst(),
		"items":   verdict.Items,
		"stats":   gateStats,
		"gates":   gates,
		"budget": map[string]interface{}{
			"wall_used_seconds":    roundTo(time.Since(nodeStart).Seconds(), 1),
			"wall_seconds_max":     wallCap,
			"per_segment_wall_max": perSegCap,
		},
		"enforced": true,
	}
	return map[string]interface{}{
		"video":        final,
		"kind":         "video",
		"parts":        clips,   // 逐镜产物（有序）——一致性门按这个顺序比段间漂移
		"plan":         rawPlan, // 回传分镜表：字幕节点按它排轴（台词时点唯一来源）
		"text":         plan.Title,
		"shots":        len(results),
		"seconds":      total,
		"shots_reused": reusedCount,
		"detail":       planJSON,
		"check":        toJSON(check),
		"check_ok":     check.OK,
		"gate_report":  toJSON(gateReport),
		"consistency":  toJSON(consistencyIssues),
		"gate_ok":      !verdict.HasFail(),
		"gate_verdict": verdict.Worst(),
	}, nil
}

// buildGatePlanMap 把解析后的 ShotPlan 归一成门检用的 plan map。
// CheckFilmGates 只认 segments/characters/width/height/gates 四种键——
// 结构化分镜表在这里编译成与富模式同构的视图，两种 schema 走同一套门。
func buildGatePlanMap(plan *ShotPlan, refImgs []string, width, height int) map[string]interface{} {
	segs := make([]interface{}, 0, len(plan.Shots))
	for i := range plan.Shots {
		sh := &plan.Shots[i]
		prompt := sh.Prompt
		if strings.TrimSpace(prompt) == "" {
			// 门检用无首帧版：首帧锚定那句不影响任何门检项
			prompt = CompileShotPrompt(plan, *sh, false)
		}
		seg := map[string]interface{}{
			"id":     sh.ID,
			"frames": sh.Frames,
			"prompt": prompt,
		}
		if sh.RefImages != nil {
			seg["ref_images"] = *sh.RefImages
		}
		if len(sh.Lines) > 0 {
			lns := make([]interface{}, 0, len(sh.Lines))
			for _, ln := range sh.Lines {
				lns = append(lns, map[string]interface{}{
					"speaker": ln.Speaker, "text": ln.Text,
				})
			}
			seg["lines"] = lns
		} else if t := sh.DialogueText(); t != "" {
			// 结构化模式的台词在 dialogue 字段，补成台词表视图，
			// 让 line_mismatch 检查同样覆盖结构化分镜
			seg["lines"] = []interface{}{map[string]interface{}{"speaker": "S1", "text": t}}
		}
		segs = append(segs, seg)
	}
	pm := map[string]interface{}{
		"segments": segs,
		"width":    width,
		"height":   height,
	}
	if len(refImgs) > 0 {
		chars := make([]interface{}, 0, len(refImgs))
		for _, c := range refImgs {
			chars = append(chars, c)
		}
		pm["characters"] = chars
	}
	if len(plan.Gates) > 0 {
		pm["gates"] = plan.Gates
	}
	return pm
}

// verifyDialogueLine ASR 验词：转写片段，核对期望原话是否出现（忽略空白差异）。
// checked=false 表示「没验到」（ASR 不可达/无期望词），调用方必须与「没念对」分开。
func verifyDialogueLine(asr *ASRService, path, expect string) (passed, checked bool) {
	if asr == nil || strings.TrimSpace(expect) == "" {
		return false, false
	}
	text, err := asr.Transcribe(path)
	if err != nil {
		return false, false
	}
	return strings.Contains(stripAllSpace(text), stripAllSpace(expect)), true
}

// gateFailMessage 门检失败的完整违约清单（用户照着改 plan，不用翻日志）
func gateFailMessage(v *GateVerdict) string {
	var b strings.Builder
	for _, it := range v.Fails() {
		b.WriteString("  [FAIL] ")
		b.WriteString(it.Gate)
		b.WriteString(": ")
		b.WriteString(it.Msg)
		b.WriteString("\n")
	}
	b.WriteString("改完 plan.gates 或分镜表再跑；门的意义是开机前拦错，不是跑完再骂。")
	return b.String()
}

// toJSON 序列化辅助（失败时给空对象，不让输出字段撑爆）
func toJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ShotListBudget 整节点预算 = 各镜预算之和 + 装配/自检/插帧。
func ShotListBudget(shotCount, totalFrames int) time.Duration {
	if shotCount <= 0 {
		return 2 * time.Hour
	}
	d := time.Duration(float64(totalFrames)*1.2)*time.Second +
		time.Duration(shotCount)*90*time.Second +
		10*time.Minute
	if d < 15*time.Minute {
		d = 15 * time.Minute
	}
	return d
}

// emitShotProgress 每镜广播一次进度：界面显示「第 3/6 镜」而不是一个转圈的点。
func (e *EngineV2) emitShotProgress(runID uint, nodeID string, idx, total, shotID int, phase string, elapsed time.Duration) {
	if e == nil {
		return
	}
	ev := Event{
		Type:   "shot_progress",
		NodeID: nodeID,
		RunID:  runID,
		Data: map[string]interface{}{
			"index":      idx + 1,
			"total":      total,
			"shot_id":    shotID,
			"phase":      phase, // generating / failed / done
			"elapsed_ms": elapsed.Milliseconds(),
		},
	}
	if e.wsHub != nil {
		e.wsHub.Broadcast(ev)
	}
	if e.eventLog != nil {
		e.eventLog.Emit(runID, ev)
	}
}

// appendNote 拼接备注（空值不产生多余分隔符）
func appendNote(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "；" + b
}
