package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Text2ImageService 文生图服务（ComfyUI 兼容）
type Text2ImageService struct {
	BaseURL    string
	Checkpoint string
	client     *http.Client
}

func NewText2ImageService(baseURL, checkpoint string) *Text2ImageService {
	if checkpoint == "" {
		checkpoint = "flux1-schnell-fp8.safetensors"
	}
	return &Text2ImageService{
		BaseURL:    baseURL,
		Checkpoint: checkpoint,
		client: &http.Client{
			Timeout: 300 * time.Second,
		},
	}
}

// GenerateImage 生成图片：POST /prompt → 轮询 /history → GET /view 下载落盘，返回本地文件路径
func (s *Text2ImageService) GenerateImage(prompt string, width, height int) (string, error) {
	if s.BaseURL == "" {
		return "", fmt.Errorf("comfyui not configured")
	}
	if width <= 0 {
		width = 1024
	}
	if height <= 0 {
		height = 1024
	}

	link := func(id string, slot int) []interface{} { return []interface{}{id, slot} }

	// flux1-schnell fp8 checkpoint 标准 txt2img API 图（4 步 / cfg 1.0 / euler+simple）
	workflow := map[string]interface{}{
		"4": map[string]interface{}{
			"class_type": "CheckpointLoaderSimple",
			"inputs": map[string]interface{}{
				"ckpt_name": s.Checkpoint,
			},
		},
		"6": map[string]interface{}{
			"class_type": "CLIPTextEncode",
			"inputs": map[string]interface{}{
				"text": prompt,
				"clip": link("4", 1),
			},
		},
		"33": map[string]interface{}{
			"class_type": "CLIPTextEncode",
			"inputs": map[string]interface{}{
				"text": "",
				"clip": link("4", 1),
			},
		},
		"5": map[string]interface{}{
			"class_type": "EmptyLatentImage",
			"inputs": map[string]interface{}{
				"width":      width,
				"height":     height,
				"batch_size": 1,
			},
		},
		"3": map[string]interface{}{
			"class_type": "KSampler",
			"inputs": map[string]interface{}{
				"seed":         time.Now().UnixNano(),
				"steps":        4,
				"cfg":          1.0,
				"sampler_name": "euler",
				"scheduler":    "simple",
				"denoise":      1.0,
				"model":        link("4", 0),
				"positive":     link("6", 0),
				"negative":     link("33", 0),
				"latent_image": link("5", 0),
			},
		},
		"8": map[string]interface{}{
			"class_type": "VAEDecode",
			"inputs": map[string]interface{}{
				"samples": link("3", 0),
				"vae":     link("4", 2),
			},
		},
		"9": map[string]interface{}{
			"class_type": "SaveImage",
			"inputs": map[string]interface{}{
				"images":          link("8", 0),
				"filename_prefix": "flowedge",
			},
		},
	}

	// ComfyUI /prompt 协议要求 {"prompt": <graph>} 包裹
	payload := map[string]interface{}{"prompt": workflow}
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal workflow failed: %w", err)
	}

	resp, err := s.client.Post(s.BaseURL+"/prompt", "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", fmt.Errorf("comfyui request failed: %w", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("comfyui rejected prompt (status %d): %.300s", resp.StatusCode, string(respBody))
	}

	var queued struct {
		PromptID string `json:"prompt_id"`
	}
	if err := json.Unmarshal(respBody, &queued); err != nil || queued.PromptID == "" {
		return "", fmt.Errorf("comfyui returned no prompt_id: %.300s", string(respBody))
	}

	// 轮询执行结果（首次运行含模型冷加载，给足 7 分钟）
	deadline := time.Now().Add(7 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(1500 * time.Millisecond)

		hresp, err := s.client.Get(s.BaseURL + "/history/" + queued.PromptID)
		if err != nil {
			continue
		}
		hbody, _ := io.ReadAll(hresp.Body)
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
			continue // 尚未开始执行
		}
		if entry.Status.StatusStr == "error" {
			return "", fmt.Errorf("comfyui generation failed (prompt_id=%s)", queued.PromptID)
		}

		var imgFile, imgSub, imgType string
		for _, out := range entry.Outputs {
			for _, im := range out.Images {
				imgFile, imgSub, imgType = im.Filename, im.Subfolder, im.Type
				break
			}
			if imgFile != "" {
				break
			}
		}
		if imgFile == "" {
			continue // 仍在生成
		}

		viewURL := fmt.Sprintf("%s/view?filename=%s&subfolder=%s&type=%s",
			s.BaseURL, url.QueryEscape(imgFile), url.QueryEscape(imgSub), url.QueryEscape(imgType))
		dresp, err := s.client.Get(viewURL)
		if err != nil {
			return "", fmt.Errorf("comfyui download failed: %w", err)
		}
		data, err := io.ReadAll(dresp.Body)
		dresp.Body.Close()
		if err != nil {
			return "", err
		}
		if dresp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("comfyui /view returned status %d", dresp.StatusCode)
		}

		dir := "data/generated"
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", err
		}
		outPath := filepath.Join(dir, fmt.Sprintf("flowedge-%d-%s", time.Now().Unix(), imgFile))
		if err := os.WriteFile(outPath, data, 0644); err != nil {
			return "", err
		}
		if abs, err := filepath.Abs(outPath); err == nil {
			return abs, nil
		}
		return outPath, nil
	}

	return "", fmt.Errorf("comfyui generation timed out after 7m (prompt_id=%s)", queued.PromptID)
}

// comfyHistoryEntry ComfyUI /history 返回条目
type comfyHistoryEntry struct {
	Status struct {
		StatusStr string `json:"status_str"`
	} `json:"status"`
	Outputs map[string]struct {
		Images []struct {
			Filename  string `json:"filename"`
			Subfolder string `json:"subfolder"`
			Type      string `json:"type"`
		} `json:"images"`
	} `json:"outputs"`
}

// ASRService 语音转写服务
type ASRService struct {
	BaseURL string
	client  *http.Client
}

func NewASRService(baseURL string) *ASRService {
	return &ASRService{
		BaseURL: baseURL,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// Transcribe 语音转文字
func (s *ASRService) Transcribe(audioPath string) (string, error) {
	if s.BaseURL == "" {
		// 本地 faster-whisper 模拟
		return fmt.Sprintf("[transcribed from %s]", filepath.Base(audioPath)), nil
	}

	file, err := os.Open(audioPath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// 简单 multipart 上传
	var buf bytes.Buffer
	buf.WriteString("--boundary\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"audio.wav\"\r\n")
	buf.WriteString("Content-Type: audio/wav\r\n\r\n")
	io.Copy(&buf, file)
	buf.WriteString("\r\n--boundary--\r\n")

	resp, err := s.client.Post(s.BaseURL+"/asr", "multipart/form-data; boundary=boundary", &buf)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("asr service returned status %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	text, _ := result["text"].(string)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("asr service returned empty transcription")
	}
	return text, nil
}

// TTSService 语音合成服务
type TTSService struct {
	BaseURL string
	client  *http.Client
}

func NewTTSService(baseURL string) *TTSService {
	return &TTSService{
		BaseURL: baseURL,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// Synthesize 文字转语音
func (s *TTSService) Synthesize(text string, outputPath string) error {
	if s.BaseURL == "" {
		// 模拟生成
		return os.WriteFile(outputPath, []byte("fake audio data"), 0644)
	}

	reqBody := map[string]interface{}{
		"text": text,
	}
	jsonBody, _ := json.Marshal(reqBody)

	resp, err := s.client.Post(s.BaseURL+"/tts", "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("tts service returned status %d: %s", resp.StatusCode, string(body))
	}

	// 保存音频文件
	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()

	n, err := io.Copy(out, resp.Body)
	if err != nil {
		return err
	}
	if n < 1000 {
		return fmt.Errorf("tts service returned too-small audio (%d bytes)", n)
	}
	return nil
}

// VideoService 视频处理服务（FFmpeg）
type VideoService struct {
	FFmpegPath string
}

func NewVideoService() *VideoService {
	return &VideoService{
		FFmpegPath: "ffmpeg",
	}
}

// ComposeVideo 合成视频
func (s *VideoService) ComposeVideo(imagePaths []string, audioPath string, outputPath string) error {
	if len(imagePaths) == 0 {
		return fmt.Errorf("no images provided")
	}

	// 简化版：用第一张图 + 音频生成视频
	args := []string{
		"-y",
		"-loop", "1",
		"-i", imagePaths[0],
	}

	if audioPath != "" {
		args = append(args, "-i", audioPath)
		args = append(args, "-c:v", "libx264", "-tune", "stillimage",
			"-c:a", "aac", "-b:a", "192k",
			"-pix_fmt", "yuv420p", "-shortest")
	} else {
		args = append(args, "-c:v", "libx264", "-t", "5",
			"-pix_fmt", "yuv420p")
	}

	args = append(args, outputPath)

	cmd := exec.Command(s.FFmpegPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg failed: %w, output: %s", err, string(output))
	}

	return nil
}

// AddSubtitles 添加字幕
func (s *VideoService) AddSubtitles(videoPath string, subtitlePath string, outputPath string) error {
	cmd := exec.Command(s.FFmpegPath,
		"-y", "-i", videoPath,
		"-vf", fmt.Sprintf("subtitles=%s", subtitlePath),
		"-c:a", "copy",
		outputPath,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("add subtitles failed: %w, output: %s", err, string(output))
	}
	return nil
}
