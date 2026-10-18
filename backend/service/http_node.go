package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// HTTPService HTTP 请求节点服务
type HTTPService struct {
	client *http.Client
}

func NewHTTPService() *HTTPService {
	return &HTTPService{
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// HTTPRequest 执行 HTTP 请求
func (s *HTTPService) HTTPRequest(method, url string, headers map[string]string, body string) (string, int, error) {
	var reqBody io.Reader
	if body != "" {
		reqBody = bytes.NewBufferString(body)
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return "", 0, fmt.Errorf("create request failed: %w", err)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode, fmt.Errorf("read response failed: %w", err)
	}

	return string(respBody), resp.StatusCode, nil
}

// TransformJSON 数据转换
func (s *HTTPService) TransformJSON(input string, template string) (string, error) {
	var data interface{}
	if err := json.Unmarshal([]byte(input), &data); err != nil {
		return "", fmt.Errorf("parse input JSON failed: %w", err)
	}

	// 简单的模板替换：{{field}}
	result := template
	result = replacePlaceholders(result, data)

	return result, nil
}

// replacePlaceholders 模板占位符替换
// 支持 {{input}} / {{text}}（原始输入）与 {{field}} / {{a.b}}（输入为 JSON 时取字段）
func replacePlaceholders(template string, data interface{}) string {
	var obj map[string]interface{}
	if m, ok := data.(map[string]interface{}); ok {
		obj = m
	}

	// 原始输入：JSON 对象场景下由调用方塞在 "input" 键里，避免打印成 map[...]
	input := ""
	if obj != nil {
		if s, ok := obj["input"].(string); ok {
			input = s
		}
	}
	if input == "" {
		input = fmt.Sprintf("%v", data)
	}

	return placeholderRe.ReplaceAllStringFunc(template, func(match string) string {
		key := strings.TrimSpace(match[2 : len(match)-2])
		switch key {
		case "input", "text":
			return input
		}
		if obj == nil {
			return match
		}
		return lookupPath(obj, key)
	})
}

// placeholderRe 匹配 {{...}} 占位符
var placeholderRe = regexp.MustCompile(`\{\{[^{}]+\}\}`)

// lookupPath 按点号路径取值，缺失时保留占位符原文
func lookupPath(obj map[string]interface{}, path string) string {
	var cur interface{} = obj
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return "{{" + path + "}}"
		}
		cur, ok = m[seg]
		if !ok {
			return "{{" + path + "}}"
		}
	}
	switch v := cur.(type) {
	case string:
		return v
	case nil:
		return ""
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}
