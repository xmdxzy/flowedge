package service

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"flowedge/backend/model"
)

// echoSpec 返回一个「打印固定文本」的命令，按平台选。
// 用系统自带的 echo 而不是写临时脚本：这两条路径在任何一台机器上都在。
func echoSpec(text string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", "echo " + text}
	}
	return "sh", []string{"-c", "echo " + text}
}

func exitSpec(code string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", "exit " + code}
	}
	return "sh", []string{"-c", "exit " + code}
}

func cmdNode(data map[string]interface{}) *model.NodeInstance {
	return &model.NodeInstance{ID: "cmd-1", Type: "command", Data: data}
}

// 命令跑通：stdout 是产出，status_code=0，meta 里带耗时。
func TestExecCommandSuccess(t *testing.T) {
	e := &EngineV2{}
	bin, args := echoSpec("hello-flowedge")
	out, err := e.execCommandNode(context.Background(), cmdNode(map[string]interface{}{
		"command": bin,
		"args":    strings.Join(args, " "),
	}), nil)
	if err != nil {
		t.Fatalf("命令执行失败：%v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("输出不是对象：%T", out)
	}
	if got := strings.TrimSpace(m["text"].(string)); got != "hello-flowedge" {
		t.Fatalf("stdout 应为 hello-flowedge，实际 %q", got)
	}
	if m["status_code"].(int) != 0 {
		t.Fatalf("status_code 应为 0，实际 %v", m["status_code"])
	}
	if _, ok := m["elapsed_ms"]; !ok {
		t.Fatal("缺 elapsed_ms：下游判「跑得慢不慢」要用它")
	}
}

// 非零退出默认必须让节点失败——「命令挂了但图继续跑」是最坏的静默失败。
func TestExecCommandNonZeroFailsByDefault(t *testing.T) {
	e := &EngineV2{}
	bin, args := exitSpec("3")
	_, err := e.execCommandNode(context.Background(), cmdNode(map[string]interface{}{
		"command": bin,
		"args":    strings.Join(args, " "),
	}), nil)
	if err == nil {
		t.Fatal("退出码 3 却判成功了：默认必须 fail")
	}
	if !strings.Contains(err.Error(), "退出码 3") {
		t.Fatalf("错误信息应带上退出码，实际：%v", err)
	}
}

// fail_on_nonzero=false 时才允许往下传，但 exit_code 必须如实。
func TestExecCommandAllowFailureKeepsExitCode(t *testing.T) {
	e := &EngineV2{}
	bin, args := exitSpec("3")
	out, err := e.execCommandNode(context.Background(), cmdNode(map[string]interface{}{
		"command":         bin,
		"args":            strings.Join(args, " "),
		"fail_on_nonzero": false,
	}), nil)
	if err != nil {
		t.Fatalf("关掉 fail_on_nonzero 后不该失败：%v", err)
	}
	m := out.(map[string]interface{})
	if m["exit_code"].(int) != 3 {
		t.Fatalf("exit_code 必须如实记 3，实际 %v", m["exit_code"])
	}
}

// 命令不存在时给的是「装好它或写绝对路径」，不是 exec 的原始 errno。
func TestExecCommandMissingBinary(t *testing.T) {
	e := &EngineV2{}
	_, err := e.execCommandNode(context.Background(), cmdNode(map[string]interface{}{
		"command": "flowedge-definitely-not-a-command",
	}), nil)
	if err == nil {
		t.Fatal("不存在的命令却执行成功了")
	}
	if !strings.Contains(err.Error(), "找不到命令") {
		t.Fatalf("错误信息应可操作，实际：%v", err)
	}
}

// 参数解析：JSON 数组优先，否则按空白分隔；两种写法都要能用。
func TestParseCommandArgs(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{`["a","b c",3]`, []string{"a", "b c", "3"}},
		{`--workdir . --json`, []string{"--workdir", ".", "--json"}},
		{``, nil},
	}
	for _, c := range cases {
		got, err := parseCommandArgs(c.raw)
		if err != nil {
			t.Fatalf("解析 %q 失败：%v", c.raw, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("解析 %q 得到 %v，期望 %v", c.raw, got, c.want)
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Fatalf("解析 %q 第 %d 项为 %q，期望 %q", c.raw, i, got[i], c.want[i])
			}
		}
	}
	if _, err := parseCommandArgs(`["unclosed"`); err == nil {
		t.Fatal("坏 JSON 数组应报错，而不是静默当成分隔符切分")
	}
}
