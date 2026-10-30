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
	"unicode"

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

镜头语法词表（shot_size/angle/camera/light 从词表取词；light 可追加色彩与方向修饰）：
- shot_size：大远景 / 远景 / 全景 / 中景 / 中近景 / 近景 / 特写 / 大特写
- angle：平视 / 仰拍 / 俯拍 / 顶拍 / 荷兰角
- camera：固定 / 缓慢推近 / 缓慢拉远 / 横移 / 跟拍 / 摇
- light：正午硬光 / 阴天柔光 / 黄金时刻 / 蓝调时刻 / 夜景霓虹 / 冷月光 / 烛光暖光 / 侧逆光

节奏型（按节拍给镜头组选景别序列，相邻两镜景别只差一档，不要逐镜随机）：
- 建立序列：远景/全景（固定或缓慢推近）→ 中景 → 近景
- 对话序列：中景（双人）→ 近景 → 特写（情绪点）
- 冲突序列：近景跟拍 → 特写 → 全景（拉开距离看局势）
- 收束序列：中景 → 全景（缓慢拉远，留白）

结构约束（给了节拍表就逐镜对齐；没给就按六拍名自判）：
11. 首镜必须是钩子：悬念/奇观/冲突预告之一，不许平铺直叙交代背景
12. 每镜的 beat 填所属节拍名（hook/setup/develop/clash/turn/finish）；转折拍（turn）优先近景/特写并给音乐
13. 相邻镜头的运镜与景别不许完全相同（全片「固定+中景」会像幻灯片）

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
      "beat": "hook",
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
	style, _ := node.Data["style"].(string)
	characters, _ := node.Data["characters"].(string)

	release := e.enterLanguageSide("")
	defer release()

	if e.llm == nil {
		return nil, fmt.Errorf("LLM 服务不可用，无法规划分镜")
	}
	// 两段式：先结构（故事/角色圣经/节拍表）后分镜；结构层失败自动回退单段式
	plan, bp, err := planShotCreatively(e.llm, e.llm.Model, brief, style, characters, target, maxShots)
	if err != nil {
		return nil, err
	}
	planJSON := toJSON(plan)
	out := map[string]interface{}{
		"text":   planJSON, // 供下游 shotlist 直接吃
		"plan":   planJSON,
		"shots":  len(plan.Shots),
		"title":  plan.Title,
		"style":  plan.Style,
		"target": target,
	}
	if bp != nil {
		out["structure"] = toJSON(bp) // 结构层产物（故事/角色/节拍），供审阅与回放
	}
	return out, nil
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

// batchRunLen 从 start 数连续可同批的镜数（含 start；<2 表示不能批量）。
// 判据全部由调用方注入（纯函数，便于单测）：
//   - canBatch(j)：这一镜本身可以进批。台词镜能否进批由 shotlist 的
//     batch_dialogue 决定（默认关：热态实测略慢，见 execShotListNode 里的数字），
//     非台词镜还要求缓存未命中；
//   - same(j)：这一镜与第 start 镜同签名（kind / steps / 低显存档一致）——
//     批量图共用一份模型链，签名不同的镜混进来只会整批重载。
//
// start 自己不能是续接镜（prevContinued），组内上一镜声明 continue 就断组
// （下一镜要等它的末帧，末帧只有生成完才有）。
func batchRunLen(shots []Shot, start, maxN int, prevContinued bool, canBatch, same func(int) bool) int {
	if start >= len(shots) || prevContinued || !canBatch(start) {
		return 0
	}
	n := 1
	for j := start + 1; j < len(shots) && n < maxN; j++ {
		if shots[j-1].Transition == "continue" {
			break
		}
		if !canBatch(j) || !same(j) {
			break
		}
		n++
	}
	return n
}

// shotDubLines 取一镜要念的台词行：结构化 lines 优先，退到 dialogue 字段
// （「身份：原话」）。单镜路径与批量路径（TTS 前置）共用，两份逻辑迟早漂移。
func shotDubLines(sh Shot) []PlanLine {
	if len(sh.Lines) > 0 {
		return sh.Lines
	}
	if t := sh.DialogueText(); t != "" {
		return []PlanLine{{Speaker: "S1", Text: t}}
	}
	return nil
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
	// 音频后期（装配后）：响度归一默认开，BGM 默认关（没有素材就不该假装有配乐）
	audioMaster := true
	if v, ok := node.Data["audio_master"].(bool); ok {
		audioMaster = v
	}
	lufs := fieldFloat(node, "lufs", -16)
	if lufs > 0 {
		lufs = -16 // LUFS 天然为负：传正数视为没配
	}
	bgmPath, _ := node.Data["bgm"].(string)
	bgmGain := fieldFloat(node, "bgm_gain", -18)
	bgmDuck := true
	if v, ok := node.Data["bgm_duck"].(bool); ok {
		bgmDuck = v
	}
	// 整片声床（room tone）：默认开。2026-10-29 多镜实测：TTS 前导静音已裁、
	// 接缝已有 acrossfade，接缝处仍有 0.85s 可听静音——裁剪修不了「没有声音的
	// 时间段」，只能铺一层低于节目声的连续底噪。room_tone_gain = 期望底噪 dBFS。
	roomTone := true
	if v, ok := node.Data["room_tone"].(bool); ok {
		roomTone = v
	}
	roomToneFloor := fieldFloat(node, "room_tone_gain", -36)
	if roomToneFloor >= 0 {
		roomToneFloor = -36 // dBFS 天然为负：传正数视为没配
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
	// 多镜批量提交（P2 结构优化）：batch_size ≥ 2 时，把「连续、非台词、参数一致、
	// 缓存未命中」的镜合并成一张图提交（换权重 3N→3，直连实测 3 镜 ~2 倍速）。
	// 0/1 = 关。批量失败自动回退逐镜，不构成新门槛。
	batchSize := int(fieldFloat(node, "batch_size", 0))
	if batchSize > 8 {
		batchSize = 8 // 屏障节点只开了 8 个槽（comfy_media_batch.go）
	}
	// 台词镜是否进批：默认关。实测（2026-10-30，同分镜 2 台词镜 608×352×124）：
	//   批量一张图 221s / 229s / 292s（三次，方差来自 VAE decode 换页）；
	//   逐镜热态 117s+70s=187s（run 358）；逐镜冷态第 1 镜就 244s 且第 2 镜卡在
	//   decode 换页 15 分钟以上（run 363，已取消）。
	// 即：批量只在「权重必须反复重载」的冷路径上占优，热态反而略慢，
	// 而换页方差远大于两者差 ⇒ 默认关，按需开（长时间/冷启动多镜台词片）。
	batchDialogue := false
	if v, ok := node.Data["batch_dialogue"].(bool); ok {
		batchDialogue = v
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
	// dubs[i] = 第 i 镜的念稿音轨报告（nil = 无台词）。与 clips 同索引同生命周期：
	// 首拍 append、重拍就地替换，装配后按它排字幕时间轴（subtitles.go）。
	dubs := make([]*DubTrackReport, 0, len(plan.Shots))
	var prevLastFrame string
	nodeStart := time.Now()

	// 镜头级作业：每镜成功即落盘，失败重跑时已完成的镜头直接复用（不重烧显存）。
	// runID=0 表示独立执行（没有 run），此时 store 不落盘，行为与旧版一致。
	runID := runIDFrom(ctx)
	reusedCount := 0

	// 本次运行的工作量可以直接从分镜表算出来（帧 × 分辨率 × 步数 + 装配/后期）。
	// 登记成预测之后，**第一次**跑也有真 ETA —— 否则只能等同类节点跑过一次
	// （ETAFor 用的历史均值），日志里的 eta_ms 长期恒为 0，进度条等于空转。
	{
		totalFrames := 0
		for _, sh := range plan.Shots {
			totalFrames += sh.Frames
		}
		pred := time.Duration(shotCostSeconds(totalFrames, width, height, steps)*float64(time.Second)) +
			time.Duration(len(plan.Shots))*10*time.Second + 20*time.Second
		e.SetRunPrediction(runID, pred)
		defer e.ClearRunPrediction(runID)
	}

	// 定点重拍：单镜验收不过就只重跑那一镜，不整片重来。
	// 实现方式是同一段生成代码跑多轮（pass=0 首拍，pass>0 只跑 needsReshoot 里的镜），
	// 而不是复制一份生成逻辑——两份逻辑迟早会漂移。
	reshootMax := int(fieldFloat(node, "reshoot_max", 1))
	if reshootMax < 0 {
		reshootMax = 0
	}
	needsReshoot := make([]bool, len(plan.Shots))
	reshootCount := 0
	cacheDir := shotCacheDir(outDir)

	// finishShot 单镜产出后的共用收尾：结果落位 → 音频治理 → 缓存 → 进度 → 末帧。
	// 单镜路径与批量路径都走这里，两份逻辑迟早漂移（AGENTS §0.12④）。
	finishShot := func(i, pass int, sh Shot, part string, genElapsed time.Duration,
		attempts int, gateNote string, dub *DubTrackReport, reusedShot bool, cacheKey string) {
		res := shotResult{ID: sh.ID, File: part, Frames: sh.Frames, Seconds: float64(sh.Frames) / 24.0,
			Transition: sh.Transition, Attempts: attempts, Gate: gateNote, Reused: reusedShot}
		if dub != nil {
			res.Note = fmt.Sprintf("念稿音轨 %.2fs（%s 后端，台词 %d 句）", dub.Seconds, dub.Backend, len(dub.Lines))
		}
		// 氛围段音频治理（三道防线里的第三道）：
		// 提示词层已经把声音字段填满，但模型仍有概率即兴说话。这里做客观抽检，
		// 检测到人声就不再只是"压低音量"，而是中置相消把它从信号里减掉。
		// 台词段**不能**走这条——中置相消会把台词本身消掉。
		if audioCheck && backend == "h3" && !sh.HasDialogue() && !reusedShot {
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
		// 缓存要在音频治理**之后**写：写进去的必须是最终交付的那一版，
		// 否则复用回来的镜头会带着没处理的人声。
		if !reusedShot && cacheKey != "" {
			shotCacheStore(cacheDir, cacheKey, part)
		}
		if pass == 0 {
			clips = append(clips, part)
			transitions = append(transitions, sh.Transition)
			results = append(results, res)
			dubs = append(dubs, dub)
		} else {
			// 重拍：就地替换该镜，保留原轮次信息（Attempts 累加，便于回溯总成本）
			res.Attempts += results[i].Attempts
			res.Note = appendNote(res.Note, fmt.Sprintf("定点重拍（第 %d 轮）", pass))
			clips[i], transitions[i], results[i] = part, sh.Transition, res
			dubs[i] = dub
			reshootCount++
		}

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

	for pass := 0; pass <= reshootMax; pass++ {
		for i := 0; i < len(plan.Shots); i++ {
			if pass > 0 && !needsReshoot[i] {
				continue // 修复轮只碰坏镜
			}
			// 镜头之间响应取消。旧版循环体完全不看 ctx，点了停止也要等整片跑完。
			if cerr := ctx.Err(); cerr != nil {
				log.Printf("[shotlist] 第 %d/%d 镜前收到取消", i+1, len(plan.Shots))
				return nil, fmt.Errorf("已取消（完成 %d/%d 镜，已完成部分已落盘，重跑会复用）：%w",
					len(results), len(plan.Shots), cerr)
			}
			// 预算总闸：on_exceed=fail 时超了就地停机（已完成的镜已落盘，重跑复用）
			// 修复轮不查预算：门已经判不过了，这时候停下等于交一条坏片。
			if pass == 0 && wallCap > 0 && onExceed == "fail" {
				used := time.Since(nodeStart).Seconds()
				if used > float64(wallCap) {
					return nil, fmt.Errorf("预算门：已用 %.0fs 超过总预算 %ds（on_exceed=fail），停机；已完成 %d/%d 镜已落盘，重跑会复用",
						used, wallCap, len(results), len(plan.Shots))
				}
			}
			// ---- 批量分支（仅首轮）：连续可同批的镜合成一张图提交，失败回退单镜 ----
			// 收益与边界见 comfy_media_batch.go 头注。任何条件不满足就地放弃，
			// 落回下面的单镜路径——批量是加速器，不是新门槛。
			// 2026-10-30：台词镜也进批（TTS 前置），但段级门照旧逐镜过。
			if batchSize >= 2 && pass == 0 {
				continuity0 := i > 0 && plan.Shots[i-1].Transition == "continue" && prevLastFrame != ""
				// 同批签名三件套：kind / steps / 低显存档。批量图共用一份模型链，
				// 签名不一致混进来只会整批重载（等于白批）。r2v 的 PDD 关（实测更慢），
				// 台词镜保 12 步、非台词 fl2va 才走 PDD 8 步。
				pddGlobal := true
				if v, ok := node.Data["pdd"].(bool); ok {
					pddGlobal = v
				}
				kindAt := func(j int) string {
					// 台词必须 r2v（只有它吃 ref_audio）；有定妆图也走 r2v
					if plan.Shots[j].HasDialogue() || len(refImgs) > 0 {
						return "h3_r2v"
					}
					return "h3"
				}
				pddAt := func(j int) bool { return pddGlobal && kindAt(j) != "h3_r2v" }
				stepsAt := func(j int) int {
					if pddAt(j) {
						return 8
					}
					return steps
				}
				lowAt := func(j int) bool { return shotNeedsLowVRAM(node, width, height, plan.Shots[j].Frames) }
				promptAt := func(j int) string {
					pj := plan.Shots[j].Prompt
					if strings.TrimSpace(pj) == "" {
						pj = CompileShotPrompt(plan, plan.Shots[j], false)
					}
					return pj
				}
				keyAt := func(j int) string {
					if plan.Shots[j].HasDialogue() {
						return "" // 台词镜不缓存：画面取决于当次 TTS 音轨
					}
					return shotCacheKey(promptAt(j), width, height, plan.Shots[j].Frames,
						stepsAt(j), backend, turbo, pddAt(j))
				}
				// canBatch：这一镜本身可进批。台词镜不吃缓存（画面取决于当次 TTS
				// 音轨），要不要进批由 batch_dialogue 决定（默认关，见上面的实测）。
				canBatch := func(j int) bool {
					if plan.Shots[j].HasDialogue() {
						return batchDialogue
					}
					return shotCacheHit(cacheDir, keyAt(j)) == ""
				}
				same := func(j int) bool {
					return kindAt(j) == kindAt(i) && stepsAt(j) == stepsAt(i) && lowAt(j) == lowAt(i)
				}
				if n := batchRunLen(plan.Shots, i, batchSize, continuity0, canBatch, same); n >= 2 {
					// TTS 前置（2026-10-30）：批量图必须在提交前拿到全部 ref_audio，
					// 所以先按序把这 n 镜的念稿音轨全部合成完，再一次提交。
					// TTS 在 CPU 上是零 GPU 成本；失败按该镜失败处理——不能拿假音频糊弄，
					// fully_copy 会把任何垃圾原样带进成片。
					runDubs := make([]*DubTrackReport, n)
					runDubPaths := make([]string, n)
					for k := i; k < i+n; k++ {
						shk := plan.Shots[k]
						if !shk.HasDialogue() || backend != "h3" {
							continue
						}
						p := dubTrackTempPath(outDir, shk.ID)
						gain := shk.AmbienceGain
						if gain <= 0 {
							gain = ambienceGain // 节点级兜底（默认 0.85）
						}
						e.emitShotProgress(runID, node.ID, k, len(plan.Shots), shk.ID, "voicing", 0)
						rep, derr := SynthesizeDubTrack(e.tts, shotDubLines(shk), p,
							float64(shk.Frames)/24.0, shk.Lead, gain, plan.AmbienceFile(shk))
						if derr != nil {
							return nil, fmt.Errorf("第 %d 镜念稿音轨合成失败：%w", shk.ID, derr)
						}
						runDubs[k-i], runDubPaths[k-i] = rep, p
					}
					reqs := make([]MediaRequest, 0, n)
					keys := make([]string, 0, n)
					for k := i; k < i+n; k++ {
						r := MediaRequest{
							Kind: kindAt(k), Prompt: promptAt(k), Width: width, Height: height,
							Frames: plan.Shots[k].Frames, FPS: 24, Steps: stepsAt(k),
							Turbo: turbo, LowVRAM: lowAt(k), PDD: pddAt(k), Ctx: ctx,
						}
						if r.Kind == "h3_r2v" {
							r.RefImages = refImgs
						}
						if runDubPaths[k-i] != "" {
							r.RefAudios = []string{runDubPaths[k-i]}
						}
						reqs = append(reqs, r)
						keys = append(keys, keyAt(k))
					}
					for k := range reqs {
						e.emitShotProgress(runID, node.ID, i+k, len(plan.Shots), plan.Shots[i+k].ID, "generating", 0)
					}
					limit := time.Duration(0)
					for k := range reqs {
						limit += ShotGenerationLimit(reqs[k].Frames, width, height, reqs[k].Steps)
					}
					wd := armShotWatchdog(limit, func() {
						log.Printf("[shotlist] 批量 %d 镜跑到 %.1f 倍逐镜基准仍未完成，中止整批",
							len(reqs), shotAbortFactor)
						e.interruptComfy()
					})
					batchStart := time.Now()
					outs, berr := e.t2i.GenerateH3Batch(ctx, reqs, func(phase string) {
						switch phase {
						case "queued":
							wd.Pause()
							for k := range reqs {
								e.emitShotProgress(runID, node.ID, i+k, len(plan.Shots), plan.Shots[i+k].ID, "queued", 0)
							}
						case "running":
							wd.Resume()
						}
					})
					batchElapsed := time.Since(batchStart)
					wd.Stop()
					// 段级门：批量是加速器，不是免检证。带音轨的镜必须在收下产物前验词，
					// 任何一镜没过就整批作废、回退逐镜（逐镜路径自带重试与明确报错）。
					// ASR 不可达按「没验」处理（与单镜路径同口径），不能冒充通过。
					gateFail := ""
					gateUnverified := false
					if berr == nil {
						for k := range outs {
							if runDubs[k] == nil {
								continue
							}
							expect := plan.Shots[i+k].DialogueText()
							passed, checked := verifyDialogueLine(e.asr, outs[k].Path, expect)
							if !checked {
								gateUnverified = true
								break
							}
							if !passed {
								gateFail = fmt.Sprintf("第 %d 镜批量产出验词未过：台词「%s」",
									plan.Shots[i+k].ID, expect)
								break
							}
						}
					}
					if berr == nil && gateFail == "" {
						log.Printf("[shotlist] 批量 %d 镜一张图完成，用时 %s（逐镜基线合计约 %s）",
							len(reqs), batchElapsed.Round(time.Second),
							(time.Duration(shotCostSeconds(sumFrames(reqs), width, height, reqs[0].Steps) * float64(time.Second))).Round(time.Second))
						for k, out := range outs {
							note := fmt.Sprintf("批量图 %d 镜合提交", len(reqs))
							if runDubs[k] != nil {
								if gateUnverified {
									note = appendNote(note, "台词校验未执行（ASR 不可达），不等于念对")
								} else {
									note = appendNote(note, "ASR 验词通过")
								}
							}
							finishShot(i+k, pass, plan.Shots[i+k], out.Path, batchElapsed, 1, note, runDubs[k], false, keys[k])
						}
						i += len(reqs) - 1
						continue
					}
					if berr != nil {
						log.Printf("[shotlist] 批量 %d 镜失败（%.200s），回退逐镜生成", len(reqs), berr.Error())
					} else {
						log.Printf("[shotlist] %s，整批作废回退逐镜生成", gateFail)
					}
					// 不 return：批量只是加速，回退后从第 i 镜开始照常逐镜
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
			// 非台词镜走 PDD 8 步；台词镜必须保 12 步（音频短板）。
			pdd := !dialogue
			if v, ok := node.Data["pdd"].(bool); ok {
				pdd = v && !dialogue
			}
			shotSteps := steps
			if pdd {
				shotSteps = 8
			}
			var dubPath string
			var dub *DubTrackReport
			if dialogue && backend == "h3" {
				// 台词行解析与批量路径共用 shotDubLines（结构化 lines 或 dialogue 字段）
				lines := shotDubLines(sh)
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

			lowVRAM := shotNeedsLowVRAM(node, width, height, sh.Frames)

			req := MediaRequest{
				Kind: "h3", Prompt: prompt,
				Width: width, Height: height,
				Frames: sh.Frames, FPS: 24, Steps: shotSteps,
				Turbo: turbo, LowVRAM: lowVRAM, PDD: pdd,
				// 把取消信号带进生成链路：节点超时/点停止时，ComfyUI 里那条
				// 提交要立刻从队列里撤掉，而不是继续烧到它自己的预算耗尽。
				Ctx: ctx,
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

			// Ref2VA 的 PDD 8 步尚未验证出收益（实测反而 3m40s/镜），先只给 FL2VA 用；
			// 等有同条件 Ref2VA A/B 再放开，避免“接了加速反而更慢”。
			if pdd && req.Kind == "h3_r2v" {
				pdd = false
				shotSteps = steps
				req.PDD = false
				req.Steps = shotSteps
			}

			// 镜头复用：提示词与全部影响画面的参数都没变时，直接拿上次的产物。
			// 这是「失败重跑不重烧」的全部依据（见 shot_cache.go 的由来）：
			// 原来文案承诺复用、代码却从没查过任何存储，一晚 6 次失败每次从第 1 镜重来。
			cacheKey := ""
			if !dialogue {
				cacheKey = shotCacheKey(prompt, width, height, sh.Frames, shotSteps, backend, turbo, pdd)
			}
			reusedShot := false

			// 生成 + 段级门：台词段 ASR 验词，不过按 segment_retry 重跑
			// （采样有随机性，重跑比放过一镜乱语便宜得多）。
			var part string
			var gerr error
			var genElapsed time.Duration
			attempts := 0
			gateNote := ""
			if cacheKey != "" {
				if cached := shotCacheHit(cacheDir, cacheKey); cached != "" {
					part, reusedShot = cached, true
					reusedCount++
					gateNote = "复用上次产物（提示词与参数未变）"
					e.emitShotProgress(runID, node.ID, i, len(plan.Shots), sh.ID, "reused", 0)
					log.Printf("[shotlist] 第 %d/%d 镜 shot=%d 命中复用缓存，跳过生成：%s",
						i+1, len(plan.Shots), sh.ID, filepath.Base(cached))
				}
			}
			for !reusedShot {
				attempts++
				e.emitShotProgress(runID, node.ID, i, len(plan.Shots), sh.ID, "generating", 0)
				genStart := time.Now()
				// 看门狗：跑到「明显不可能正常完成」就掐断，别等节点预算烧完（见 shot_watchdog.go）
				limit := ShotGenerationLimit(sh.Frames, width, height, shotSteps)
				wd := armShotWatchdog(limit, func() {
					logShotAborted(sh.ID, i+1, len(plan.Shots), limit)
					e.interruptComfy()
				})
				// 排队期间暂停计时：否则"排队 6 分钟"会被算成"生成 6 分钟"，
				// 健康镜头会在 5 倍基准处被误杀（run 224 就是这么死的）。
				req.OnPhase = func(phase string) {
					switch phase {
					case "queued":
						wd.Pause()
						// 让界面/事件流看得见"在排队"，而不是只有一个转圈的"生成中"。
						// 实测排队 6 分钟时日志与进度条都没说过一句，看起来像卡死。
						e.emitShotProgress(runID, node.ID, i, len(plan.Shots), sh.ID, "queued", 0)
					case "running":
						wd.Resume()
					}
				}
				part, _, gerr = e.t2i.GenerateMedia(req)
				genElapsed = time.Since(genStart)
				wd.Stop()
				req.OnPhase = nil
				if gerr != nil {
					e.emitShotProgress(runID, node.ID, i, len(plan.Shots), sh.ID, "failed", genElapsed)
					// 逐镜耗时与错误必须落 engine.log。
					// 由来（2026-10-26）：查 run 203 那次失败时，日志里只有「超时」，
					// 逐镜实际用了多久得去 sqlite 里挖 shot_progress 的 elapsed_ms 才知道
					// （第 2 镜 2269.8s，是第 1 镜的 27 倍）。一条日志能省掉整套考古。
					if wd.Aborted() {
						log.Printf("[shotlist] 第 %d/%d 镜 shot=%d 被看门狗中止，用时 %s（省下约 %s 的无效 GPU 时间）",
							i+1, len(plan.Shots), sh.ID, genElapsed.Round(time.Second),
							(limit - genElapsed).Round(time.Second))
						return nil, fmt.Errorf("第 %d 镜跑满 %.0f 倍基准仍未完成，已主动中止（避免白烧到节点超时）"+
							"——先确认 8188/8189 上各只有一个实例、关掉占显存的程序再重跑",
							sh.ID, shotAbortFactor)
					}
					log.Printf("[shotlist] 第 %d/%d 镜 shot=%d 生成失败，用时 %s，疑似 OOM=%v：%.200s",
						i+1, len(plan.Shots), sh.ID, genElapsed.Round(time.Second), IsOOMError(gerr), gerr.Error())
					return nil, fmt.Errorf("第 %d 镜生成失败（已完成 %d/%d 镜并已落盘）：%w",
						sh.ID, len(results), len(plan.Shots), gerr)
				}
				log.Printf("[shotlist] 第 %d/%d 镜 shot=%d 生成完成，用时 %s（第 %d 次）",
					i+1, len(plan.Shots), sh.ID, genElapsed.Round(time.Second), attempts)
				// 慢镜就地喊一声：那一晚 27 倍慢没有任何一行日志提示，
				// 用户只能连着重试，每次白烧 15 分钟（见 JudgeShotSpeed 注释）。
				if freeMB, ferr := e.t2i.FreeVRAMMB(); ferr == nil {
					if warn := JudgeShotSpeed(sh.Frames, width, height, steps, genElapsed, freeMB); warn != "" {
						log.Printf("[shotlist] 第 %d/%d 镜 shot=%d 异常慢：%s", i+1, len(plan.Shots), sh.ID, warn)
					}
				} else if warn := JudgeShotSpeed(sh.Frames, width, height, steps, genElapsed, 0); warn != "" {
					log.Printf("[shotlist] 第 %d/%d 镜 shot=%d 异常慢：%s", i+1, len(plan.Shots), sh.ID, warn)
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

			finishShot(i, pass, sh, part, genElapsed, attempts, gateNote, dub, reusedShot, cacheKey)
		}

		// 本轮生成完立刻逐镜验收，决定下一轮重拍谁。
		// 判据是「这一镜自己能不能用」，与成片终检解耦：
		// 成片 FAIL 往往就是某镜没有音轨 / 帧数不足 / 音画漂了。
		if pass < reshootMax && len(clips) == len(plan.Shots) {
			bad := markNeedsReshoot(clips, plan.Shots, 0.2)
			if len(bad) == 0 {
				break // 都合格，不必再拍一轮
			}
			for _, i := range bad {
				needsReshoot[i] = true
			}
		}
	}

	// 装配：cut=1 帧、continue=2 帧、dissolve=0.3 秒的交叉淡化
	//
	// 装配前先量各镜色度：plan 层管不到"生成侧色温漂移"（表里两镜都写冷色，
	// 出来却一暖一冷，实测发生过）。这是唯一能在烧完全部 GPU 之后、交付之前
	// 抓住接缝观感跳变的地方。
	if colorIssues := AuditClipChroma(clips); len(colorIssues) > 0 {
		for _, it := range colorIssues {
			log.Printf("[shotlist] 一致性告警 %s: %s", it.Code, it.Msg)
		}
		consistencyIssues = append(consistencyIssues, colorIssues...)
	}

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
	// ---- 装配后处理：音画对齐 → 响度归一 → 可选 BGM ----
	// 每一步都只重编码音轨、视频流 copy，且失败不阻断产出（降级为 WARN）。
	// 顺序不可换：先对齐（否则响度链会以错误时长工作），再归一，最后混 BGM。
	postNotes := make([]string, 0, 3)
	if d, fixed, aerr := alignAVToVideo(final, 0.05); aerr != nil {
		verdict.Add("WARN", "post.av_align", "音画对齐未执行："+aerr.Error())
		postNotes = append(postNotes, "音画对齐失败")
	} else if fixed {
		msg := fmt.Sprintf("音画对齐：修正 %.3fs 时长差", d)
		verdict.Add("OK", "post.av_align", msg)
		postNotes = append(postNotes, msg)
	}
	if audioMaster {
		m2 := strings.TrimSuffix(final, filepath.Ext(final)) + "-master" + filepath.Ext(final)
		if merr := masterLoudness(final, m2, lufs, -1.5, 11); merr != nil {
			verdict.Add("WARN", "post.loudness", "响度归一未执行："+merr.Error())
			postNotes = append(postNotes, "响度归一失败")
		} else {
			final = m2
			postNotes = append(postNotes, fmt.Sprintf("响度归一 %.0f LUFS", lufs))
		}
	}
	// 整片声床：放在响度归一之后（电平相对最终节目声是确定的）、BGM 之前
	// （有配乐时 duck 的是主轨，声床跟着沉下去正好）。失败降级为 WARN 不阻断。
	if roomTone {
		m4 := strings.TrimSuffix(final, filepath.Ext(final)) + "-roomtone" + filepath.Ext(final)
		if terr := mixRoomTone(final, m4, roomToneFloor); terr != nil {
			verdict.Add("WARN", "post.room_tone", "声床未铺："+terr.Error())
			postNotes = append(postNotes, "声床失败")
		} else {
			final = m4
			postNotes = append(postNotes, fmt.Sprintf("声床 %.0f dBFS", roomToneFloor))
		}
	}
	if strings.TrimSpace(bgmPath) != "" {
		if !filepath.IsAbs(bgmPath) {
			if abs, aerr := filepath.Abs(bgmPath); aerr == nil {
				bgmPath = abs
			}
		}
		if _, serr := os.Stat(bgmPath); serr != nil {
			verdict.Add("WARN", "post.bgm", "BGM 文件不存在，未混音："+bgmPath)
			postNotes = append(postNotes, "BGM 缺失")
		} else {
			m3 := strings.TrimSuffix(final, filepath.Ext(final)) + "-bgm" + filepath.Ext(final)
			if berr := mixBGM(final, bgmPath, m3, bgmGain, bgmDuck); berr != nil {
				verdict.Add("WARN", "post.bgm", "BGM 混音未执行："+berr.Error())
				postNotes = append(postNotes, "BGM 混音失败")
			} else {
				final = m3
				postNotes = append(postNotes, "BGM 已混入")
			}
		}
	}
	if abs, aerr := filepath.Abs(final); aerr == nil {
		final = abs
	}

	// 字幕产物：把配音逐行实测时间戳排到全片时间轴，写成 SRT（与成片同名 .srt）。
	// 烧不烧由 produce 层 subtitles/overlay_srt 参数决定；文件本身总是落盘，
	// 剪映/PR 导入直接用。接缝重叠值与装配器同源（transitionOverlapD），
	// 镜时长用探测值（装配器吃的就是这个）。
	var srtPath string
	if len(dubs) == len(clips) && len(clips) > 0 {
		durs := make([]float64, len(clips))
		probed := true
		for i, c := range clips {
			if info, perr := probeMedia(c); perr == nil && info.dur > 0 {
				durs[i] = info.dur
			} else {
				probed = false
				break
			}
		}
		if probed {
			if lines := filmSubtitleLines(dubs, durs, transitions); len(lines) > 0 {
				srtPath = strings.TrimSuffix(final, filepath.Ext(final)) + ".srt"
				if werr := writeShotSRT(srtPath, lines); werr != nil {
					log.Printf("[shotlist] 字幕文件写入失败：%v", werr)
					srtPath = ""
				}
			}
		}
	}

	// 成片自检：数帧 + 音画时长比对。肉眼看时长看不出「中间整段画面丢失」，
	// 但流水线必须自己知道。
	check, _ := inspectMedia(final)
	if !check.OK {
		for _, is := range check.Issues {
			log.Printf("[shotlist] 成片自检未通过：%s", is)
		}
	}

	// ---- 终检门：把成片当「外来素材」再审一遍，并把结论并进 verdict ----
	// 以前这层只在外面用 verify-media.py 跑：人忘了跑就等于没检。
	// 现在 FAIL 会进 gate_ok/gate_verdict，调用方按门决策，不靠记性。
	expectedFrames := 0
	for _, r := range results {
		expectedFrames += r.Frames
	}
	finalChecks := make([]gateCheck, 0, 8)
	if finfo, perr := probeGateMedia(final); perr != nil {
		verdict.Add("WARN", "final.probe", "终检未执行（读不到媒体信息）："+perr.Error())
	} else {
		finalChecks = runGateChecks(ctx, final, finfo, filmFinalRules(gates, node, expectedFrames))
		for _, c := range finalChecks {
			if c.OK {
				verdict.Add("OK", "final."+c.Name, c.Detail)
				continue
			}
			level := "FAIL"
			if strings.EqualFold(c.Level, "warn") {
				level = "WARN"
			}
			verdict.Add(level, "final."+c.Name, "终检未通过："+c.Detail)
		}
	}
	if verdict.HasFail() {
		for _, it := range verdict.Fails() {
			log.Printf("[shotlist] 终检 FAIL %s: %s", it.Gate, it.Msg)
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
	out := map[string]interface{}{
		"video":        final,
		"kind":         "video",
		"parts":        clips,   // 逐镜产物（有序）——一致性门按这个顺序比段间漂移
		"plan":         rawPlan, // 回传分镜表：字幕节点按它排轴（台词时点唯一来源）
		"text":         plan.Title,
		"shots":        len(results),
		"seconds":      total,
		"shots_reused": reusedCount,
		"reshoots":     reshootCount,
		"detail":       planJSON,
		"check":        toJSON(check),
		"check_ok":     check.OK,
		"final_check":  toJSON(finalChecks),
		"post":         strings.Join(postNotes, "；"),
		"gate_report":  toJSON(gateReport),
		"consistency":  toJSON(consistencyIssues),
		"gate_ok":      !verdict.HasFail(),
		"gate_verdict": verdict.Worst(),
	}
	if srtPath != "" {
		out["subtitles"] = srtPath // produce 层 subtitles=true 时按它烧录
	}
	return out, nil
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
// 除了「念没念对」，还要拦「念了两遍」：实测 run 367 第 2 镜 H3 把同一句连说两次
// （「明天见，做个好梦。明天见，做个好梦。」），而纯 Contains 判据照样通过——
// 用户听到的是重复台词，门却说通过（§0.12②「工具没报错=没问题」的镜像）。
// checked=false 表示「没验到」（ASR 不可达/无期望词），调用方必须与「没念对」分开。
func verifyDialogueLine(asr *ASRService, path, expect string) (passed, checked bool) {
	if asr == nil || strings.TrimSpace(expect) == "" {
		return false, false
	}
	text, err := asr.Transcribe(path)
	if err != nil {
		return false, false
	}
	ok, _ := dialogueLineMatch(text, expect)
	return ok, true
}

// dialogueLineMatch 判「念对了，且只念了一遍」。第二个返回值是整句出现次数。
// 重复判定用剥标点后的整句计数：H3 会把同一句连说两遍（run 367 实测），
// 那不是「念错」而是「多念」，观众听得出 ⇒ ≥2 次一律判不过，交给段级重试
// （采样有随机性，重跑比放过一镜重复台词便宜）。
func dialogueLineMatch(asrText, expect string) (bool, int) {
	got, want := stripASRPunct(asrText), stripASRPunct(expect)
	if want == "" {
		return false, 0
	}
	n := strings.Count(got, want)
	return n == 1, n
}

// stripASRPunct 验词只比字、不比标点：faster-whisper 对同一句台词可能回全角
// 标点、半角标点或完全不回（实测「晚安，今晚的海很平静。」三种都出过，
// 连 TTS 念轨本尊都转不出全角句号）——按标点严格比对会把念对的台词判死。
func stripASRPunct(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, s)
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

// shotSecPerFrame 健康状态下的单帧生成成本（秒/帧），**只在基准参数下成立**。
//
// 原始依据：run 202 / run 208 实测 73–82s / 56–73 帧 ≈ 1.1–1.2 秒/帧，
// 那一档是 608×352、12 步（turbo LoRA）——**但那是小模型 + 图像时代的数**。
//
// 2026-10-26 重新标定到 4.5。同一档参数（56 帧 / 608×352 / 12 步）在 MiniMax H3
// （20GB 权重、12 步采样）上的成功样本是 84 / 129 / 167 / 190 / 322 / 352 / 509s，
// 单条 prompt 总耗时 477s，中位约 240s ⇒ 4.3 秒/帧；取 4.5 留余量。
//
// 不改的代价（10-26 现场实测，一天之内）：
//
//	基准 67s ⇒ 看门狗上限 5×67 = 335s；而 H3 换页时一步就要 69s、整镜 828s，
//	于是每一镜都在第 5 步左右被误杀，10:26 / 10:50 / 11:11 连杀三次。
//	更讽刺的是日志里那两行「省下约 -1m38s / -7m28s」是**负数**：
//	/interrupt 是协作式的，要等当前步跑完，中止本身反而倒赔时间。
//
// 改后：基准 252s ⇒ 看门狗 1260s（21min，覆盖 828s 的换页档），
// 慢镜告警 756s（509s 的真实慢镜不再误报），仍能掐住 2701s（10.7 倍）那种真换页地狱。
//
// **预算公式和「慢镜告警」必须共用这一个常数族**，否则两边会各自漂移：
// 预算按 A 算、告警按 B 算，就会出现「明明超预算了却不告警」这种事。
const shotSecPerFrame = 4.5

// 基准参数：4.5 秒/帧 只对这三个数成立。
const (
	shotBaseWidth  = 608
	shotBaseHeight = 352
	shotBaseSteps  = 12
)

// shotNeedsLowVRAM 决定这一镜是否启用分块注意力/FFN。
//
// 24GB 卡在 608×352×124 上不需要分块（实测峰值 23.8GB，仍能全速跑）；
// 分块会把 attention/FFN 切开，单镜明显变慢。只有工作量超过基准 1.15 倍
// （例如 736×416×124、512×896×73）才默认开启，避免高分辨率 OOM。
// 节点显式 low_vram（true/false）优先，给用户留手动覆盖。
func shotNeedsLowVRAM(node *model.NodeInstance, width, height, frames int) bool {
	if v, ok := node.Data["low_vram"].(bool); ok {
		return v
	}
	const base = 608 * 352 * 124
	return int64(width)*int64(height)*int64(frames) > int64(base)*115/100
}

// shotCostSeconds 单镜的健康成本（秒）= shotSecPerFrame × 帧数 × 像素比 × 步数比。
//
// 为什么要带分辨率与步数：原来只有一个 frames×1.2，等于假设"每镜都是 608×352 / 12 步"。
// 实测 768×448@20 步（run 217 / run 222）的单镜成本是它的约 2.5 倍——那一档每一镜
// 都会被「慢镜告警」误判成异常，预算也差出一倍多（长镜因此更容易撞节点超时）。
func shotCostSeconds(frames, width, height, steps int) float64 {
	if frames <= 0 {
		return 0
	}
	if width <= 0 || height <= 0 {
		width, height = shotBaseWidth, shotBaseHeight
	}
	if steps <= 0 {
		steps = shotBaseSteps
	}
	px := float64(width*height) / float64(shotBaseWidth*shotBaseHeight)
	st := float64(steps) / float64(shotBaseSteps)
	return shotSecPerFrame * float64(frames) * px * st
}

// shotSlowFactor 慢到什么程度才值得喊一声（相对基准的倍数）。
// 取 3 倍：正常抖动到不了 3 倍；而换页/同卡多实例一来就是 10–27 倍（实测）。
const shotSlowFactor = 3.0

// ShotListBudget 整节点预算 = 各镜预算之和 + 装配/自检/插帧。
func ShotListBudget(shotCount, totalFrames, width, height, steps int) time.Duration {
	if shotCount <= 0 {
		return 2 * time.Hour
	}
	d := time.Duration(shotCostSeconds(totalFrames, width, height, steps)*float64(time.Second)) +
		time.Duration(shotCount)*90*time.Second +
		10*time.Minute
	if d < 15*time.Minute {
		d = 15 * time.Minute
	}
	return d
}

// JudgeShotSpeed 单镜跑完后的速度判据：返回空串表示正常。
//
// 由来（2026-10-26）：那一晚第 2 镜从 84.9s 变成 2269.8s（27 倍），
// 但当时**没有任何一行日志**说「这一镜不正常」——只有最后一句 node timeout。
// 于是用户连着重试四次，每次白烧 15 分钟。
// 这里在每镜结束时就地判一次：慢就喊，并把当时的空闲显存一起写进去，
// 因为 27 倍慢的成因几乎只有两个——显存换页、或同一张卡上跑了两份同一个实例。
func JudgeShotSpeed(frames, width, height, steps int, elapsed time.Duration, freeMB int) string {
	if frames <= 0 || elapsed <= 0 {
		return ""
	}
	base := time.Duration(shotCostSeconds(frames, width, height, steps) * float64(time.Second))
	if base <= 0 {
		return ""
	}
	ratio := elapsed.Seconds() / base.Seconds()
	if ratio < shotSlowFactor {
		return ""
	}
	cause := "疑似显存换页（权重在显存/内存之间来回搬）"
	if freeMB > 0 && freeMB < 1500 {
		cause = "空闲显存只剩 " + itoaMB(freeMB) + "MB，基本可以确定在换页"
	} else if freeMB <= 0 {
		cause = "拿不到空闲显存读数；仍应先怀疑换页或同卡多实例"
	}
	return fmt.Sprintf("比基准慢 %.1f 倍（%s / 基准 %s）——%s",
		ratio, elapsed.Round(time.Second), base.Round(time.Second), cause)
}

// itoaMB 小工具：避免为了拼一个数字引入 strconv 依赖争议。
func itoaMB(v int) string {
	if v <= 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
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
			"phase":      phase, // queued / generating / reused / failed / done
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
