package service

import (
	"testing"

	"flowedge/backend/model"
)

func TestShotNeedsLowVRAM(t *testing.T) {
	cases := []struct {
		name   string
		data   map[string]interface{}
		w, h   int
		frames int
		want   bool
	}{
		{"base 608x352x124 不开", nil, 608, 352, 124, false},
		{"高工作量 736x416x124 自动开", nil, 736, 416, 124, true},
		{"竖屏 512x896x73 自动开", nil, 512, 896, 73, true},
		{"显式 true 覆盖基准", map[string]interface{}{"low_vram": true}, 608, 352, 124, true},
		{"显式 false 覆盖高工作量", map[string]interface{}{"low_vram": false}, 736, 416, 124, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := &model.NodeInstance{Data: c.data}
			if got := shotNeedsLowVRAM(n, c.w, c.h, c.frames); got != c.want {
				t.Fatalf("shotNeedsLowVRAM(%dx%d x%d) = %v, want %v", c.w, c.h, c.frames, got, c.want)
			}
		})
	}
}
