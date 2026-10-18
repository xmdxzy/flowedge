package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Text2ImageService 文生图服务（ComfyUI 兼容）
type Text2ImageService struct {
	BaseURL string
	client  *http.Client
}

func NewText2ImageService(baseURL string) *Text2ImageService {
	return &Text2ImageService{
		BaseURL: baseURL,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// GenerateImage 生成图片
func (s *Text2ImageService) GenerateImage(prompt string, width, height int) (string, error) {
	if s.BaseURL == "" {
		return "", fmt.Errorf("comfyui not configured")
	}

	// ComfyUI API 格式
	workflow := map[string]interface{}{
		"3": map[string]interface{}{
			"class_type": "KSampler",
			"inputs": map[string]interface{}{
				"seed":     123456789,
				"steps":    20,
				"cfg":      7.5,
				"sampler_name": "euler",
				"scheduler": "normal",
				"denoise":  1.0,
			},
		},
		"4": map[string]interface{}{
			"class_type": "CheckpointLoaderSimple",
			"inputs": map[string]interface{}{
				"ckpt_name": "sd_xl_base_1.0.safetensors",
			},
		},
		"5": map[string]interface{}{
			"class_type": "EmptyLatentImage",
			"inputs": map[string]interface{}{
				"width":  width,
				"height": height,
				"batch_size": 1,
			},
		},
		"6": map[string]interface{}{
			"class_type": "CLIPTextEncode",
			"inputs": map[string]interface{}{
				"text": prompt,
			},
		},
	}

	jsonBody, _ := json.Marshal(workflow)
	resp, err := s.client.Post(s.BaseURL+"/prompt", "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", fmt.Errorf("comfyui request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("comfyui returned status %d", resp.StatusCode)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	promptID, _ := result["prompt_id"].(string)
	return fmt.Sprintf("generated://%s", promptID), nil
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

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	text, _ := result["text"].(string)
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

	// 保存音频文件
	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
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
