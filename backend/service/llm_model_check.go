package service

import (
	"encoding/json"
	"strings"
)

// CheckOllamaModel 核对某个模型名是否在本机 Ollama 里。
//
// 为什么需要它：OLLAMA_MODEL 指向一个本机没有的模型时，引擎每条 LLM 调用都会
// 以「404 model not found」失败——而 404 会被误读成"服务没起/端口错了"，
// 排查方向一开始就跑偏。启动时说一句，比跑到第一个节点才炸要好。
//
// 返回 (missing, available, err)：err != nil 表示 Ollama 不可达（此时既不能说
// 缺也不能说在，调用方应保持沉默而不是误报）。
func CheckOllamaModel(baseURL, model string) (bool, string, error) {
	body, err := httpGetBody(strings.TrimRight(baseURL, "/") + "/api/tags")
	if err != nil {
		return false, "", err
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &tags); err != nil {
		return false, "", err
	}
	names := make([]string, 0, len(tags.Models))
	for _, m := range tags.Models {
		names = append(names, m.Name)
	}
	if len(names) == 0 {
		return model != "", "（本机 Ollama 一个模型都没有）", nil
	}
	want := strings.TrimSpace(model)
	for _, n := range names {
		// Ollama 的 tags 里可能带 :latest 后缀，而配置里常写裸名
		if n == want || strings.TrimSuffix(n, ":latest") == want {
			return false, strings.Join(names, ", "), nil
		}
	}
	return true, strings.Join(names, ", "), nil
}
