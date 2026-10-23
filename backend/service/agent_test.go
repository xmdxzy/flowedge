package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flowedge/backend/model"
)

// 外部智能体这条路的风险不在"能不能跑通"，在两件事：
//   ① 命令注入：节点数据里如果能有任意命令，一张工作流就等于任意代码执行
//   ② 环境泄漏：父进程环境（尤其 http_proxy）传下去，agent 的网络全被劫持
// 所以测试重心放在"拒绝"与"隔离"上，而不是"成功调用"。

func boolPtr(b bool) *bool { return &b }

func newTestAgentEngine(defs ...AgentDef) *EngineV2 {
	e := &EngineV2{}
	s := &AgentStore{agents: defs}
	e.SetAgents(s)
	return e
}

// 用系统命令验证 CLI 通路（Windows 上 cmd /c echo 是最小可用样例）
func cliEchoAgent() AgentDef {
	return AgentDef{
		ID: "echo", Label: "echo", Kind: AgentCLI,
		Command: "cmd", Args: []string{"/c", "echo", "{{prompt}}"},
	}
}

// 走 stdin 的回显 agent。
//
// 为什么需要第二个：`cmd /c echo "多行参数"` 会在第一个换行处截断（cmd 的解析问题，
// 不是引擎的），用它测多行注入会误判成"没注入"。more 只把 stdin 原样倒到 stdout，
// 不经过参数解析与代码页转换，才能验证完整的多行协议。
func cliStdinAgent() AgentDef {
	return AgentDef{
		ID: "cat", Label: "cat", Kind: AgentCLI,
		Command: "cmd", Args: []string{"/c", "more"}, Stdin: "{{prompt}}",
	}
}

func agentNode(data map[string]interface{}) *model.NodeInstance {
	return &model.NodeInstance{ID: "a1", Type: "agent_task", Data: data}
}

// 正常调用：产出走 text，元信息带 agent / 非确定性标记
func TestAgentTask_CLISuccess(t *testing.T) {
	e := newTestAgentEngine(cliEchoAgent())
	out, err := e.execAgentTaskNode(context.Background(),
		agentNode(map[string]interface{}{"agent_id": "echo", "prompt": "hello"}),
		map[string]interface{}{})
	if err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	m := out.(map[string]interface{})
	if !strings.Contains(m["text"].(string), "hello") {
		t.Fatalf("stdout 没带回来：%v", m["text"])
	}
	if m["nondeterministic"] != true {
		t.Fatal("必须标记非确定性 —— 下游与评测器都靠它分组")
	}
	if m["exit_code"] != 0 {
		t.Fatalf("exit_code = %v", m["exit_code"])
	}
}

// 没有 agent_id → 报错并把可用清单给出来（不能只说"缺少参数"）
func TestAgentTask_MissingAgentListsAvailable(t *testing.T) {
	e := newTestAgentEngine(cliEchoAgent())
	_, err := e.execAgentTaskNode(context.Background(), agentNode(map[string]interface{}{}), nil)
	if err == nil {
		t.Fatal("缺 agent_id 应报错")
	}
	if !strings.Contains(err.Error(), "echo") {
		t.Fatalf("错误里应列出可用 id，实际：%v", err)
	}
}

// 未知 id → 同样列可用
func TestAgentTask_UnknownAgent(t *testing.T) {
	e := newTestAgentEngine(cliEchoAgent())
	_, err := e.execAgentTaskNode(context.Background(),
		agentNode(map[string]interface{}{"agent_id": "nope", "prompt": "x"}), nil)
	if err == nil || !strings.Contains(err.Error(), "echo") {
		t.Fatalf("应报未知并列出可用，实际：%v", err)
	}
}

// 停用 → 明确说"去改 enabled"，而不是当成不存在
func TestAgentTask_DisabledAgent(t *testing.T) {
	d := cliEchoAgent()
	d.Enabled = boolPtr(false)
	e := newTestAgentEngine(d)
	_, err := e.execAgentTaskNode(context.Background(),
		agentNode(map[string]interface{}{"agent_id": "echo", "prompt": "x"}), nil)
	if err == nil || !strings.Contains(err.Error(), "停用") {
		t.Fatalf("应报已停用，实际：%v", err)
	}
}

// 没有任何任务描述 → 报错（不能默默跑一个空 prompt 浪费一次调用）
func TestAgentTask_NoPrompt(t *testing.T) {
	e := newTestAgentEngine(cliEchoAgent())
	_, err := e.execAgentTaskNode(context.Background(),
		agentNode(map[string]interface{}{"agent_id": "echo"}), map[string]interface{}{})
	if err == nil || !strings.Contains(err.Error(), "任务描述") {
		t.Fatalf("应报缺少任务描述，实际：%v", err)
	}
}

// 上游文本可以当任务描述（省一次复制粘贴）
func TestAgentTask_PromptFromUpstream(t *testing.T) {
	e := newTestAgentEngine(cliEchoAgent())
	out, err := e.execAgentTaskNode(context.Background(),
		agentNode(map[string]interface{}{"agent_id": "echo"}),
		map[string]interface{}{"s": map[string]interface{}{"text": "from-upstream"}})
	if err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	if !strings.Contains(out.(map[string]interface{})["text"].(string), "from-upstream") {
		t.Fatalf("没吃到上游文本：%v", out)
	}
}

// 非零退出 → 默认阻断（这是"这一步没做成"，不该让下游拿着半截输出继续）
func TestAgentTask_NonZeroExitFails(t *testing.T) {
	d := AgentDef{ID: "bad", Kind: AgentCLI, Command: "cmd", Args: []string{"/c", "exit", "3"}}
	e := newTestAgentEngine(d)
	_, err := e.execAgentTaskNode(context.Background(),
		agentNode(map[string]interface{}{"agent_id": "bad", "prompt": "x"}), nil)
	if err == nil || !strings.Contains(err.Error(), "exit 3") {
		t.Fatalf("应报 exit 3，实际：%v", err)
	}
}

// allow_failure=true → 不阻断，但元信息必须如实记退出码（不许伪装成成功）
func TestAgentTask_AllowFailureKeepsExitCode(t *testing.T) {
	d := AgentDef{ID: "bad", Kind: AgentCLI, Command: "cmd", Args: []string{"/c", "exit", "3"}}
	e := newTestAgentEngine(d)
	out, err := e.execAgentTaskNode(context.Background(),
		agentNode(map[string]interface{}{"agent_id": "bad", "prompt": "x", "allow_failure": true}), nil)
	if err != nil {
		t.Fatalf("allow_failure 不该报错：%v", err)
	}
	if out.(map[string]interface{})["exit_code"] != 3 {
		t.Fatalf("exit_code 应如实为 3，实际 %v", out.(map[string]interface{})["exit_code"])
	}
}

// 超时 → 报错且提示"可能是交互式提问"
func TestAgentTask_Timeout(t *testing.T) {
	// ping 本机的无响应地址会挂住；用 timeout_ms 强制很短
	d := AgentDef{ID: "slow", Kind: AgentCLI, Command: "cmd",
		Args: []string{"/c", "ping", "-n", "10", "127.0.0.1"}, TimeoutSec: 1}
	e := newTestAgentEngine(d)
	start := time.Now()
	_, err := e.execAgentTaskNode(context.Background(),
		agentNode(map[string]interface{}{"agent_id": "slow", "prompt": "x", "timeout_ms": float64(300)}), nil)
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("应报超时，实际：%v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("超时没生效，耗时 %v", d)
	}
}

// 命令不存在 → 明确指出"装好或改绝对路径"，而不是裸的 exec 错误
func TestAgentTask_CommandNotFound(t *testing.T) {
	d := AgentDef{ID: "ghost", Kind: AgentCLI, Command: "definitely-not-a-real-command-xyz"}
	e := newTestAgentEngine(d)
	_, err := e.execAgentTaskNode(context.Background(),
		agentNode(map[string]interface{}{"agent_id": "ghost", "prompt": "x"}), nil)
	if err == nil || !strings.Contains(err.Error(), "找不到命令") {
		t.Fatalf("应报找不到命令，实际：%v", err)
	}
}

// ===== 环境隔离（今天踩了三次代理坑，必须有测试盯住）=====

func TestAgentEnv_DoesNotInheritProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:5751")
	t.Setenv("http_proxy", "http://127.0.0.1:5751")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:5751")

	for _, kv := range buildAgentEnv(nil) {
		l := strings.ToLower(kv)
		if strings.HasPrefix(l, "http_proxy=") || strings.HasPrefix(l, "https_proxy=") || strings.HasPrefix(l, "all_proxy=") {
			t.Fatalf("不许把代理传给子进程（会让 agent 的网络全被劫持）：%s", kv)
		}
	}
}

func TestAgentEnv_AllowsExplicitDeclaration(t *testing.T) {
	env := buildAgentEnv(map[string]string{"HTTP_PROXY": "http://127.0.0.1:7890"})
	found := false
	for _, kv := range env {
		if kv == "HTTP_PROXY=http://127.0.0.1:7890" {
			found = true
		}
	}
	if !found {
		t.Fatal("用户在 agents.json 里显式声明的 env 应当透传（那是他的选择）")
	}
}

// 模板只做 {{name}} 替换
func TestRenderTemplate(t *testing.T) {
	got := renderTemplate("-p {{prompt}} @ {{workdir}}", map[string]string{"prompt": "hi", "workdir": "C:/tmp"})
	if got != "-p hi @ C:/tmp" {
		t.Fatalf("渲染结果：%s", got)
	}
}

// 输出上限：超限要截断并置标记，不能无上限收完再截（那会先吃穿内存）
func TestLimitedBuffer_Truncates(t *testing.T) {
	b := &limitedBuffer{limit: 10}
	n, err := b.Write([]byte("0123456789ABCDEF"))
	if err != nil || n != 16 {
		t.Fatalf("应假装写成功（避免子进程 EPIPE），n=%d err=%v", n, err)
	}
	if b.String() != "0123456789" || !b.truncated {
		t.Fatalf("截断不对：%q truncated=%v", b.String(), b.truncated)
	}
}

// ===== 名册 =====

func TestAgentStore_DefaultsAreDisabled(t *testing.T) {
	s := LoadAgents(filepath.Join(t.TempDir(), "none.json"))
	list := s.List()
	if len(list) == 0 {
		t.Fatal("无文件时应给内置示例")
	}
	for _, a := range list {
		if a.IsEnabled() {
			t.Fatalf("内置示例必须默认停用（装没装只有本机知道，猜错会误导）：%s", a.ID)
		}
	}
}

func TestAgentStore_PersistAndReload(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agents.json")
	s := LoadAgents(p)
	if err := s.Upsert(AgentDef{ID: "x", Kind: AgentCLI, Command: "cmd"}); err != nil {
		t.Fatal(err)
	}
	again := LoadAgents(p)
	got, ok := again.Get("x")
	if !ok || got.Command != "cmd" {
		t.Fatalf("没落盘或读回不对：%+v", got)
	}
	// 临时文件不许残留
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("临时文件应已 rename")
	}
}

func TestAgentStore_UpsertValidation(t *testing.T) {
	s := LoadAgents(filepath.Join(t.TempDir(), "a.json"))
	cases := []AgentDef{
		{ID: "", Kind: AgentCLI, Command: "cmd"}, // 空 id
		{ID: "a", Kind: "weird", Command: "cmd"}, // 未知 kind
		{ID: "b", Kind: AgentCLI},                // cli 缺 command
		{ID: "c", Kind: AgentHTTP},               // http 缺 base_url
	}
	for _, c := range cases {
		if err := s.Upsert(c); err == nil {
			t.Fatalf("应拒绝：%+v", c)
		}
	}
}

func TestAgentStore_ProbeStates(t *testing.T) {
	s := &AgentStore{agents: []AgentDef{
		{ID: "on", Kind: AgentCLI, Command: "cmd"},
		{ID: "off", Kind: AgentCLI, Command: "cmd", Enabled: boolPtr(false)},
		{ID: "ghost", Kind: AgentCLI, Command: "no-such-cmd-xyz"},
	}}
	ctx := context.Background()

	if st := s.Probe(ctx, "on"); !st.Available {
		t.Fatalf("cmd 应可用：%s", st.Detail)
	}
	if st := s.Probe(ctx, "off"); st.Available || !strings.Contains(st.Detail, "停用") {
		t.Fatalf("停用应判不可用：%+v", st)
	}
	if st := s.Probe(ctx, "ghost"); st.Available || !strings.Contains(st.Detail, "PATH") {
		t.Fatalf("找不到命令应判不可用并说明原因：%+v", st)
	}
	if st := s.Probe(ctx, "missing"); st.Available {
		t.Fatal("名册里没有的 id 应判不可用")
	}
}

// ===== 契约与任务书 =====

// agent_task 必须在契约里标非确定性，并且任务书把它写出来
func TestAgentTask_ContractMarksNondeterministic(t *testing.T) {
	c, ok := ModuleContractOf("agent_task")
	if !ok {
		t.Fatal("agent_task 没有契约")
	}
	if !c.Nondeterministic {
		t.Fatal("agent_task 必须在契约里标 Nondeterministic —— 否则评测会把不确定的图当可复现图比")
	}

	wf := &model.Workflow{ID: 1, Name: "t",
		Nodes: json.RawMessage(`[
			{"id":"s","type":"start","data":{}},
			{"id":"a","type":"agent_task","data":{"agent_id":"echo"}},
			{"id":"e","type":"end","data":{}}]`),
		Edges: json.RawMessage(`[
			{"id":"e1","source":"s","target":"a"},
			{"id":"e2","source":"a","target":"e"}]`),
	}
	doc, err := CompileSpec(wf, "md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Markdown, "非确定性") {
		t.Fatal("任务书必须标出非确定性：评测按它分组，人按它决定要不要跑")
	}
}

// ===== 并发配额 =====

// 配额满了以后，后来的任务必须在超时前失败并说清原因（而不是默默排队到天荒地老）
func TestAgentTask_ConcurrencyQuotaTimesOut(t *testing.T) {
	t.Setenv("AGENT_MAX_CONCURRENCY", "1")
	e := newTestAgentEngine(cliEchoAgent())

	// 先占满唯一的槽位
	slots := e.agentSlots()
	slots <- struct{}{}
	defer func() { <-slots }()

	start := time.Now()
	_, err := e.execAgentTaskNode(context.Background(),
		agentNode(map[string]interface{}{"agent_id": "echo", "prompt": "x", "timeout_ms": float64(300)}), nil)
	if err == nil {
		t.Fatal("配额被占满时应失败，不能无限等")
	}
	if !strings.Contains(err.Error(), "并发配额") {
		t.Fatalf("错误要指明是配额问题（否则用户会去查 agent 本身）：%v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("没按节点超时退出，耗时 %v", d)
	}
}

// 配额释放后能继续用（release 不能漏）
func TestAgentTask_QuotaReleasedAfterRun(t *testing.T) {
	t.Setenv("AGENT_MAX_CONCURRENCY", "1")
	e := newTestAgentEngine(cliEchoAgent())

	for i := 0; i < 3; i++ {
		if _, err := e.execAgentTaskNode(context.Background(),
			agentNode(map[string]interface{}{"agent_id": "echo", "prompt": "hi"}), nil); err != nil {
			t.Fatalf("第 %d 次应成功（说明上一次的槽位没释放）：%v", i+1, err)
		}
	}
	if n := len(e.agentSlots()); n != 0 {
		t.Fatalf("跑完应无占用，实际 %d", n)
	}
}

// 会话级配额上限要被夹住（防止有人设 1000 把机器压死）
func TestAgentMaxConcurrencyClamped(t *testing.T) {
	t.Setenv("AGENT_MAX_CONCURRENCY", "999")
	if got := agentMaxConcurrency(); got != 16 {
		t.Fatalf("上限应夹到 16，实际 %d", got)
	}
	t.Setenv("AGENT_MAX_CONCURRENCY", "0")
	if got := agentMaxConcurrency(); got != 2 {
		t.Fatalf("非法值应退回默认 2，实际 %d", got)
	}
}

// 贴在 agent_task 上的角色卡必须真的生效。
//
// 之前的实现只读 node.Data["prompt"]，贴卡既不报错也不起作用——用户以为给外部
// 智能体装上了协议，实际什么都没变。通用工作流平台里最需要卡片注入的正是这个节点：
// 长任务纪律、分派方法、评测规矩约束的是"怎么干"，写不进节点参数。
func TestAgentTask_BodyCardInjected(t *testing.T) {
	e := newTestAgentEngine(cliStdinAgent())
	e.SetCards(LoadCards(filepath.Join(t.TempDir(), "cards.json")))

	node := agentNode(map[string]interface{}{
		"agent_id": "cat",
		"prompt":   "hello",
		"bindings": []interface{}{
			map[string]interface{}{"kind": "agent", "id": "long-run-executor", "stage": "body"},
		},
	})
	got := e.composeAgentPrompt(node, "hello")
	if !strings.Contains(got, "每轮必须有 delta") {
		t.Fatalf("角色卡没拼进任务描述，实际：%q", got)
	}
	if !strings.Contains(got, "hello") {
		t.Fatalf("拼角色卡时把原任务描述弄丢了：%q", got)
	}
	// 协议在前、任务在后：顺序反了等于让模型先看到活儿再看到规矩
	if strings.Index(got, "每轮必须有 delta") > strings.Index(got, "hello") {
		t.Fatalf("角色卡应前置在任务描述之前：%q", got)
	}
	// 端到端也要走一遍：证明组装结果真的进了这个节点的执行链路（不抛错即通）
	if _, err := e.execAgentTaskNode(context.Background(), node, map[string]interface{}{}); err != nil {
		t.Fatalf("执行链路不该失败：%v", err)
	}
}

// 没有贴卡时，任务描述必须原样透传（不能因为新逻辑给所有调用都加上前缀）
func TestAgentTask_NoCardKeepsPrompt(t *testing.T) {
	e := newTestAgentEngine(cliEchoAgent())
	e.SetCards(LoadCards(filepath.Join(t.TempDir(), "cards.json")))
	node := agentNode(map[string]interface{}{"agent_id": "echo", "prompt": "plain task"})
	if got := e.composeAgentPrompt(node, "plain task"); got != "plain task" {
		t.Fatalf("没贴卡却改写了任务描述：%q", got)
	}
	out, err := e.execAgentTaskNode(context.Background(), node, map[string]interface{}{})
	if err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	text := out.(map[string]interface{})["text"].(string)
	if !strings.Contains(text, "plain task") {
		t.Fatalf("任务描述丢了，stdout：%q", text)
	}
}

// skill_refs：领域包不能压成提示词（40–190KB 压不下、复制还会漂移），
// 只能给路径让智能体自己去读。这条测试盯住"路径真的写进去了"。
func TestAgentTask_SkillRefsInjected(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "demo-skill")
	if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\nname: demo-skill\ndescription: 演示包。第二句不该进来\n---\n正文"), 0o644)
	os.WriteFile(filepath.Join(dir, "references", "01-gate.md"), []byte("x"), 0o644)

	e := newTestAgentEngine(cliStdinAgent())
	e.SetCards(LoadCards(filepath.Join(t.TempDir(), "cards.json")))
	e.SetSkillRoots([]string{root})

	node := agentNode(map[string]interface{}{
		"agent_id":   "cat",
		"prompt":     "do it",
		"skill_refs": "demo-skill, no-such-pack",
	})
	got := e.composeAgentPrompt(node, "do it")
	for _, want := range []string{"demo-skill", "SKILL.md", "references", "01-gate.md", "no-such-pack"} {
		if !strings.Contains(got, want) {
			t.Fatalf("参考资料缺 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, "第二句不该进来") {
		t.Fatalf("描述只该取第一句，实际：\n%s", got)
	}
	// 找不到的包必须写出来：静默略过会让用户以为参考已经给了
	if !strings.Contains(got, "未找到") {
		t.Fatalf("缺包没报出来：\n%s", got)
	}
	// 任务描述在最后：参考资料是"依据"，不是"任务"
	if strings.Index(got, "do it") < strings.Index(got, "参考资料") {
		t.Fatalf("任务描述应排在参考资料之后：\n%s", got)
	}
}

// 没填 skill_refs 就不该多出任何一段（空字段不能污染所有 agent 调用）
func TestAgentTask_NoSkillRefsNoBlock(t *testing.T) {
	e := newTestAgentEngine(cliStdinAgent())
	node := agentNode(map[string]interface{}{"agent_id": "cat", "prompt": "x"})
	if got := e.composeAgentPrompt(node, "x"); got != "x" {
		t.Fatalf("没填参考却改了任务描述：%q", got)
	}
}
