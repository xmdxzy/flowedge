package service

// 本文件只放「节点执行体」：每个节点的业务逻辑。
// 引擎内核（DAG 调度 / 超时 / 事件 / 取消 / 注册表）留在 engine_v2.go。
// 拆分动因见 docs/系统评估-2026-10-21.md §3 S2：单文件 2283 行时，
// 改一个节点要滚 2000 行上下文，且内核与节点混杂无法独立推理。

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

// ===== 各节点执行逻辑 =====

func (e *EngineV2) execStartNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	if prompt, ok := node.Data["prompt"].(string); ok && prompt != "" {
		return map[string]interface{}{"text": prompt}, nil
	}
	for _, out := range upstream {
		if m, ok := out.(map[string]interface{}); ok {
			if text, ok := m["text"].(string); ok {
				return map[string]interface{}{"text": text}, nil
			}
		}
	}
	return map[string]interface{}{"text": ""}, nil
}

func (e *EngineV2) execLLMNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	// LLM 与扩散模型共用 24GB：显存真装得下就共存（省一次约 183s 冷加载），
	// 装不下才让扩散侧释放。判定见 canCoexistWithLanguage。
	llmModel, _ := node.Data["model"].(string)
	release := e.enterLanguageSide(llmModel)
	defer release()

	// 节点级提示词 = 指令前缀；上游文本 = 输入主体。两者拼接，缺一不可丢
	instruction := ""
	if p, ok := node.Data["prompt"].(string); ok {
		instruction = p
	}

	upstreamText := ""
	for _, out := range upstream {
		if m, ok := out.(map[string]interface{}); ok {
			if text, ok := m["text"].(string); ok && text != "" {
				upstreamText = text
			}
		}
	}

	prompt := upstreamText
	if instruction != "" {
		if upstreamText != "" {
			prompt = instruction + "\n\n" + upstreamText
		} else {
			prompt = instruction
		}
	}

	if prompt == "" {
		return nil, fmt.Errorf("LLM node has no input prompt")
	}

	// 节点级模型覆盖（空 = 使用服务级默认模型）
	reqModel, _ := node.Data["model"].(string)

	// 显存调度：请求加载 LLM
	if e.gpuSched != nil {
		schedName := reqModel
		if schedName == "" {
			schedName = "qwen2.5:7b"
		}
		_ = e.gpuSched.AcquireModel(schedName)
		defer e.gpuSched.ReleaseModel(schedName)
	}

	// 节点级模型 profile：指向别的端点（云端 API / 另一台机器上的 Ollama）。
	// 没填 profile 就用引擎全局那一个，行为与以前完全一致。
	svc := e.llm
	if name, _ := node.Data["llm_profile"].(string); strings.TrimSpace(name) != "" {
		if e.llmProfiles == nil {
			return nil, fmt.Errorf("节点引用了模型 %q，但引擎没有加载模型清单", name)
		}
		p, ok := e.llmProfiles.Get(name)
		if !ok {
			return nil, fmt.Errorf("模型 %q 不存在：请在节点面板的「我的模型」里新建，或清空该选择", name)
		}
		if custom := e.llmServiceForProfile(p); custom != nil {
			svc = custom
		}
	}

	// 流式只对引擎自带的那个服务做：自定义端点的流式协议未必一致，
	// 硬套会得到"半个字都没有"的空结果，比慢更糟。
	if svc != e.llm {
		result, u, err := svc.ChatWithModelUsage(prompt, reqModel)
		if err != nil {
			return nil, err
		}
		out := map[string]interface{}{"text": result}
		if u.PromptTokens > 0 || u.CompletionTokens > 0 {
			out["_usage"] = u
		}
		return out, nil
	}

	// 流式（llm_stream）已随画布删除：出片台不显示 token 打字机效果，
	// 保留一条非流式调用即可，少一条协议分支。
	result, u, err := e.llm.ChatWithModelUsage(prompt, reqModel)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{"text": result}
	if u.PromptTokens > 0 || u.CompletionTokens > 0 {
		out["_usage"] = u
	}
	return out, nil
}

// splitPromptLines 把「一行一条」的批量提示词拆成切片
func splitPromptLines(v interface{}) []string {
	raw, ok := v.(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func (e *EngineV2) execText2ImageNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	prompt := ""
	if p, ok := node.Data["prompt"].(string); ok {
		prompt = p
	}

	if prompt == "" {
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if text, ok := m["text"].(string); ok {
					prompt = text
				}
			}
		}
	}

	if prompt == "" {
		return nil, fmt.Errorf("text2image node has no prompt")
	}

	// 节点级权重覆盖：不同节点可以用不同 checkpoint（快/质两条线并存）
	ckptOverride, _ := node.Data["ckpt"].(string)

	// 后端（flux / moss / auto）。
	// MOSS 的价值是**图内文字渲染与 bbox 布局控制**——flux 这两件事都做不到，
	// 而且换 checkpoint 名也换不来：两套模型的 ComfyUI 构图完全不同。
	backendExplicit, _ := node.Data["backend"].(string)

	// 中文提示词自动翻译（flux 文本编码器以英文为主，纯中文提示词经常只画出
	// 通用风景——README「已知限制」第一条）。默认关；开了才会走 LLM。
	// 必须在进扩散侧之前做：LLM 是语言侧，顺序反了会和 flux 互相卸载权重
	// （同 H3 prompt_opt 的顺序纪律）。
	translated := false
	translateNote := ""
	if mode := translateModeOfNode(node); mode != "off" && e.llm != nil && hasCJK(prompt) {
		// 「自动」只在语言侧权重**已经驻留**时才翻：
		// 为一次翻译单独加载 LLM 再卸载，代价约 180 秒，比出图本身贵一个量级。
		// 这就是三态存在的理由——不是所有中文都值得付这次换侧。
		if mode == "auto" && e.ResidentSide() != sideLanguage {
			translateNote = "auto 模式：语言模型未驻留，本次跳过翻译（省一次换侧）；需要必翻请切到「总是先翻译」"
		} else {
			en, terr := e.translatePromptToEnglish(prompt)
			if terr != nil {
				// 翻译失败降级为原文，不阻断执行（flux 中文弱是降级不是错误），
				// 但必须在输出里留痕，不许假装翻过了
				translateNote = terr.Error()
			} else {
				prompt = en
				translated = true
			}
		}
	}

	// P2 壁垒：跨镜头一致性——图像节点同样吃场景锁。
	// 此前锁只作用于文生视频与提示词优化，分镜图 / 角色表这一支不受约束，
	// 于是同一角色在不同分镜图里长相漂移（视频锁得住、图锁不住 = 半个一致性）。
	// 锁取自上游 scene_grid 节点，或节点自填 scene_lock / grid。
	// 注：必须在翻译之后注入——锁是英文物理事实，不能被翻译再改写一遍。
	sceneLock, lockErr := resolveSceneLock(node, upstream)
	if lockErr != nil {
		return nil, lockErr
	}
	lockInjected := false
	withLock := func(p string) string {
		out, inj := applySceneLock(p, sceneLock)
		if inj {
			lockInjected = true
		}
		return out
	}
	prompt = withLock(prompt)

	// 24GB 上不能同时养 flux 与 14B LLM：换侧（清语言侧）并独占执行
	release := e.enterSide(sideDiffusion)
	defer release()

	// 显存调度：flux 与 LLM 抢同一块 24GB 显存，不纳入调度就会出现
	// 「LLM 已加载 + 直接起 flux」→ OOM。此前只有 LLM 节点走调度。
	if e.gpuSched != nil && e.t2i != nil && e.t2i.Checkpoint != "" {
		// 串行门：同层多个文生图节点不会同时进 ComfyUI（并行只会互相 OOM）
		e.gpuSched.LockText2Image()
		defer e.gpuSched.UnlockText2Image()

		_ = e.gpuSched.AcquireModel(e.t2i.Checkpoint)
		defer e.gpuSched.ReleaseModel(e.t2i.Checkpoint)
	}

	width := 1024
	height := 1024
	if w, ok := node.Data["width"].(float64); ok {
		width = int(w)
	}
	if h, ok := node.Data["height"].(float64); ok {
		height = int(h)
	}

	// 布局（MOSS 专有）：来自节点参数或上游 JSON 端口。
	// 上游能算出来（比如 LLM 输出版式）就比手填坐标靠谱，所以两边都收。
	layoutJSON := ""
	boxes := ParseMOSSBoxes(node.Data["layout"])
	if len(boxes) == 0 {
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if b := ParseMOSSBoxes(m["layout"]); len(b) > 0 {
					boxes = b
					break
				}
			}
		}
	}
	if len(boxes) > 0 {
		var lerr error
		layoutJSON, lerr = BuildMOSSLayoutJSON(prompt, boxes, width, height)
		if lerr != nil {
			return nil, fmt.Errorf("布局 JSON 编译失败：%w", lerr)
		}
	}
	backend, backendReason := ResolveT2IBackend(backendExplicit, prompt, layoutJSON != "")
	t2iOpt := T2IOptions{
		Backend: backend, Prompt: prompt,
		Width: width, Height: height, Ckpt: ckptOverride, Layout: layoutJSON,
	}
	if n, ok := node.Data["negative"].(string); ok {
		t2iOpt.Negative = n
	}
	if s, ok := node.Data["steps"].(float64); ok && int(s) > 0 {
		t2iOpt.Steps = int(s)
	}
	if c, ok := node.Data["cfg"].(float64); ok && c > 0 {
		t2iOpt.Cfg = c
	}

	// 实例选择：H3 在跑时默认实例是停的，出图前自动切回（已健康时是空操作）。
	// MOSS 走第三个实例（8190）：主实例是 0.31，没有 UNETLoader 的 moss 权重、
	// 没有 CLIPLoader 的 moss_image 类型、也没有 EmptyFlux2LatentImage，发过去必 400。
	profile := ""
	if backend == BackendMOSS {
		profile = "moss"
	}
	if err := e.ensureComfy(profile); err != nil {
		return nil, err
	}
	// TODO(改到这里被打断)：backend 解析出去了还没接上使用点，先用空引用占位避免编译断链
	_ = backend

	// 多提示词批量（分镜 / 角色表 / 表情包）：
	//   prompts 字段一行一条；或勾了 split_lines 时把上游整段文本按行拆。
	// 关键收益不是并行，而是**只加载一次 17GB 模型**——逐条跑每条都要重载。
	prompts := splitPromptLines(node.Data["prompts"])
	if len(prompts) < 2 {
		if v, ok := node.Data["split_lines"].(bool); ok && v {
			prompts = splitPromptLines(prompt)
		}
	}
	// 批量出图逐条注入：每张都是独立画面，只锁第一条等于没锁
	for i := range prompts {
		prompts[i] = withLock(prompts[i])
	}
	if len(prompts) > 1 {
		if len(prompts) > 12 {
			prompts = prompts[:12] // 单次图上限：再多只会把排队时间拉长，收益不再增长
		}
		var (
			paths []string
			err   error
		)
		if backend == BackendMOSS {
			paths, err = e.t2i.GenerateImagesWithBackend(prompts, t2iOpt)
		} else {
			paths, err = e.t2i.GenerateImagesFromPrompts(prompts, width, height)
		}
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"text":                prompts[0],
			"image":               paths[0],
			"images":              paths,
			"count":               len(paths),
			"prompts":             prompts,
			"translated":          translated,
			"translate_error":     translateNote,
			"scene_lock":          sceneLock,
			"scene_lock_injected": lockInjected,
			"backend":             string(backend),
			"backend_reason":      backendReason,
		}, nil
	}

	// 批量：一次提交出 n 张，共用同一次模型加载（省掉 n 次冷启动）。
	// 输出同时给 image（第一张）与 images（全部），旧工作流不用改。
	batch := 1
	if v, ok := node.Data["batch"].(float64); ok && int(v) > 1 {
		batch = int(v)
		if batch > 8 {
			batch = 8 // 24GB 上一次 8 张已经够用，再多只会把显存与时间拉满
		}
	}

	if batch > 1 {
		var paths []string
		var err error
		if backend == BackendMOSS {
			many := make([]string, 0, batch)
			for i := 0; i < batch; i++ {
				many = append(many, prompt)
			}
			paths, err = e.t2i.GenerateImagesWithBackend(many, t2iOpt)
		} else {
			paths, err = e.t2i.GenerateImagesWithCkpt(prompt, width, height, batch, ckptOverride)
		}
		if err != nil {
			return nil, err
		}
		if len(paths) == 0 {
			return nil, fmt.Errorf("批量出图没有产出")
		}
		return map[string]interface{}{
			"text":                prompt,
			"image":               paths[0],
			"images":              paths,
			"count":               len(paths),
			"translated":          translated,
			"translate_error":     translateNote,
			"scene_lock":          sceneLock,
			"scene_lock_injected": lockInjected,
			"backend":             string(backend),
			"backend_reason":      backendReason,
		}, nil
	}

	var (
		imagePath string
		err       error
	)
	if backend == BackendMOSS {
		// MOSS 的提示词可能是 layout JSON（有布局时）——输出里仍然回原始提示词，
		// 下游拿到的必须是人写的那句，不是引擎编出来的 JSON。
		imagePath, err = e.t2i.GenerateImageWithBackend(t2iOpt)
	} else {
		imagePath, err = e.t2i.GenerateImageWithCkpt(prompt, width, height, ckptOverride)
	}
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"text":                prompt,
		"image":               imagePath,
		"translated":          translated,
		"translate_error":     translateNote,
		"scene_lock":          sceneLock,
		"scene_lock_injected": lockInjected,
		"backend":             string(backend),
		"backend_reason":      backendReason,
	}, nil
}

// hasCJK 是否含中日韩文字（flux 文本编码器对 CJK 弱，英文才吃得动）。
// 日文假名一并算入：混在中文里说明是日系题材提示词，同样该翻。
func hasCJK(s string) bool {
	for _, r := range s {
		if (r >= 0x4E00 && r <= 0x9FFF) || // CJK 统一表意
			(r >= 0x3400 && r <= 0x4DBF) || // 扩展 A
			(r >= 0x3040 && r <= 0x30FF) || // 平假名 / 片假名
			(r >= 0xAC00 && r <= 0xD7AF) { // 韩文音节
			return true
		}
	}
	return false
}

func (e *EngineV2) execASRNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	audioPath := ""
	if p, ok := node.Data["audio_path"].(string); ok {
		audioPath = p
	}

	if audioPath == "" {
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if audio, ok := m["audio"].(string); ok {
					audioPath = audio
				}
			}
		}
	}

	if audioPath == "" {
		return nil, fmt.Errorf("asr node has no audio input")
	}

	release := e.enterSide(sideAudio)
	defer release()

	// 节点级模型档位：台词镜头用 large-v3、氛围镜头用 small，按镜头挑精度
	// （服务端多档缓存，切档不重启）。默认留空 = 服务端配置（small / cpu）。
	opt := ASROptions{}
	if m, ok := node.Data["model"].(string); ok && strings.TrimSpace(m) != "" {
		opt.Model = strings.TrimSpace(m)
	}
	if d, ok := node.Data["device"].(string); ok && strings.TrimSpace(d) != "" {
		opt.Device = strings.TrimSpace(d)
	}

	text, err := e.asr.TranscribeWith(audioPath, opt)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{"text": text, "model": opt.Model}, nil
}

func (e *EngineV2) execTTSNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	text := ""
	if p, ok := node.Data["text"].(string); ok {
		text = p
	}

	if text == "" {
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					text = t
				}
			}
		}
	}

	if text == "" {
		return nil, fmt.Errorf("tts node has no text input")
	}

	outputPath := fmt.Sprintf("data/generated/tts-%d.wav", time.Now().UnixNano())
	if p, ok := node.Data["output_path"].(string); ok && p != "" {
		outputPath = p
	}

	// 语音名 / 语速：Windows 内置 SAPI 直接支持；HTTP 服务端可选支持
	voice, _ := node.Data["voice"].(string)
	rate := 0
	if v, ok := node.Data["rate"].(float64); ok {
		rate = int(v)
	}
	// 零样本克隆（Audio8 专用）：参考音频 + 逐字稿。给了参考音频就不看 voice，
	// 因为克隆走的是 prompt 里的参考码本，voice 名对它没有意义。
	refAudio, _ := node.Data["ref_audio"].(string)
	refText, _ := node.Data["ref_text"].(string)
	if refAudio != "" {
		if refText == "" {
			return nil, fmt.Errorf("tts 节点指定了参考音频但没填参考音频文本：克隆需要「与录音逐字一致」的文本，空着会明显掉音质")
		}
		if _, statErr := os.Stat(refAudio); statErr != nil {
			return nil, fmt.Errorf("tts 参考音频不存在：%s（%v）", refAudio, statErr)
		}
		voice = ""
	}

	release := e.enterSide(sideAudio)
	defer release()

	// TTS 依赖没起时按需拉起。
	// `autostart=false` 的语义是「不常驻、不白占显存」，不是「必须手动点启动」——
	// 不自动起的话，节点会静默回退 Windows SAPI：产出的是系统音色，用户还以为是
	// Audio8 的结果（2026-10-23 实测踩到）。宁可多花 6 秒起服务，也不能给出错音色。
	if e.deps != nil && !e.deps.Healthy("tts") && e.deps.Startable("tts") {
		if startErr := e.deps.Start("tts"); startErr != nil {
			log.Printf("[tts] 按需启动失败（继续尝试，可能回退 SAPI）：%v", startErr)
		} else if waitErr := e.deps.WaitReady("tts", 300*time.Second); waitErr != nil {
			log.Printf("[tts] 依赖已启动但未就绪：%v", waitErr)
		}
	}

	backend, err := e.tts.SynthesizeWith(text, outputPath, TTSOptions{
		Voice: voice, Rate: rate, RefAudio: refAudio, RefText: refText,
	})
	if err != nil {
		return nil, err
	}
	// 统一输出绝对路径：下游节点与画廊复制粘贴都能直接用，不依赖进程 CWD
	if abs, absErr := filepath.Abs(outputPath); absErr == nil {
		outputPath = abs
	}

	return map[string]interface{}{
		"text":    text,
		"audio":   outputPath,
		"backend": backend,
	}, nil
}

func (e *EngineV2) execVideoNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	var images []string
	audioPath := ""

	for _, out := range upstream {
		if m, ok := out.(map[string]interface{}); ok {
			if img, ok := m["image"].(string); ok {
				images = append(images, img)
			}
			if audio, ok := m["audio"].(string); ok {
				audioPath = audio
			}
		}
	}

	if len(images) == 0 {
		return nil, fmt.Errorf("video compose node has no image input")
	}

	outputPath := fmt.Sprintf("data/generated/video-%d.mp4", time.Now().UnixNano())
	if p, ok := node.Data["output_path"].(string); ok && p != "" {
		outputPath = p
	}

	// 每张图轮播时长（秒），默认 3；上限由 ComposeVideoMulti 钳制
	perImage := 3.0
	if v, ok := node.Data["image_duration"].(float64); ok && v > 0 {
		perImage = v
	}

	err := e.video.ComposeVideoMulti(images, audioPath, outputPath, perImage)
	if err != nil {
		return nil, err
	}
	if abs, absErr := filepath.Abs(outputPath); absErr == nil {
		outputPath = abs
	}

	return map[string]interface{}{
		"video": outputPath,
	}, nil
}

func (e *EngineV2) execIfElseNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	// 获取上游文本
	inputText := ""
	for _, out := range upstream {
		if m, ok := out.(map[string]interface{}); ok {
			if text, ok := m["text"].(string); ok {
				inputText = text
			}
		}
	}

	// 条件配置
	conditionType, _ := node.Data["condition_type"].(string)
	compareValue, _ := node.Data["compare_value"].(string)

	branch := "false"
	if matchCondition(conditionType, inputText, compareValue) {
		branch = "true"
	}

	return map[string]interface{}{
		"text":   inputText,
		"branch": branch,
	}, nil
}

// matchCondition 通用条件判定，if_else 分支与 loop 的 until 终止共用
func matchCondition(condType, text, compareValue string) bool {
	switch condType {
	case "contains":
		return strings.Contains(text, compareValue)
	case "not_contains":
		return !strings.Contains(text, compareValue)
	case "not_empty":
		return strings.TrimSpace(text) != ""
	case "empty":
		return strings.TrimSpace(text) == ""
	case "equals":
		return text == compareValue
	}
	return false
}

func (e *EngineV2) execEndNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	for _, out := range upstream {
		return out, nil
	}
	return map[string]interface{}{"text": "done"}, nil
}

// executeInnerWorkflow 执行被循环引用的内层工作流（loop 节点的执行体）。
func (e *EngineV2) executeInnerWorkflow(ctx context.Context, wfID uint, injectText string, overwrite bool) (interface{}, error) {
	nodesJSON, edgesJSON, err := e.resolveWorkflow(wfID)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow %d failed: %w", wfID, err)
	}

	var sub []model.NodeInstance
	if err := json.Unmarshal(nodesJSON, &sub); err != nil {
		return nil, fmt.Errorf("parse workflow %d nodes failed: %w", wfID, err)
	}

	if injectText != "" && len(sub) > 0 && sub[0].Type == "start" {
		if cur, _ := sub[0].Data["prompt"].(string); overwrite || cur == "" {
			sub[0].Data["prompt"] = injectText
		}
	}

	subNodesJSON, err := json.Marshal(sub)
	if err != nil {
		return nil, err
	}

	states, err := e.runWithContext(ctx, subNodesJSON, edgesJSON, nil, "", nil)
	if err != nil {
		return nil, fmt.Errorf("workflow %d failed: %w", wfID, err)
	}

	// 返回内层 end 节点输出；缺失时退化为最后一个节点输出
	for i := range states {
		if states[i].Type == "end" && states[i].Status == "success" {
			return states[i].Output, nil
		}
	}
	if len(states) > 0 {
		return states[len(states)-1].Output, nil
	}
	return map[string]interface{}{"text": ""}, nil
}

func dataInt(data map[string]interface{}, key string, def int) int {
	switch v := data[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return def
}

// upstreamText 取上游输出里第一个非空 text
func upstreamText(upstream map[string]interface{}) string {
	for _, out := range upstream {
		if m, ok := out.(map[string]interface{}); ok {
			if t, ok := m["text"].(string); ok && t != "" {
				return t
			}
		}
		if s, ok := out.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// execLoopNode 循环节点：迭代执行内层工作流
//
//	mode=count  固定次数（iterations），每轮以上一轮输出为输入（链式精炼）
//	mode=foreach 遍历 items（或按 separator 切分上游文本），逐项输入
//	mode=until  至少跑一轮，直到 stop_condition 命中后停止
//
// 安全阀：max_iterations（默认 20，硬上限 100）
func (e *EngineV2) execLoopNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	runID := runIDFrom(ctx)
	if e.resolveWorkflow == nil {
		return nil, fmt.Errorf("loop resolver not configured")
	}

	wfID := uint(dataInt(node.Data, "workflow_id", 0))
	if wfID == 0 {
		return nil, fmt.Errorf("loop node has no workflow_id")
	}

	mode, _ := node.Data["mode"].(string)
	if mode == "" {
		mode = "count"
	}

	maxIter := dataInt(node.Data, "max_iterations", 20)
	if maxIter <= 0 {
		maxIter = 20
	}
	if maxIter > maxLoopIterations {
		maxIter = maxLoopIterations
	}

	if d := e.depth.Add(1); d > maxSubWorkflowDepth {
		e.depth.Add(-1)
		return nil, fmt.Errorf("loop nesting too deep (>%d)", maxSubWorkflowDepth)
	}
	defer e.depth.Add(-1)

	seed := upstreamText(upstream)
	if v, ok := node.Data["input"].(string); ok && v != "" {
		seed = v
	}

	// foreach：优先显式 items 数组，其次按分隔符切分上游文本
	var items []string
	if mode == "foreach" {
		if arr, ok := node.Data["items"].([]interface{}); ok {
			for _, it := range arr {
				items = append(items, fmt.Sprintf("%v", it))
			}
		}
		if len(items) == 0 {
			sep, _ := node.Data["separator"].(string)
			if sep == "" {
				sep = "\n"
			}
			for _, line := range strings.Split(seed, sep) {
				if t := strings.TrimSpace(line); t != "" {
					items = append(items, t)
				}
			}
		}
		if len(items) == 0 {
			return map[string]interface{}{
				"text":       "",
				"iterations": 0,
				"stopped_by": "empty_items",
				"items":      []interface{}{},
			}, nil
		}
		if len(items) > maxIter {
			items = items[:maxIter]
		}
	}

	condType, _ := node.Data["condition_type"].(string)
	condValue, _ := node.Data["compare_value"].(string)
	if condType == "" {
		condType = "not_empty"
	}

	current := seed
	var texts []string
	records := make([]interface{}, 0, maxIter)
	stoppedBy := ""
	iter := 0

	for {
		if iter >= maxIter {
			stoppedBy = "max_iterations"
			break
		}
		if mode == "foreach" && iter >= len(items) {
			stoppedBy = "exhausted"
			break
		}

		input := current
		if mode == "foreach" {
			input = items[iter]
		}

		out, err := e.executeInnerWorkflow(ctx, wfID, input, true)
		if err != nil {
			return nil, fmt.Errorf("loop iteration %d failed: %w", iter+1, err)
		}

		text := ""
		if m, ok := out.(map[string]interface{}); ok {
			text, _ = m["text"].(string)
		}
		texts = append(texts, text)
		records = append(records, map[string]interface{}{
			"index":  iter + 1,
			"input":  input,
			"output": text,
		})
		iter++

		// 每轮广播进度（低频事件，前端可显示迭代计数）
		e.emit(runID, Event{
			Type:   "loop_iteration",
			NodeID: node.ID,
			Data: map[string]interface{}{
				"iteration": iter,
				"max":       maxIter,
				"output":    truncateRunes(text, 120),
			},
		})
		if mode == "until" && matchCondition(condType, text, condValue) {
			stoppedBy = "condition"
			break
		}

		current = text
		if mode == "count" {
			// count 模式按 iterations 指定的次数跑
			want := dataInt(node.Data, "iterations", 3)
			if want <= 0 {
				want = 3
			}
			if want > maxIter {
				want = maxIter
			}
			if iter >= want {
				stoppedBy = "completed"
				break
			}
		}
	}

	last := seed
	if len(texts) > 0 {
		last = texts[len(texts)-1]
	}

	return map[string]interface{}{
		"text":       last,
		"all":        strings.Join(texts, "\n---\n"),
		"iterations": iter,
		"stopped_by": stoppedBy,
		"items":      records,
	}, nil
}

// truncateRunes 按字符截断（避免切坏多字节）
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// translateModeOfNode 提示词翻译策略：off / auto / force。
//
// 兼容旧数据：只有 auto_translate 布尔时，true 视为 force（保持老工作流行为完全一致），
// false 视为 off。新字段优先。
func translateModeOfNode(n *model.NodeInstance) string {
	if n == nil || n.Data == nil {
		return "off"
	}
	if m, ok := n.Data["translate_mode"].(string); ok {
		switch m {
		case "off", "auto", "force":
			return m
		}
	}
	if v, ok := n.Data["auto_translate"].(bool); ok && v {
		return "force"
	}
	return "off"
}
