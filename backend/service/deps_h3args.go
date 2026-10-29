package service

import (
	"strconv"
	"strings"
)

// H3 启动参数的可配置覆盖（data/deps.json 的 "h3" 段）。
//
// 为什么专门给 reserve_vram 开一个口子（2026-10-26）：
//   ComfyUI 的 --reserve-vram 会变成 EXTRA_RESERVED_VRAM（model_management.py:883），
//   而全量加载的判据是「模型 + extra_reserved」同时空闲（model_management.py:944）。
//   24GB 卡上 H3 权重约 20GB、桌面基线约 3.1GB ⇒ 空闲 0.3–0.9GB，4GB 预留**永远差**，
//   于是每次都退化成部分加载：权重留在内存、按步换进显存。
//   实测同一镜 87s → 2269s（27 倍），并撞上节点超时。
//
// 原来想改这个值只能改环境变量 H3_ARGS 并重启**引擎**（只在 NewSupervisor 读一次）。
// 做成配置后：改 data/deps.json → POST /api/deps/reload → 重启该依赖，就能 A/B。
// 环境变量 H3_ARGS 仍然是最高优先级（保持既有行为不变）。

// setArgValue 把 args 里 --flag 后面那个值换掉；没有这个 flag 就追加。
// 只替换紧接着的那一个元素：ComfyUI 的参数都是 "--flag value" 两段式。
func setArgValue(args []string, flag, value string) []string {
	out := make([]string, 0, len(args)+2)
	replaced := false
	for i := 0; i < len(args); i++ {
		out = append(out, args[i])
		if args[i] == flag && i+1 < len(args) {
			out = append(out, value)
			i++ // 跳过被替换掉的旧值
			replaced = true
		}
	}
	if !replaced {
		out = append(out, flag, value)
	}
	return out
}

// applyH3Config 按配置覆盖 H3 启动参数。
//
// 顺序契约：代码内置默认 → conf.H3.ReserveVRAM → conf.H3.Args（整体替换）→ 环境变量。
// 环境变量最后生效，保证「改了配置但想临时用 env 顶一下」仍然可行。
func applyH3Config(base []string, cfgArgs []string, reserveVRAM *int) []string {
	out := base
	if reserveVRAM != nil && *reserveVRAM >= 0 {
		out = setArgValue(out, "--reserve-vram", strconv.Itoa(*reserveVRAM))
	}
	if len(cfgArgs) > 0 {
		// 整体覆盖：只覆盖「参数」，脚本路径必须保留在最前。
		// 判断依据是**配置里有没有自带脚本**，不是 base[0] 长什么样——
		// 第一版写成看 base[0] 是不是 main.py，结果恒为真、main.py 被整段丢掉。
		out = append([]string{}, cfgArgs...)
		if len(base) > 0 && !strings.Contains(strings.ToLower(cfgArgs[0]), "main.py") {
			out = append([]string{base[0]}, out...)
		}
	}
	return out
}

// dropArg 从参数表里删掉某个开关（只删这个 flag 本身，不带值）。
// 用于 cache_none=false 时把默认带的 --cache-none 去掉。
func dropArg(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == flag {
			continue
		}
		out = append(out, a)
	}
	return out
}

// H3ArgValue 从参数里读出某个 flag 的值（读不到返回空串）。
// 用于「验证配置真的生效」——读回运行中进程的 argv 才叫验证过。
func H3ArgValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}
