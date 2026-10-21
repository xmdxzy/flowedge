package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"

	"flowedge/backend/model"
)

// 代码执行节点（code）。
//
// 为什么用 yaegi 而不是容器/子进程：
//   - 单 exe 零依赖红线：纯 Go 解释器直接编进二进制，无需 Docker/Python/glibc
//   - 无网络、无 CGO，符号表白名单从根上限制能力面
//
// 安全边界（重要）：
//  1. 只注入白名单包的符号（见 codeSymbols），黑名单 import 静态预检直接拒绝
//  2. 不暴露 stdin/stdout 文件句柄，也不注入 os/os/exec/net/syscall/unsafe
//  3. 已知限制：yaegi 无 Go 程序级中断能力。代码死循环时无法杀掉该线程，
//     只能放弃等待其结果（goroutine 泄漏）。因此默认 timeout 上限给得比普通节点低，
//     且务必配合节点的 timeout_ms 使用。要硬中断应外挂沙箱子进程。

// codeAllowedPkg 允许代码使用的标准库（键为 stdlib.Symbols 的 "path/name" 形式）
var codeAllowedPkg = []string{
	"strings/strings",
	"strconv/strconv",
	"fmt/fmt",
	"sort/sort",
	"math/math",
	"math/rand/rand",
	"time/time",
	"encoding/json/json",
	"unicode/unicode",
	"bytes/bytes",
	"crypto/sha256/sha256",
	"regexp/regexp",
	"errors/errors",
	"path/path",
}

// codeForbiddenImport 静态预检黑名单（白名单之外的双保险）
var codeForbiddenImport = []string{
	"os/exec", "os", "net", "net/http", "syscall", "unsafe",
	"plugin", "runtime", "reflect", "debug/pe", "os/signal",
}

// codeDefaultTimeout 代码节点默认超时。
// 比普通节点的 180s 短得多：本地脚本跑越久越可能死循环，而 yaegi 无法硬中断。
const codeDefaultTimeout = 30 * time.Second

// checkCodeSafety 静态预检：禁止 import 危险包。
//
// 必须解析**真正的 import 语句**，不能对整段源码做 `strings.Contains(code, "\"os\"")`。
// 那个写法在 2026-10-21 咬过一次：插件示例的注释里写了 `import "os" 之类会被拒绝`，
// 结果整个插件被拦下——注释不是代码，却决定了代码能不能跑。
func checkCodeSafety(code string) error {
	for _, pkg := range importedPackages(code) {
		for _, f := range codeForbiddenImport {
			if pkg == f || strings.HasPrefix(pkg, f+"/") {
				return fmt.Errorf("import %q 在代码沙箱中被禁止", pkg)
			}
		}
	}
	return nil
}

// importedPackages 提取代码里真正 import 的包路径。
//
// 支持 `import "x"`、`import alias "x"` 与 `import ( ... )` 块。
// 先剥行注释，避免注释里的引号被当成 import。
func importedPackages(code string) []string {
	var out []string
	inBlock := false
	for _, raw := range strings.Split(code, "\n") {
		line := strings.TrimSpace(stripLineComment(raw))
		if line == "" {
			continue
		}
		if !inBlock {
			if line == "import (" || strings.HasPrefix(line, "import (") {
				inBlock = true
				continue
			}
			if strings.HasPrefix(line, "import ") {
				out = append(out, quotedPaths(line)...)
			}
			continue
		}
		if line == ")" || strings.HasPrefix(line, ")") {
			inBlock = false
			continue
		}
		out = append(out, quotedPaths(line)...)
	}
	return out
}

// quotedPaths 取出一行里所有双引号内的内容（import 路径）
func quotedPaths(line string) []string {
	var out []string
	for {
		i := strings.Index(line, `"`)
		if i < 0 {
			return out
		}
		rest := line[i+1:]
		j := strings.Index(rest, `"`)
		if j < 0 {
			return out
		}
		out = append(out, rest[:j])
		line = rest[j+1:]
	}
}

// stripLineComment 去掉行注释（字符串里的 // 不算）
func stripLineComment(line string) string {
	inStr := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			if i == 0 || line[i-1] != '\\' {
				inStr = !inStr
			}
		case '/':
			if !inStr && i+1 < len(line) && line[i+1] == '/' {
				return line[:i]
			}
		}
	}
	return line
}

// buildCodeInterpreter 构造只注入白名单符号的解释器。
//
// 两层防护：
//  1. i.Use 只注入白名单包的符号表（Eval 里 import 白名单外的包会因找不到符号而失败）
//  2. checkCodeSafety 静态预检黑名单 import（白名单之外的 import 一律拒绝）
//
// 注意：import 语句必须出现在传给 Eval 的源码里（见 codeImportBlock），
// 仅 Use 不会让包名在解释器作用域内可见。
func buildCodeInterpreter() *interp.Interpreter {
	i := interp.New(interp.Options{})
	exports := interp.Exports{}
	for _, pathName := range codeAllowedPkg {
		if sym, ok := stdlib.Symbols[pathName]; ok {
			exports[pathName] = sym
		}
	}
	i.Use(exports)
	return i
}

// execCodeNode 执行用户代码。
//
// 约定：代码里可读取 input（上游输出的 JSON 字符串）与 inputText（上游文本），
// 把结果赋给 result 即可：
//
//	result = strings.ToUpper(inputText)
//
// result 为字符串时直接作为输出 text；其他类型序列化为 JSON 字符串，
// 若为 JSON object/array 则同时结构化展开到输出字段。
func (e *EngineV2) execCodeNode(ctx context.Context, node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	code, _ := node.Data["code"].(string)
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("code 节点没有代码内容")
	}
	if err := checkCodeSafety(code); err != nil {
		return nil, err
	}

	// 上游输入：优先 JSON 序列化（保留结构），同时提供 inputText 方便直接写字符串逻辑
	upstreamInput := upstreamJSON(upstream)
	upstreamText := firstUpstreamText(upstream)

	// 节点级 timeout 覆盖（默认 30s，比 LLM 等外部服务短）
	timeout := codeDefaultTimeout
	if ms, ok := node.Data["timeout_ms"].(float64); ok && ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
		if timeout > maxNodeTimeout {
			timeout = maxNodeTimeout
		}
	}

	type codeResult struct {
		out interface{}
		err error
	}
	resCh := make(chan codeResult, 1)

	go func() {
		out, err := runCode(code, upstreamInput, upstreamText)
		resCh <- codeResult{out, err}
	}()

	var raw interface{}
	select {
	case res := <-resCh:
		if res.err != nil {
			return nil, res.err
		}
		raw = res.out
	case <-time.After(timeout):
		return nil, &nodeExecError{
			Code: model.ErrorCodeTimeout,
			Err:  fmt.Errorf("code 执行超时（%s）。yaegi 无法硬中断，请检查是否存在死循环，并为节点设置更短的 timeout_ms", timeout),
		}
	case <-ctx.Done():
		return nil, &nodeExecError{Code: model.ErrorCodeCancelled, Err: fmt.Errorf("运行已取消")}
	}

	out := map[string]interface{}{"code_out": raw}
	switch v := raw.(type) {
	case string:
		out["text"] = v
	case map[string]interface{}:
		out["text"] = jsonString(v)
		for k, val := range v {
			out[k] = val
		}
	case []interface{}:
		out["text"] = jsonString(v)
		out["items"] = v
	case nil:
		out["text"] = ""
	default:
		out["text"] = fmt.Sprintf("%v", v)
	}
	return out, nil
}

// codeImportBlock 注入到解释器的白名单 import。
// 用户代码不必（也不能）写 import：这里一次性注入安全包，
// 白名单之外的包既没有符号、也会被静态预检拒绝。
const codeImportBlock = `import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)`

// runCode 在 yaegi 里执行代码并取回 result 变量。
//
// 用户代码被包进一个函数体内：yaegi 的顶层只接受声明，而用户写的是脚本式语句
// （result = ...）。包进函数后，if / for / type 声明与赋值都是合法函数体内容。
// 可用变量：input（上游输出 JSON 字符串）、inputText（上游第一个非空文本）。
func runCode(code, upstreamJSON, inputText string) (raw interface{}, retErr error) {
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("代码执行崩溃：%v", r)
		}
	}()

	i := buildCodeInterpreter()

	wrapper := fmt.Sprintf(`
%s

var input = %q

var inputText = %q

func __flowedge_run() interface{} {
	var result interface{}
%s
	return result
}

var result = __flowedge_run()
`, codeImportBlock, upstreamJSON, inputText, code)

	if _, err := i.Eval(wrapper); err != nil {
		return nil, fmt.Errorf("代码编译/执行失败：%v", err)
	}

	v, err := i.Eval("result")
	if err != nil {
		return nil, fmt.Errorf("读取 result 失败：%v", err)
	}

	if !v.IsValid() {
		return nil, nil
	}
	return v.Interface(), nil
}

// upstreamJSON 把上游输出序列化为紧凑 JSON（代码节点的 input）
func upstreamJSON(upstream map[string]interface{}) string {
	if len(upstream) == 0 {
		return "{}"
	}
	// 多前驱时优先取文本字段，保持 input 直观可用
	if len(upstream) == 1 {
		for _, out := range upstream {
			if m, ok := out.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					return t
				}
			}
		}
	}
	b, err := json.Marshal(upstream)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// firstUpstreamText 取上游第一个非空文本（代码节点的 inputText）
func firstUpstreamText(upstream map[string]interface{}) string {
	for _, out := range upstream {
		if m, ok := out.(map[string]interface{}); ok {
			if t, ok := m["text"].(string); ok && t != "" {
				return t
			}
		}
	}
	return ""
}

func jsonString(v interface{}) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
