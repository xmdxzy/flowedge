package service

import (
	"os"
	"strings"
	"testing"
)

// 托管进程必须拿到一个**确定可写**的 TEMP。
//
// 由来（2026-10-26）：引擎拉起的 H3 一起就退，日志里是
// "No usable temporary directory found in [...\Temp, ...\Temp\2]"，
// 而同一个 python 从交互 shell 跑完全正常 —— 差别只在"谁派生的它"。
// 所以不猜权限细节，直接把 TEMP/TMP/TMPDIR 指到项目内目录。
func TestEnsureTempEnv_OverridesTempVars(t *testing.T) {
	env := []string{"PATH=C:\\Windows", "TEMP=C:\\bad\\temp", "TMP=C:\\bad\\tmp2", "TMPDIR=C:\\bad\\tmp3", "FOO=bar"}
	out := ensureTempEnv(env)

	var temps []string
	keptFoo := false
	for _, kv := range out {
		up := strings.ToUpper(kv)
		switch {
		case strings.HasPrefix(up, "TEMP="), strings.HasPrefix(up, "TMP="), strings.HasPrefix(up, "TMPDIR="):
			temps = append(temps, kv)
		case kv == "FOO=bar":
			keptFoo = true
		}
	}
	if len(temps) != 3 {
		t.Fatalf("应恰好保留 3 个临时目录变量，实际 %v", temps)
	}
	for _, kv := range temps {
		if strings.Contains(kv, `C:\bad`) {
			t.Fatalf("旧的坏临时目录必须被丢掉：%s", kv)
		}
	}
	if !keptFoo {
		t.Fatal("其他环境变量不能被顺手丢掉")
	}
	// 指向的目录必须真实存在（否则等于没修）
	dir := strings.TrimPrefix(temps[0], "TEMP=")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("TEMP 目录没建出来：%s（%v）", dir, err)
	}
	// 空环境也要能用（继承 os.Environ）
	if len(ensureTempEnv(nil)) == 0 {
		t.Fatal("空环境时也要返回一份可用的 env")
	}
}
