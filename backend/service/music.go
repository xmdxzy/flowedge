package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MusicService ACE-Step 1.5 本地音乐生成服务客户端。
//
// 服务端是 ACE-Step 自己的 REST API（acestep/api_server.py），三段式：
//
//	POST /release_task  -> {data:{task_id,status,queue_position}}
//	POST /query_result  -> {data:[{task_id,status,result}]}  status: 0跑着/1成功/2失败
//	GET  /v1/audio?path=... -> 音频字节
//
// 为什么直接用它自带的 API 而不在 Go 侧重写：模型加载、LM 规划、队列与显存
// 清理都在 Python 侧，重写等于把上游的版本跟进工作接过来。FlowEdge 只负责
// 进程托管（deps.json）+ 提交/轮询/落盘。
//
// 与 TTS 的差异（重要）：ACE-Step **没有 /unload 端点**，唯一可靠的腾显存
// 手段是停进程（deps.Stop）。所以 Unload 一律返回错误，让 vram.go 走
// 「停服务」兜底，而不是假装卸载成功——「卸载了但显存没还」是这个项目
// 已经踩过两次的坑（Breeze TTS 假卸载）。
type MusicService struct {
	BaseURL string
	client  *http.Client
}

func NewMusicService(baseURL string) *MusicService {
	return &MusicService{
		BaseURL: strings.TrimRight(baseURL, "/"),
		// 这里只是兜底；真正的预算在 Generate 里按「时长 + 冷加载」算。
		client: &http.Client{Timeout: 30 * time.Minute},
	}
}

// MusicOptions 一次音乐生成请求。
type MusicOptions struct {
	Prompt   string  // 风格/氛围描述（caption）
	Lyrics   string  // 留空 = 纯音乐（BGM 默认走这条）
	Duration float64 // 秒；≤0 用服务端默认（约 30s）
	Model    string  // DiT 模型目录名，留空 = 服务端默认
	Steps    int     // 推理步数；turbo 推荐 8
	Language string  // 歌词语言，仅在有歌词时有意义
	Thinking bool    // true = 用 5Hz LM 生成 audio codes（质量更好、更慢）
	Seed     int64   // <0 表示随机
}

// MusicResult 一次生成的结果。
type MusicResult struct {
	Path     string  `json:"path"`
	Model    string  `json:"model"`
	Duration float64 `json:"duration"` // 实测时长（秒）
	Seed     string  `json:"seed"`
	Seconds  float64 `json:"seconds"` // 服务端耗时（墙钟，秒）
}

// musicFirstCallBudget 买「模型此刻不在显存里」这件事。
//
// 实测口径（2026-11-03 本机，3090 24GB）：XL 4B 冷启动 **15 分钟以上**——
// 进程拉起 ~40s、4 个 shard 约 19GB、LM tokenizer 49s、LM 权重，外加它
// 自己回头去 ModelScope 校验 29 个文件。热生成一首 30s 曲子只有十几秒。
// 所以固定部分必须给 20 分钟，再按目标时长线性加。
const musicFirstCallBudget = 20 * time.Minute

// Unload 释放显存。ACE-Step 没有卸载端点 ⇒ 显式报错，让调用方走停进程。
func (s *MusicService) Unload() error {
	return fmt.Errorf("ACE-Step 无 /unload 端点（只能停进程释放显存）")
}

// Health 探活（/health 返回 200 且 status=ok 才算活）。
func (s *MusicService) Health() error {
	if s == nil || s.BaseURL == "" {
		return fmt.Errorf("%w: 未配置音乐服务地址", ErrServiceNotConfigured)
	}
	resp, err := s.client.Get(s.BaseURL + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("音乐服务 /health 返回 %d", resp.StatusCode)
	}
	return nil
}

// apiEnvelope ACE-Step 的统一响应外壳。
type apiEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Code  int             `json:"code"`
	Error *string         `json:"error"`
}

func (s *MusicService) post(ctx context.Context, path string, body map[string]interface{}) (json.RawMessage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("音乐服务 %s 请求失败：%w", path, err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	var env apiEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("音乐服务 %s 返回无法解析（HTTP %d）：%s", path, resp.StatusCode, truncateForErr(payload))
	}
	if env.Code != 0 && env.Code != http.StatusOK {
		msg := "未知错误"
		if env.Error != nil {
			msg = *env.Error
		}
		return nil, fmt.Errorf("音乐服务 %s 报错：%s", path, msg)
	}
	if env.Error != nil && *env.Error != "" {
		return nil, fmt.Errorf("音乐服务 %s 报错：%s", path, *env.Error)
	}
	return env.Data, nil
}

// Generate 提交一次生成并等到音频落盘。
//
// 超时按工作量给（不是魔法常数）：musicFirstCallBudget + 每秒目标音频 1s，
// 上限 30 分钟。冷启动窗口内第一次请求不会被 HTTP 层误杀。
func (s *MusicService) Generate(ctx context.Context, opt MusicOptions, outputPath string) (MusicResult, error) {
	var res MusicResult
	if s == nil || s.BaseURL == "" {
		return res, fmt.Errorf("%w: 未配置音乐服务地址（data/deps.json 的 acestep.base_url）", ErrServiceNotConfigured)
	}
	if strings.TrimSpace(opt.Prompt) == "" && strings.TrimSpace(opt.Lyrics) == "" {
		return res, fmt.Errorf("音乐生成缺少 prompt：至少给一句风格/氛围描述")
	}

	budget := musicFirstCallBudget
	if opt.Duration > 0 {
		budget += time.Duration(opt.Duration) * time.Second
	}
	if budget > 30*time.Minute {
		budget = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	req := map[string]interface{}{
		"prompt": opt.Prompt,
		"lyrics": opt.Lyrics,
		// 落盘统一用 wav：下游混音与响度归一都不该在解码上再赌一次
		"audio_format": "wav",
		"batch_size":   1,
	}
	if opt.Duration > 0 {
		req["audio_duration"] = opt.Duration
	}
	if opt.Steps > 0 {
		req["inference_steps"] = opt.Steps
	}
	if opt.Model != "" {
		req["model"] = opt.Model
	}
	if opt.Language != "" {
		req["vocal_language"] = opt.Language
	}
	if opt.Thinking {
		req["thinking"] = true
	}
	// 纯音乐（无歌词）默认**关掉 LM 的 CoT 与语言识别**：
	//
	// 2026-11-03 实测：只给 prompt 的一次 12s 请求，卡在
	// 「Phase 1: Generating CoT metadata」5 分钟以上（progress 稳定 0.1），
	// 而 BGM 根本不需要它——CoT 是用来改写 caption / 猜歌词语言的。
	// 关掉之后整条 LM 阶段直接跳过：省一次 3.4GB 权重载入 + 数十秒 tokenizer
	// + 生成的几十秒到几分钟。需要 LM 规划（写歌/带歌词）时显式 thinking=true。
	if !opt.Thinking && strings.TrimSpace(opt.Lyrics) == "" {
		req["use_cot_caption"] = false
		req["use_cot_language"] = false
	}
	if opt.Seed >= 0 {
		req["use_random_seed"] = false
		req["seed"] = opt.Seed
	}

	started := time.Now()
	data, err := s.post(ctx, "/release_task", req)
	if err != nil {
		return res, err
	}
	var submitted struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &submitted); err != nil || submitted.TaskID == "" {
		// 有些版本 data 直接是 task_id 字符串
		var id string
		if json.Unmarshal(data, &id) == nil && id != "" {
			submitted.TaskID = id
		}
	}
	if submitted.TaskID == "" {
		return res, fmt.Errorf("音乐服务未返回 task_id：%s", truncateForErr(data))
	}

	audioURL, err := s.waitTask(ctx, submitted.TaskID)
	if err != nil {
		return res, err
	}

	if err := s.download(ctx, audioURL, outputPath); err != nil {
		return res, err
	}

	res.Path = outputPath
	res.Model = opt.Model
	res.Seconds = time.Since(started).Seconds()
	// 时长以 ffprobe 实测为准：服务端自报的 duration 是「请求目标」，
	// 实际产出可能因为 LM 规划而不同（BGM 混音要按真实时长算循环）。
	if d, derr := mediaSeconds(outputPath); derr == nil {
		res.Duration = d
	}
	return res, nil
}

// waitTask 轮询直到 status=1（成功）或 2（失败）。
func (s *MusicService) waitTask(ctx context.Context, taskID string) (string, error) {
	interval := 2 * time.Second
	for {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("等待音乐生成超时（task %s）：%w", taskID, err)
		}
		data, err := s.post(ctx, "/query_result", map[string]interface{}{
			"task_id_list": []string{taskID},
		})
		if err != nil {
			return "", err
		}
		var items []struct {
			TaskID string `json:"task_id"`
			Status int    `json:"status"`
			Result string `json:"result"`
		}
		if err := json.Unmarshal(data, &items); err != nil {
			return "", fmt.Errorf("音乐服务 query_result 返回无法解析：%s", truncateForErr(data))
		}
		for _, it := range items {
			if it.TaskID != "" && it.TaskID != taskID {
				continue
			}
			switch it.Status {
			case 1:
				return parseAudioURL(it.Result)
			case 2:
				return "", fmt.Errorf("音乐生成失败（task %s）：%s", taskID, truncateForErr([]byte(it.Result)))
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("等待音乐生成超时（task %s）：%w", taskID, ctx.Err())
		case <-time.After(interval):
		}
		if interval < 5*time.Second {
			interval += 500 * time.Millisecond
		}
	}
}

// parseAudioURL 从 result 字符串里取第一个音频文件地址。
//
// result 是一个**被转义的 JSON 字符串**（见 API.md §5.3），所以要二次解析；
// 有些版本直接给数组。两种都认，取不到就报原文，不猜。
func parseAudioURL(result string) (string, error) {
	result = strings.TrimSpace(result)
	if result == "" || result == "null" {
		return "", fmt.Errorf("音乐生成完成但 result 为空（没有音频产物）")
	}
	var entries []struct {
		File   string `json:"file"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal([]byte(result), &entries); err != nil {
		var one struct {
			File string `json:"file"`
		}
		if err2 := json.Unmarshal([]byte(result), &one); err2 == nil && one.File != "" {
			return one.File, nil
		}
		return "", fmt.Errorf("音乐服务 result 无法解析：%s", truncateForErr([]byte(result)))
	}
	for _, e := range entries {
		if strings.TrimSpace(e.File) != "" {
			return e.File, nil
		}
	}
	return "", fmt.Errorf("音乐服务 result 里没有 file 字段：%s", truncateForErr([]byte(result)))
}

// download 把 /v1/audio?path=... 的字节写到 outputPath。
func (s *MusicService) download(ctx context.Context, fileURL, outputPath string) error {
	url := fileURL
	if strings.HasPrefix(url, "/") {
		url = s.BaseURL + url
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("下载音乐产物失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("下载音乐产物失败：HTTP %d %s", resp.StatusCode, truncateForErr(body))
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return err
	}
	tmp := outputPath + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("写音乐产物失败：%w", copyErr)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return closeErr
	}
	// 零字节当失败：写个空文件让下游「成功」出片是本项目的典型假绿（§0.12 ②）
	if n == 0 {
		os.Remove(tmp)
		return fmt.Errorf("音乐产物是空文件（0 字节）")
	}
	if err := os.Rename(tmp, outputPath); err != nil {
		os.Remove(tmp)
		return err
	}
	log.Printf("[music] 已落盘 %s（%.1f KB）", filepath.Base(outputPath), float64(n)/1024)
	return nil
}

// truncateForErr 把返回体截断到可读长度，避免把整页 HTML 塞进错误信息。
func truncateForErr(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len([]rune(s)) > 300 {
		r := []rune(s)
		return string(r[:300]) + "…"
	}
	return s
}
