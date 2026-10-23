package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"flowedge/backend/model"
)

// command 节点：在本机跑一条外部命令，把 stdout / stderr / 退出码交给下游。
//
// 为什么需要它（2026-10-23 编排基准暴露的缺口）：
// 引擎有 code 节点，但那是 yaegi 解释器，注入的白名单里没有 os/exec —— 它跑不了
// 任何外部程序。于是「跑一遍测试 / 跑一次构建 / 调本地校验脚本」这类**确定性验收**
// 只能外挂一个 HTTP 服务，再让 http_request 去打它。基准里为此多起了一个进程、
// 多了一条失败面（服务没起，整张图就废了），而这件事本该是引擎自己会的。
//
// 定位写清：它是 agent_task 的确定性对偶。
//   - agent_task：把需要判断的活儿交出去，**不承诺可复现**，输出带 nondeterministic 标记；
//   - command：跑我们自己的东西并**如实报告**结果，同一命令同一输入必然同一结论。
// 编排里最需要的组合正是这两者相邻：agent 改，command 验。
//
// 安全边界：引擎是本地单用户工具，agent_task 早就能起任意命令，所以本节点不扩大攻击面。
// 真正的收益是**可审计**——「跑了什么」写在节点参数里、进任务书（spec），
// 而不是藏在某个 agent 的临时决定里。
const (
	commandDefaultTimeout = 2 * time.Minute
	commandOutputLimit    = 512 << 10 // 单流 512KB，超了截断并置 truncated
)

// parseCommandArgs 解析 args 参数：优先按 JSON 数组，否则按空白分隔。
//
// 不做 shell 解析（不认引号、管道、通配符）：参数是 argv 逐个传的，
// 因此不存在注入面。需要 shell 语义就显式写 command=cmd /c 或 bash -c。
func parseCommandArgs(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if strings.HasPrefix(raw, "[") {
		var arr []interface{}
		if err := json.Unmarshal([]byte(raw), &arr); err != nil {
			return nil, fmt.Errorf("args 以 [ 开头但不是合法 JSON 数组：%v。要么写成 JSON 数组，要么用空格分隔", err)
		}
		out := make([]string, 0, len(arr))
		for _, v := range arr {
			if s, ok := v.(string); ok {
				out = append(out, s)
				continue
			}
			out = append(out, fmt.Sprintf("%v", v))
		}
		return out, nil
	}
	return strings.Fields(raw), nil
}

// commandTimeoutOf 取本次命令的超时：节点参数优先，受引擎硬上限约束
func commandTimeoutOf(node *model.NodeInstance) time.Duration {
	d := commandDefaultTimeout
	if ms, ok := node.Data["timeout_ms"].(float64); ok && ms > 0 {
		d = time.Duration(ms) * time.Millisecond
	}
	if d > maxNodeTimeout {
		d = maxNodeTimeout
	}
	return d
}

func (e *EngineV2) execCommandNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	rawCommand, _ := node.Data["command"].(string)
	rawCommand = strings.TrimSpace(rawCommand)
	if rawCommand == "" {
		return nil, fmt.Errorf("command 节点没有指定命令：在节点参数 command 里写可执行文件名或绝对路径")
	}

	// {{input}} 只在参数与命令里替换，取上游第一个非空文本。
	// 写成占位符而不是拼接，是为了让「哪些值来自上游」在图上看得见。
	vars := map[string]string{"input": firstUpstreamText(upstream)}
	command := renderTemplate(rawCommand, vars)

	rawArgs, _ := node.Data["args"].(string)
	args, err := parseCommandArgs(renderTemplate(rawArgs, vars))
	if err != nil {
		return nil, err
	}

	bin, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("找不到命令 %q：装好它，或在节点参数里写绝对路径（当前 PATH 里没有这个可执行文件）", command)
	}

	workdir, _ := node.Data["workdir"].(string)
	workdir = strings.TrimSpace(workdir)
	if workdir != "" {
		if st, statErr := os.Stat(workdir); statErr != nil || !st.IsDir() {
			return nil, fmt.Errorf("工作目录不存在或不是目录：%s", workdir)
		}
	}

	timeout := commandTimeoutOf(node)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin, args...)
	if workdir != "" {
		cmd.Dir = workdir
	}
	// 命令本身可能不是"叶子"（python 会拉子进程、npm 会拉 node），
	// CommandContext 默认只杀父进程，超时后残留的子进程会继续吃 CPU。
	cmd.WaitDelay = 2 * time.Second

	stdout := &limitedBuffer{limit: commandOutputLimit}
	stderr := &limitedBuffer{limit: commandOutputLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	started := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(started)

	exitCode := 0
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exitCode = ee.ExitCode()
		} else if runCtx.Err() == context.DeadlineExceeded {
			// 超时：exitCode 保持 0 会让下游误判成功，统一记 -1
			exitCode = -1
		} else {
			return nil, fmt.Errorf("命令启动失败：%v", runErr)
		}
	}

	// 超时后补一刀杀进程树（父进程已被 CommandContext 杀掉，子进程还在）
	if runCtx.Err() == context.DeadlineExceeded && cmd.Process != nil {
		killAgentTree(cmd.Process.Pid)
		return nil, &nodeExecError{
			Code: model.ErrorCodeTimeout,
			Err: fmt.Errorf("命令超时（%s）：%s %s。看 stderr 前 400 字：%s",
				timeout, command, strings.Join(args, " "), truncateStr(stderr.String(), 400)),
		}
	}

	out := map[string]interface{}{
		"text":        stdout.String(),
		"status_code": exitCode,
		"exit_code":   exitCode,
		"stderr":      stderr.String(),
		"elapsed_ms":  elapsed.Milliseconds(),
		"truncated":   stdout.truncated || stderr.truncated,
		"command":     command,
		"args":        args,
	}

	// fail_on_nonzero 默认 true：命令失败必须让图失败。
	// 显式设 false 才会把非零退出当产出往下传（此时 exit_code 仍然如实记录，
	// 下游用 {{id.status_code}} 自行判断）——「失败也往下传」不能变成「假装成功」。
	failOnNonzero := true
	if v, ok := node.Data["fail_on_nonzero"].(bool); ok {
		failOnNonzero = v
	}
	if failOnNonzero && exitCode != 0 {
		return nil, fmt.Errorf("命令退出码 %d（%s %s）。stderr 前 400 字：%s",
			exitCode, command, strings.Join(args, " "), truncateStr(stderr.String(), 400))
	}

	return out, nil
}
