package service

import (
	"fmt"
	"sync"
)

// NodePlugin 节点插件接口
type NodePlugin interface {
	// Type 节点类型标识
	Type() string
	// Name 节点显示名称
	Name() string
	// Category 节点分类
	Category() string
	// Description 节点描述
	Description() string
	// Execute 执行节点逻辑
	Execute(input map[string]interface{}, config map[string]interface{}) (map[string]interface{}, error)
}

// PluginRegistry 插件注册中心
type PluginRegistry struct {
	plugins map[string]NodePlugin
	mu      sync.RWMutex
}

func NewPluginRegistry() *PluginRegistry {
	return &PluginRegistry{
		plugins: make(map[string]NodePlugin),
	}
}

// Register 注册插件
func (r *PluginRegistry) Register(plugin NodePlugin) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	nodeType := plugin.Type()
	if _, exists := r.plugins[nodeType]; exists {
		return fmt.Errorf("plugin %s already registered", nodeType)
	}

	r.plugins[nodeType] = plugin
	return nil
}

// Get 获取插件
func (r *PluginRegistry) Get(nodeType string) (NodePlugin, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	plugin, exists := r.plugins[nodeType]
	if !exists {
		return nil, fmt.Errorf("plugin %s not found", nodeType)
	}

	return plugin, nil
}

// ListAll 列出所有插件
func (r *PluginRegistry) ListAll() []map[string]interface{} {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]map[string]interface{}, 0, len(r.plugins))
	for _, plugin := range r.plugins {
		result = append(result, map[string]interface{}{
			"type":        plugin.Type(),
			"name":        plugin.Name(),
			"category":    plugin.Category(),
			"description": plugin.Description(),
		})
	}
	return result
}

// ExecutePlugin 执行插件节点
func (r *PluginRegistry) ExecutePlugin(nodeType string, input, config map[string]interface{}) (map[string]interface{}, error) {
	plugin, err := r.Get(nodeType)
	if err != nil {
		return nil, err
	}
	return plugin.Execute(input, config)
}

// ===== 内置插件实现 =====

// EchoPlugin 回声插件（测试用）
type EchoPlugin struct{}

func (p *EchoPlugin) Type() string        { return "echo" }
func (p *EchoPlugin) Name() string       { return "回声" }
func (p *EchoPlugin) Category() string   { return "utility" }
func (p *EchoPlugin) Description() string { return "原样返回输入内容，用于测试" }

func (p *EchoPlugin) Execute(input map[string]interface{}, config map[string]interface{}) (map[string]interface{}, error) {
	return map[string]interface{}{
		"text": input["text"],
	}, nil
}

// StringLengthPlugin 字符串长度插件
type StringLengthPlugin struct{}

func (p *StringLengthPlugin) Type() string        { return "string_length" }
func (p *StringLengthPlugin) Name() string       { return "字符串长度" }
func (p *StringLengthPlugin) Category() string   { return "utility" }
func (p *StringLengthPlugin) Description() string { return "计算输入文本的字符数" }

func (p *StringLengthPlugin) Execute(input map[string]interface{}, config map[string]interface{}) (map[string]interface{}, error) {
	text, _ := input["text"].(string)
	return map[string]interface{}{
		"text":  fmt.Sprintf("文本长度: %d 个字符", len([]rune(text))),
		"length": len([]rune(text)),
	}, nil
}

// UpperCasePlugin 大写转换插件
type UpperCasePlugin struct{}

func (p *UpperCasePlugin) Type() string        { return "uppercase" }
func (p *UpperCasePlugin) Name() string       { return "转大写" }
func (p *UpperCasePlugin) Category() string   { return "utility" }
func (p *UpperCasePlugin) Description() string { return "将文本转为大写" }

func (p *UpperCasePlugin) Execute(input map[string]interface{}, config map[string]interface{}) (map[string]interface{}, error) {
	text, _ := input["text"].(string)
	return map[string]interface{}{
		"text": toUpper(text),
	}, nil
}

func toUpper(s string) string {
	var result []rune
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			r -= 32
		}
		result = append(result, r)
	}
	return string(result)
}

// RegisterBuiltinPlugins 注册所有内置插件
func RegisterBuiltinPlugins(registry *PluginRegistry) {
	registry.Register(&EchoPlugin{})
	registry.Register(&StringLengthPlugin{})
	registry.Register(&UpperCasePlugin{})
}
