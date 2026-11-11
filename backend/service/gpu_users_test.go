package service

import (
	"context"
	"strings"
	"testing"
)

func TestParseNvidiaSMIComputeApps(t *testing.T) {
	// 真实输出形态（带表头 + 路径含反斜杠 + 显存 N/A）
	raw := `pid, process_name
3752, C:\Windows\System32\dwm.exe
11204, D:\Epic\UE_5.8\UE_5.8\Engine\Binaries\Win64\UnrealEditor.exe
27952, C:\Users\you\AppData\Local\Programs\Python\Python311\python.exe
`
	got := ParseNvidiaSMIComputeApps(raw)
	if len(got) != 3 {
		t.Fatalf("应解析 3 个进程，实际 %d：%+v", len(got), got)
	}
	if got[1].PID != 11204 || !strings.Contains(got[1].Name, "UnrealEditor") {
		t.Errorf("第 2 条解析错误：%+v", got[1])
	}

	// 带显存列（有版本会带 MiB）
	raw2 := `1234, /usr/bin/python, 5120 MiB
`
	got2 := ParseNvidiaSMIComputeApps(raw2)
	if len(got2) != 1 || got2[0].PID != 1234 {
		t.Fatalf("带显存列解析失败：%+v", got2)
	}
	if strings.Contains(got2[0].Name, "MiB") {
		t.Errorf("进程名不应包含显存列：%q", got2[0].Name)
	}

	// 脏数据不能 panic、不能编造
	for _, bad := range []string{"", "\n\n", "garbage", "abc, def", "0, x"} {
		if got := ParseNvidiaSMIComputeApps(bad); len(got) != 0 {
			t.Errorf("脏输入 %q 应返回空，实际 %+v", bad, got)
		}
	}
}

// 自家进程必须被排除，否则每条提醒都在说「ComfyUI 占了 GPU」——那是废话。
func TestExternalGPUUsers_FiltersOwn(t *testing.T) {
	raw := `100, engine.exe
200, python.exe
300, UnrealEditor.exe
`
	all := ParseNvidiaSMIComputeApps(raw)
	if len(all) != 3 {
		t.Fatalf("前置条件错：%d", len(all))
	}

	own := map[int]bool{100: true, 200: true}
	var ext []GPUUser
	for _, u := range all {
		if !own[u.PID] {
			ext = append(ext, u)
		}
	}
	if len(ext) != 1 || ext[0].PID != 300 {
		t.Fatalf("应只剩 1 个外部进程，实际 %+v", ext)
	}
}

func TestDescribeExternalGPUUsers(t *testing.T) {
	if s := DescribeExternalGPUUsers(nil); s != "" {
		t.Errorf("无外部占用应返回空串，实际 %q", s)
	}
	s := DescribeExternalGPUUsers([]GPUUser{{PID: 11204, Name: `D:\Epic\UE_5.8\Engine\Binaries\Win64\UnrealEditor.exe`}})
	if !strings.Contains(s, "UnrealEditor.exe") {
		t.Errorf("必须点名进程（用户要知道关哪个），实际：%q", s)
	}
	if !strings.Contains(s, "11204") {
		t.Errorf("应带 pid 便于定位，实际：%q", s)
	}
	// 不能说「已拦下」——本功能只提醒不拦
	if strings.Contains(s, "已拦") {
		t.Errorf("只提醒不拦，措辞不能暗示拦截：%q", s)
	}
}

// 无 nvidia-smi 时不能报错、不能阻塞出片（返回空即可）。
func TestExternalGPUUsers_NoSMI(t *testing.T) {
	// PATH 置空，保证找不到 nvidia-smi
	t.Setenv("PATH", "")
	users := ExternalGPUUsers(context.Background(), nil)
	if len(users) != 0 {
		t.Logf("环境里居然有 nvidia-smi：%+v", users)
	}
}
