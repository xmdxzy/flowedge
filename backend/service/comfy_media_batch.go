package service

// H3 多镜批量构图（P2 结构优化）：把 N 个非台词镜合并成**一张图**提交。
//
// 为什么要合并：逐镜提交时 ComfyUI 按"每镜 TE→DiT→VAE"循环换权重，而
// 40GB 工作集在 24GB 卡上每次都要从内存/磁盘重搬（实测每镜 ~35GB 盘读）。
// 合并后用两个屏障节点强制两阶段——所有 TE 编码完成才开始任何采样、
// 所有采样完成才开始解码——换权重次数从 3N 降到 3。
// 直连实例 A/B 实测（608×352×124，12 步，3 镜）：逐镜 654–752s vs 批量 306–381s，
// 峰值显存持平（~24GB）。脚本 scripts/h3-batch-bench.py，屏障节点在
// D:/AI/ComfyUI-H3/custom_nodes/flowedge_batch.py（实例重启后自动加载）。
//
// 刻意不做的：
//  - 不解释失败。批量图一镜构图错误整批失败，由调用方（shotlist）决定回退逐镜；
//    这里只把"哪一镜缺产物"说清楚。
//  - 不混 kind。fl2va 与 ref2va 是两个 checkpoint，一组内必须同 kind；
//    续接镜（要上一镜末帧）不收。
//
// 台词镜（2026-10-30 起可同批，但默认关，由 `batch_dialogue` 字段打开）：
// 每镜自带一条念稿音轨，各自一个 LoadAudio(150+i) → 自己的
// ReferenceToVideo.ref_audios，模型侧仍是「一镜一音轨」，只是换权重次数
// 仍是 3 次而不是 3N 次。音轨必须在提交前**全部**合成好（调用方做 TTS 前置），
// 图本身不产音频。每镜只放 1 段参考音频：多镜片每镜只有一条念稿轨，
// 多段只增图复杂度不增收益（单镜路径仍支持 ≤3 段）。
// 为什么默认关：同分镜实测批量 221/229/292s vs 逐镜热态 187s、逐镜冷态 244s+（第 2 镜
// 换页卡死）。批量只在「权重必须反复重载」的冷路径占优，热态略慢，且换页方差远大于
// 两者差 —— 默认按逐镜走，冷启动/长片再显式打开。

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// BatchSaveNodeBase 批量图里第 i 镜的 SaveVideo 节点 id 基数。
// 产物→镜头的映射全靠它（collectOutputsAt 会带出 NodeID），不许改。
const BatchSaveNodeBase = 140

// h3BatchMax 最大一批 8 镜：屏障节点只开了 8 个槽，且单批失败重试的代价随批长线性涨。
const h3BatchMax = 8

// BatchAudioNodeBase 台词镜念稿音轨的 LoadAudio 节点 id 基数（150+i）。
// 必须避开 SaveVideo 段（140+i，产物→镜头的映射靠它）与单镜段（50–137）。
const BatchAudioNodeBase = 150

// BuildH3BatchGraph 把 N 个同构 H3 请求合并为一张两阶段图。
// 返回图与按镜序排列的 SaveVideo 节点 id。
func (s *Text2ImageService) BuildH3BatchGraph(reqs []MediaRequest) (map[string]interface{}, []string, error) {
	n := len(reqs)
	if n < 2 || n > h3BatchMax {
		return nil, nil, fmt.Errorf("批量构图需要 2–%d 镜，当前 %d", h3BatchMax, n)
	}
	kind := reqs[0].Kind
	if kind != "h3" && kind != "h3_r2v" {
		return nil, nil, fmt.Errorf("批量构图只支持 h3 / h3_r2v，当前 %s", kind)
	}
	width, height := reqs[0].Width, reqs[0].Height
	steps, turbo, lowVRAM, pdd := reqs[0].Steps, reqs[0].Turbo, reqs[0].LowVRAM, reqs[0].PDD
	for i, r := range reqs {
		if r.Kind != kind {
			return nil, nil, fmt.Errorf("第 %d 镜 kind=%s 与第 1 镜 %s 不同，不能同批", i+1, r.Kind, kind)
		}
		if r.Width != width || r.Height != height {
			return nil, nil, fmt.Errorf("第 %d 镜分辨率 %dx%d 与第 1 镜 %dx%d 不同，不能同批", i+1, r.Width, r.Height, width, height)
		}
		if r.Image != "" {
			return nil, nil, fmt.Errorf("第 %d 镜是续接镜（需要首帧图），不能批量", i+1)
		}
		// 台词镜可同批，但每镜只放 1 段参考音频（多镜片每镜只有一条念稿轨）。
		if len(r.RefAudios) > 1 {
			return nil, nil, fmt.Errorf("第 %d 镜带 %d 段参考音频，批量只支持每镜 1 段", i+1, len(r.RefAudios))
		}
		if len(r.RefAudios) == 1 && kind != "h3_r2v" {
			return nil, nil, fmt.Errorf("第 %d 镜带参考音频但 kind=%s（只有 ref2va 吃音频）", i+1, kind)
		}
		if r.Steps != steps || r.Turbo != turbo || r.LowVRAM != lowVRAM || r.PDD != pdd {
			return nil, nil, fmt.Errorf("第 %d 镜采样参数与第 1 镜不一致，不能同批", i+1)
		}
		if r.Frames <= 0 || (r.Frames-5)%17 != 0 {
			return nil, nil, fmt.Errorf("第 %d 镜帧数 %d 不满足 17k+5", i+1, r.Frames)
		}
		for j := 0; j < i; j++ {
			if len(r.RefImages) != len(reqs[j].RefImages) {
				return nil, nil, fmt.Errorf("第 %d 镜参考图数量与第 %d 镜不同，不能同批", i+1, j+1)
			}
			for k := range r.RefImages {
				if r.RefImages[k] != reqs[j].RefImages[k] {
					return nil, nil, fmt.Errorf("第 %d 镜参考图与第 %d 镜不同，不能同批（共享 LoadImage 才有批量收益）", i+1, j+1)
				}
			}
		}
	}

	h3Role := RoleVideoH3Fl2va
	if kind == "h3_r2v" {
		h3Role = RoleVideoH3Ref2va
	}
	if steps <= 0 {
		steps = s.bundleParamInt(h3Role, "steps", 20)
	}

	graph := map[string]interface{}{
		"1": map[string]interface{}{"class_type": "UNETLoader", "inputs": map[string]interface{}{
			"unet_name": s.bundleFile(h3Role, "unet"), "weight_dtype": "default"}},
		"2": map[string]interface{}{"class_type": "CLIPLoader", "inputs": map[string]interface{}{
			"clip_name": s.bundleFile(h3Role, "clip"), "type": "minimax", "device": "default"}},
		"3": map[string]interface{}{"class_type": "VAELoader", "inputs": map[string]interface{}{
			"vae_name": s.bundleFile(h3Role, "video_vae")}},
		"4": map[string]interface{}{"class_type": "VAELoader", "inputs": map[string]interface{}{
			"vae_name": s.bundleFile(h3Role, "audio_vae")}},
	}

	// LoRA / 调度 / 低显存链：与单镜图同构（PDD 与 turbo 互斥；低显存必须接在
	// LoRA 与 sigma shift 之后，否则等于悄悄关掉它们——见单镜图里的教训）。
	modelRef := link("1", 0)
	pddOn := false
	if pdd {
		if turbo {
			return nil, nil, fmt.Errorf("H3 PDD 与 turbo 不能同时开")
		}
		lora := H3PDDLoRAFile(kind)
		if lora == "" {
			return nil, nil, fmt.Errorf("H3 PDD 8 步 LoRA 缺失（kind=%s）", kind)
		}
		graph["40"] = map[string]interface{}{"class_type": "LoraLoaderModelOnly", "inputs": map[string]interface{}{
			"model": modelRef, "lora_name": lora, "strength_model": 1.0}}
		modelRef = link("40", 0)
		pddOn = true
		steps = 8
	}
	turboOn := false
	if turbo && !pdd {
		if lora := H3TurboLoRAFile(); lora != "" {
			graph["40"] = map[string]interface{}{"class_type": "LoraLoaderModelOnly", "inputs": map[string]interface{}{
				"model": modelRef, "lora_name": lora, "strength_model": 1.0}}
			modelRef = link("40", 0)
			turboOn = true
			if steps < 8 {
				steps = 8 // 8 步以下是音频崩坏区间
			}
		}
	}
	shiftAudio := h3ShiftAudioBase
	if turboOn || pddOn {
		shiftAudio = h3ShiftAudioTurbo
	}
	graph["41"] = map[string]interface{}{"class_type": "MiniMaxH3SigmaShift", "inputs": map[string]interface{}{
		"model": modelRef, "shift_video": h3ShiftVideo, "shift_audio": shiftAudio}}
	modelRef = link("41", 0)
	if lowVRAM {
		graph["30"] = map[string]interface{}{"class_type": "MiniMaxLowVRAMAttention", "inputs": map[string]interface{}{
			"model": modelRef, "head_chunks": 4}}
		graph["31"] = map[string]interface{}{"class_type": "MiniMaxChunkFeedForward", "inputs": map[string]interface{}{
			"model": link("30", 0), "chunks": 2, "seq_threshold": 4096}}
		modelRef = link("31", 0)
	}

	// r2v 的参考图共享：所有镜的 ReferenceToVideo 接同一组 LoadImage（45+j），
	// 上传一次即可。参考图列表已在校验阶段确认逐张一致。
	if kind == "h3_r2v" {
		for j, p := range reqs[0].RefImages {
			if p == "" {
				continue
			}
			name, err := s.UploadImageAt(s.baseForKind(kind), p)
			if err != nil {
				return nil, nil, fmt.Errorf("参考图上传失败：%w", err)
			}
			graph[fmt.Sprintf("%d", 45+j)] = map[string]interface{}{"class_type": "LoadImage",
				"inputs": map[string]interface{}{"image": name}}
		}
	}
	batchRefLinks := func() map[string]interface{} {
		refs := map[string]interface{}{}
		for j := range reqs[0].RefImages {
			if reqs[0].RefImages[j] == "" {
				continue
			}
			refs[fmt.Sprintf("ref_image_%d", j+1)] = link(fmt.Sprintf("%d", 45+j), 0)
		}
		return refs
	}

	seedBase := time.Now().UnixNano() % (1 << 31)
	condIn := map[string]interface{}{}
	latIn := map[string]interface{}{}
	saveIDs := make([]string, 0, n)
	for i, r := range reqs {
		e, c, noise, ksel, sched, samp := 50+i, 70+i, 60+i, 80+i, 85+i, 100+i
		vdec, adec, cvid, save := 110+i, 120+i, 130+i, BatchSaveNodeBase+i
		saveIDs = append(saveIDs, strconv.Itoa(save))
		condIn[fmt.Sprintf("cond_%d", i+1)] = link(strconv.Itoa(e), 0)
		latIn[fmt.Sprintf("lat_%d", i+1)] = link(strconv.Itoa(samp), 0)

		// 台词镜的念稿音轨：每镜一个 LoadAudio（150+i），只接自己的 ref_audios。
		// 上传走 /upload/image（与单镜路径同一条：LoadAudio 也从 input 目录读）。
		audRefs := map[string]interface{}{}
		if len(r.RefAudios) > 0 {
			name, aerr := s.UploadImageAt(s.baseForKind(kind), r.RefAudios[0])
			if aerr != nil {
				return nil, nil, fmt.Errorf("第 %d 镜参考音频上传失败：%w", i+1, aerr)
			}
			aid := strconv.Itoa(BatchAudioNodeBase + i)
			graph[aid] = map[string]interface{}{"class_type": "LoadAudio",
				"inputs": map[string]interface{}{"audio": name}}
			audRefs["ref_audio_1"] = link(aid, 0)
		}
		prompt := r.Prompt
		if kind == "h3_r2v" {
			// 提示词里的 <Picture k>/<Audio k> 引用必须与实际接线条数一致，
			// 否则模型会去找不存在的参考（六段式改写依赖这个数）
			prompt = ensureRef2VAPrompt(prompt, len(r.RefImages), len(audRefs))
		}
		if kind == "h3_r2v" {
			graph[strconv.Itoa(e)] = map[string]interface{}{"class_type": "MiniMaxH3ReferenceToVideo",
				"inputs": map[string]interface{}{
					"clip": link("2", 0), "vae": link("3", 0), "audio_vae": link("4", 0),
					"prompt": prompt, "width": width, "height": height, "length": r.Frames,
					"ref_image_size": "match",
					"ref_images":     batchRefLinks(),
					"ref_audios":     audRefs,
				}}
		} else {
			graph[strconv.Itoa(e)] = map[string]interface{}{"class_type": "MiniMaxH3ImageToVideo", "inputs": map[string]interface{}{
				"clip": link("2", 0), "vae": link("3", 0), "prompt": prompt,
				"width": width, "height": height, "length": r.Frames,
			}}
		}
		seed := r.Seed
		if seed <= 0 {
			seed = int64(seedBase) + int64(i) // 同一拍内第 i 镜的确定性偏移
		}
		graph[strconv.Itoa(noise)] = map[string]interface{}{"class_type": "RandomNoise", "inputs": map[string]interface{}{"noise_seed": seed}}
		graph[strconv.Itoa(ksel)] = map[string]interface{}{"class_type": "KSamplerSelect", "inputs": map[string]interface{}{"sampler_name": "res_multistep"}}
		graph[strconv.Itoa(sched)] = map[string]interface{}{"class_type": "BasicScheduler", "inputs": map[string]interface{}{
			"model": modelRef, "scheduler": "simple", "steps": steps, "denoise": 1.0}}
		graph[strconv.Itoa(c)] = map[string]interface{}{"class_type": "BasicGuider", "inputs": map[string]interface{}{
			"model": modelRef, "conditioning": link("90", i)}}
		graph[strconv.Itoa(samp)] = map[string]interface{}{"class_type": "SamplerCustomAdvanced", "inputs": map[string]interface{}{
			"noise": link(strconv.Itoa(noise), 0), "guider": link(strconv.Itoa(c), 0), "sampler": link(strconv.Itoa(ksel), 0),
			"sigmas": link(strconv.Itoa(sched), 0), "latent_image": link(strconv.Itoa(e), 1)}}
		graph[strconv.Itoa(vdec)] = map[string]interface{}{"class_type": "VAEDecode", "inputs": map[string]interface{}{
			"samples": link("91", i), "vae": link("3", 0)}}
		graph[strconv.Itoa(adec)] = map[string]interface{}{"class_type": "VAEDecodeAudio", "inputs": map[string]interface{}{
			"samples": link("91", i), "vae": link("4", 0)}}
		graph[strconv.Itoa(cvid)] = map[string]interface{}{"class_type": "CreateVideo", "inputs": map[string]interface{}{
			"images": link(strconv.Itoa(vdec), 0), "audio": link(strconv.Itoa(adec), 0), "fps": 24.0, "bit_depth": 8}}
		graph[strconv.Itoa(save)] = map[string]interface{}{"class_type": "SaveVideo", "inputs": map[string]interface{}{
			"video": link(strconv.Itoa(cvid), 0), "filename_prefix": fmt.Sprintf("flowedge-h3-b%d", i), "format": "mp4", "codec": "auto"}}
	}
	// 屏障：所有采样器等所有 TE 编码；所有解码等所有采样。
	graph["90"] = map[string]interface{}{"class_type": "FlowEdgeCondBarrier", "inputs": condIn}
	graph["91"] = map[string]interface{}{"class_type": "FlowEdgeLatentBarrier", "inputs": latIn}
	return graph, saveIDs, nil
}

// sumFrames 批量日志用的合计帧数。
func sumFrames(reqs []MediaRequest) int {
	total := 0
	for _, r := range reqs {
		total += r.Frames
	}
	return total
}

// GenerateH3Batch 提交批量图并把产物按镜序排列返回。
// 超时 = 45 分钟（权重加载基础盘，与单镜一致）+ 各镜生成估值的 2 倍。
// onPhase 收到整批的 queued/running（一镜进度粒度到不了批量图，由调用方广播给组内各镜）。
func (s *Text2ImageService) GenerateH3Batch(ctx context.Context, reqs []MediaRequest, onPhase func(string)) ([]MediaOutput, error) {
	graph, saveIDs, err := s.BuildH3BatchGraph(reqs)
	if err != nil {
		return nil, err
	}
	estimate := 0.0
	for _, r := range reqs {
		estimate += shotCostSeconds(r.Frames, r.Width, r.Height, r.Steps)
	}
	timeout := 45*time.Minute + time.Duration(estimate*2*float64(time.Second))
	base := s.baseForKind(reqs[0].Kind)
	outs, err := s.RunGraphAtCtx(ctx, base, graph, timeout, onPhase)
	if err != nil {
		return nil, err
	}
	byNode := make(map[string]MediaOutput, len(outs))
	for _, o := range outs {
		byNode[o.NodeID] = o
	}
	ordered := make([]MediaOutput, 0, len(saveIDs))
	var missing []int
	for i, id := range saveIDs {
		if o, ok := byNode[id]; ok {
			ordered = append(ordered, o)
		} else {
			missing = append(missing, i+1)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("批量图完成但缺 %d 镜产物（第 %v 镜的 SaveVideo 没有输出）", len(missing), missing)
	}
	return ordered, nil
}
