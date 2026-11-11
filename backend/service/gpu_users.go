package service

// 外部 GPU 占用探测（2026-10-31）
//
// 由来：一次本该 85–120s 的单镜跑了 12 分钟并撞上节点超时。逐层排查后真相是
// **同一张卡上跑着 UnrealEditor（GPU 利用率 100%）**，H3 的动态显存加载被迫反复换页。
//
// 现有 `vram_advisory.go` 只算「显存够不够」，`HasGPUWork()` 只看「自家队列有没有活」——
// 两者都回答不了「这卡现在被谁占着」。缺了这一问，用户看到的只有「今天怎么这么慢」。
//
// 本文件只做「查 + 说清楚」，不拦任务：外部占用可能正是用户要用的东西（游戏/编辑器），
// 拦下来等于替用户做决定。但它必须在**开跑前**说出来，而不是烧完 12 分钟再解释。

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GPUUser 一个占用 GPU 的进程。
type GPUUser struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
}

// ParseNvidiaSMIComputeApps 解析 `nvidia-smi --query-compute-apps=pid,process_name --format=csv,noheader`。
//
// 容错：驱动版本不同会带不带表头、名字里带逗号（路径）、显存列 N/A —— 都按「第一个逗号前是 pid」处理。
func ParseNvidiaSMIComputeApps(out string) []GPUUser {
	var users []GPUUser
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 表头行
		if strings.HasPrefix(strings.ToLower(line), "pid") {
			continue
		}
		parts := strings.SplitN(line, ",", 2)
		if len(parts) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || pid <= 0 {
			continue
		}
		name := strings.TrimSpace(parts[1])
		// 去掉可能跟着的显存列（"xxx.exe, [N/A]"）
		if i := strings.LastIndex(name, ","); i > 0 {
			if tail := strings.TrimSpace(name[i+1:]); strings.Contains(tail, "MiB") || strings.Contains(tail, "N/A") {
				name = strings.TrimSpace(name[:i])
			}
		}
		users = append(users, GPUUser{PID: pid, Name: name})
	}
	return users
}

// nvidiaSMIComputeApps 跑一次 nvidia-smi。失败返回空（**不报错**：
// 没有 nvidia-smi 的环境下这只是少一条提醒，不该让出片失败）。
func nvidiaSMIComputeApps(ctx context.Context) []GPUUser {
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	out, err := hiddenExecContext(cctx, "nvidia-smi",
		"--query-compute-apps=pid,process_name", "--format=csv,noheader").Output()
	if err != nil {
		return nil
	}
	return ParseNvidiaSMIComputeApps(string(out))
}

// ExternalGPUUsers 返回**不是自家进程**的 GPU 占用者。
//
// ownPids 传本进程与自家托管依赖的 pid（引擎自己、ComfyUI、TTS、ASR、Ollama）。
// 传空切片等于「所有占用都算外部」——调用方要清楚这一点。
func ExternalGPUUsers(ctx context.Context, ownPids []int) []GPUUser {
	own := make(map[int]bool, len(ownPids))
	for _, p := range ownPids {
		if p > 0 {
			own[p] = true
		}
	}
	var out []GPUUser
	for _, u := range nvidiaSMIComputeApps(ctx) {
		if own[u.PID] {
			continue
		}
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// DescribeExternalGPUUsers 把外部占用说成人话（空 = 没有外部占用，返回空串）。
//
// 刻意点名进程名：只说「有外部占用」等于没说——用户得知道关哪个。
func DescribeExternalGPUUsers(users []GPUUser) string {
	if len(users) == 0 {
		return ""
	}
	names := make([]string, 0, len(users))
	for _, u := range users {
		base := filepath.Base(strings.ReplaceAll(u.Name, "\\", "/"))
		names = append(names, fmt.Sprintf("%s(pid=%d)", base, u.PID))
	}
	return "检测到外部进程占用 GPU：" + strings.Join(names, "、") +
		"。同一张卡上跑 GPU 密集型程序（编辑器/游戏/另一个推理服务）会让 H3 退化成动态换页，" +
		"单镜可能从 1–2 分钟变成 10 分钟以上并撞节点超时；跑片前建议先关掉它们"
}

// Supervisor.PIDs 返回本进程托管的依赖 pid（用于把「自家占用」从外部占用里排除）。
//
// 为什么需要它：ComfyUI/TTS 就是吃显存的自家进程，把它们报成「外部占用」
// 会让提醒永远在响，等于没有提醒。
func (s *Supervisor) PIDs() []int {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, 0, len(s.procs))
	for _, cmd := range s.procs {
		if cmd != nil && cmd.Process != nil {
			out = append(out, cmd.Process.Pid)
		}
	}
	return out
}
