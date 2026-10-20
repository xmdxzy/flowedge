package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ComfyUI 通用媒体执行层
//
// 之前只有「文生图」一个硬编码图；图生图 / 重绘 / 超分 / 文生视频都要各自造一遍
// 提交+轮询+下载的代码。这里抽出一层：
//   BuildMediaGraph(kind, req) → 具体节点图
//   RunGraph(graph)            → 提交 / 轮询 / 下载，返回落盘文件
// 新模态只需要多一个 build 分支，不再碰 IO 逻辑。

// MediaRequest 一次媒体生成请求（字段按 kind 取用，空的走默认）
type MediaRequest struct {
	Kind     string // img2img | inpaint | upscale | t2v
	Prompt   string
	Negative string
	Image    string // 本地图片路径（img2img / inpaint / upscale）
	Mask     string // 本地蒙版路径（inpaint，白色为重绘区）
	Width    int
	Height   int
	Steps    int
	Batch    int
	Frames   int     // 视频帧数
	FPS      int     // 视频帧率
	Strength float64 // img2img 去噪强度 / inpaint denoise
	Seed     int64
	Model    string // 模型名覆盖：超分模型（upscale）/ checkpoint（inpaint，如 Flux-Fill）；空 = 服务默认
	LowVRAM  bool   // H3：用分块注意力/分块 FFN 换显存，换更高分辨率
	Segments int    // H3：分几段生成再拼接（每段 124 帧 ≈ 5 秒）
	Interp   bool   // 出片后帧率翻倍（ffmpeg minterpolate，CPU，不占显存）
	Turbo    bool   // H3 turbo LoRA：20 步 → 8 步（约 2.5 倍速）

	// h3_r2v（ref2va）：参考图 + 参考音频 → 口型同步的视频。
	//
	// 这是 H3 唯一「用自己的音频念稿」的链路：ref_audios 里的声音会被模型
	// 当作口型基准，配合提示词里的 <Audio 1>: fully_copy 可以 1:1 透传，
	// 不再由模型即兴生成（那正是"伪中文乱语"的来源）。
	RefImages []string // 参考图（≤9），锁定角色外貌；提示词里用 <Picture i> 引用
	RefAudios []string // 参考音频（≤3），念稿/音色基准；提示词里用 <Audio j> 引用
}

// MediaOutput 一个产出文件
type MediaOutput struct {
	Kind string // image | video | audio
	Path string
}

// UploadImage 把本地图片上传到 ComfyUI 的 input 目录，返回可被 LoadImage 引用的文件名
func (s *Text2ImageService) UploadImage(localPath string) (string, error) {
	return s.UploadImageAt(s.BaseURL, localPath)
}

// UploadImageAt 把图片传到指定实例。
// H3 跑在独立端口上，用默认 BaseURL 上传会直接连不上（图生视频首帧报 dial refused）。
func (s *Text2ImageService) UploadImageAt(base, localPath string) (string, error) {
	if base == "" {
		return "", fmt.Errorf("comfyui not configured")
	}
	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("图片打不开：%s", localPath)
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("image", filepath.Base(localPath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	_ = w.WriteField("overwrite", "true")
	w.Close()

	req, err := http.NewRequest(http.MethodPost, base+"/upload/image", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("上传图片失败：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("comfyui 拒绝上传（%d）：%.200s", resp.StatusCode, string(body))
	}
	var out struct {
		Name      string `json:"name"`
		Subfolder string `json:"subfolder"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Name == "" {
		return "", fmt.Errorf("上传返回无法解析：%.200s", string(body))
	}
	if out.Subfolder != "" {
		return out.Subfolder + "/" + out.Name, nil
	}
	return out.Name, nil
}

// RunGraph 提交节点图并等待产物落盘（默认实例 8188）
func (s *Text2ImageService) RunGraph(graph map[string]interface{}, timeout time.Duration) ([]MediaOutput, error) {
	return s.RunGraphAt(s.BaseURL, graph, timeout)
}

// RunGraphAt 指定实例提交。
//
// 为什么要有：H3 必须跑在自己的工作树与 venv 里（torch 2.14 + 完整量化支持，
// 启动参数也不同），占另一个端口；flux/Wan 继续走默认实例。两个实例靠
// 「谁在跑就用谁」的端口探测区分，不能共用一个 BaseURL。
func (s *Text2ImageService) RunGraphAt(base string, graph map[string]interface{}, timeout time.Duration) ([]MediaOutput, error) {
	if base == "" {
		return nil, fmt.Errorf("comfyui not configured")
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}

	payload, _ := json.Marshal(map[string]interface{}{"prompt": graph})
	resp, err := s.client.Post(base+"/prompt", "application/json", bytes.NewBuffer(payload))
	if err != nil {
		return nil, fmt.Errorf("comfyui request failed: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("comfyui rejected prompt (%d): %.400s", resp.StatusCode, string(body))
	}
	var queued struct {
		PromptID string `json:"prompt_id"`
	}
	if err := json.Unmarshal(body, &queued); err != nil || queued.PromptID == "" {
		return nil, fmt.Errorf("comfyui returned no prompt_id: %.300s", string(body))
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(1500 * time.Millisecond)

		hresp, err := s.client.Get(base + "/history/" + queued.PromptID)
		if err != nil {
			continue
		}
		hbody, _ := io.ReadAll(io.LimitReader(hresp.Body, 4<<20))
		hresp.Body.Close()
		if hresp.StatusCode != http.StatusOK {
			continue
		}
		var hist map[string]comfyHistoryEntry
		if json.Unmarshal(hbody, &hist) != nil {
			continue
		}
		entry, ok := hist[queued.PromptID]
		if !ok {
			continue
		}
		if entry.Status.StatusStr == "error" {
			return nil, fmt.Errorf("comfyui 执行失败（prompt_id=%s），首个报错节点见 ComfyUI 日志", queued.PromptID)
		}

		outs := s.collectOutputsAt(base, entry)
		if len(outs) == 0 {
			continue // 还没产出
		}
		return outs, nil
	}
	return nil, fmt.Errorf("生成超时（%s）", timeout)
}

// collectOutputs 从 history 条目里挑出可下载的产物并落盘
func (s *Text2ImageService) collectOutputs(entry comfyHistoryEntry) []MediaOutput {
	return s.collectOutputsAt(s.BaseURL, entry)
}

// collectOutputsAt 从指定实例下载产物
func (s *Text2ImageService) collectOutputsAt(base string, entry comfyHistoryEntry) []MediaOutput {
	dir := s.OutputDir
	if dir == "" {
		dir = "data/generated"
	}
	_ = os.MkdirAll(dir, 0755)

	var outs []MediaOutput
	for _, o := range entry.Outputs {
		// SaveVideo / SaveAnimatedWEBP 的产物分别落在 gifs / videos 下，
		// 只看 images 会拿到空列表（视频「生成成功但没有文件」就是这个坑）。
		all := append(append(append([]comfyFileRef{}, o.Images...), o.Gifs...), o.Videos...)
		for _, im := range all {
			kind := "image"
			lower := strings.ToLower(im.Filename)
			if strings.HasSuffix(lower, ".mp4") || strings.HasSuffix(lower, ".webm") || strings.HasSuffix(lower, ".gif") {
				kind = "video"
			}
			q := url.Values{}
			q.Set("filename", im.Filename)
			q.Set("subfolder", im.Subfolder)
			q.Set("type", im.Type)
			dresp, err := s.client.Get(base + "/view?" + q.Encode())
			if err != nil {
				continue
			}
			data, _ := io.ReadAll(dresp.Body)
			dresp.Body.Close()
			if len(data) < 1024 {
				continue
			}
			dest := filepath.Join(dir, fmt.Sprintf("%s-%d-%s", kind, time.Now().UnixNano(), im.Filename))
			if err := os.WriteFile(dest, data, 0644); err != nil {
				continue
			}
			if abs, e := filepath.Abs(dest); e == nil {
				dest = abs
			}
			outs = append(outs, MediaOutput{Kind: kind, Path: dest})
		}
	}
	return outs
}

// ===== 各类媒体图 =====

func link(id string, slot int) []interface{} { return []interface{}{id, slot} }

// BuildMediaGraph 按 kind 组装 ComfyUI API 图
//
// 约定：kind 只决定「用什么图」，参数默认值都在这里收口，
// 节点层不需要知道 ComfyUI 的节点名与连线。
func (s *Text2ImageService) BuildMediaGraph(req MediaRequest) (map[string]interface{}, error) {
	seed := req.Seed
	if seed == 0 {
		seed = time.Now().UnixNano() % (1 << 31)
	}

	// 兼容矩阵预检：已知不兼容的 checkpoint 在构图前显式失败（VAE 架构误判类），
	// 不让用户等到 ComfyUI 执行期才看到一句不指向根因的 size mismatch
	if err := CheckCheckpointCompat(s.Checkpoint); err != nil {
		return nil, err
	}

	switch req.Kind {
	case "img2img":
		name, err := s.UploadImage(req.Image)
		if err != nil {
			return nil, err
		}
		strength := req.Strength
		if strength <= 0 {
			strength = 0.6
		}
		steps := req.Steps
		if steps <= 0 {
			steps = 8
		}
		return map[string]interface{}{
			"4":  ckptNode(s.Checkpoint),
			"10": map[string]interface{}{"class_type": "LoadImage", "inputs": map[string]interface{}{"image": name}},
			"11": map[string]interface{}{"class_type": "VAEEncode", "inputs": map[string]interface{}{"pixels": link("10", 0), "vae": link("4", 2)}},
			"6":  clipEncode(req.Prompt, "4"),
			"33": clipEncode(req.Negative, "4"),
			"3": map[string]interface{}{"class_type": "KSampler", "inputs": map[string]interface{}{
				"seed": seed, "steps": steps, "cfg": 1.0, "sampler_name": "euler", "scheduler": "simple",
				"denoise": strength, "model": link("4", 0), "positive": link("6", 0),
				"negative": link("33", 0), "latent_image": link("11", 0),
			}},
			"8": map[string]interface{}{"class_type": "VAEDecode", "inputs": map[string]interface{}{"samples": link("3", 0), "vae": link("4", 2)}},
			"9": saveImage("flowedge-img2img"),
		}, nil

	case "inpaint":
		name, err := s.UploadImage(req.Image)
		if err != nil {
			return nil, err
		}
		maskName, err := s.UploadImage(req.Mask)
		if err != nil {
			return nil, err
		}
		denoise := req.Strength
		if denoise <= 0 {
			denoise = 1.0
		}
		steps := req.Steps
		if steps <= 0 {
			steps = 10
		}
		// 蒙版按原图尺寸缩放，避免尺寸不一致导致 latent 对不上
		// checkpoint 可被节点覆盖：填 Flux-Fill 得专业重绘（~11GB，先落位
		// D:/AI/Models 再用），留空走服务默认（flux-schnell，弱修补）
		return map[string]interface{}{
			"4":  ckptNode(firstNonEmpty(req.Model, s.Checkpoint)),
			"10": map[string]interface{}{"class_type": "LoadImage", "inputs": map[string]interface{}{"image": name}},
			"20": map[string]interface{}{"class_type": "LoadImage", "inputs": map[string]interface{}{"image": maskName}},
			"11": map[string]interface{}{"class_type": "VAEEncode", "inputs": map[string]interface{}{"pixels": link("10", 0), "vae": link("4", 2)}},
			// mask 必须接 LoadImage 的 MASK 槽（1）：IMAGE 槽（0）会直接报 return_type_mismatch。
			// 尺寸一致性由 Go 侧先校验（ResizeImageMaskNode 是动态 schema，接线脆弱，不值得为一层缩放去赌）。
			"12": map[string]interface{}{"class_type": "SetLatentNoiseMask", "inputs": map[string]interface{}{"samples": link("11", 0), "mask": link("20", 1)}},
			"6":  clipEncode(req.Prompt, "4"),
			"33": clipEncode(req.Negative, "4"),
			"3": map[string]interface{}{"class_type": "KSampler", "inputs": map[string]interface{}{
				"seed": seed, "steps": steps, "cfg": 1.0, "sampler_name": "euler", "scheduler": "simple",
				"denoise": denoise, "model": link("4", 0), "positive": link("6", 0),
				"negative": link("33", 0), "latent_image": link("12", 0),
			}},
			"8": map[string]interface{}{"class_type": "VAEDecode", "inputs": map[string]interface{}{"samples": link("3", 0), "vae": link("4", 2)}},
			"9": saveImage("flowedge-inpaint"),
		}, nil

	case "upscale":
		name, err := s.UploadImage(req.Image)
		if err != nil {
			return nil, err
		}
		model := req.Model
		if model == "" {
			model = "RealESRGAN_x4plus.pth"
		}
		return map[string]interface{}{
			"1":  map[string]interface{}{"class_type": "UpscaleModelLoader", "inputs": map[string]interface{}{"model_name": model}},
			"10": map[string]interface{}{"class_type": "LoadImage", "inputs": map[string]interface{}{"image": name}},
			"2":  map[string]interface{}{"class_type": "ImageUpscaleWithModel", "inputs": map[string]interface{}{"upscale_model": link("1", 0), "image": link("10", 0)}},
			"9":  saveImageFrom("2", "flowedge-upscale"),
		}, nil

	case "t2v":
		// Wan2.1-T2V-1.3B：官方推荐的轻量视频底座，24GB 可跑
		width, height, frames, fps, steps := req.Width, req.Height, req.Frames, req.FPS, req.Steps
		if width <= 0 {
			width = 832
		}
		if height <= 0 {
			height = 480
		}
		if frames <= 0 {
			frames = 33
		}
		if fps <= 0 {
			fps = 16
		}
		if steps <= 0 {
			steps = 20
		}
		return map[string]interface{}{
			"1": map[string]interface{}{"class_type": "UNETLoader", "inputs": map[string]interface{}{
				"unet_name": "wan2.1_t2v_1.3B_fp16.safetensors", "weight_dtype": "default",
			}},
			"2": map[string]interface{}{"class_type": "ModelSamplingSD3", "inputs": map[string]interface{}{"model": link("1", 0), "shift": 8.0}},
			"3": map[string]interface{}{"class_type": "CLIPLoader", "inputs": map[string]interface{}{
				"clip_name": "umt5_xxl_fp8_e4m3fn_scaled.safetensors", "type": "wan",
			}},
			"4": map[string]interface{}{"class_type": "CLIPTextEncode", "inputs": map[string]interface{}{"text": req.Prompt, "clip": link("3", 0)}},
			"5": map[string]interface{}{"class_type": "CLIPTextEncode", "inputs": map[string]interface{}{"text": req.Negative, "clip": link("3", 0)}},
			"6": map[string]interface{}{"class_type": "EmptyHunyuanLatentVideo", "inputs": map[string]interface{}{
				"width": width, "height": height, "length": frames, "batch_size": 1,
			}},
			"7": map[string]interface{}{"class_type": "KSampler", "inputs": map[string]interface{}{
				"seed": seed, "steps": steps, "cfg": 6.0, "sampler_name": "euler", "scheduler": "simple",
				"denoise": 1.0, "model": link("2", 0), "positive": link("4", 0),
				"negative": link("5", 0), "latent_image": link("6", 0),
			}},
			"8":  map[string]interface{}{"class_type": "VAELoader", "inputs": map[string]interface{}{"vae_name": "wan_2.1_vae.safetensors"}},
			"9":  map[string]interface{}{"class_type": "VAEDecode", "inputs": map[string]interface{}{"samples": link("7", 0), "vae": link("8", 0)}},
			"10": map[string]interface{}{"class_type": "CreateVideo", "inputs": map[string]interface{}{"images": link("9", 0), "fps": float64(fps)}},
			"11": map[string]interface{}{"class_type": "SaveVideo", "inputs": map[string]interface{}{
				"video": link("10", 0), "filename_prefix": "flowedge-t2v", "format": "mp4", "codec": "auto",
			}},
		}, nil

	case "h3", "h3_i2v", "h3_r2v":
		// MiniMax-H3 官方链路：**一次推理同时出画面与立体声**。
		// 参数来自 D:/AI/ComfyUI-H3/MINIMAX_H3_LOCAL.md 的实测结论：
		//   608x352 / 124 帧(=5.17s@24fps) / 20 步 / res_multistep / simple
		//   172 秒出片，峰值显存 21.4GB（--reserve-vram 4）
		// 帧数必须落在 17k+5 网格上（124=17*7+5），否则采样器直接报错。
		width, height, frames, steps := req.Width, req.Height, req.Frames, req.Steps
		if width <= 0 {
			width = 608
		}
		if height <= 0 {
			height = 352
		}
		if frames <= 0 {
			frames = 124
		}
		if steps <= 0 {
			steps = 20
		}
		if (frames-5)%17 != 0 {
			return nil, fmt.Errorf("H3 帧数必须满足 17k+5（如 124 / 141 / 158），当前 %d", frames)
		}
		// 图生视频：把首帧「钉」进潜空间（frame_idx=0），其余帧由模型续写并同时生成声音。
		// 这是 fl2va 架构的本来用法——t2v 只是不给引导。
		guideCond := link("5", 0)
		extra := map[string]interface{}{}
		if req.Kind == "h3_i2v" {
			if req.Image == "" {
				return nil, fmt.Errorf("H3 图生视频缺少首帧图片")
			}
			imgName, err := s.UploadImageAt(s.H3BaseURL, req.Image)
			if err != nil {
				return nil, err
			}
			extra["20"] = map[string]interface{}{"class_type": "LoadImage", "inputs": map[string]interface{}{"image": imgName}}
			extra["21"] = map[string]interface{}{"class_type": "MiniMaxH3AddGuide", "inputs": map[string]interface{}{
				"positive": link("5", 0), "latent": link("5", 1), "frame_idx": 0,
				"vae": link("3", 0), "image": link("20", 0),
			}}
			guideCond = link("21", 0)
		}

		modelRef := link("1", 0)
		graphExtra := map[string]interface{}{}
		// ---------- h3_r2v：参考图 + 参考音频（念稿/口型同步） ----------
		//
		// 关键点（官方 ref2va 指南）：
		//   1. 参考图用 <Picture i> 引用，锁定角色外貌，不必逐镜重复描述
		//   2. 参考音频用 <Audio j> 引用；提示词里标 fully_copy 时，
		//      这段音频会作为成片的**完整最终音轨 1:1 复用**，不会被模型重生成
		//   3. 只有 ref2va 的节点暴露 ref_audios —— 这就是"正常念稿"的唯一入口
		if req.Kind == "h3_r2v" {
			if len(req.RefAudios) > 3 {
				return nil, fmt.Errorf("参考音频最多 3 段，当前 %d", len(req.RefAudios))
			}
			if len(req.RefImages) > 9 {
				return nil, fmt.Errorf("参考图最多 9 张，当前 %d", len(req.RefImages))
			}
			if len(req.RefAudios) == 0 && len(req.RefImages) == 0 {
				return nil, fmt.Errorf("参考生视频至少要给 1 张参考图或 1 段参考音频")
			}
			// 六段式补齐：缺 subject_definitions / retention_analysis 时模型会把
			// 参考音频当成"风格提示"重新生成，念稿就退化成伪中文
			req.Prompt = ensureRef2VAPrompt(req.Prompt, len(req.RefImages), len(req.RefAudios))
			refImgs := map[string]interface{}{}
			for i, p := range req.RefImages {
				if strings.TrimSpace(p) == "" {
					continue
				}
				name, err := s.UploadImageAt(s.H3BaseURL, p)
				if err != nil {
					return nil, fmt.Errorf("参考图上传失败：%w", err)
				}
				id := fmt.Sprintf("%d", 50+i)
				graphExtra[id] = map[string]interface{}{"class_type": "LoadImage",
					"inputs": map[string]interface{}{"image": name}}
				refImgs[fmt.Sprintf("ref_image_%d", i+1)] = link(id, 0)
			}
			refAuds := map[string]interface{}{}
			for i, p := range req.RefAudios {
				if strings.TrimSpace(p) == "" {
					continue
				}
				// LoadAudio 读的是 input 目录；/upload/image 接受任意文件并落到同一目录
				name, err := s.UploadImageAt(s.H3BaseURL, p)
				if err != nil {
					return nil, fmt.Errorf("参考音频上传失败：%w", err)
				}
				id := fmt.Sprintf("%d", 60+i)
				graphExtra[id] = map[string]interface{}{"class_type": "LoadAudio",
					"inputs": map[string]interface{}{"audio": name}}
				refAuds[fmt.Sprintf("ref_audio_%d", i+1)] = link(id, 0)
			}
			// 用 ReferenceToVideo 顶替默认的 ImageToVideo（同样输出 positive + latent）
			graphExtra["5"] = map[string]interface{}{"class_type": "MiniMaxH3ReferenceToVideo",
				"inputs": map[string]interface{}{
					"clip": link("2", 0), "vae": link("3", 0), "audio_vae": link("4", 0),
					"prompt": req.Prompt, "width": width, "height": height, "length": frames,
					"ref_image_size": "match",
					"ref_images":     refImgs, "ref_audios": refAuds,
				}}
			// 参考模式不接首帧引导：Ref2V 自己管身份，叠 AddGuide 反而打架
			guideCond = link("5", 0)
		}

		// 低显存模式：把注意力与 FFN 分块。峰值显存下来，代价是稍慢。
		stepsExplicit := req.Steps > 0

		// 涡轮 LoRA：蒸馏采样步数，是长片最省时间的一招。
		// 没有权重文件时静默跳过（用 base 的 20 步），不让工作流失败。
		//
		// 2026-10-19 修正：turbo 的默认步数从 8 提到 12。
		// 官方 prompt/参数指南写明「音频在联合去噪中比画面晚收敛；台词与音色需要
		// 12 步以上，8 步时音频往往就是短板」——我们之前锁 8 步，正好把音频
		// 推进了欠采样区间，这是"胡言乱语"的直接成因之一。
		turboOn := false
		if req.Turbo {
			if lora := H3TurboLoRAFile(); lora != "" {
				graphExtra["40"] = map[string]interface{}{"class_type": "LoraLoaderModelOnly", "inputs": map[string]interface{}{
					"model": modelRef, "lora_name": lora, "strength_model": 1.0,
				}}
				modelRef = link("40", 0)
				turboOn = true
				if !stepsExplicit {
					steps = h3TurboDefaultSteps
				}
				if steps < 8 {
					// 8 步以下是音频崩坏的区间，宁可慢一点也不让声音报废
					steps = 8
				}
			}
		}
		// 双调度：视频/音频各自的 sigma shift。不接这个节点时，DiT 用模型默认
		// （12 / 3）；turbo 蒸馏卡要求 12 / 6，否则音频轨容易爆音、错位。
		// 官方：Base 12/3；turbo LoRA 模型卡 12/6。
		shiftAudio := h3ShiftAudioBase
		if turboOn {
			shiftAudio = h3ShiftAudioTurbo
		}
		graphExtra["41"] = map[string]interface{}{"class_type": "MiniMaxH3SigmaShift", "inputs": map[string]interface{}{
			"model": modelRef, "shift_video": h3ShiftVideo, "shift_audio": shiftAudio,
		}}
		modelRef = link("41", 0)

		if req.LowVRAM {
			// 注意：这里必须接 modelRef 而不是 link("1",0)。
			// 旧写法会把 LoRA 与 sigma shift 一起丢掉——开了低显存就等于
			// 悄悄关掉 turbo 与音频调度，属于静默功能失效。
			graphExtra["30"] = map[string]interface{}{"class_type": "MiniMaxLowVRAMAttention", "inputs": map[string]interface{}{
				"model": modelRef, "head_chunks": 4,
			}}
			graphExtra["31"] = map[string]interface{}{"class_type": "MiniMaxChunkFeedForward", "inputs": map[string]interface{}{
				"model": link("30", 0), "chunks": 2, "seq_threshold": 4096,
			}}
			modelRef = link("31", 0)
		}
		// r2v 不是"图生视频的升级版"，而是**另一个 checkpoint**：
		// 只有 ref2va 的节点暴露 ref_audios，也只有它做口型同步。
		unetName := "minimax_h3_fl2va_pruned_int8_convrot.safetensors"
		if req.Kind == "h3_r2v" {
			unetName = "minimax_h3_ref2va_pruned_int8_convrot.safetensors"
		}
		graph := map[string]interface{}{
			"1": map[string]interface{}{"class_type": "UNETLoader", "inputs": map[string]interface{}{
				"unet_name": unetName, "weight_dtype": "default",
			}},
			"2": map[string]interface{}{"class_type": "CLIPLoader", "inputs": map[string]interface{}{
				"clip_name": "qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors", "type": "minimax", "device": "default",
			}},
			"3": map[string]interface{}{"class_type": "VAELoader", "inputs": map[string]interface{}{
				"vae_name": "minimax_h3_video_vae_fp16.safetensors",
			}},
			"4": map[string]interface{}{"class_type": "VAELoader", "inputs": map[string]interface{}{
				"vae_name": "minimax_h3_audio_vae_fp32.safetensors",
			}},
			"5": map[string]interface{}{"class_type": "MiniMaxH3ImageToVideo", "inputs": map[string]interface{}{
				"clip": link("2", 0), "vae": link("3", 0), "prompt": req.Prompt,
				"width": width, "height": height, "length": frames,
			}},
			"6": map[string]interface{}{"class_type": "RandomNoise", "inputs": map[string]interface{}{"noise_seed": seed}},
			"7": map[string]interface{}{"class_type": "BasicGuider", "inputs": map[string]interface{}{
				"model": modelRef, "conditioning": guideCond,
			}},
			"8": map[string]interface{}{"class_type": "KSamplerSelect", "inputs": map[string]interface{}{"sampler_name": "res_multistep"}},
			"9": map[string]interface{}{"class_type": "BasicScheduler", "inputs": map[string]interface{}{
				"model": modelRef, "scheduler": "simple", "steps": steps, "denoise": 1.0,
			}},
			"10": map[string]interface{}{"class_type": "SamplerCustomAdvanced", "inputs": map[string]interface{}{
				"noise": link("6", 0), "guider": link("7", 0), "sampler": link("8", 0),
				"sigmas": link("9", 0), "latent_image": link("5", 1),
			}},
			"11": map[string]interface{}{"class_type": "VAEDecode", "inputs": map[string]interface{}{"samples": link("10", 0), "vae": link("3", 0)}},
			"12": map[string]interface{}{"class_type": "VAEDecodeAudio", "inputs": map[string]interface{}{"samples": link("10", 0), "vae": link("4", 0)}},
			"13": map[string]interface{}{"class_type": "CreateVideo", "inputs": map[string]interface{}{
				"images": link("11", 0), "audio": link("12", 0), "fps": 24.0, "bit_depth": 8,
			}},
			"14": map[string]interface{}{"class_type": "SaveVideo", "inputs": map[string]interface{}{
				"video": link("13", 0), "filename_prefix": "flowedge-h3", "format": "mp4", "codec": "auto",
			}},
		}
		for k, v := range extra {
			graph[k] = v
		}
		for k, v := range graphExtra {
			graph[k] = v
		}
		return graph, nil
	}
	return nil, fmt.Errorf("未知的媒体类型：%s", req.Kind)
}

// GenerateMedia 组装图 → 执行 → 返回第一个产物路径
func (s *Text2ImageService) GenerateMedia(req MediaRequest) (string, string, error) {
	graph, err := s.BuildMediaGraph(req)
	if err != nil {
		return "", "", err
	}
	timeout := 10 * time.Minute
	base := s.BaseURL
	switch req.Kind {
	case "t2v":
		timeout = 25 * time.Minute // 视频首次要加载 10GB 权重
	case "h3", "h3_i2v", "h3_r2v":
		base = s.H3BaseURL
		timeout = 45 * time.Minute // 40GB 权重 + 40GB 内存工作集，慢是正常的
	}
	outs, err := s.RunGraphAt(base, graph, timeout)
	if err != nil {
		return "", "", err
	}
	if len(outs) == 0 {
		return "", "", fmt.Errorf("没有产出任何文件")
	}
	return outs[0].Path, outs[0].Kind, nil
}

func ckptNode(name string) map[string]interface{} {
	return map[string]interface{}{
		"class_type": "CheckpointLoaderSimple",
		"inputs":     map[string]interface{}{"ckpt_name": name},
	}
}

func clipEncode(text, ckptID string) map[string]interface{} {
	return map[string]interface{}{
		"class_type": "CLIPTextEncode",
		"inputs":     map[string]interface{}{"text": text, "clip": link(ckptID, 1)},
	}
}

// saveImageFrom 把 SaveImage 接到指定节点的 IMAGE 输出上。
// 别写死 "8"：超分图的解码节点是 "2"，写死会让 ComfyUI 报 prompt_outputs_failed_validation。
func saveImageFrom(srcID, prefix string) map[string]interface{} {
	return map[string]interface{}{
		"class_type": "SaveImage",
		"inputs":     map[string]interface{}{"images": link(srcID, 0), "filename_prefix": prefix},
	}
}

func saveImage(prefix string) map[string]interface{} { return saveImageFrom("8", prefix) }

// GenerateImages 批量出图：一次提交 batch 张，共用一次模型加载。
//
// 为什么单独一条路径：逐张调用会在 --disable-smart-memory 下每张都重载 17GB，
// 10 张就是十分钟的纯加载时间。batch_size 直接交给 ComfyUI，显存里只加载一次。
func (s *Text2ImageService) GenerateImages(prompt string, width, height, batch int) ([]string, error) {
	if batch <= 1 {
		one, err := s.GenerateImage(prompt, width, height)
		if err != nil {
			return nil, err
		}
		return []string{one}, nil
	}
	if width <= 0 {
		width = 1024
	}
	if height <= 0 {
		height = 1024
	}
	graph := map[string]interface{}{
		"4":  ckptNode(s.Checkpoint),
		"6":  clipEncode(prompt, "4"),
		"33": clipEncode("", "4"),
		"5": map[string]interface{}{"class_type": "EmptyLatentImage", "inputs": map[string]interface{}{
			"width": width, "height": height, "batch_size": batch,
		}},
		"3": map[string]interface{}{"class_type": "KSampler", "inputs": map[string]interface{}{
			"seed": time.Now().UnixNano() % (1 << 31), "steps": 4, "cfg": 1.0,
			"sampler_name": "euler", "scheduler": "simple", "denoise": 1.0,
			"model": link("4", 0), "positive": link("6", 0),
			"negative": link("33", 0), "latent_image": link("5", 0),
		}},
		"8": map[string]interface{}{"class_type": "VAEDecode", "inputs": map[string]interface{}{"samples": link("3", 0), "vae": link("4", 2)}},
		"9": saveImage("flowedge-batch"),
	}
	outs, err := s.RunGraph(graph, 15*time.Minute)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(outs))
	for _, o := range outs {
		if o.Kind == "image" {
			paths = append(paths, o.Path)
		}
	}
	return paths, nil
}

// GenerateImagesFromPrompts 多提示词批量：N 个不同 prompt，**一次模型加载**。
//
// 为什么不能直接用 batch_size：ComfyUI 的 batch 共用同一份 conditioning，
// 只适合「同一句话出多张」。分镜/角色表需要不同提示词，如果逐条调用，
// 每条都要重新走一遍 checkpoint 载入（冷 60s+），10 条就是十分钟纯等待。
// 这里把 N 条 prompt 编成一个图、N 个 KSampler 共享同一个已加载的模型。
func (s *Text2ImageService) GenerateImagesFromPrompts(prompts []string, width, height int) ([]string, error) {
	prompts = cleanPrompts(prompts)
	if len(prompts) == 0 {
		return nil, fmt.Errorf("批量提示词为空")
	}
	// 兼容矩阵预检（见 comfy_compat.go）：不兼容组合构图前显式失败
	if err := CheckCheckpointCompat(s.Checkpoint); err != nil {
		return nil, err
	}
	if len(prompts) == 1 {
		one, err := s.GenerateImage(prompts[0], width, height)
		if err != nil {
			return nil, err
		}
		return []string{one}, nil
	}
	if width <= 0 {
		width = 1024
	}
	if height <= 0 {
		height = 1024
	}
	seed := time.Now().UnixNano() % (1 << 31)

	graph := map[string]interface{}{
		"1": ckptNode(s.Checkpoint), // 只加载一次，所有分支共用
	}
	for i, p := range prompts {
		k := fmt.Sprintf("%d", i+1)
		pos := "p" + k
		neg := "n" + k
		lat := "l" + k
		ks := "k" + k
		dec := "d" + k
		sv := "s" + k
		graph[pos] = clipEncode(p, "1")
		graph[neg] = clipEncode("", "1")
		graph[lat] = map[string]interface{}{"class_type": "EmptyLatentImage", "inputs": map[string]interface{}{
			"width": width, "height": height, "batch_size": 1,
		}}
		graph[ks] = map[string]interface{}{"class_type": "KSampler", "inputs": map[string]interface{}{
			"seed": seed + int64(i), "steps": 4, "cfg": 1.0,
			"sampler_name": "euler", "scheduler": "simple", "denoise": 1.0,
			"model": link("1", 0), "positive": link(pos, 0),
			"negative": link(neg, 0), "latent_image": link(lat, 0),
		}}
		graph[dec] = map[string]interface{}{"class_type": "VAEDecode", "inputs": map[string]interface{}{
			"samples": link(ks, 0), "vae": link("1", 2),
		}}
		graph[sv] = map[string]interface{}{"class_type": "SaveImage", "inputs": map[string]interface{}{
			"images": link(dec, 0), "filename_prefix": fmt.Sprintf("flowedge-multi-%02d", i+1),
		}}
	}

	// 每个 prompt 都要单独给足时间（同一次加载后按张累计）
	outs, err := s.RunGraph(graph, time.Duration(90*len(prompts))*time.Second)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(outs))
	for _, o := range outs {
		if o.Kind == "image" {
			paths = append(paths, o.Path)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("批量出图没有产出")
	}
	return paths, nil
}

// cleanPrompts 去掉空行与首尾空白，保留顺序
func cleanPrompts(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
