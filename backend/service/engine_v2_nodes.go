package service

// 本文件只放「节点执行体」：每个节点的业务逻辑。
// 引擎内核（DAG 调度 / 超时 / 事件 / 取消 / 注册表）留在 engine_v2.go。
// 拆分动因见 docs/系统评估-2026-10-21.md §3 S2：单文件 2283 行时，
// 改一个节点要滚 2000 行上下文，且内核与节点混杂无法独立推理。

import (
	"context"
	"encoding/json"
	"fmt"
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
	runID := runIDFrom(ctx)
	instruction := ""
	if p, ok := node.Data["prompt"].(string); ok {
		instruction = p
	}

	// 能力管线：输入段先把上游文本过一遍（翻译 / 清洗 / 抽字段），
	// 本体段用角色卡替换提示词，输出段再把结果整形。
	pipe := ParsePipeline(node.Data)

	// 本体角色卡优先于节点自填的提示词：贴卡的动作本身就是"我要按这张卡的方式干活"，
	// 若两者拼接，卡片里"只输出 JSON"这类硬约束会被旧提示词稀释掉。
	boundTools := []string(nil)
	if pipe.Body != nil {
		if sys, ok := e.cardSystem(*pipe.Body); ok {
			instruction = sys
			if pipe.Body.Kind == CardAgent && e.cards != nil {
				if c, ok := e.cards.Agent(pipe.Body.ID); ok {
					boundTools = c.Tools
				}
			}
		}
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

	// 工具调用：节点声明了 tools 时走「工具轮（非流式）+ 最终答案轮（流式）」
	toolNames := nodeToolNames(node)
	if len(toolNames) == 0 && len(boundTools) > 0 {
		toolNames = boundTools // 节点没勾就用卡片默认工具（就近优先：节点 > 卡片）
	}
	if len(toolNames) > 0 && svc != nil {
		res, err := e.execLLMWithTools(ctx, node, prompt, reqModel, toolNames, svc)
		if err != nil {
			return nil, err
		}
		return res, nil
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

	// 流式优先：token 经 WS 实时推送前端（打字机效果）
	if e.llmStream != nil {
		ch, err := e.llmStream.ChatStreamWithModel(prompt, reqModel)
		if err != nil {
			return nil, err
		}
		var sb strings.Builder
		usage := LLMUsage{}
		for chunk := range ch {
			if chunk.Done {
				// Done 块携带本次请求的完整 token 用量
				usage = chunk.Usage
				break
			}
			sb.WriteString(chunk.Content)
			e.emit(runID, Event{Type: "node_token", NodeID: node.ID, Data: map[string]interface{}{"delta": chunk.Content}})
		}
		out := map[string]interface{}{"text": sb.String()}
		if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
			out["_usage"] = usage // 供事件日志汇总 token 消耗
		}
		return out, nil
	}

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

// nodeToolNames 读取节点声明的工具名列表（data.tools，支持数组或逗号串）
func nodeToolNames(node *model.NodeInstance) []string {
	raw, ok := node.Data["tools"]
	if !ok {
		return nil
	}
	var names []string
	switch v := raw.(type) {
	case []interface{}:
		for _, it := range v {
			if str, ok := it.(string); ok && str != "" {
				names = append(names, str)
			}
		}
	case []string:
		for _, str := range v {
			if str != "" {
				names = append(names, str)
			}
		}
	case string:
		for _, part := range strings.Split(v, ",") {
			if t := strings.TrimSpace(part); t != "" {
				names = append(names, t)
			}
		}
	}
	return names
}

// execLLMWithTools 工具调用循环：
//  1. 带 tools 声明请求模型（非流式，需要完整 tool_calls）
//  2. 模型要工具 → 本地执行 → 结果回填 messages → 再请求
//  3. 模型给出最终答案 → 流式重放一次以保留打字机效果
//
// 轮次上限 4，防止模型无限索要工具
func (e *EngineV2) execLLMWithTools(ctx context.Context, node *model.NodeInstance, prompt, reqModel string, toolNames []string, svc *LLMService) (interface{}, error) {
	// 轮数可配：写死 4 轮会把"查三次知识库再总结"这类合法任务截断在第三步。
	maxRounds := 4
	if v, ok := node.Data["max_tool_rounds"].(float64); ok && v >= 1 && v <= 12 {
		maxRounds = int(v)
	}
	// 工具失败是否中止：默认继续（工具是辅助），流水线要求严格时可打开。
	strictTools, _ := node.Data["tool_strict"].(bool)
	runID := runIDFrom(ctx)
	usage := LLMUsage{} // 累计所有工具轮与最终答案轮的 token 消耗

	if e.tools == nil {
		e.tools = NewToolExecutor(e.http, e.rag)
	}
	specs := e.tools.Specs(toolNames)
	if len(specs) == 0 {
		return nil, fmt.Errorf("LLM node declares tools %v but none are known", toolNames)
	}

	messages := []toolChatMessage{{Role: "user", Content: prompt}}
	trace := make([]map[string]interface{}, 0, 4)

	for round := 0; round < maxRounds; round++ {
		msg, u, err := svc.ChatWithToolsUsage(messages, specs, reqModel)
		if err != nil {
			return nil, err
		}
		usage = usage.add(u)

		calls := msg.ToolCalls
		textMode := false
		if len(calls) == 0 {
			// 兼容不支持 tool_calls 字段的模型（如 qwen2.5 会把调用写成 content 里的 JSON）
			if parsed := parseTextToolCalls(msg.Content, specs); len(parsed) > 0 {
				calls = parsed
				textMode = true
			} else {
				final := msg.Content
				// 自定义端点不用流式（协议未必一致），其余保持打字机效果
				if e.llmStream != nil && svc == e.llm {
					messages = append(messages, toolChatMessage{Role: "assistant", Content: msg.Content})
					if streamed, answerUsage, serr := e.streamFinalAnswer(ctx, node, messages, reqModel); serr == nil && streamed != "" {
						final = streamed
						usage = usage.add(answerUsage)
					}
				}
				out := map[string]interface{}{"text": final}
				if len(trace) > 0 {
					out["tool_calls"] = trace
					out["tool_rounds"] = len(trace)
				}
				if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
					out["_usage"] = usage
				}
				return out, nil
			}
		}

		// 标准 tool_calls：assistant 消息带工具请求入历史
		// 文本形态：把模型原始输出作为 assistant 内容，工具结果用 user 消息回填（模型无关）
		if textMode {
			messages = append(messages, toolChatMessage{Role: "assistant", Content: msg.Content})
		} else {
			messages = append(messages, *msg)
		}

		for _, tc := range calls {
			e.emit(runID, Event{
				Type:   "tool_call",
				NodeID: node.ID,
				Data:   map[string]interface{}{"tool": tc.Function.Name, "arguments": tc.Function.Arguments},
			})
			result, runErr := e.tools.Run(tc.Function.Name, tc.Function.Arguments)
			status := "ok"
			if runErr != nil {
				result = "工具执行失败: " + runErr.Error()
				status = "error"
				// strict 模式：工具挂了就停，而不是让模型拿着"工具执行失败"继续编答案。
				// 需要严格门禁的流水线打开这个开关。
				if strictTools {
					return nil, fmt.Errorf("工具 %s 执行失败（tool_strict 开启，中止）：%w", tc.Function.Name, runErr)
				}
			}
			rec := map[string]interface{}{
				"tool":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
				"result":    truncateRunes(result, 500),
				"status":    status,
			}
			if textMode {
				rec["mode"] = "text"
			}
			trace = append(trace, rec)

			if textMode {
				messages = append(messages, toolChatMessage{
					Role: "user",
					Content: "工具 " + tc.Function.Name + " 的执行结果：\n" + result +
						"\n\n请基于这个结果直接给出最终回答，不要再输出工具调用的 JSON。",
				})
			} else {
				messages = append(messages, toolChatMessage{Role: "tool", ToolCallID: tc.ID, Content: result})
			}
		}
	}

	return nil, fmt.Errorf("tool calling exceeded %d rounds", maxRounds)
}

// streamFinalAnswer 用完整消息历史再做一次流式补全（保留打字机效果）
func (e *EngineV2) streamFinalAnswer(ctx context.Context, node *model.NodeInstance, messages []toolChatMessage, reqModel string) (string, LLMUsage, error) {
	runID := runIDFrom(ctx)
	ch, err := e.llmStream.ChatStreamMessages(messages, reqModel)
	if err != nil {
		return "", LLMUsage{}, err
	}
	var sb strings.Builder
	usage := LLMUsage{}
	for chunk := range ch {
		if chunk.Done {
			usage = chunk.Usage
			break
		}
		sb.WriteString(chunk.Content)
		e.emit(runID, Event{Type: "node_token", NodeID: node.ID, Data: map[string]interface{}{"delta": chunk.Content}})
	}
	return sb.String(), usage, nil
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

	release := e.enterSide(sideAudio)
	defer release()

	backend, err := e.tts.SynthesizeWith(text, outputPath, TTSOptions{Voice: voice, Rate: rate})
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

func (e *EngineV2) execHTTPNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	method, _ := node.Data["method"].(string)
	if method == "" {
		method = "GET"
	}

	url, _ := node.Data["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("http node has no url")
	}

	body, _ := node.Data["body"].(string)
	headers := make(map[string]string)
	if h, ok := node.Data["headers"].(map[string]interface{}); ok {
		for k, v := range h {
			headers[k] = fmt.Sprintf("%v", v)
		}
	}

	// 凭据注入：节点只写凭据名，明文 header 不落在工作流 JSON 里
	credName, _ := node.Data["credential"].(string)
	if credName != "" {
		if e.creds == nil {
			return nil, fmt.Errorf("http node 引用了凭据 %q，但凭据仓库未初始化", credName)
		}
		credHeaders, credQuery, err := e.creds.Resolve(credName)
		if err != nil {
			return nil, err
		}
		for k, v := range credHeaders {
			// 节点显式写的 header 优先，便于临时覆盖
			if _, exists := headers[k]; !exists {
				headers[k] = v
			}
		}
		if len(credQuery) > 0 {
			url = appendQuery(url, credQuery)
		}
	}

	respBody, statusCode, err := e.http.HTTPRequest(method, url, headers, body)
	if err != nil {
		return nil, err
	}

	out := map[string]interface{}{
		"text":        respBody,
		"status_code": statusCode,
	}
	// 契约声明了 json 端口就得真给：响应是合法 JSON 时解成结构化值，
	// 下游可以直接 {{id.json.field}} 取值，不用再套一层 code 节点去 parse。
	// 之前这里只返回 text —— 契约与实现不一致，用户按契约连线会拿到空值。
	if trimmed := strings.TrimSpace(respBody); strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var any interface{}
		if json.Unmarshal([]byte(trimmed), &any) == nil {
			out["json"] = any
		}
	}
	return out, nil
}

func (e *EngineV2) execTransformNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	inputText := upstreamText(upstream)

	template, _ := node.Data["template"].(string)
	if template == "" {
		return map[string]interface{}{"text": inputText}, nil
	}

	// 输入是合法 JSON 时按对象处理（支持 {{a.b}}），否则只替换 {{input}}
	var data interface{} = inputText
	if m, err := parseJSONObject(inputText); err == nil {
		m["input"] = inputText
		data = m
	}

	return map[string]interface{}{"text": replacePlaceholders(template, data)}, nil
}

// parseJSONObject 尝试把文本解析为 JSON 对象
func parseJSONObject(s string) (map[string]interface{}, error) {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (e *EngineV2) execRAGNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	// 获取查询文本
	query := ""
	if q, ok := node.Data["query"].(string); ok {
		query = q
	}

	if query == "" {
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if text, ok := m["text"].(string); ok {
					query = text
				}
			}
		}
	}

	if query == "" {
		return nil, fmt.Errorf("rag node has no query")
	}

	// 检索知识库（top_k 可由节点数据配置，默认 3）
	topK := 3
	if tk, ok := node.Data["top_k"].(float64); ok && tk > 0 {
		topK = int(tk)
	}
	// 检索一次，同时产出「文本上下文」与「结构化引用」
	// （此前 GetContext 内部会再检索一次，等于每次 RAG 节点多付一轮 embedding）
	results, err := e.rag.Query(query, topK)
	if err != nil {
		return nil, err
	}
	context := e.rag.ContextFrom(results)

	// 结构化引用：下游可写 {{rag-1.sources[0].content}} 精确取某一条，
	// 也能据此在 UI 上做点击跳转溯源
	sources := make([]interface{}, 0, len(results))
	for _, r := range results {
		idx := 0
		if v, ok := r.Record.Metadata["chunk_index"].(float64); ok {
			idx = int(v)
		}
		sources = append(sources, map[string]interface{}{
			"source":      r.Record.Source,
			"chunk_index": idx,
			"score":       r.Score,
			"content":     r.Record.Content,
		})
	}

	// 拼接上下文和查询
	fullPrompt := query
	if context != "" {
		fullPrompt = fmt.Sprintf("参考以下资料回答问题：\n\n%s\n\n问题：%s", context, query)
	}

	return map[string]interface{}{
		"text":    fullPrompt,
		"context": context,
		"sources": sources,
		"hits":    len(results),
	}, nil
}

// executeInnerWorkflow 解析并内联执行一个内层工作流
// injectText 非空时注入内层 start：overwrite=true 强制覆盖（循环每轮迭代用），
// false 仅在 start 未配置 prompt 时注入（子工作流节点语义）
// executeInnerWorkflow 解析并内联执行一个内层工作流。
// ctx 透传使内层节点的事件也归属同一次运行（否则子工作流执行过程不可追溯）。
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

// execSubWorkflowNode 子工作流：解析 workflow_id → 注入上游文本到内层 start → 嵌套执行 → 返回内层 end 输出
func (e *EngineV2) execSubWorkflowNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	if e.resolveWorkflow == nil {
		return nil, fmt.Errorf("sub_workflow resolver not configured")
	}

	wfID := uint(dataInt(node.Data, "workflow_id", 0))
	if wfID == 0 {
		return nil, fmt.Errorf("sub_workflow node has no workflow_id")
	}

	if d := e.depth.Add(1); d > maxSubWorkflowDepth {
		e.depth.Add(-1)
		return nil, fmt.Errorf("sub_workflow nesting too deep (>%d)", maxSubWorkflowDepth)
	}
	defer e.depth.Add(-1)

	out, err := e.executeInnerWorkflow(ctx, wfID, upstreamText(upstream), false)
	if err != nil {
		return nil, fmt.Errorf("sub workflow %d: %w", wfID, err)
	}
	return out, nil
}

// dataInt 从节点数据里取整数（JSON 数字是 float64，也兼容 int）
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
