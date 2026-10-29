package service

import "testing"

func TestSetArgValue(t *testing.T) {
	base := []string{"main.py", "--port", "8189", "--reserve-vram", "4", "--cache-none"}

	got := setArgValue(base, "--reserve-vram", "1")
	if v := H3ArgValue(got, "--reserve-vram"); v != "1" {
		t.Fatalf("应替换成 1，实际 %q（args=%v）", v, got)
	}
	if len(got) != len(base) {
		t.Fatalf("替换不该改变长度：%d -> %d（%v）", len(base), len(got), got)
	}
	// 其它参数必须原样保留（顺序也不能乱）：base 是 6 个元素
	if got[0] != "main.py" || got[2] != "8189" || got[5] != "--cache-none" {
		t.Fatalf("其它参数被改动了：%v", got)
	}

	// 没有这个 flag 时追加
	bare := []string{"main.py", "--port", "8189"}
	got2 := setArgValue(bare, "--reserve-vram", "2")
	if v := H3ArgValue(got2, "--reserve-vram"); v != "2" {
		t.Fatalf("缺 flag 时应追加，实际 %q（%v）", v, got2)
	}
}

func TestApplyH3Config(t *testing.T) {
	base := []string{"main.py", "--listen", "127.0.0.1", "--port", "8189", "--reserve-vram", "4"}
	one := 1

	// 没配 → 保持默认 4
	if v := H3ArgValue(applyH3Config(base, nil, nil), "--reserve-vram"); v != "4" {
		t.Fatalf("未配置时应保持默认 4，实际 %q", v)
	}
	// 配了 1 → 覆盖
	if v := H3ArgValue(applyH3Config(base, nil, &one), "--reserve-vram"); v != "1" {
		t.Fatalf("配置 1 应生效，实际 %q", v)
	}
	// 负数视为无效（保持默认），避免写出 --reserve-vram -1 这种会直接让进程退出的参数
	neg := -1
	if v := H3ArgValue(applyH3Config(base, nil, &neg), "--reserve-vram"); v != "4" {
		t.Fatalf("负数应被忽略，实际 %q", v)
	}
	// 整体覆盖：args 段优先，且脚本路径仍然在最前
	over := applyH3Config(base, []string{"--port", "8189", "--reserve-vram", "1", "--cache-none"}, nil)
	if over[0] != "main.py" {
		t.Fatalf("整体覆盖后 main.py 仍应在最前，实际 %v", over)
	}
	if v := H3ArgValue(over, "--reserve-vram"); v != "1" {
		t.Fatalf("整体覆盖应生效，实际 %q（%v）", v, over)
	}
}

func TestDropArg(t *testing.T) {
	base := []string{"main.py", "--listen", "127.0.0.1", "--reserve-vram", "0", "--cache-none"}
	got := dropArg(base, "--cache-none")
	for _, a := range got {
		if a == "--cache-none" {
			t.Fatalf("--cache-none 应被删掉：%v", got)
		}
	}
	if len(got) != len(base)-1 {
		t.Fatalf("只该少一个元素：%v → %v", base, got)
	}
	if got[0] != "main.py" {
		t.Fatalf("脚本路径必须留在最前：%v", got)
	}
	// 没有该开关时是空操作
	if again := dropArg(got, "--cache-none"); len(again) != len(got) {
		t.Fatalf("重复删除不该改变长度：%v", again)
	}
}

func TestH3ArgValue_Missing(t *testing.T) {
	if v := H3ArgValue([]string{"main.py"}, "--reserve-vram"); v != "" {
		t.Fatalf("没有该 flag 时应返回空串，实际 %q", v)
	}
}
