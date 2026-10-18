package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func replacePlaceholders(template string, data interface{}) string {
	// 简单实现：递归查找字段
	// 实际生产中应该用模板引擎
	return template
}
