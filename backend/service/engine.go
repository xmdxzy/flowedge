package service

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"flowedge/backend/model"
)

// Engine 工作流执行引擎
type Engine struct {
	llm *LLMService
	mu  sync.Mutex
}

func NewEngine(llm *LLMService) *Engine {
	return &Engine{llm: llm}
}

// Execute 执行工作流，返回所有节点的最终状态
func (e *Engine) Execute(nodesJSON, edgesJSON []byte) ([]model.NodeInstance, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// 解析节点和边
	var nodes []model.NodeInstance
	var edges []model.EdgeInstance

	if err := json.Unmarshal(nodesJSON, &nodes); err != nil {
		return nil, fmt.Errorf("parse nodes failed: %w", err)
	}
	if err := json.Unmarshal(edgesJSON, &edges); err != nil {
		return nil, fmt.Errorf("parse edges failed: %w", err)
	}

	// 构建邻接表和入度表
	adj := make(map[string][]string)  // source -> targets
	inDegree := make(map[string]int)   // nodeID -> in-degree
	nodeMap := make(map[string]*model.NodeInstance)

	for i := range nodes {
		nodeMap[nodes[i].ID] = &nodes[i]
		nodes[i].Status = "pending"
		inDegree[nodes[i].ID] = 0
	}

	for _, edge := range edges {
		adj[edge.Source] = append(adj[edge.Source], edge.Target)
		inDegree[edge.Target]++
	}

	// 拓扑排序执行（Kahn算法）
	queue := []string{}
	for id, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, id)
		}
	}

	// 收集上游输出，传递给下游
	upstreamOutputs := make(map[string]interface{})

	for len(queue) > 0 {
		currentID := queue[0]
		queue = queue[1:]

		node := nodeMap[currentID]
		now := time.Now()
		node.Status = "running"
		node.StartedAt = &now

		// 执行节点逻辑
		output, err := e.executeNode(node, upstreamOutputs)
		if err != nil {
			node.Status = "failed"
			node.Error = err.Error()
			endNow := time.Now()
			node.EndedAt = &endNow
			return nodes, fmt.Errorf("node %s failed: %w", node.ID, err)
		}

		endNow := time.Now()
		node.Status = "success"
		node.Output = output
		node.EndedAt = &endNow
		upstreamOutputs[currentID] = output

		// 减少下游入度
		for _, targetID := range adj[currentID] {
			inDegree[targetID]--
			if inDegree[targetID] == 0 {
				queue = append(queue, targetID)
			}
		}
	}

	return nodes, nil
}

// executeNode 执行单个节点
func (e *Engine) executeNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	switch node.Type {
	case "start":
		// 开始节点：返回输入文本
		if prompt, ok := node.Data["prompt"].(string); ok && prompt != "" {
			return map[string]interface{}{
				"text": prompt,
			}, nil
		}
		// 从上游获取
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if text, ok := m["text"].(string); ok {
					return map[string]interface{}{"text": text}, nil
				}
			}
		}
		return map[string]interface{}{"text": ""}, nil

	case "llm":
		// LLM节点：调用大模型
		prompt := ""
		if p, ok := node.Data["prompt"].(string); ok {
			prompt = p
		}

		// 如果模板包含 {{input}}，替换为上游输出
		if prompt == "" {
			for _, out := range upstream {
				if m, ok := out.(map[string]interface{}); ok {
					if text, ok := m["text"].(string); ok {
						prompt = text
					}
				}
			}
		}

		if prompt == "" {
			return nil, fmt.Errorf("LLM node has no input prompt")
		}

		result, err := e.llm.Chat(prompt)
		if err != nil {
			return nil, err
		}

		return map[string]interface{}{
			"text": result,
		}, nil

	case "end":
		// 结束节点：汇总输出
		for _, out := range upstream {
			return out, nil
		}
		return map[string]interface{}{"text": "done"}, nil

	default:
		return nil, fmt.Errorf("unknown node type: %s", node.Type)
	}
}
