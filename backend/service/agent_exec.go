package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"flowedge/backend/model"
)

// agent_task 节点：把一步活儿交给外部智能体做。
//
// 与确定性模块的界线（这条界线必须写在代码里，不能只写在文档里）：
//
//	确定性模块 —— 同一个输入、同一份权重、同一个 seed，产出可复现。
//	agent_task —— 不承诺可复现。同 prompt 两次结果可能不同。
//
// 所以 agent_task 的契约里带 Nondeterministic=true，任务书会显式标注，
// 评测时必须单独分组（否则「可复现」这个前提被悄悄破坏，而测试还是绿的）。
//
// 它能做什么 / 不能做什么：
//   能：写文案、改代码、查资料、给审片意见 —— 需要判断的活儿
//   不能：出图、转写、合成、验收 —— 那些有确定性模块，别用 agent 去替代（更慢更贵更不稳）

// ===== 并发配额 =====

// agentMaxConcurrency 同时最多几个外部智能体在跑。默认 2：一个 Claude Code 起来
// 就是几百 MB 到几 GB 的内存，加它自己 spawn 的子进程，3 个以上在 24G 机器上很危险。
// AGENT_MAX_CONCURRENCY 可覆盖（设 1 = 完全串行）。
func agentMaxConcurrency() int {
	n := 2
	if v := strings.TrimSpace(os.Getenv("AGENT_MAX_CONCURRENCY")); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 {
			n = x
		}
	}
	if n > 16 {
		n = 16
	}
	return n
}

// agentSlots 懒初始化配额池（首次执行时才建，不拖慢启动）
func (e *EngineV2) agentSlots() chan struct{} {
	e.agentGateOnce.Do(func() {
		e.agentGate = make(chan struct{}, agentMaxConcurrency())
	})
	return e.agentGate
}

// agentPool 一次运行内的智能体并发池。
//
// 为什么按运行隔离，而不是继续用引擎级单例：配额是运行级参数。引擎级池会让
// **两次并发 run 互相抢名额**，也会让「按机器能力调高并发」变成改环境变量+重启。
// 放进 context 后，池的生命周期天然等于一次运行，无需清理，也不引入全局锁。
type agentPool struct {
	slots chan struct{}
	size  int
}

type ctxKeyAgentPool struct{}

// withAgentPool 给本次运行装上专属配额池；size<=0 表示不注入（走引擎默认池）
func withAgentPool(ctx context.Context, size int) context.Context {
	if size <= 0 {
		return ctx
	}
	if size > 16 {
		size = 16
	}
	return context.WithValue(ctx, ctxKeyAgentPool{}, &agentPool{
		slots: make(chan struct{}, size),
		size:  size,
	})
}

func agentPoolFrom(ctx context.Context) *agentPool {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(ctxKeyAgentPool{}).(*agentPool); ok {
		return v
	}
	return nil
}

// acquireAgentSlot 申请一个并发名额，返回**实际使用的池**（释放时必须交回同一个）。
// 返回池而不是让 Release 再去猜，是因为 ctx 可能在 defer 之前就被取消/替换，
// 猜错的后果是漏释放名额——表现为「第二次运行凭空少一个并发位」。
func (e *EngineV2) acquireAgentSlot(ctx context.Context) (*agentPool, error) {
	pool := agentPoolFrom(ctx)
	if pool == nil {
		e.agentGateOnce.Do(func() {
			e.agentGate = make(chan struct{}, agentMaxConcurrency())
		})
		pool = &agentPool{slots: e.agentGate, size: cap(e.agentGate)}
	}
	select {
	case pool.slots <- struct{}{}:
		return pool, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("等智能体并发配额超时（本次运行上限 %d 个，可用 run 参数 agent_concurrency 或环境变量 AGENT_MAX_CONCURRENCY 调整）：先让当前这批跑完，或把图里的 agent_task 改成串行",
			pool.size)
	}
}

func (p *agentPool) release() {
	if p == nil {
		return
	}
	select {
	case <-p.slots:
	default:
	}
}

// ===== 进程树回收 =====

// killAgentTree 杀掉子进程**树**。
//
// 为什么需要：agent 自己会 spawn 子进程（node、python、编辑器、LSP…）。
// 只杀父进程会留下一堆孤儿在后台吃内存，而且下次执行还会撞上它们。
//
// 为什么用 taskkill 而不是 Job Object：Job Object 要 golang.org/x/sys/windows，
// 那是新增依赖；taskkill /T 是系统自带，效果等价。
//
// 边界（必须写清）：只杀**本次 agent_task 自己 spawn 的进程树**（pid 来自 cmd.Process），
// 不碰任何用户进程。这条与「禁杀用户进程」不冲突 —— 它是我们自己起的子进程，
// 属 AGENTS §1.2 明确允许的范围。
func killAgentTree(pid int) {
	if pid <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	if err := cmd.Run(); err != nil {
		log.Printf("[Agent] 回收进程树 pid=%d 失败（可能已自行退出）：%v", pid, err)
		return
	}
	log.Printf("[Agent] 超时回收进程树 pid=%d 完成", pid)
}

// 输出上限：agent 可能一次吐几十万字符（贴了整个文件、或者在刷屏）。
// 不设上限会让一次 run 把内存和事件日志一起吃穿。
const agentOutputLimit = 2 << 20 // 2MB

// agentEnvWhitelist 允许透传给子进程的环境变量。
//
// 刻意**不含** http_proxy / https_proxy / all_proxy：本机常驻代理，
// 传给 agent 会让它所有网络请求走代理，表现为"莫名其妙连不上"。
// 需要代理的 agent 自己在 agents.json 的 env 里显式声明。
var agentEnvWhitelist = []string{
	"PATH", "PATHEXT", "COMSPEC", "SYSTEMROOT", "WINDIR",
	"TEMP", "TMP", "HOME", "USERPROFILE",
	"APPDATA", "LOCALAPPDATA", "PROGRAMFILES",
	"NODE_PATH", "PYTHONPATH", "LANG", "LC_ALL",
}

// composeAgentPrompt 把角色卡与技能包参考前置到任务描述之前。
//
// 单独抽出来是为了能直接单测：多行协议经 `cmd /c` 这类 shell wrapper 传递时，
// Windows 会在第一个换行处截断参数，用进程 stdout 断言会误判成"没注入"。
// 把组装与 I/O 分开，逻辑本身就能被稳定验证。
//
// 顺序：角色卡（怎么干）→ 参考资料（依据什么）→ 任务描述（干什么）。
func (e *EngineV2) composeAgentPrompt(node *model.NodeInstance, prompt string) string {
	pipe := ParsePipeline(node.Data)
	if pipe.Body != nil {
		if sys, ok := e.cardSystem(*pipe.Body); ok && strings.TrimSpace(sys) != "" {
			// 协议在前、任务在后：反了等于让模型先看到活儿再看到规矩。
			prompt = strings.TrimSpace(sys) + "\n\n" + prompt
		}
	}
	if block := e.skillRefBlock(node.Data["skill_refs"]); block != "" {
		prompt = block + "\n\n" + prompt
	}
	return prompt
}

// skillRefBlock 把 skill_refs 编译成一段「参考资料」前置。
//
// 为什么给路径而不给内容：领域技能包动辄 40–190KB，压进任务描述会把上下文吃光，
// 而且复制一份就多一个漂移源（改了 skill 忘了改卡片，两边说法不一致）。
// 外部智能体本来就能读本地文件——给它路径与目录清单，让它自己去读，是唯一
// 既能用上完整内容、又不用维护第二份副本的方式。
func (e *EngineV2) skillRefBlock(raw interface{}) string {
	names := skillRefNames(raw)
	if len(names) == 0 {
		return ""
	}
	roots := e.skillRoots
	if len(roots) == 0 {
		roots = SkillRoots("")
	}
	pluginRoots := e.pluginRoots
	if len(pluginRoots) == 0 {
		pluginRoots = PluginSkillRoots("")
	}
	var b strings.Builder
	b.WriteString("参考资料（动手前先读；这些是本机踩过坑总结的领域经验，别凭常识猜）：\n")
	var missing []string
	for _, name := range names {
		// 技能根优先，插件市场兜底：同名时以本机沉淀的方法论包为准
		p, ok := LookupSkillPack(roots, name)
		if !ok {
			p, ok = LookupPluginSkillPack(pluginRoots, name)
		}
		if !ok {
			missing = append(missing, name)
			continue
		}
		desc := p.Description
		if i := strings.IndexAny(desc, "。\n"); i > 0 {
			desc = desc[:i]
		}
		b.WriteString("- " + p.Name)
		if strings.TrimSpace(desc) != "" {
			b.WriteString(" —— " + strings.TrimSpace(desc))
		}
		b.WriteString("\n  " + filepath.Join(p.Path, "SKILL.md") + "\n")
		if len(p.Refs) > 0 {
			b.WriteString("  参考目录：" + filepath.Join(p.Path, "references") +
				"（" + strings.Join(p.Refs, "、") + "）\n")
		}
	}
	// 找不到要写出来，不能静默略过：否则用户以为参考已经给了，实际一个字都没读到
	if len(missing) > 0 {
		b.WriteString("- 未找到：" + strings.Join(missing, "、") +
			"（检查包名；扫描目录见 /api/skills 的 roots）\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// skillRefNames 解析 skill_refs：接受逗号/分号/空格分隔的字符串，也接受数组。
func skillRefNames(raw interface{}) []string {
	var out []string
	push := func(s string) {
		for _, part := range strings.FieldsFunc(s, func(r rune) bool {
			switch r {
			case ',', ';', '，', '；', ' ', '\t', '\n':
				return true
			}
			return false
		}) {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	switch v := raw.(type) {
	case string:
		push(v)
	case []string:
		for _, s := range v {
			push(s)
		}
	case []interface{}:
		for _, x := range v {
			if s, ok := x.(string); ok {
				push(s)
			}
		}
	}
	return out
}

// execAgentTaskNode 执行 agent_task
func (e *EngineV2) execAgentTaskNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	if e.agents == nil {
		return nil, fmt.Errorf("外部智能体名册未加载：请检查 data/agents.json")
	}

	agentID, _ := node.Data["agent_id"].(string)
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		avail := availableAgentIDs(e.agents)
		return nil, fmt.Errorf("agent_task 没指定 agent_id。可用：%s", avail)
	}

	def, ok := e.agents.Get(agentID)
	if !ok {
		return nil, fmt.Errorf("名册里没有 %q。可用：%s", agentID, availableAgentIDs(e.agents))
	}
	if !def.IsEnabled() {
		return nil, fmt.Errorf("智能体 %q 已停用：在 data/agents.json 里把 enabled 改成 true（或删掉该字段）后再用", agentID)
	}

	// 任务描述：节点上写了就用节点的，没写就吃上游文本
	prompt, _ := node.Data["prompt"].(string)
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = firstUpstreamText(upstream)
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("agent_task 没有任务描述：在节点 prompt 里写，或连一条来自上游的文本线")
	}

	// 能力卡：贴在 agent_task 上的角色卡必须真的生效。
	//
	// 之前这里只读 node.Data["prompt"]，贴卡既不报错也不起作用——是最坏的一种失败：
	// 用户以为给外部智能体装上了协议，实际什么都没变。而通用工作流平台里最需要
	// 卡片注入的恰恰是这个节点：长任务纪律、分派方法、评测规矩约束的是「怎么干」，
	// 写不进节点参数，只能作为角色前置拼进任务描述。
	prompt = e.composeAgentPrompt(node, prompt)

	timeout := def.Timeout()
	if ms, ok := node.Data["timeout_ms"].(float64); ok && ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 并发配额：同一条时间线上最多几个外部智能体在跑。
	// 等配额的时间计入本次超时 —— 排队不是免费的，用户设 5 分钟就是 5 分钟。
	pool, err := e.acquireAgentSlot(runCtx)
	if err != nil {
		return nil, err
	}
	defer pool.release()

	started := time.Now()
	var text string
	var meta map[string]interface{}

	switch def.Kind {
	case AgentCLI:
		t, m, err := e.runCLIAgent(runCtx, def, node, prompt)
		if err != nil {
			return nil, err
		}
		text, meta = t, m
	case AgentHTTP:
		t, m, err := e.runHTTPAgent(runCtx, def, prompt)
		if err != nil {
			return nil, err
		}
		text, meta = t, m
	default:
		return nil, fmt.Errorf("智能体 %q 的 kind %q 不认识", def.ID, def.Kind)
	}

	// allow_failure=true 时把失败也当产出往下传（下游能看到"它说了什么"），
	// 但元信息里必须留下 exit_code，不能伪装成成功。
	out := map[string]interface{}{
		"text":       text,
		"agent":      def.ID,
		"agent_kind": string(def.Kind),
		"elapsed_ms": time.Since(started).Milliseconds(),
		// 非确定性标记：下游节点与评测器都该看得见
		"nondeterministic": true,
	}
	for k, v := range meta {
		out[k] = v
	}
	return out, nil
}

// runCLIAgent 起子进程执行
func (e *EngineV2) runCLIAgent(ctx context.Context, def AgentDef, node *model.NodeInstance, prompt string) (string, map[string]interface{}, error) {
	bin, err := exec.LookPath(def.Command)
	if err != nil {
		return "", nil, fmt.Errorf("找不到命令 %q：%v。装好它，或在 data/agents.json 里把 command 改成绝对路径", def.Command, err)
	}

	workdir, cleanup, err := resolveAgentWorkdir(def, node)
	if err != nil {
		return "", nil, err
	}
	if cleanup != nil {
		defer cleanup()
	}

	vars := map[string]string{"prompt": prompt, "workdir": workdir}
	args := make([]string, 0, len(def.Args))
	for _, a := range def.Args {
		args = append(args, renderTemplate(a, vars))
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = workdir
	cmd.Env = buildAgentEnv(def.Env)
	// 超时后先礼后兵：给 2s 收尾，再强杀。WaitDelay 是 Go 1.20+ 的正式手段，
	// 比 Setpgid（Windows 无效）可靠。
	cmd.WaitDelay = 2 * time.Second

	stdin := renderTemplate(def.Stdin, vars)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var stdout, stderr limitedBuffer
	stdout.limit = agentOutputLimit
	stderr.limit = 64 << 10 // stderr 只留够看报错的量
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	text := stdout.String()
	meta := map[string]interface{}{
		"exit_code": exitCode,
		"workdir":   workdir,
		"command":   filepath.Base(bin),
	}
	if stdout.truncated {
		meta["output_truncated"] = true
	}
	// 渲染后还残留 {{...}} 说明引用了不存在的变量。不报错（有些 CLI 自己也用
	// 花括号语法，误杀会更烦），但必须说出来：用户多半以为它会被替换成上游文本。
	if strings.Contains(strings.Join(args, " ")+stdin, "{{") {
		meta["warnings"] = []string{
			"命令参数里还有未替换的 {{...}}：只支持 {{prompt}} 与 {{workdir}}，其余原样传给程序",
		}
	}
	if s := strings.TrimSpace(stderr.String()); s != "" {
		meta["stderr"] = truncateStr(s, 2000)
	}

	// 超时不看退出码：ctx 到期时进程被杀，退出码取决于平台，判据只认 ctx。
	if ctx.Err() == context.DeadlineExceeded {
		// 顺手回收进程树：只杀父进程会留下一堆孤儿（agent 自己 spawn 的
		// node/python/LSP），它们在后台继续吃内存，下次执行还会撞上。
		if cmd.Process != nil {
			killAgentTree(cmd.Process.Pid)
		}
		return "", nil, fmt.Errorf("智能体 %q 超时（%s）。它可能卡在交互式提问上——CLI 必须用非交互模式（如 -p / --yes）",
			def.ID, def.Timeout())
	}
	if runErr != nil && !allowFailure(node.Data) {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = text
		}
		return "", nil, fmt.Errorf("智能体 %q 失败（exit %d）：%s", def.ID, exitCode, truncateStr(detail, 800))
	}
	return text, meta, nil
}

// runHTTPAgent 调 OpenAI 兼容端点
func (e *EngineV2) runHTTPAgent(ctx context.Context, def AgentDef, prompt string) (string, map[string]interface{}, error) {
	base := strings.TrimRight(def.BaseURL, "/")
	// 兼容两种写法：写到 /v1 或写到 /v1/chat/completions
	endpoint := base
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint += "/chat/completions"
	}

	msgs := make([]map[string]string, 0, 2)
	if strings.TrimSpace(def.SystemPrompt) != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": def.SystemPrompt})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": prompt})

	payload, err := json.Marshal(map[string]interface{}{
		"model":    def.Model,
		"messages": msgs,
	})
	if err != nil {
		return "", nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// 凭据：与 llm_profile 同一套（只存名字，密钥在加密仓库里）
	if def.Credential != "" && e.creds != nil {
		if headers, _, err := e.creds.Resolve(def.Credential); err == nil {
			auth := headers["Authorization"]
			if auth == "" {
				auth = headers["authorization"]
			}
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
		}
	}

	// 代理必须显式关闭：本机常驻 http_proxy，走默认 Transport 会把
	// localhost 请求发给代理，拿回假 502（今天已经踩过三次）。
	client := &http.Client{
		Timeout:   def.Timeout(),
		Transport: &http.Transport{Proxy: nil},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("智能体 %q 请求失败（%s）：%v", def.ID, endpoint, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, agentOutputLimit))
	if resp.StatusCode >= 400 {
		return "", nil, fmt.Errorf("智能体 %q 返回 %d：%s", def.ID, resp.StatusCode, truncateStr(string(raw), 600))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", nil, fmt.Errorf("智能体 %q 的响应不是 OpenAI 兼容格式：%v", def.ID, err)
	}
	if len(out.Choices) == 0 {
		return "", nil, fmt.Errorf("智能体 %q 返回了空 choices：%s", def.ID, truncateStr(string(raw), 400))
	}
	text := out.Choices[0].Message.Content
	meta := map[string]interface{}{"model": def.Model, "endpoint": endpoint}
	if out.Usage.PromptTokens > 0 || out.Usage.CompletionTokens > 0 {
		meta["_usage"] = TokenUsage{
			PromptTokens:     out.Usage.PromptTokens,
			CompletionTokens: out.Usage.CompletionTokens,
		}
	}
	return text, meta, nil
}

// resolveAgentWorkdir 决定工作目录。
//
// 默认给每个节点一个独立临时目录：并发跑两个 agent 时，如果都写在同一个
// 工作目录里，会互相覆盖文件，而且这种覆盖看起来像"agent 抽风"。
func resolveAgentWorkdir(def AgentDef, node *model.NodeInstance) (string, func(), error) {
	if w, ok := node.Data["workdir"].(string); ok && strings.TrimSpace(w) != "" {
		if err := os.MkdirAll(w, 0o755); err != nil {
			return "", nil, fmt.Errorf("建工作目录失败 %s：%v", w, err)
		}
		return w, nil, nil
	}
	if strings.TrimSpace(def.WorkDir) != "" {
		if err := os.MkdirAll(def.WorkDir, 0o755); err != nil {
			return "", nil, fmt.Errorf("建工作目录失败 %s：%v", def.WorkDir, err)
		}
		return def.WorkDir, nil, nil
	}
	dir, err := os.MkdirTemp("", "flowedge-agent-"+sanitizeID(node.ID)+"-")
	if err != nil {
		return "", nil, fmt.Errorf("建临时工作目录失败：%v", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// buildAgentEnv 父进程环境 → 白名单 + 用户声明
func buildAgentEnv(extra map[string]string) []string {
	out := make([]string, 0, len(agentEnvWhitelist)+len(extra))
	for _, k := range agentEnvWhitelist {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// renderTemplate 只做 {{name}} 替换，不做表达式求值。
// 模板来自本机配置与节点数据，不做转义——但**绝不拼进 shell**：
// 参数是 argv 逐个传的，不存在注入面。
func renderTemplate(tpl string, vars map[string]string) string {
	if tpl == "" {
		return ""
	}
	out := tpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out
}

func allowFailure(data map[string]interface{}) bool {
	v, _ := data["allow_failure"].(bool)
	return v
}

func availableAgentIDs(s *AgentStore) string {
	var ids []string
	for _, a := range s.List() {
		if a.IsEnabled() {
			ids = append(ids, a.ID)
		}
	}
	if len(ids) == 0 {
		return "（一个都没有启用，去 data/agents.json 打开）"
	}
	return strings.Join(ids, " / ")
}

// limitedBuffer 有上限的 writer：超过 limit 后丢弃并置 truncated。
//
// 不能先无上限收完再截断 —— agent 刷屏时内存在截断前就已经被吃穿。
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	remain := w.limit - w.buf.Len()
	if remain <= 0 {
		w.truncated = true
		return len(p), nil // 假装写成功，避免子进程因 EPIPE 提前退出
	}
	if len(p) > remain {
		w.buf.Write(p[:remain])
		w.truncated = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

func (w *limitedBuffer) String() string { return w.buf.String() }

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…（已截断）"
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "node"
	}
	return out
}
