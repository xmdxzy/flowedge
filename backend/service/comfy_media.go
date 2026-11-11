package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	// Precision 精度档覆盖（权重文件名）。只由 OOM 降级链写入：
	// 优先换更省显存的权重（如 gguf/Q4），而不是先降分辨率——
	// 换精度不改变构图，降分辨率会。空 = 用 bundle 默认。
	Precision string
	LowVRAM   bool // H3：用分块注意力/分块 FFN 换显存，换更高分辨率
	Segments  int  // H3：分几段生成再拼接（每段 124 帧 ≈ 5 秒）
	Interp    bool // 出片后帧率翻倍（ffmpeg minterpolate，CPU，不占显存）
	Turbo     bool // H3 turbo LoRA：20 步 → 8 步（约 2.5 倍速）
	PDD       bool // H3 PDD 8-step Acc LoRA：非台词镜 8 步（与 Turbo 互斥）

	// 段间引导（motion context）：把上一镜末 N 帧（+同段音轨）钉进本镜前缀，
	// 生成后再裁掉前缀，让镜与镜在模型内部「一口气接着演」，而不是各拍各的再缝。
	//
	// 做法走**官方节点** MiniMaxH3AddGuide（image 口吃 5/22/39/56 帧片段、audio 口
	// 同起点锚定音轨，见 comfy_extras/nodes_minimax_h3.py），不 monkeypatch ComfyUI。
	// 思路来自 AIMixer/ComfyUI_MiniMaxH3_Director 的 h3_motion_context（Apache-2.0）：
	// 它注入上一段 AV latent 省一次 VAE 编码；这里用解码帧，换来不绑版本。
	GuideClip   string // 上一镜末 N 帧的 mp4（本地路径）
	GuideFrames int    // 上面那段片段实际帧数（5/22/39/56）
	GuideAudio  string // 上一镜末 N 帧对应的音轨 wav（本地路径，可空 = 只续画面）
	TrimHead    int    // 生成后从头部裁掉的帧数 = GuideFrames

	// 二采（同分辨率精修）：0 = 关。取值是第二遍的 denoise（0.25–0.5 是常用区间），
	// 越大改得越多、越容易把一采的构图改掉。RefineSteps 是第二遍的步数（默认 4）。
	RefineDenoise float64
	RefineSteps   int

	// v2v / rv2v：拿一段现成视频当参考（`<Video 1>`），只改要改的部分。
	//
	// 官方 ReferenceToVideo 的 ref_videos 吃的是 **IMAGE 批次**（不是 VIDEO 类型），
	// 所以走 VHS_LoadVideo 取帧片段的路径——和段间引导同一条，不引入新依赖。
	SourceVideo      string // 源视频路径（整段）
	SourceStartFrame int    // 从源视频第几帧开始取（每镜按累计帧数切片）

	// h3_r2v（ref2va）：参考图 + 参考音频 → 口型同步的视频。
	//
	// 这是 H3 唯一「用自己的音频念稿」的链路：ref_audios 里的声音会被模型
	// 当作口型基准，配合提示词里的 <Audio 1>: fully_copy 可以 1:1 透传，
	// 不再由模型即兴生成（那正是"伪中文乱语"的来源）。
	RefImages []string // 参考图（≤9），锁定角色外貌；提示词里用 <Picture i> 引用
	RefAudios []string // 参考音频（≤3），念稿/音色基准；提示词里用 <Audio j> 引用

	// Ctx 取消信号。节点超时 / 用户点停止时，必须把 ComfyUI 里那条提交**从队列里删掉**，
	// 而不是让它在引擎已经宣布失败之后继续烧 GPU（实测白烧 21 分钟，见 comfy_queue.go）。
	// 为空等价于 Background（老调用方行为不变）。
	Ctx context.Context

	// OnPhase 生成阶段变化回调："queued"（在 ComfyUI 队列里等着）/ "running"（正在算）。
	// 用途：看门狗在排队期间暂停计时——run 224 就是被"排队 6 分钟"耗到 5 倍基准误杀的。
	OnPhase func(phase string)
}

// MediaOutput 一个产出文件
type MediaOutput struct {
	Kind   string // image | video | audio
	Path   string
	NodeID string // 产出它的输出节点 id；批量图靠它把产物映射回各镜
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
	return s.RunGraphAtCtx(context.Background(), base, graph, timeout, nil)
}

// RunGraphAtCtx 指定实例提交，并支持取消（这是一条**可中断**的生成链路）。
//
// 为什么要有：原来这条链路完全不看 ctx——节点已经判超时失败，它还在轮询 /history，
// 直到自己 20 分钟的预算烧完。实测 run 224：引擎 08:56:21 宣布失败，那条提交
// 跑到 09:02:42 才返回，中间 6 分钟 GPU 全白烧；而看门狗在 5m35s 就触发过了，
// 因为那条 prompt 当时还在**排队**，/interrupt 对它无效。
// 现在 ctx 一断就：删 pending → 停 running → 立刻返回。
func (s *Text2ImageService) RunGraphAtCtx(ctx context.Context, base string, graph map[string]interface{}, timeout time.Duration, onPhase func(string)) ([]MediaOutput, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if base == "" {
		return nil, fmt.Errorf("comfyui not configured")
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}

	// 提交前预检（/object_info）：把「模型名写错 / 节点没装」拦在这里，
	// 而不是等 ComfyUI 返回 400 validation failed。
	//
	// 关键语义：探不到 object_info **不阻断**（老实例、自定义节点包返回畸形 JSON、
	// 代理劫持都会让探活失败），但会把原因写进返回的警告里——
	// 「没预检」与「预检通过」必须能被区分（AGENTS §0.6）。
	issues, pfErr := preflightGraph(base, graph)
	if pfErr == nil {
		if err := blockingIssues(issues); err != nil {
			return nil, err
		}
	}

	// 预检通过后再腾显存：图本身是坏的就不该付这个代价，
	// 更不该因为一次 400 把别的实例的热模型赶走（见 comfy_evict.go）。
	s.evictSiblingsFor(base)

	payload, _ := json.Marshal(map[string]interface{}{"prompt": graph})
	resp, err := s.httpClient().Post(base+"/prompt", "application/json", bytes.NewBuffer(payload))
	if err != nil {
		return nil, fmt.Errorf("comfyui request failed: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("comfyui rejected prompt (%d): %.400s", resp.StatusCode, string(body))
		if pfErr != nil {
			// 400 时说清「预检没能跑成」——否则用户会以为是预检放过了这个错误
			msg += "（提交前预检未执行：" + pfErr.Error() + "）"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	var queued struct {
		PromptID string `json:"prompt_id"`
	}
	if err := json.Unmarshal(body, &queued); err != nil || queued.PromptID == "" {
		return nil, fmt.Errorf("comfyui returned no prompt_id: %.300s", string(body))
	}

	deadline := time.Now().Add(timeout)
	// 提交时队列非空 = 本次注定要先排队。说明白，别让进度条空转
	// （实测排了 6 分钟，日志里一个字都没有，看起来像"卡死"）。
	if r, p := s.QueueBusy(base); r > 0 || p > 0 {
		log.Printf("[queue] %s 提交时队列非空（运行中 %d / 排队 %d）：本次生成会排在后面，等待时间不计入生成预算",
			strings.TrimRight(base, "/"), r, p)
	}
	phase := ""
	reportPhase := func(p string) {
		if p == "" || p == phase {
			return
		}
		phase = p
		if onPhase != nil {
			onPhase(p)
		}
	}
	reportPhase("queued") // 刚提交一定是排队态；真正开始算时再切 running
	missing := 0
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			gone, wasRunning := s.AbortPrompt(base, queued.PromptID)
			log.Printf("[queue] 生成被取消，已中止 prompt=%s（曾在运行=%v，已离开队列=%v）",
				shortID(queued.PromptID), wasRunning, gone)
			return nil, fmt.Errorf("已取消（prompt=%s 已离开队列=%v）：%w", shortID(queued.PromptID), gone, ctx.Err())
		case <-time.After(1500 * time.Millisecond):
		}

		hresp, err := s.httpClient().Get(base + "/history/" + queued.PromptID)
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
		if ok {
			missing = 0
			if entry.Status.StatusStr == "error" {
				return nil, fmt.Errorf("comfyui 执行失败（prompt_id=%s），首个报错节点见 ComfyUI 日志", queued.PromptID)
			}
			outs := s.collectOutputsAt(base, entry)
			if len(outs) == 0 {
				continue // 还没产出
			}
			return outs, nil
		}
		// history 里没有：要么还在排队，要么在跑，要么已经被人从队列里弄走了。
		// 后一种要能尽快失败——否则会一直等到 timeout，把"已经作废的提交"当成"还在生成"。
		//
		// 但判据必须**保守**：ComfyUI 在"离开队列"和"写进 history"之间有一个真空期，
		// 而实例正忙于加载权重时（实测 23:22 那次：TE 15GB staging 中），
		// /history/<id> 与 /queue 都可能短暂读不到它。真空期一过就被判"作废"，
		// 等于把一次本来正常的生成杀掉（2026-10-26 run 260 就是这么死的）。
		// 所以：连续 miss 阈值放宽到 ≈12 秒，并在判决前再做一次双向复核。
		running, pending, qerr := s.QueueIDs(base)
		if qerr != nil {
			continue
		}
		if containsID(running, queued.PromptID) {
			reportPhase("running")
		} else if containsID(pending, queued.PromptID) {
			reportPhase("queued")
		}
		if !containsID(running, queued.PromptID) && !containsID(pending, queued.PromptID) {
			missing++
			if missing >= droppedAfterMisses {
				// 判决前复核：history 里出现了（哪怕 error）就不算丢
				if _, found, _ := s.historyEntryAt(base, queued.PromptID); found {
					missing = 0
					continue
				}
				if r2, p2, err2 := s.QueueIDs(base); err2 == nil &&
					(containsID(r2, queued.PromptID) || containsID(p2, queued.PromptID)) {
					missing = 0
					continue
				}
				return nil, fmt.Errorf("提交 %s 已不在 ComfyUI 队列里（连续 %d 次、约 %d 秒既不在队列也不在历史；"+
					"被中断/清空/实例重启），本次生成作废",
					shortID(queued.PromptID), missing, missing*3/2)
			}
		} else {
			missing = 0
		}
	}
	return nil, fmt.Errorf("生成超时（%s）", timeout)
}

// droppedAfterMisses 连续多少次"既不在队列、也不在历史"才判定提交作废。
// 每次轮询 1.5 秒，8 次 ≈ 12 秒——足够跨过 ComfyUI 的队列↔历史真空期。
const droppedAfterMisses = 8

// historyEntryAt 读一条提交的 history（found=false 表示还没有记录）。
func (s *Text2ImageService) historyEntryAt(base, promptID string) (comfyHistoryEntry, bool, error) {
	resp, err := s.httpClient().Get(strings.TrimRight(base, "/") + "/history/" + promptID)
	if err != nil {
		return comfyHistoryEntry{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return comfyHistoryEntry{}, false, fmt.Errorf("GET /history → %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var hist map[string]comfyHistoryEntry
	if err := json.Unmarshal(body, &hist); err != nil {
		return comfyHistoryEntry{}, false, err
	}
	entry, ok := hist[promptID]
	return entry, ok, nil
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
	for nodeID, o := range entry.Outputs {
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
			dresp, err := s.httpClient().Get(base + "/view?" + q.Encode())
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
			outs = append(outs, MediaOutput{Kind: kind, Path: dest, NodeID: nodeID})
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
			strength = s.bundleParam(RoleImg2Img, "strength", 0.6)
		}
		steps := req.Steps
		if steps <= 0 {
			steps = s.bundleParamInt(RoleImg2Img, "steps", 8)
		}
		return map[string]interface{}{
			"4":  ckptNode(firstNonEmpty(req.Precision, s.ckptName())),
			"10": map[string]interface{}{"class_type": "LoadImage", "inputs": map[string]interface{}{"image": name}},
			"11": map[string]interface{}{"class_type": "VAEEncode", "inputs": map[string]interface{}{"pixels": link("10", 0), "vae": link("4", 2)}},
			"6":  clipEncode(req.Prompt, "4"),
			"33": clipEncode(req.Negative, "4"),
			"3": map[string]interface{}{"class_type": "KSampler", "inputs": map[string]interface{}{
				"seed": seed, "steps": steps, "cfg": s.bundleParam(RoleImg2Img, "cfg", 1.0), "sampler_name": "euler", "scheduler": "simple",
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
			denoise = s.bundleParam(RoleInpaint, "denoise", 1.0)
		}
		steps := req.Steps
		if steps <= 0 {
			steps = s.bundleParamInt(RoleInpaint, "steps", 10)
		}
		// 蒙版按原图尺寸缩放，避免尺寸不一致导致 latent 对不上
		// checkpoint 可被节点覆盖：填 Flux-Fill 得专业重绘（~11GB，先落位
		// D:/AI/Models 再用），留空走服务默认（flux-schnell，弱修补）
		return map[string]interface{}{
			"4":  ckptNode(firstNonEmpty(req.Precision, req.Model, s.ckptName())),
			"10": map[string]interface{}{"class_type": "LoadImage", "inputs": map[string]interface{}{"image": name}},
			"20": map[string]interface{}{"class_type": "LoadImage", "inputs": map[string]interface{}{"image": maskName}},
			"11": map[string]interface{}{"class_type": "VAEEncode", "inputs": map[string]interface{}{"pixels": link("10", 0), "vae": link("4", 2)}},
			// mask 必须接 LoadImage 的 MASK 槽（1）：IMAGE 槽（0）会直接报 return_type_mismatch。
			// 尺寸一致性由 Go 侧先校验（ResizeImageMaskNode 是动态 schema，接线脆弱，不值得为一层缩放去赌）。
			"12": map[string]interface{}{"class_type": "SetLatentNoiseMask", "inputs": map[string]interface{}{"samples": link("11", 0), "mask": link("20", 1)}},
			"6":  clipEncode(req.Prompt, "4"),
			"33": clipEncode(req.Negative, "4"),
			"3": map[string]interface{}{"class_type": "KSampler", "inputs": map[string]interface{}{
				"seed": seed, "steps": steps, "cfg": s.bundleParam(RoleInpaint, "cfg", 1.0), "sampler_name": "euler", "scheduler": "simple",
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
			model = s.bundleFile(RoleUpscale, "model")
		}
		return map[string]interface{}{
			"1":  map[string]interface{}{"class_type": "UpscaleModelLoader", "inputs": map[string]interface{}{"model_name": model}},
			"10": map[string]interface{}{"class_type": "LoadImage", "inputs": map[string]interface{}{"image": name}},
			"2":  map[string]interface{}{"class_type": "ImageUpscaleWithModel", "inputs": map[string]interface{}{"upscale_model": link("1", 0), "image": link("10", 0)}},
			"9":  saveImageFrom("2", "flowedge-upscale"),
		}, nil

	case "t2v":
		// Wan2.1-T2V-1.3B：官方推荐的轻量视频底座，24GB 可跑。
		// 全部参数与权重名走 bundle（data/models.json），换底座不用改代码。
		width, height, frames, fps, steps := req.Width, req.Height, req.Frames, req.FPS, req.Steps
		if width <= 0 {
			width = s.bundleParamInt(RoleVideoWan, "width", 832)
		}
		if height <= 0 {
			height = s.bundleParamInt(RoleVideoWan, "height", 480)
		}
		if frames <= 0 {
			frames = s.bundleParamInt(RoleVideoWan, "frames", 33)
		}
		if fps <= 0 {
			fps = s.bundleParamInt(RoleVideoWan, "fps", 16)
		}
		if steps <= 0 {
			steps = s.bundleParamInt(RoleVideoWan, "steps", 20)
		}
		wanUNet := firstNonEmpty(req.Precision, s.bundleFile(RoleVideoWan, "unet"))
		wanCLIP := s.bundleFile(RoleVideoWan, "clip")
		wanVAE := s.bundleFile(RoleVideoWan, "vae")
		wanCFG := s.bundleParam(RoleVideoWan, "cfg", 6.0)
		wanShift := s.bundleParam(RoleVideoWan, "shift", 8.0)
		return map[string]interface{}{
			"1": map[string]interface{}{"class_type": "UNETLoader", "inputs": map[string]interface{}{
				"unet_name": wanUNet, "weight_dtype": "default",
			}},
			"2": map[string]interface{}{"class_type": "ModelSamplingSD3", "inputs": map[string]interface{}{"model": link("1", 0), "shift": wanShift}},
			"3": map[string]interface{}{"class_type": "CLIPLoader", "inputs": map[string]interface{}{
				"clip_name": wanCLIP, "type": "wan",
			}},
			"4": map[string]interface{}{"class_type": "CLIPTextEncode", "inputs": map[string]interface{}{"text": req.Prompt, "clip": link("3", 0)}},
			"5": map[string]interface{}{"class_type": "CLIPTextEncode", "inputs": map[string]interface{}{"text": req.Negative, "clip": link("3", 0)}},
			"6": map[string]interface{}{"class_type": "EmptyHunyuanLatentVideo", "inputs": map[string]interface{}{
				"width": width, "height": height, "length": frames, "batch_size": 1,
			}},
			"7": map[string]interface{}{"class_type": "KSampler", "inputs": map[string]interface{}{
				"seed": seed, "steps": steps, "cfg": wanCFG, "sampler_name": "euler", "scheduler": "simple",
				"denoise": 1.0, "model": link("2", 0), "positive": link("4", 0),
				"negative": link("5", 0), "latent_image": link("6", 0),
			}},
			"8":  map[string]interface{}{"class_type": "VAELoader", "inputs": map[string]interface{}{"vae_name": wanVAE}},
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
		// 权重与分辨率走 bundle：fl2va 与 ref2va 是**两个不同的 checkpoint**，
		// 各自一条角色记录，换权重只改 data/models.json。
		h3Role := RoleVideoH3Fl2va
		if req.Kind == "h3_r2v" {
			h3Role = RoleVideoH3Ref2va
		}
		width, height, frames, steps := req.Width, req.Height, req.Frames, req.Steps
		if width <= 0 {
			width = s.bundleParamInt(h3Role, "width", 608)
		}
		if height <= 0 {
			height = s.bundleParamInt(h3Role, "height", 352)
		}
		if frames <= 0 {
			frames = s.bundleParamInt(h3Role, "frames", 124)
		}
		if steps <= 0 {
			steps = s.bundleParamInt(h3Role, "steps", 20)
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
			// 段间引导自带一段 N 帧锚定，若再叠一个「单帧 frame_idx=0」的 AddGuide，
			// 同一帧上会挂两个 keyframe（1 帧 + 22 帧片段），采样时互相打架。
			// 有片段就只走片段；单帧是片段不可用时的退路，两者不同时出现。
			if strings.TrimSpace(req.GuideClip) == "" {
				imgName, err := s.UploadImageAt(s.baseForKind(req.Kind), req.Image)
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
			if len(req.RefAudios) == 0 && len(req.RefImages) == 0 && strings.TrimSpace(req.SourceVideo) == "" {
				return nil, fmt.Errorf("参考生视频至少要给 1 张参考图、1 段参考音频或 1 段源视频")
			}
			// 六段式补齐：缺 subject_definitions / retention_analysis 时模型会把
			// 参考音频当成"风格提示"重新生成，念稿就退化成伪中文
			req.Prompt = ensureRef2VAPrompt(req.Prompt, len(req.RefImages), len(req.RefAudios))
			// 有源视频时补一条 <Video 1> 的保留说明（否则模型不知道那段素材是"要改的源片"）
			if strings.TrimSpace(req.SourceVideo) != "" {
				req.Prompt = ensureSourceVideoPrompt(req.Prompt)
			}
			refImgs := map[string]interface{}{}
			for i, p := range req.RefImages {
				if strings.TrimSpace(p) == "" {
					continue
				}
				name, err := s.UploadImageAt(s.baseForKind(req.Kind), p)
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
				name, err := s.UploadImageAt(s.baseForKind(req.Kind), p)
				if err != nil {
					return nil, fmt.Errorf("参考音频上传失败：%w", err)
				}
				id := fmt.Sprintf("%d", 60+i)
				graphExtra[id] = map[string]interface{}{"class_type": "LoadAudio",
					"inputs": map[string]interface{}{"audio": name}}
				refAuds[fmt.Sprintf("ref_audio_%d", i+1)] = link(id, 0)
			}
			// ---- v2v / rv2v：源视频当 <Video 1> ----
			//
			// 官方 ref_videos 吃 IMAGE 批次（不是 VIDEO 类型），所以用 VHS 按帧切片，
			// 与段间引导同一条素材通路。每镜切自己那一段：多镜片里
			// SourceStartFrame 是前面各镜帧数的累计（由 shotlist 算）。
			refVids := map[string]interface{}{}
			if strings.TrimSpace(req.SourceVideo) != "" {
				name, verr := s.UploadImageAt(s.baseForKind(req.Kind), req.SourceVideo)
				if verr != nil {
					return nil, fmt.Errorf("源视频上传失败：%w", verr)
				}
				skip := req.SourceStartFrame
				if skip < 0 {
					skip = 0
				}
				graphExtra["76"] = map[string]interface{}{"class_type": "VHS_LoadVideo", "inputs": map[string]interface{}{
					"video": name, "force_rate": 0, "custom_width": 0, "custom_height": 0,
					"frame_load_cap": frames, "skip_first_frames": skip, "select_every_nth": 1,
				}}
				// 槽位从 **0** 起：官方 Ref2VA 的 ref_videos 是 autogrow（prefix ref_video_，
				// min=0），序位型的参考按槽位顺序编号，`<Video 1>` 指的就是第一个槽。
				// 写成 ref_video_1 会把唯一那段视频放进第二个槽，`<Video 1>` 指向空槽 ——
				// 表面上节点收到参数、也跑了 vae.encode，但模型看不到任何视频。
				// 实测（2026-11-01）：ref_video_1 + "原样复刻"提示词，SSIM 0.53（= 没用上）；
				// AIMixer 自己的 v2v 用的就是 ref_video_0。
				refVids["ref_video_0"] = link("76", 0)
			}
			// 用 ReferenceToVideo 顶替默认的 ImageToVideo（同样输出 positive + latent）
			graphExtra["5"] = map[string]interface{}{"class_type": "MiniMaxH3ReferenceToVideo",
				"inputs": map[string]interface{}{
					"clip": link("2", 0), "vae": link("3", 0), "audio_vae": link("4", 0),
					"prompt": req.Prompt, "width": width, "height": height, "length": frames,
					"ref_image_size": "match",
					"ref_images":     refImgs, "ref_audios": refAuds,
					"ref_videos": refVids,
				}}
			// 参考模式不接首帧引导：Ref2V 自己管身份，叠 AddGuide 反而打架
			guideCond = link("5", 0)
		}

		// ---------- 段间引导（motion context）：上一镜末 N 帧 + 音轨 → 本镜前缀 ----------
		//
		// 与「单帧首帧引导」不是一回事：
		//   单帧 i2v = 只钉 1 帧画面，运动从零开始，且必须放弃 ref2va（身份锁）才接得上；
		//   本方案   = 钉 N 帧（5/22/39/56）+ 同段音轨，模型先「重演」这段尾巴再往下写，
		//              并且 AddGuide 是**追加** keyframe（读 positive 里已有的 minimax_keyframes），
		//              所以能叠在 ReferenceToVideo 之上，身份锁不丢。
		//
		// 生成帧数 = align17(正文帧 + N)，解码后裁掉头部 N 帧（trimHead），正文长度不变。
		// 这条链路只用官方节点 + VHS，不依赖任何 monkeypatch。
		if strings.TrimSpace(req.GuideClip) != "" {
			clipName, err := s.UploadImageAt(s.baseForKind(req.Kind), req.GuideClip)
			if err != nil {
				return nil, fmt.Errorf("段间引导片段上传失败：%w", err)
			}
			graphExtra["70"] = map[string]interface{}{"class_type": "VHS_LoadVideo", "inputs": map[string]interface{}{
				"video": clipName, "force_rate": 0, "custom_width": 0, "custom_height": 0,
				"frame_load_cap": 0, "skip_first_frames": 0, "select_every_nth": 1,
			}}
			guideInputs := map[string]interface{}{
				"positive": guideCond, "latent": link("5", 1), "frame_idx": 0,
				"vae": link("3", 0), "image": link("70", 0),
			}
			if strings.TrimSpace(req.GuideAudio) != "" {
				audName, aerr := s.UploadImageAt(s.baseForKind(req.Kind), req.GuideAudio)
				if aerr != nil {
					return nil, fmt.Errorf("段间引导音频上传失败：%w", aerr)
				}
				graphExtra["71"] = map[string]interface{}{"class_type": "LoadAudio", "inputs": map[string]interface{}{"audio": audName}}
				guideInputs["audio"] = link("71", 0)
				guideInputs["audio_vae"] = link("4", 0)
			}
			graphExtra["72"] = map[string]interface{}{"class_type": "MiniMaxH3AddGuide", "inputs": guideInputs}
			guideCond = link("72", 0)
		}

		// 低显存模式：把注意力与 FFN 分块。峰值显存下来，代价是稍慢。
		stepsExplicit := req.Steps > 0

		// PDD 8 步（阿里 PAI 官方加速 LoRA）：只给非台词镜用。
		// 与 turbo 互斥；权重缺失必须显式报错，不能静默退回 12 步。
		pddOn := false
		if req.PDD {
			if req.Turbo {
				return nil, fmt.Errorf("H3 PDD 与 turbo 不能同时开")
			}
			lora := H3PDDLoRAFile(req.Kind)
			if lora == "" {
				return nil, fmt.Errorf("H3 PDD 8 步 LoRA 缺失（kind=%s）", req.Kind)
			}
			graphExtra["40"] = map[string]interface{}{"class_type": "LoraLoaderModelOnly", "inputs": map[string]interface{}{
				"model": modelRef, "lora_name": lora, "strength_model": 1.0,
			}}
			modelRef = link("40", 0)
			pddOn = true
			steps = 8
		}

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
		if turboOn || pddOn {
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
		// bundle 里 fl2va / ref2va 各有一份 unet，这里按 kind 取。
		unetName := firstNonEmpty(req.Precision, s.bundleFile(h3Role, "unet"))
		h3Clip := s.bundleFile(h3Role, "clip")
		h3VideoVAE := s.bundleFile(h3Role, "video_vae")
		h3AudioVAE := s.bundleFile(h3Role, "audio_vae")
		// ---------- 二采（Refine）：同分辨率精修 ----------
		//
		// 一采定构图与运动，二采只补细节：把一采的 latent 当起点，用一段
		// **从中间切进去的 sigma 表**（denoise < 1）再采一遍。
		//
		// 与「放大二采」的区别：这里分辨率、帧数、时长都不变，所以镜头节奏、
		// 音画对齐、字幕时间轴全不受影响——这也是它能直接挂在分镜链路上的原因。
		// 思路对照 AIMixer 的 refine（同尺寸路径必须接 SIGMAS，不是 denoise<1 硬灌）：
		// 我们用 BasicScheduler 的 denoise 生成那条部分 sigma 表，等价且不用自定义节点。
		finalLatent := link("10", 0)
		if req.RefineDenoise > 0 {
			refineSteps := req.RefineSteps
			if refineSteps <= 0 {
				refineSteps = 4
			}
			graphExtra["73"] = map[string]interface{}{"class_type": "RandomNoise",
				"inputs": map[string]interface{}{"noise_seed": seed + 1}}
			graphExtra["74"] = map[string]interface{}{"class_type": "BasicScheduler", "inputs": map[string]interface{}{
				"model": modelRef, "scheduler": "simple", "steps": refineSteps, "denoise": req.RefineDenoise,
			}}
			graphExtra["75"] = map[string]interface{}{"class_type": "SamplerCustomAdvanced", "inputs": map[string]interface{}{
				"noise": link("73", 0), "guider": link("7", 0), "sampler": link("8", 0),
				"sigmas": link("74", 0), "latent_image": link("10", 0),
			}}
			finalLatent = link("75", 0)
		}

		graph := map[string]interface{}{
			"1": map[string]interface{}{"class_type": "UNETLoader", "inputs": map[string]interface{}{
				"unet_name": unetName, "weight_dtype": "default",
			}},
			"2": map[string]interface{}{"class_type": "CLIPLoader", "inputs": map[string]interface{}{
				"clip_name": h3Clip, "type": "minimax", "device": "default",
			}},
			"3": map[string]interface{}{"class_type": "VAELoader", "inputs": map[string]interface{}{
				"vae_name": h3VideoVAE,
			}},
			"4": map[string]interface{}{"class_type": "VAELoader", "inputs": map[string]interface{}{
				"vae_name": h3AudioVAE,
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
			"11": map[string]interface{}{"class_type": "VAEDecode", "inputs": map[string]interface{}{"samples": finalLatent, "vae": link("3", 0)}},
			"12": map[string]interface{}{"class_type": "VAEDecodeAudio", "inputs": map[string]interface{}{"samples": finalLatent, "vae": link("4", 0)}},
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
	// 实例路由：按实际权重名决定跑哪套环境（H3 要 torch 2.14 的独立工作树）。
	// 没有配置 registry 时等价于旧逻辑（h3 → H3BaseURL，其余 → BaseURL）。
	base := s.baseForKind(req.Kind)
	// 互斥组：同组实例不能同时驻留，锁到执行结束（旧行为靠"人记得先停一个"）
	unlock := s.Instances.LockForBase(base)
	defer unlock()
	switch req.Kind {
	case "t2v":
		timeout = 25 * time.Minute // 视频首次要加载 10GB 权重
	case "h3", "h3_i2v", "h3_r2v":
		timeout = 45 * time.Minute // 40GB 权重 + 40GB 内存工作集，慢是正常的
	}
	outs, err := s.RunGraphAtCtx(req.Ctx, base, graph, timeout, req.OnPhase)
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
	return s.GenerateImagesWithCkpt(prompt, width, height, batch, "")
}

// GenerateImagesWithBackend 按后端批量出图（MOSS / flux）。
//
// MOSS 为什么逐条调而不是塞 batch_size：官方 50 步 @1024² 下单张就已经是几十秒级，
// 一次提交 N 张会把超时与显存同时顶到上限，而且失败时整批一起废掉。
// flux 那条 batch 路径（GenerateImagesWithCkpt）不动——它靠「共用一次 17GB 加载」
// 省时间，那是 4 步模型才成立的逻辑。
func (s *Text2ImageService) GenerateImagesWithBackend(prompts []string, opt T2IOptions) ([]string, error) {
	out := make([]string, 0, len(prompts))
	for i, p := range prompts {
		one := opt
		one.Prompt = p
		path, err := s.GenerateImageWithBackend(one)
		if err != nil {
			if len(out) == 0 {
				return nil, err
			}
			// 已经出了一部分就带着已有结果返回：让下游能看见前几张，
			// 错误写进日志而不是整批作废（出片流程里 N-1 张也是资产）
			log.Printf("[t2i] 批量第 %d/%d 张失败：%v", i+1, len(prompts), err)
			return out, nil
		}
		out = append(out, path)
	}
	return out, nil
}

// GenerateImagesWithCkpt 批量出图 + 节点级 checkpoint 覆盖（空 = 服务默认）。
func (s *Text2ImageService) GenerateImagesWithCkpt(prompt string, width, height, batch int, ckpt string) ([]string, error) {
	if strings.TrimSpace(ckpt) == "" {
		ckpt = s.Checkpoint
	}
	if batch <= 1 {
		one, err := s.GenerateImageWithCkpt(prompt, width, height, ckpt)
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
		"4":  ckptNode(ckpt),
		"6":  clipEncode(prompt, "4"),
		"33": clipEncode("", "4"),
		"5": map[string]interface{}{"class_type": "EmptyLatentImage", "inputs": map[string]interface{}{
			"width": width, "height": height, "batch_size": batch,
		}},
		"3": map[string]interface{}{"class_type": "KSampler", "inputs": map[string]interface{}{
			"seed": time.Now().UnixNano() % (1 << 31), "steps": s.bundleParamInt(RoleT2IBatch, "steps", 4), "cfg": s.bundleParam(RoleT2IBatch, "cfg", 1.0),
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
			"seed": seed + int64(i), "steps": s.bundleParamInt(RoleT2IMultiPrompt, "steps", 4), "cfg": s.bundleParam(RoleT2IMultiPrompt, "cfg", 1.0),
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
