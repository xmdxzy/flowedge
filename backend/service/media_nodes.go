package service

import (
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"flowedge/backend/model"
)

// 图像编辑类节点：图生图 / 局部重绘 / 超分 / 文生视频
//
// 四个节点共用「上游找图 → 交给 ComfyUI 图 → 落盘」这条链路，
// 差异只在 BuildMediaGraph 的分支里，节点本身保持很薄。

// ensureComfy 确保指定 ComfyUI 实例在跑（"h3" 走 8189，"moss" 走 8190，其它走默认 8188）。
//
// 为什么必须显式做：两个实例互斥，H3 在跑时默认实例是停的；
// 用户切去出图如果不自动切回，节点会直接收到 connection refused。
// 已经健康时 Start 是空操作，不产生额外开销。
func (e *EngineV2) ensureComfy(profile string) error {
	if e.deps == nil {
		return nil
	}
	name := "comfyui"
	switch profile {
	case "h3":
		name = "comfy_h3"
	case "moss":
		name = "comfy_moss"
	}
	if err := e.deps.Start(name); err != nil {
		return fmt.Errorf("ComfyUI 实例（%s）不可用：%w", name, err)
	}
	return nil
}

// pickImageInput 从节点配置或上游输出里找一张图
func pickImageInput(node *model.NodeInstance, upstream map[string]interface{}, key string) string {
	if p, ok := node.Data[key].(string); ok && strings.TrimSpace(p) != "" {
		return strings.TrimSpace(p)
	}
	if img, ok := firstUpstreamImage(upstream); ok {
		return img
	}
	return ""
}

func firstUpstreamImage(upstream map[string]interface{}) (string, bool) {
	for _, out := range upstream {
		m, ok := out.(map[string]interface{})
		if !ok {
			continue
		}
		if img, ok := m["image"].(string); ok && img != "" {
			return img, true
		}
		if arr, ok := m["images"].([]interface{}); ok && len(arr) > 0 {
			if s, ok := arr[0].(string); ok && s != "" {
				return s, true
			}
		}
	}
	return "", false
}

// imageSize 读图片宽高（重绘要把蒙版缩放到同尺寸）
func imageSize(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("图片打不开：%s", path)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, fmt.Errorf("无法识别图片尺寸：%s", filepath.Base(path))
	}
	return cfg.Width, cfg.Height, nil
}

// execImage2ImageNode 图生图：以输入图为底，按 strength 重绘
func (e *EngineV2) execImage2ImageNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	release := e.enterSide(sideDiffusion)
	defer release()
	if err := e.ensureComfy(""); err != nil {
		return nil, err
	}

	src := pickImageInput(node, upstream, "image_path")
	if src == "" {
		return nil, fmt.Errorf("图生图缺少输入图：连一条来自文生图的线，或在配置里填图片路径")
	}
	prompt, _ := node.Data["prompt"].(string)
	// 场景锁（P2）：图生图同样受跨镜头一致性约束，规则与文生图一致
	sceneLock, lockErr := resolveSceneLock(node, upstream)
	if lockErr != nil {
		return nil, lockErr
	}
	lockInjected := false
	if out, inj := applySceneLock(prompt, sceneLock); inj {
		prompt, lockInjected = out, true
	}
	strength := 0.6
	if v, ok := node.Data["strength"].(float64); ok && v > 0 {
		strength = v
	}
	steps := 8
	if v, ok := node.Data["steps"].(float64); ok && v > 0 {
		steps = int(v)
	}

	path, kind, err := e.t2i.GenerateMedia(MediaRequest{
		Kind: "img2img", Prompt: prompt, Image: src, Strength: strength, Steps: steps,
	})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"image": path, "kind": kind, "source": src, "strength": strength, "text": prompt,
		"scene_lock": sceneLock, "scene_lock_injected": lockInjected,
	}, nil
}

// execInpaintNode 局部重绘：蒙版白色区域按 prompt 重画
func (e *EngineV2) execInpaintNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	release := e.enterSide(sideDiffusion)
	defer release()
	if err := e.ensureComfy(""); err != nil {
		return nil, err
	}

	src := pickImageInput(node, upstream, "image_path")
	mask, _ := node.Data["mask_path"].(string)
	if src == "" {
		return nil, fmt.Errorf("重绘缺少原图：连一条上游图片线，或填写图片路径")
	}
	if strings.TrimSpace(mask) == "" {
		return nil, fmt.Errorf("重绘缺少蒙版：请填 mask_path（白色区域会被重画）")
	}
	w, h, err := imageSize(src)
	if err != nil {
		return nil, err
	}
	mw, mh, merr := imageSize(mask)
	if merr != nil {
		return nil, merr
	}
	if mw != w || mh != h {
		return nil, fmt.Errorf("蒙版尺寸（%d×%d）必须与原图（%d×%d）一致", mw, mh, w, h)
	}
	prompt, _ := node.Data["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("重绘缺少提示词：蒙版区域要画成什么")
	}
	denoise := 1.0
	if v, ok := node.Data["denoise"].(float64); ok && v > 0 {
		denoise = v
	}
	steps := 10
	if v, ok := node.Data["steps"].(float64); ok && v > 0 {
		steps = int(v)
	}
	// checkpoint 覆盖：填 Flux-Fill 得专业重绘（先落位 D:/AI/Models 再用），
	// 留空走服务默认（flux-schnell，蒙版内只做弱修补）
	inpaintModel := ""
	if m, ok := node.Data["model"].(string); ok {
		inpaintModel = strings.TrimSpace(m)
	}

	path, kind, err := e.t2i.GenerateMedia(MediaRequest{
		Kind: "inpaint", Prompt: prompt, Image: src, Mask: mask,
		Width: w, Height: h, Strength: denoise, Steps: steps,
		Model: inpaintModel,
	})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"image": path, "kind": kind, "source": src, "mask": mask, "text": prompt,
	}, nil
}

// execUpscaleNode 超分：RealESRGAN 等放大模型
func (e *EngineV2) execUpscaleNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	release := e.enterSide(sideDiffusion)
	defer release()
	if err := e.ensureComfy(""); err != nil {
		return nil, err
	}

	src := pickImageInput(node, upstream, "image_path")
	if src == "" {
		return nil, fmt.Errorf("超分缺少输入图：连一条上游图片线，或填写图片路径")
	}
	modelName, _ := node.Data["model"].(string)

	path, kind, err := e.t2i.GenerateMedia(MediaRequest{Kind: "upscale", Image: src, Model: modelName})
	if err != nil {
		return nil, err
	}
	w, h, _ := imageSize(path)
	return map[string]interface{}{
		"image": path, "kind": kind, "source": src, "width": w, "height": h,
	}, nil
}

// execText2VideoNode 文生视频（Wan2.1-T2V-1.3B）
func (e *EngineV2) execText2VideoNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	prompt, _ := node.Data["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		prompt = upstreamText(upstream)
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("文生视频缺少提示词")
	}

	// 两条后端可选：
	//   wan（默认）9.2GB 权重，832x480x33 帧，实测 64 秒，无声
	//   h3        34GB 权重，608x352x124 帧，实测 ~2 分钟，**自带立体声**
	backend, _ := node.Data["backend"].(string)
	kind := "t2v"
	firstFrame := ""
	seconds := 2
	frames := int(fieldFloat(node, "frames", 33))
	seconds = (frames + 23) / 24
	if seconds < 1 {
		seconds = 1
	}

	// H3 的提示词质量对成片影响极大（官方有专门 prompt guide）：
	// 默认先把原始想法改写成「镜头 + 声音分镜」的标准格式再进模型。
	// 必须在进扩散侧之前做（LLM 是语言侧，顺序反了会互相卸载权重）。
	optimize := false
	if backend == "h3" {
		optimize = true
		if v, ok := node.Data["prompt_opt"].(bool); ok {
			optimize = v
		}
	}
	// 分段数提前算：优化（语言侧）阶段就要把总指令拆成各段的衔接提示词
	segs := 1
	if v, ok := node.Data["segments"].(float64); ok && int(v) > 1 {
		segs = int(v)
		if segs > 8 {
			segs = 8 // 8×5.17s≈41s；再长应该拆成多个镜头节点编排
		}
	}

	original := prompt
	beats := []string{}
	// 场景锁在优化之前取：来自上游「场景九宫格」节点（或节点自填）。
	// 只能取一次——多段各自编译就等于各自解释场景，锁就没有意义了。
	sceneLock, lockErr := resolveSceneLock(node, upstream)
	if lockErr != nil {
		return nil, lockErr
	}
	// 参考资产先看在优化之前：有图/音频就要按 Ref2VA 的六段式写。
	// 旧实现一律用 T2VA 指令优化，再拿字符串套一层六段式外壳——
	// 结果是 subject_definitions 里出现 "<Picture 1> defines the visual identity"，
	// 而官方要求身份靠 <Subject N> 定义、图片源引在其中，标签语义错，身份就靠运气。
	refImgHint := stringList(node.Data, "ref_images")
	refAudHint := stringList(node.Data, "ref_audio")
	useRef2VA := backend == "h3" && (len(refImgHint) > 0 || len(refAudHint) > 0)
	var promptWarnings []string
	if optimize {
		releaseLang := e.enterLanguageSide("")
		var opt string
		var oerr error
		if useRef2VA {
			opt, oerr = e.OptimizePromptForRef2VAWithGrid(ctx, prompt, seconds, len(refImgHint), len(refAudHint), sceneLock)
		} else {
			opt, oerr = e.OptimizePromptForH3WithGrid(ctx, prompt, seconds, sceneLock)
		}
		if oerr == nil {
			prompt = opt
		}
		// 多段时把总指令拆成 N 条衔接提示词（起承转合 + 逐段重复角色外貌）。
		// 同样在语言侧完成，失败则所有段共用总指令（退化为同题续接）。
		if oerr == nil && backend == "h3" && segs > 1 {
			if b, berr := e.SplitPromptIntoBeats(prompt, segs); berr == nil {
				beats = b
			}
		}
		releaseLang()
		if oerr != nil {
			// 优化失败不炸工作流：用原文继续，并在输出里注明。
			// 但声音字段仍要兜底——空字段会让 H3 自行脑补人声。
			prompt = original + ""
			prompt, _ = ensureH3AudioFields(prompt)
			if sceneLock != "" {
				prompt = InjectSceneLock(prompt, sceneLock)
			}
			_ = oerr
		} else {
			prompt = opt
		}
		// 三段式字段兜底 + 无台词镜头的正面口型约束（官方：cfg=1 无负分支，
		// 靠「嘴唇闭合」这类可见事实压制人声，比写「无对白」有效）
		if backend == "h3" {
			prompt, _ = ensureH3AudioFields(prompt)
			prompt = ensureDialogueFree(prompt)
			// 多段共用同一句环境声，避免每段音景重置（接缝处会"断一下"）
			sharedSound := h3FieldValue(prompt, "overall_soundscape")
			for i := range beats {
				beats[i], _ = ensureH3AudioFields(beats[i])
				beats[i] = ensureDialogueFree(beats[i])
				if sceneLock != "" {
					// 先剥后注：拆段时 LLM 可能改写锁句（换个介词、调个词序），
					// 肉眼看不出来，但两段已经不是同一串字节——场景就在这一步开始漂。
					beats[i] = InjectSceneLock(beats[i], sceneLock)
				}
				if sharedSound != "" {
					beats[i] = setH3Field(beats[i], "overall_soundscape", sharedSound)
				}
			}
			// 场景锁门：只检查"有没有写"没有意义，必须按原文全等比对。
			// 缺段 / 漂移都算 FAIL——成片里表现为接缝处换场地，而时长帧数全部正常。
			if sceneLock != "" && len(beats) > 0 {
				for _, it := range auditSceneConsistency(append([]string{prompt}, beats...)) {
					log.Printf("[h3] 场景锁体检：%s %s", it.Code, it.Msg)
					promptWarnings = append(promptWarnings, it.Code+": "+it.Msg)
				}
			}
			// 机器体检：官方指南里最容易漏的是结构性缺失（说话人没音色、
			// 镜头没幅度、身份标签用错、有音频却没 fully_copy），这些能确定性判断
			if issues := auditH3Prompt(prompt); len(issues) > 0 {
				log.Printf("[h3] 提示词体检：%s", auditSummary(issues))
				for _, it := range issues {
					if it.Level == "FAIL" {
						promptWarnings = append(promptWarnings, it.Code+": "+it.Msg)
					}
				}
			}
		}
	}

	release := e.enterSide(sideDiffusion)
	defer release()

	kindIsH3 := false
	var refImgs, refAuds []string
	if backend == "h3" {
		kind = "h3"
		kindIsH3 = true
		// 有首帧就走图生视频（fl2va 的本职），没有就是文生视频
		if img := pickImageInput(node, upstream, "image_path"); img != "" {
			kind = "h3_i2v"
			firstFrame = img
		}
		// 参考图 / 参考音频 → 走 ref2va（唯一支持"用自己的音频念稿 + 口型同步"的链路）。
		// 注意它跟首帧引导不是一回事：ref2va 自己管身份，不能叠 AddGuide。
		refImgs = stringList(node.Data, "ref_images")
		refAuds = stringList(node.Data, "ref_audio")
		if len(refImgs) > 0 || len(refAuds) > 0 {
			kind = "h3_r2v"
			firstFrame = ""
		}
		if e.deps != nil {
			if err := e.deps.Start("comfy_h3"); err != nil {
				return nil, fmt.Errorf("H3 实例启动失败：%w", err)
			}
		}
	}
	_ = kindIsH3
	if kind == "t2v" {
		if err := e.ensureComfy(""); err != nil {
			return nil, err
		}
	}
	req := MediaRequest{Kind: kind, Prompt: prompt, Image: firstFrame}
	req.RefImages = refImgs
	req.RefAudios = refAuds
	if v, ok := node.Data["width"].(float64); ok && v > 0 {
		req.Width = int(v)
	}
	if v, ok := node.Data["height"].(float64); ok && v > 0 {
		req.Height = int(v)
	}
	if v, ok := node.Data["frames"].(float64); ok && v > 0 {
		req.Frames = int(v)
	}
	if v, ok := node.Data["fps"].(float64); ok && v > 0 {
		req.FPS = int(v)
	}
	if v, ok := node.Data["interp"].(bool); ok && v {
		req.Interp = true
	}
	if v, ok := node.Data["low_vram"].(bool); ok && v {
		req.LowVRAM = true
	}
	if v, ok := node.Data["turbo"].(bool); ok && v {
		req.Turbo = true
	}
	if v, ok := node.Data["pdd"].(bool); ok && v {
		req.PDD = true
	}
	if v, ok := node.Data["steps"].(float64); ok && v > 0 {
		req.Steps = int(v)
	}
	if v, ok := node.Data["negative"].(string); ok {
		req.Negative = v
	}

	// 分段拼接：H3 单次 124 帧（5.17s）是硬上限。
	// 上一段末帧作为下一段首帧；每段用各自拆解出的衔接提示词（beats），
	// 全部失败时退化为同题续接。拼接时统一重编码保音画同步。
	if (kind == "h3" || kind == "h3_i2v") && segs > 1 {
		parts := make([]string, 0, segs)
		guide := req.Image
		var segNote string
		segDegrade := 0
		for i := 0; i < segs; i++ {
			req.Image = guide
			if guide == "" {
				req.Kind = "h3"
			} else {
				req.Kind = "h3_i2v"
			}
			// 段专属提示词：优先用拆解出的 beat，保证剧情推进而不是同题复读
			if i < len(beats) {
				req.Prompt = beats[i]
			} else {
				req.Prompt = prompt
			}
			part, _, perr := e.t2i.GenerateMedia(req)
			// P3 显存降级：只在「一段都还没出来」时允许降档。
			// 已经出了段再降，新段分辨率/帧数与旧段不一致，硬拼会跳变——
			// 那时如实报错，让用户降参数重跑，比交一条半截变画质的片强。
			// 降的是共享的 req，所以后续所有段都用同一档参数。
			if perr != nil && IsOOMError(perr) && len(parts) == 0 && segDegrade < maxOOMDegrade {
				e.releaseDiffusionAndWait("分段 OOM 降级") // CUDA 异步释放，不等就白降
				segLadder := e.t2i.Bundles.Ladder(roleForKind(req.Kind))
				if next, changed := degradeMediaRequest(req, segLadder); changed {
					segNote = degradeNote(req, next)
					req = next
					segDegrade++
					i--
					continue
				}
			}
			if perr != nil {
				return nil, fmt.Errorf("第 %d/%d 段失败：%w", i+1, segs, perr)
			}
			parts = append(parts, part)
			if i == segs-1 {
				break
			}
			tmp := filepath.Join(os.TempDir(), fmt.Sprintf("flowedge-seg-%d-%d.png", time.Now().UnixNano(), i))
			if err := extractLastFrame(part, tmp); err != nil {
				return nil, fmt.Errorf("第 %d 段取末帧失败：%w", i+1, err)
			}
			guide = tmp // 下一段用这一段最后一帧开场，画面才连得上
		}
		outDir := e.t2i.OutputDir
		if outDir == "" {
			outDir = "data/generated"
		}
		_ = os.MkdirAll(outDir, 0755)
		final := filepath.Join(outDir, fmt.Sprintf("video-%d-h3-joined.mp4", time.Now().UnixNano()))
		if err := concatVideos(parts, final); err != nil {
			return nil, err
		}
		if abs, aerr := filepath.Abs(final); aerr == nil {
			final = abs
		}
		if req.Interp {
			final2 := strings.TrimSuffix(final, ".mp4") + "-48.mp4"
			if err := interpolateVideo(final, final2, 48); err == nil {
				final = final2
			}
		}
		out := map[string]interface{}{
			"video": final, "kind": "video", "text": prompt,
			"segments": segs, "parts": parts, "beats": beats,
			"duration_sec": float64(segs) * float64(req.Frames) / 24.0,
			// 降档留痕：用户有权知道这一版是靠降分辨率/帧数跑出来的
			"degraded": segDegrade > 0, "degrade_level": segDegrade, "degrade_note": segNote,
		}
		if sceneLock != "" {
			out["scene_lock"] = sceneLock
		}
		if len(promptWarnings) > 0 {
			out["prompt_warnings"] = promptWarnings
		}
		// 与 film 同源的装配后音频处理：响度归一 + 声床，失败降级 WARN 不阻断出片。
		audioMaster := true
		if v, ok := node.Data["audio_master"].(bool); ok {
			audioMaster = v
		}
		lufs := fieldFloat(node, "lufs", -16)
		if lufs > 0 {
			lufs = -16
		}
		roomTone := true
		if v, ok := node.Data["room_tone"].(bool); ok {
			roomTone = v
		}
		roomToneFloor := fieldFloat(node, "room_tone_gain", -36)
		if roomToneFloor >= 0 {
			roomToneFloor = -36
		}
		final = videoAudioFinish(final, audioMaster, lufs, roomTone, roomToneFloor, out)
		out["video"] = muxPreparedAudio(node, final, out)
		return out, nil
	}

	// P3 显存降级：OOM 时先还显存再降档重试（最多两档），
	// 比「直接失败、用户改参数、重新加载 34GB 权重」便宜一个数量级
	path, kindRet, degLevel, degNote, err := e.generateMediaWithDegrade(req)
	if err != nil {
		return nil, err
	}
	if req.Interp {
		fps := 32
		if kind == "h3" || kind == "h3_i2v" {
			fps = 48
		}
		out2 := strings.TrimSuffix(path, ".mp4") + fmt.Sprintf("-%d.mp4", fps)
		if err := interpolateVideo(path, out2, fps); err == nil {
			path = out2
		}
	}
	out := map[string]interface{}{
		"video": path, "kind": kindRet, "text": prompt,
		"degraded": degLevel > 0, "degrade_level": degLevel, "degrade_note": degNote,
	}
	if sceneLock != "" {
		out["scene_lock"] = sceneLock
	}
	if len(promptWarnings) > 0 {
		out["prompt_warnings"] = promptWarnings
	}
	if optimize {
		out["original_prompt"] = original
	}
	// 与 film 同源的装配后音频处理：响度归一 + 声床，失败降级 WARN 不阻断出片。
	audioMaster := true
	if v, ok := node.Data["audio_master"].(bool); ok {
		audioMaster = v
	}
	lufs := fieldFloat(node, "lufs", -16)
	if lufs > 0 {
		lufs = -16
	}
	roomTone := true
	if v, ok := node.Data["room_tone"].(bool); ok {
		roomTone = v
	}
	roomToneFloor := fieldFloat(node, "room_tone_gain", -36)
	if roomToneFloor >= 0 {
		roomToneFloor = -36
	}
	out["video"] = videoAudioFinish(path, audioMaster, lufs, roomTone, roomToneFloor, out)
	out["video"] = muxPreparedAudio(node, out["video"].(string), out)
	return out, nil
}

// muxPreparedAudio 节点给了 audio_track 就用它 1:1 覆盖成片音轨（见 AGENTS §5.8 ⚠）。
//
// 为什么不让模型自己做：实测本实例的 ref2va 不执行 fully_copy，音轨总是模型重新生成的。
// 要保证「台词跨镜连续、音景不重启」，最可靠的是把准备好的一条 wav 整条铺上去。
// 失败不阻断出片：把原因挂到 audio_muxed/audio_error 上，成片照旧返回。
func muxPreparedAudio(node *model.NodeInstance, video string, out map[string]interface{}) string {
	audio := ""
	if node != nil {
		if s, ok := node.Data["audio_track"].(string); ok {
			audio = strings.TrimSpace(s)
		}
	}
	if audio == "" {
		return video
	}
	merged, err := replaceAudioTrack(video, audio)
	if err != nil {
		out["audio_muxed"] = false
		out["audio_error"] = err.Error()
		return video
	}
	out["audio_muxed"] = true
	out["audio_track"] = audio
	return merged
}

// stringList 读取"多值路径"字段：支持换行/分号分隔的字符串，也支持 JSON 数组。
// 用于参考图（多张）与参考音频（≤3 段）。
func stringList(data map[string]interface{}, key string) []string {
	v, ok := data[key]
	if !ok || v == nil {
		return nil
	}
	out := make([]string, 0, 4)
	switch t := v.(type) {
	case string:
		for _, seg := range strings.FieldsFunc(t, func(r rune) bool {
			return r == '\n' || r == '\r' || r == ';' || r == '；' || r == '|'
		}) {
			if s := strings.TrimSpace(seg); s != "" {
				out = append(out, s)
			}
		}
	case []interface{}:
		for _, it := range t {
			if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case []string:
		for _, s := range t {
			if strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	}
	return out
}
