package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"flowedge/backend/model"
)

// 保存期节点参数校验。
//
// 背景（2026-10-20 执行记录审计）：`text2image node has no prompt`、
// `超分缺少输入图` 这类失败，工作流在创建/更新时就是必然失败的——
// 缺 prompt、缺输入图的图能保存、能点运行，然后必然炸。
// 校验下沉到保存层：这类图根本不应该存进库。
//
// 与执行器的分工：执行器错误信息保持不变（运行期还要防上游产出为空等
// 保存期看不到的情况），这里只拦「静态可判定」的必填缺失。

// NodeParamIssue 一个节点参数问题
type NodeParamIssue struct {
	NodeID   string `json:"node_id"`
	NodeType string `json:"node_type"`
	Field    string `json:"field"`
	Message  string `json:"message"`
}

// ValidateNodeParams 按节点类型核对静态必填项。
// hasInbound: nodeID -> 是否有入边（上游可能动态提供输入，有入边即放行）。
// 返回的每个 issue 都是保存期就必须修的硬错误。
func ValidateNodeParams(nodes []model.NodeInstance, hasInbound map[string]bool) []NodeParamIssue {
	var issues []NodeParamIssue
	nonEmpty := func(v interface{}) bool {
		s, ok := v.(string)
		return ok && strings.TrimSpace(s) != ""
	}
	for i := range nodes {
		n := &nodes[i]
		switch n.Type {
		case "text2image":
			// 提示词可以来自两处：节点自带的 prompt / prompts（老工作流与 API 调用），
			// 或者上游文本线（UI 现在只有这一条路）。
			// 有入边就放行——运行期真的会从上游取 text（engine_v2_nodes.go 的 upstream 循环）。
			if !nonEmpty(n.Data["prompt"]) && len(splitPromptLines(n.Data["prompts"])) == 0 && !hasInbound[n.ID] {
				issues = append(issues, NodeParamIssue{n.ID, n.Type, "prompt",
					"文生图缺少提示词：请连一条文本线到本节点（提示词只来自上游）"})
			}
		case "image2image", "upscale":
			// 输入图可来自 image_path 字段或上游连线
			if !nonEmpty(n.Data["image_path"]) && !hasInbound[n.ID] {
				issues = append(issues, NodeParamIssue{n.ID, n.Type, "image_path",
					"缺少输入图：连一条上游图片线，或填写图片路径（image_path）"})
			}
		case "inpaint":
			if !nonEmpty(n.Data["image_path"]) && !hasInbound[n.ID] {
				issues = append(issues, NodeParamIssue{n.ID, n.Type, "image_path",
					"重绘缺少原图：连一条上游图片线，或填写图片路径（image_path）"})
			}
			if !nonEmpty(n.Data["mask_path"]) {
				issues = append(issues, NodeParamIssue{n.ID, n.Type, "mask_path",
					"重绘缺少蒙版：请填 mask_path（白色区域会被重画）"})
			}
			if !nonEmpty(n.Data["prompt"]) && !hasInbound[n.ID] {
				issues = append(issues, NodeParamIssue{n.ID, n.Type, "prompt",
					"重绘缺少提示词：蒙版区域要画成什么"})
			}
		case "text2video":
			if !nonEmpty(n.Data["prompt"]) && !hasInbound[n.ID] {
				issues = append(issues, NodeParamIssue{n.ID, n.Type, "prompt",
					"文生视频缺少提示词：填 prompt，或连一条上游文本线"})
			}
		case "llm":
			if !nonEmpty(n.Data["prompt"]) && !hasInbound[n.ID] {
				issues = append(issues, NodeParamIssue{n.ID, n.Type, "prompt",
					"LLM 缺少输入：填 prompt 作为指令，或连一条上游文本线"})
			}
		case "tts":
			if !nonEmpty(n.Data["text"]) && !hasInbound[n.ID] {
				issues = append(issues, NodeParamIssue{n.ID, n.Type, "text",
					"配音缺少文本：填 text，或连一条上游文本线"})
			}
		case "asr":
			if !nonEmpty(n.Data["audio_path"]) && !hasInbound[n.ID] {
				issues = append(issues, NodeParamIssue{n.ID, n.Type, "audio_path",
					"转写缺少音频：填 audio_path，或连一条上游音频线"})
			}
		case "comfy":
			// 图的两个来源（模板名 / 内联 JSON）都必须非空才跑得起来；
			// 这一项与端口无关（comfy 没有必填端口），只能在这里拦。
			if !nonEmpty(n.Data["template"]) && !nonEmpty(n.Data["workflow_json"]) {
				issues = append(issues, NodeParamIssue{n.ID, n.Type, "template",
					"comfy 节点缺图：填模板名（template，JSON 放 data/comfy_workflows/）或内联 JSON（workflow_json）"})
			}
		}
	}
	return issues
}

// ParseEdges 解析 edges JSON（组装流水线后校验用）。
func ParseEdges(edgesJSON []byte) ([]model.EdgeInstance, error) {
	var edges []model.EdgeInstance
	if len(edgesJSON) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(edgesJSON, &edges); err != nil {
		return nil, fmt.Errorf("parse edges failed: %w", err)
	}
	return edges, nil
}

// InboundSet 从边列表推导「哪些节点有入边」
func InboundSet(edges []model.EdgeInstance) map[string]bool {
	m := make(map[string]bool, len(edges))
	for _, e := range edges {
		m[e.Target] = true
	}
	return m
}

// ParseNodeInstances 解析 nodes JSON（保存期校验入口）
func ParseNodeInstances(nodesJSON []byte) ([]model.NodeInstance, error) {
	var nodes []model.NodeInstance
	if err := json.Unmarshal(nodesJSON, &nodes); err != nil {
		return nil, fmt.Errorf("parse nodes failed: %w", err)
	}
	return nodes, nil
}
