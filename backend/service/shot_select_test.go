package service

import (
	"reflect"
	"testing"
)

// only_shots 的解析：宁可报错也不猜。
//
// 代价不对称：猜成"全都跑"要烧十几分钟 GPU；猜成"一个都不跑"会得到一条空片。
func TestParseShotSpec(t *testing.T) {
	ok := []struct {
		in   interface{}
		want []int
	}{
		{"", nil},
		{nil, nil},
		{"  ", nil},
		{"2", []int{2}},
		{"2,3", []int{2, 3}},
		{"2，3", []int{2, 3}}, // 中文逗号（手打很自然会用）
		{"2、3", []int{2, 3}}, // 顿号
		{"2 3", []int{2, 3}}, // 空格
		{" 1 ; 4 ", []int{1, 4}},
		{float64(3), []int{3}}, // API/MCP 直接传数字
	}
	for _, c := range ok {
		got, err := parseShotSpec(c.in)
		if err != nil {
			t.Fatalf("parseShotSpec(%v) 不该报错：%v", c.in, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("parseShotSpec(%v) = %v，期望 %v", c.in, sortedShotIDs(got), c.want)
		}
		for _, w := range c.want {
			if !got[w] {
				t.Fatalf("parseShotSpec(%v) 缺镜号 %d", c.in, w)
			}
		}
	}

	bad := []interface{}{"abc", "0", "-1", "2,x", "2.5", float64(2.5), float64(0)}
	for _, in := range bad {
		if _, err := parseShotSpec(in); err == nil {
			t.Fatalf("parseShotSpec(%v) 应该报错（镜号必须是从 1 开始的整数）", in)
		}
	}
}

// 未勾选的镜能不能靠缓存复用：键为空就说明它属于"不进缓存"的三类。
func TestShotSkipBlocker(t *testing.T) {
	if got := shotSkipBlocker("abc123"); got != "" {
		t.Fatalf("有缓存键时应可复用，实际阻塞：%s", got)
	}
	if got := shotSkipBlocker(""); got == "" {
		t.Fatal("没有缓存键时必须给出阻塞原因（否则会静默重烧或静默跳过）")
	}
}

// 日志/报错里的镜号顺序必须稳定：map 遍历是随机的，同一件事两次跑打出不同顺序
// 会让人误以为分镜表变了。
func TestSortedShotIDsStable(t *testing.T) {
	m := map[int]bool{5: true, 1: true, 3: true}
	want := []int{1, 3, 5}
	for i := 0; i < 20; i++ {
		if got := sortedShotIDs(m); !reflect.DeepEqual(got, want) {
			t.Fatalf("sortedShotIDs 顺序不稳定：%v", got)
		}
	}
}
