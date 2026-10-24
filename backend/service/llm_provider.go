package service

import (
	"net/url"
	"os"
	"strings"
)

// LLM 后端抽象。
//
// 为什么需要（2026-10-21）：
//
//	补全链路本来就走 OpenAI 兼容格式（`/v1/chat/completions`），但代码里
//	处处号称 "ollama"：错误文案写 "ollama request failed"、视觉理解硬编码
//	`/api/chat`、卸载走 `/api/generate`。结果实际上只支持 Ollama 一个后端，
//	把已经存在的兼容性浪费掉了——vLLM / SGLang / LM Studio / llama.cpp server
//	/ 任意 OpenAI 兼容端点本该开箱可用。
//
// 两种协议的关系必须说清：
//
//	ollama  —— OpenAI 兼容补全 + 原生 /api/chat（vision）+ /api/generate（卸载）
//	openai  —— 只有 OpenAI 兼容端点；vision 走 image_url，**无法**从外部卸载模型
//
// 所以 openai 后端不是"残缺"，它失去的只是 Ollama 私有的两件事；
// 显存卸载改为由该后端自己管理（vLLM 的 gpu_memory_utilization 等）。
const (
	BackendOllama = "ollama"
	BackendOpenAI = "openai"
)

// ResolveLLMBackend 判定后端类型。
//
// 显式 LLM_PROVIDER 优先（ollama / openai）；否则按地址猜：
// 默认端口 11434 视为 Ollama，其余一律按 OpenAI 兼容处理。
// 敢按地址猜的原因是：Ollama 也讲 OpenAI 协议，猜错只会少两个私有能力，
// 不会让补全整个失效。
func ResolveLLMBackend(baseURL, explicit string) string {
	switch strings.ToLower(strings.TrimSpace(explicit)) {
	case BackendOllama:
		return BackendOllama
	case BackendOpenAI, "vllm", "lmstudio", "llamacpp", "compatible":
		return BackendOpenAI
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return BackendOllama
	}
	if u.Port() == "11434" {
		return BackendOllama
	}
	if strings.Contains(strings.ToLower(u.Host), "ollama") {
		return BackendOllama
	}
	return BackendOpenAI
}

// BackendLabel 给用户看的中文后端名（错误文案里用，不再写死 "ollama"）
func BackendLabel(backend string) string {
	if backend == BackendOllama {
		return "Ollama"
	}
	return "OpenAI 兼容服务"
}

// backend 当前后端（字段为空时按 BaseURL 现场判定，兼容老的构造路径）
func (s *LLMService) backend() string {
	if s == nil {
		return BackendOllama
	}
	if s.Backend != "" {
		return s.Backend
	}
	return ResolveLLMBackend(s.BaseURL, os.Getenv("LLM_PROVIDER"))
}

// BackendLabel 当前后端展示名
func (s *LLMService) BackendLabel() string { return BackendLabel(s.backend()) }

// SupportsNativeUnload openai 协议下无法从外部卸载模型。
// 这不是失败，是能力边界——调用方据此决定「等显存」还是「直接继续」。
func (s *LLMService) SupportsNativeUnload() bool { return s.backend() == BackendOllama }

// ErrUnloadUnsupported 后端不支持外部卸载（调用方应视为「无需卸载」而非故障）
type unloadUnsupportedError struct{ backend string }

func (e unloadUnsupportedError) Error() string {
	return BackendLabel(e.backend) + " 不支持从外部卸载模型（显存由该后端自行管理）"
}

// IsUnloadUnsupported 判断错误是否为「后端不支持卸载」
func IsUnloadUnsupported(err error) bool {
	_, ok := err.(unloadUnsupportedError)
	return ok
}

// streamBackend 流式服务同样按后端判定（错误文案用）
