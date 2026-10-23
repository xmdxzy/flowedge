package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// 资源准入闸：跑重活之前先问一句「现在能不能跑」。
//
// 为什么要有
//
//	FlowEdge 能真实把机器打死：一张图里十个重节点、ComfyUI 队列堆到几十、
//	LLM 循环空转，都不需要写错就能发生。原来只有 maxLoopIterations=100 和
//	显存 LRU 两层，它们都是**运行内计数**，看不见机器。这里补的是**执行前**的
//	机器视角准入。
//
// 为什么复用序策的治理器而不是在 Go 里重写一份
//
//	序策的 resource-governor 已经做了 1s 采样、压力分级防抖、GPU 预留、
//	权重配额、死循环检测（1457 行 / 73 断言）。在 Go 里再写一份 = 两套阈值、
//	两套判据，迟早互相打脸。这里只做**准入询问**，不做执法 ——
//	执法要杀进程，两边都有执法权就是双杀风险。
//
// 为什么 fail-open（问不到就放行）
//
//	FlowEdge 是独立可分发的产品。序策没起时拒绝出片，等于把引擎变成一个
//	必须有另一个进程才能用的东西。所以：**问不到 → 放行 + 记日志**；
//	**问到了且说不 → 尊重**。前者是我们的降级，后者是别人的判决。

// governorTimeout 预检是本地快照，正常 <10ms。3s 是给序策正忙时留的余量，
// 再慢就该放行而不是让出片卡在准入上。
const governorTimeout = 3 * time.Second

// GovernorVerdict 一次准入判定
type GovernorVerdict struct {
	Allowed bool     `json:"allowed"`
	Kind    string   `json:"kind"`
	Weight  int      `json:"weight"`
	Level   string   `json:"level,omitempty"`
	Advice  string   `json:"advice,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
	// Source 判定来源：governor（问到了）/ unreachable（问不到，放行）/ disabled（开关关闭）
	Source string `json:"source"`
	// Detail 不可达时记下原因，便于排查"为什么这次没拦"
	Detail string `json:"detail,omitempty"`
}

// GovernorGate 准入闸
type GovernorGate struct {
	baseURL string
	kind    string
	enabled bool
	client  *http.Client
}

// NewGovernorGate 从环境变量构造。
//
//	GOVERNOR_BASE_URL  序策治理器地址，默认 http://127.0.0.1:30002
//	GOVERNOR_KIND      权重档，默认 flowedge（序策配置里已按最重档 3 预留）
//	FLOWEDGE_GOVERNOR  off 关闭准入闸（离线单测 / 隔离环境用）
func NewGovernorGate() *GovernorGate {
	g := &GovernorGate{
		baseURL: strings.TrimRight(getEnvDefault("GOVERNOR_BASE_URL", "http://127.0.0.1:30002"), "/"),
		kind:    getEnvDefault("GOVERNOR_KIND", "flowedge"),
		enabled: !strings.EqualFold(os.Getenv("FLOWEDGE_GOVERNOR"), "off"),
		// 必须显式禁用代理：本机常驻 http_proxy=127.0.0.1:5751，
		// 走默认 Transport 会把 127.0.0.1 的请求发给代理，拿回一个假的 502，
		// 于是"序策没起"和"序策拒绝"就分不清了（AGENTS §0.6 的同款坑）。
		client: &http.Client{
			Timeout:   governorTimeout,
			Transport: &http.Transport{Proxy: nil},
		},
	}
	return g
}

// Enabled 是否启用
func (g *GovernorGate) Enabled() bool { return g != nil && g.enabled }

// Check 询问当前能否启动一次 FlowEdge 运行。
//
// 永不返回 error：准入是旁路，它坏了的后果只能是"没拦住"，
// 不可以是"出片失败"。所有异常都落成 Allowed=true + Source=unreachable。
func (g *GovernorGate) Check(ctx context.Context) GovernorVerdict {
	v := GovernorVerdict{Kind: "flowedge", Source: "disabled", Allowed: true}
	if g == nil || !g.enabled {
		return v
	}
	v.Kind = g.kind

	body, _ := json.Marshal(map[string]interface{}{"kind": g.kind})
	ctx, cancel := context.WithTimeout(ctx, governorTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		g.baseURL+"/api/flow/governor/preflight", bytes.NewReader(body))
	if err != nil {
		v.Source, v.Detail = "unreachable", err.Error()
		log.Printf("[Governor] 构造预检请求失败，放行：%v", err)
		return v
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		v.Source, v.Detail = "unreachable", err.Error()
		log.Printf("[Governor] 序策治理器不可达（%s），本次放行：%v", g.baseURL, err)
		return v
	}
	defer resp.Body.Close()

	// 只有 200 与 429 是有意义的答复。其它状态码（5xx / 代理页 / 路由不匹配）
	// 都说明对方的治理器没在正常工作，按不可达处理 —— 不能拿一个坏掉的服务
	// 去否决出片。
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTooManyRequests {
		v.Source, v.Detail = "unreachable", fmt.Sprintf("status %d", resp.StatusCode)
		log.Printf("[Governor] 预检返回异常状态 %d，本次放行", resp.StatusCode)
		return v
	}

	var r struct {
		OK           bool     `json:"ok"`
		Kind         string   `json:"kind"`
		Weight       int      `json:"weight"`
		Level        string   `json:"level"`
		EffectiveMax int      `json:"effectiveMax"`
		UsedWeight   int      `json:"usedWeight"`
		Queued       int      `json:"queued"`
		Reasons      []string `json:"reasons"`
		Advice       string   `json:"advice"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		v.Source, v.Detail = "unreachable", "decode: "+err.Error()
		log.Printf("[Governor] 预检响应无法解析，本次放行：%v", err)
		return v
	}

	v.Source = "governor"
	v.Allowed = r.OK
	v.Weight = r.Weight
	v.Level = r.Level
	v.Advice = r.Advice
	v.Reasons = r.Reasons
	if !r.OK {
		log.Printf("[Governor] 拒绝本次运行：level=%s weight=%d/%d used=%d advice=%s reasons=%v",
			r.Level, r.Weight, r.EffectiveMax, r.UsedWeight, r.Advice, r.Reasons)
	}
	return v
}

// getEnvDefault 取环境变量，空则回退默认值
func getEnvDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
