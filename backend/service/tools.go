package service

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ToolCall 模型请求的一次工具调用（OpenAI 兼容格式）
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ToolSpec 工具声明（发给模型的 tools 数组元素）
type ToolSpec struct {
	Type     string `json:"type"`
	Function struct {
		Name        string                 `json:"name"`
		Description string                 `json:"description"`
		Parameters  map[string]interface{} `json:"parameters"`
	} `json:"function"`
}

// ToolExecutor 内置工具执行器：不依赖外部服务，全部本地完成
type ToolExecutor struct {
	http *HTTPService
	rag  *RAGServiceV3
}

func NewToolExecutor(httpSvc *HTTPService, rag *RAGServiceV3) *ToolExecutor {
	return &ToolExecutor{http: httpSvc, rag: rag}
}

// AllToolNames 可用工具清单（前端勾选用）
func AllToolNames() []string {
	return []string{"http_get", "kb_search", "calc", "now"}
}

func toolSpec(name, desc string, params map[string]interface{}) ToolSpec {
	var spec ToolSpec
	spec.Type = "function"
	spec.Function.Name = name
	spec.Function.Description = desc
	spec.Function.Parameters = params
	return spec
}

func obj(props map[string]interface{}, required ...string) map[string]interface{} {
	if required == nil {
		required = []string{}
	}
	return map[string]interface{}{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

func strProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

// Specs 按名称列表返回工具声明（未知名称忽略）
func (t *ToolExecutor) Specs(names []string) []ToolSpec {
	specs := make([]ToolSpec, 0, len(names))
	for _, n := range names {
		switch n {
		case "http_get":
			specs = append(specs, toolSpec("http_get", "发送 HTTP GET 请求并返回响应正文，用于查询外部接口或网页数据",
				obj(map[string]interface{}{"url": strProp("完整 URL，含 http(s) 前缀")}, "url")))
		case "kb_search":
			specs = append(specs, toolSpec("kb_search", "检索本地专业知识库，返回最相关的知识片段，用于回答内部问题",
				obj(map[string]interface{}{"query": strProp("检索问题")}, "query")))
		case "calc":
			specs = append(specs, toolSpec("calc", "计算四则运算表达式（支持括号与小数），用于精确计算",
				obj(map[string]interface{}{"expression": strProp("如 (12.5+3)*4")}, "expression")))
		case "now":
			specs = append(specs, toolSpec("now", "获取当前日期时间（本地时区）",
				obj(map[string]interface{}{})))
		}
	}
	return specs
}

// Run 执行一次工具调用，返回给模型的文本结果
func (t *ToolExecutor) Run(name, argsJSON string) (string, error) {
	args := map[string]interface{}{}
	if strings.TrimSpace(argsJSON) != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("invalid tool arguments: %w", err)
		}
	}

	str := func(k string) string {
		if v, ok := args[k].(string); ok {
			return v
		}
		return ""
	}

	switch name {
	case "http_get":
		url := str("url")
		if url == "" {
			return "", fmt.Errorf("http_get requires url")
		}
		if t.http == nil {
			return "", fmt.Errorf("http service unavailable")
		}
		body, code, err := t.http.HTTPRequest("GET", url, nil, "")
		if err != nil {
			return "", err
		}
		return truncateRunes(fmt.Sprintf("HTTP %d\n%s", code, body), 4000), nil

	case "kb_search":
		query := str("query")
		if query == "" {
			return "", fmt.Errorf("kb_search requires query")
		}
		if t.rag == nil {
			return "", fmt.Errorf("knowledge base unavailable")
		}
		results, err := t.rag.Query(query, 3)
		if err != nil {
			return "", err
		}
		if len(results) == 0 {
			return "（知识库没有匹配内容）", nil
		}
		var sb strings.Builder
		for i, r := range results {
			fmt.Fprintf(&sb, "[%d] %s（相似度 %.3f）\n%s\n", i+1, r.Record.Source, r.Score, r.Record.Content)
		}
		return truncateRunes(sb.String(), 4000), nil

	case "calc":
		expr := str("expression")
		if expr == "" {
			return "", fmt.Errorf("calc requires expression")
		}
		v, err := EvalExpression(expr)
		if err != nil {
			return "", err
		}
		return strconv.FormatFloat(v, 'f', -1, 64), nil

	case "now":
		return time.Now().Format("2006-01-02 15:04:05 MST"), nil
	}
	return "", fmt.Errorf("unknown tool: %s", name)
}

// EvalExpression 四则运算求值（递归下降，不使用 eval，无注入风险）
func EvalExpression(expr string) (float64, error) {
	p := &exprParser{src: []rune(strings.ReplaceAll(expr, " ", ""))}
	v, err := p.parseExpr()
	if err != nil {
		return 0, err
	}
	if p.pos != len(p.src) {
		return 0, fmt.Errorf("unexpected character at %d", p.pos)
	}
	return v, nil
}

type exprParser struct {
	src []rune
	pos int
}

func (p *exprParser) peek() (rune, bool) {
	if p.pos >= len(p.src) {
		return 0, false
	}
	return p.src[p.pos], true
}

func (p *exprParser) parseExpr() (float64, error) {
	left, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		c, ok := p.peek()
		if !ok || (c != '+' && c != '-') {
			return left, nil
		}
		p.pos++
		right, err := p.parseTerm()
		if err != nil {
			return 0, err
		}
		if c == '+' {
			left += right
		} else {
			left -= right
		}
	}
}

func (p *exprParser) parseTerm() (float64, error) {
	left, err := p.parseFactor()
	if err != nil {
		return 0, err
	}
	for {
		c, ok := p.peek()
		if !ok || (c != '*' && c != '/') {
			return left, nil
		}
		p.pos++
		right, err := p.parseFactor()
		if err != nil {
			return 0, err
		}
		if c == '*' {
			left *= right
		} else {
			if right == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			left /= right
		}
	}
}

func (p *exprParser) parseFactor() (float64, error) {
	c, ok := p.peek()
	if !ok {
		return 0, fmt.Errorf("unexpected end of expression")
	}
	if c == '(' {
		p.pos++
		v, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		if c2, ok2 := p.peek(); !ok2 || c2 != ')' {
			return 0, fmt.Errorf("missing closing parenthesis")
		}
		p.pos++
		return v, nil
	}
	if c == '-' {
		p.pos++
		v, err := p.parseFactor()
		return -v, err
	}
	start := p.pos
	for {
		c, ok := p.peek()
		if !ok || (c < '0' || c > '9') && c != '.' {
			break
		}
		p.pos++
	}
	if start == p.pos {
		return 0, fmt.Errorf("expected number at %d", start)
	}
	return strconv.ParseFloat(string(p.src[start:p.pos]), 64)
}
