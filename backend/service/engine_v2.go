package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"flowedge/backend/model"
)

// EngineV2 增强版工作流引擎
type EngineV2 struct {
	llm       *LLMService
	t2i       *Text2ImageService
	asr       *ASRService
	tts       *TTSService
	video     *VideoService
	http      *HTTPService
	rag       *RAGService
	gpuSched  *GPUScheduler
	wsHub     *WSHub
}

func NewEngineV2(
	llm *LLMService,
	t2i *Text2ImageService,
	asr *ASRService,
	tts *TTSService,
	video *VideoService,
	http *HTTPService,
	rag *RAGService,
	gpuSched *GPUScheduler,
	wsHub *WSHub,
) *EngineV2 {
	return &EngineV2{
		llm:      llm,
		t2i:      t2i,
		asr:      asr,
		tts:      tts,
		video:    video,
		http:     http,
		rag:      rag,
		gpuSched: gpuSched,
		wsHub:    wsHub,
	}
}

// Execute 执行工作流（按层并行 DAG + 按边传递输入 + 分支级联跳过 + 节点超时）
func (e *EngineV2) Execute(nodesJSON, edgesJSON []byte) ([]model.NodeInstance, error) {
	var nodes []model.NodeInstance
	var edges []model.EdgeInstance

	if err := json.Unmarshal(nodesJSON, &nodes); err != nil {
		return nil, fmt.Errorf("parse nodes failed: %w", err)
	}
	if err := json.Unmarshal(edgesJSON, &edges); err != nil {
		return nil, fmt.Errorf("parse edges failed: %w", err)
	}

	// 构建邻接表
	adj := make(map[string][]model.EdgeInstance)
	nodeMap := make(map[string]*model.NodeInstance)
	order := make([]string, 0, len(nodes))
	remaining := make(map[string]int)            // 该节点尚未完成的前驱数
	nodeInputs := make(map[string]map[string]interface{}) // nodeID -> {sourceID: output}

	for i := range nodes {
		nodes[i].Status = "pending"
		nodeMap[nodes[i].ID] = &nodes[i]
		order = append(order, nodes[i].ID)
		remaining[nodes[i].ID] = 0
		nodeInputs[nodes[i].ID] = make(map[string]interface{})
	}
	for _, edge := range edges {
		adj[edge.Source] = append(adj[edge.Source], edge)
		remaining[edge.Target]++
	}

	var mu sync.Mutex
	var wfErr error
	var errOnce sync.Once
	markFailed := func(id, msg string) {
		errOnce.Do(func() { wfErr = fmt.Errorf("node %s (%s) failed: %s", id, nodeMap[id].Type, msg) })
	}

	// skipBFS：从被跳过的节点出发，级联标记整条未激活分支为 skipped
	skipBFS := func(startID string) {
		queue := []string{startID}
		seen := map[string]bool{}
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			if seen[id] {
				continue
			}
			seen[id] = true
			n := nodeMap[id]
			if n.Status == "success" || n.Status == "failed" {
				continue
			}
			n.Status = "skipped"
			for _, eg := range adj[id] {
				queue = append(queue, eg.Target)
			}
		}
	}

	// 初始层：没有任何前驱的节点
	current := []string{}
	for _, id := range order {
		if remaining[id] == 0 {
			current = append(current, id)
		}
	}

	const nodeTimeout = 180 * time.Second

	for len(current) > 0 {
		var wg sync.WaitGroup
		next := []string{}
		inNext := map[string]bool{}
		layerFailed := false
		var layerMu sync.Mutex

		// 同一层内所有节点并行执行
		for _, id := range current {
			wg.Add(1)
			go func(nid string) {
				defer wg.Done()

				mu.Lock()
				node := nodeMap[nid]
				node.Status = "running"
				now := time.Now()
				node.StartedAt = &now
				// 快照输入：只来自与本节点入边相连的前驱
				upstream := make(map[string]interface{})
				for k, v := range nodeInputs[nid] {
					upstream[k] = v
				}
				mu.Unlock()

				if e.wsHub != nil {
					e.wsHub.Broadcast(Event{Type: "node_started", NodeID: nid, Data: map[string]interface{}{"type": node.Type}})
				}

				// 带超时执行节点（防止单个节点卡死整个引擎）
				type execResult struct {
					out interface{}
					err error
				}
				resCh := make(chan execResult, 1)
				go func() {
					out, err := e.executeNode(node, upstream)
					resCh <- execResult{out, err}
				}()

				var out interface{}
				var execErr error
				select {
				case res := <-resCh:
					out, execErr = res.out, res.err
				case <-time.After(nodeTimeout):
					execErr = fmt.Errorf("node timeout after %s", nodeTimeout)
				}

				mu.Lock()
				endNow := time.Now()
				node.EndedAt = &endNow

				if execErr != nil {
					node.Status = "failed"
					node.Error = execErr.Error()
					layerMu.Lock()
					layerFailed = true
					layerMu.Unlock()
					markFailed(nid, execErr.Error())
					mu.Unlock()
					if e.wsHub != nil {
						e.wsHub.Broadcast(Event{Type: "node_failed", NodeID: nid, Message: execErr.Error()})
					}
					return
				}

				node.Status = "success"
				node.Output = out
				mu.Unlock()
				if e.wsHub != nil {
					e.wsHub.Broadcast(Event{Type: "node_finished", NodeID: nid, Data: map[string]interface{}{"output": out}})
				}

				// 处理出边
				mu.Lock()
				outEdges := adj[nid]
				if node.Type == "if_else" {
					branch, _ := out.(map[string]interface{})
					branchPath, _ := branch["branch"].(string)
					for _, eg := range outEdges {
						label := eg.SourceHandle
						if label == "" {
							label = "true"
						}
						if branchPath == label {
							nodeInputs[eg.Target][nid] = out
							remaining[eg.Target]--
							if remaining[eg.Target] == 0 && nodeMap[eg.Target].Status == "pending" && !inNext[eg.Target] {
								inNext[eg.Target] = true
								next = append(next, eg.Target)
							}
						} else {
							skipBFS(eg.Target)
						}
					}
				} else {
					for _, eg := range outEdges {
						nodeInputs[eg.Target][nid] = out
						remaining[eg.Target]--
						if remaining[eg.Target] == 0 && nodeMap[eg.Target].Status == "pending" && !inNext[eg.Target] {
							inNext[eg.Target] = true
							next = append(next, eg.Target)
						}
					}
				}
				mu.Unlock()
			}(id)
		}

		wg.Wait()

		if layerFailed {
			for _, id := range order {
				if nodeMap[id].Status == "pending" {
					nodeMap[id].Status = "skipped"
				}
			}
			return nodes, wfErr
		}

		current = next
	}

	return nodes, nil
}

// executeNode 执行单个节点（支持所有节点类型）
func (e *EngineV2) executeNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	switch node.Type {
	case "start":
		return e.execStartNode(node, upstream)

	case "llm":
		return e.execLLMNode(node, upstream)

	case "text2image":
		return e.execText2ImageNode(node, upstream)

	case "asr":
		return e.execASRNode(node, upstream)

	case "tts":
		return e.execTTSNode(node, upstream)

	case "video_compose":
		return e.execVideoNode(node, upstream)

	case "if_else":
		return e.execIfElseNode(node, upstream)

	case "http_request":
		return e.execHTTPNode(node, upstream)

	case "data_transform":
		return e.execTransformNode(node, upstream)

	case "rag_query":
		return e.execRAGNode(node, upstream)

	case "end":
		return e.execEndNode(node, upstream)

	default:
		return nil, fmt.Errorf("unknown node type: %s", node.Type)
	}
}

// ===== 各节点执行逻辑 =====

func (e *EngineV2) execStartNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	if prompt, ok := node.Data["prompt"].(string); ok && prompt != "" {
		return map[string]interface{}{"text": prompt}, nil
	}
	for _, out := range upstream {
		if m, ok := out.(map[string]interface{}); ok {
			if text, ok := m["text"].(string); ok {
				return map[string]interface{}{"text": text}, nil
			}
		}
	}
	return map[string]interface{}{"text": ""}, nil
}

func (e *EngineV2) execLLMNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	prompt := ""
	if p, ok := node.Data["prompt"].(string); ok {
		prompt = p
	}

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

	// 节点级模型覆盖（空 = 使用服务级默认模型）
	reqModel, _ := node.Data["model"].(string)

	// 显存调度：请求加载 LLM
	if e.gpuSched != nil {
		schedName := reqModel
		if schedName == "" {
			schedName = "qwen2.5:7b"
		}
		_ = e.gpuSched.AcquireModel(schedName)
		defer e.gpuSched.ReleaseModel(schedName)
	}

	result, err := e.llm.ChatWithModel(prompt, reqModel)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{"text": result}, nil
}

func (e *EngineV2) execText2ImageNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	prompt := ""
	if p, ok := node.Data["prompt"].(string); ok {
		prompt = p
	}

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
		return nil, fmt.Errorf("text2image node has no prompt")
	}

	width := 1024
	height := 1024
	if w, ok := node.Data["width"].(float64); ok {
		width = int(w)
	}
	if h, ok := node.Data["height"].(float64); ok {
		height = int(h)
	}

	imagePath, err := e.t2i.GenerateImage(prompt, width, height)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"text":  prompt,
		"image": imagePath,
	}, nil
}

func (e *EngineV2) execASRNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	audioPath := ""
	if p, ok := node.Data["audio_path"].(string); ok {
		audioPath = p
	}

	if audioPath == "" {
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if audio, ok := m["audio"].(string); ok {
					audioPath = audio
				}
			}
		}
	}

	if audioPath == "" {
		return nil, fmt.Errorf("asr node has no audio input")
	}

	text, err := e.asr.Transcribe(audioPath)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{"text": text}, nil
}

func (e *EngineV2) execTTSNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	text := ""
	if p, ok := node.Data["text"].(string); ok {
		text = p
	}

	if text == "" {
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					text = t
				}
			}
		}
	}

	if text == "" {
		return nil, fmt.Errorf("tts node has no text input")
	}

	outputPath := "/tmp/output.wav"
	if p, ok := node.Data["output_path"].(string); ok {
		outputPath = p
	}

	err := e.tts.Synthesize(text, outputPath)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"text":  text,
		"audio": outputPath,
	}, nil
}

func (e *EngineV2) execVideoNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	var images []string
	audioPath := ""

	for _, out := range upstream {
		if m, ok := out.(map[string]interface{}); ok {
			if img, ok := m["image"].(string); ok {
				images = append(images, img)
			}
			if audio, ok := m["audio"].(string); ok {
				audioPath = audio
			}
		}
	}

	if len(images) == 0 {
		return nil, fmt.Errorf("video compose node has no image input")
	}

	outputPath := "/tmp/output.mp4"
	if p, ok := node.Data["output_path"].(string); ok {
		outputPath = p
	}

	err := e.video.ComposeVideo(images, audioPath, outputPath)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"video": outputPath,
	}, nil
}

func (e *EngineV2) execIfElseNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	// 获取上游文本
	inputText := ""
	for _, out := range upstream {
		if m, ok := out.(map[string]interface{}); ok {
			if text, ok := m["text"].(string); ok {
				inputText = text
			}
		}
	}

	// 条件配置
	conditionType, _ := node.Data["condition_type"].(string)
	compareValue, _ := node.Data["compare_value"].(string)

	branch := "false"

	switch conditionType {
	case "contains":
		if strings.Contains(inputText, compareValue) {
			branch = "true"
		}
	case "not_empty":
		if inputText != "" {
			branch = "true"
		}
	case "equals":
		if inputText == compareValue {
			branch = "true"
		}
	}

	return map[string]interface{}{
		"text":   inputText,
		"branch": branch,
	}, nil
}

func (e *EngineV2) execEndNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	for _, out := range upstream {
		return out, nil
	}
	return map[string]interface{}{"text": "done"}, nil
}

func (e *EngineV2) execHTTPNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	method, _ := node.Data["method"].(string)
	if method == "" {
		method = "GET"
	}

	url, _ := node.Data["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("http node has no url")
	}

	body, _ := node.Data["body"].(string)
	headers := make(map[string]string)
	if h, ok := node.Data["headers"].(map[string]interface{}); ok {
		for k, v := range h {
			headers[k] = fmt.Sprintf("%v", v)
		}
	}

	respBody, statusCode, err := e.http.HTTPRequest(method, url, headers, body)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"text":       respBody,
		"status_code": statusCode,
	}, nil
}

func (e *EngineV2) execTransformNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	// 获取上游文本
	inputText := ""
	for _, out := range upstream {
		if m, ok := out.(map[string]interface{}); ok {
			if text, ok := m["text"].(string); ok {
				inputText = text
			}
		}
	}

	template, _ := node.Data["template"].(string)
	if template == "" {
		template = inputText
	}

	result, err := e.http.TransformJSON(inputText, template)
	if err != nil {
		// 简单回退：直接返回
		result = template
	}

	return map[string]interface{}{"text": result}, nil
}

func (e *EngineV2) execRAGNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	// 获取查询文本
	query := ""
	if q, ok := node.Data["query"].(string); ok {
		query = q
	}

	if query == "" {
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if text, ok := m["text"].(string); ok {
					query = text
				}
			}
		}
	}

	if query == "" {
		return nil, fmt.Errorf("rag node has no query")
	}

	// 检索知识库
	context, err := e.rag.GetContext(query)
	if err != nil {
		return nil, err
	}

	// 拼接上下文和查询
	fullPrompt := query
	if context != "" {
		fullPrompt = fmt.Sprintf("参考以下资料回答问题：\n\n%s\n\n问题：%s", context, query)
	}

	return map[string]interface{}{
		"text":    fullPrompt,
		"context": context,
	}, nil
}
