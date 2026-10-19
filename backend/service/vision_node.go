package service

import (
	"context"
	"fmt"
	"strings"

	"flowedge/backend/model"
)

// 视觉理解节点（vision）：图片 + 提问 → 结构化描述
//
// 为什么单独开一个节点而不是塞进 llm 节点：
//   - 多模态输入是「另一种能力」，需要在节点库与环境面板里可见、可按模型置灰；
//   - 视觉模型与文本模型的显存占用/加载策略不同，分开才好调度；
//   - 上游可能是文生图的输出（image 字段），一条边直接连过来即可，符合画布直觉。
//
// 配置：prompt（问什么）、model（留空自动挑本地 VL 模型）、image_path（可选，覆盖上游）
func (e *EngineV2) execVisionNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	prompt, _ := node.Data["prompt"].(string)
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = "请客观描述这张图片：主体、构图、风格、文字（如果有）。"
	}

	// 图片来自显式路径或上游 image 字段（多张去重保序）
	var images []string
	seen := map[string]bool{}
	addImage := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		images = append(images, p)
	}
	if p, ok := node.Data["image_path"].(string); ok {
		addImage(strings.TrimSpace(p))
	}
	for _, out := range upstream {
		m, ok := out.(map[string]interface{})
		if !ok {
			continue
		}
		if img, ok := m["image"].(string); ok {
			addImage(img)
		}
		// 上游是数组（如变量聚合的 values）时，逐个尝试
		if arr, ok := m["values"].([]interface{}); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok && looksLikeImagePath(s) {
					addImage(s)
				}
			}
		}
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("vision 节点没有图片输入：连一条来自文生图 / 聚合节点的线，或在配置里填图片路径")
	}

	modelName, _ := node.Data["model"].(string)
	if e.llm == nil {
		return nil, fmt.Errorf("视觉模型不可用：LLM 服务未初始化")
	}
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		modelName = e.visionModel
	}
	if modelName == "" {
		// 启动时 Ollama 可能还没起来（探测为空），这里再探一次，避免「明明装了模型却报没有」
		if m := PickVisionModel(e.llm.BaseURL); m != "" {
			modelName = m
			e.visionModel = m
		}
	}
	if modelName == "" {
		return nil, fmt.Errorf("没有可用的视觉模型：先拉一个 VL 模型（如 ollama pull qwen2.5vl:7b）")
	}

	text, usage, err := e.llm.VisionChat(prompt, images, modelName)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"text":   text,
		"model":  modelName,
		"images": len(images),
		"tokens": usage.PromptTokens + usage.CompletionTokens,
	}, nil
}

func looksLikeImagePath(s string) bool {
	lower := strings.ToLower(s)
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp", ".bmp"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}
