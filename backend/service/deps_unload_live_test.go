package service

import (
	"os"
	"testing"
)

// 真机验证：对**真实**的 ComfyUI-H3（8189）调一次卸载接口。
//
// 为什么单列一个文件并加门禁：这个测试会真的让线上实例释放模型。
// 空闲时它是安全的（本来就没加载），但如果正在出片，它会把那一镜打断。
// 所以默认 skip，只在明确知道机器空闲时手动跑：
//
//	$env:FLOWEDGE_LIVE_GPU=1; go test ./service/ -run TestUnload_LiveComfyH3 -v
//
// 单测能证明「逻辑对」，但证明不了「真服务器接受这个请求、返回的形状能被解析」——
// 后者只有跑一次才知道（§0.13：代码已落地 ≠ 效果已验证）。
func TestUnload_LiveComfyH3(t *testing.T) {
	if os.Getenv("FLOWEDGE_LIVE_GPU") != "1" {
		t.Skip("跳过真机卸载验证：设 FLOWEDGE_LIVE_GPU=1 且确认当前没有出片任务")
	}
	s := newBareSupervisor()
	s.specs["comfy_h3"] = &DepSpec{
		Name: "comfy_h3", Label: "ComfyUI-H3",
		Endpoint:   "http://127.0.0.1:8189",
		HealthPath: "/system_stats",
	}
	if !s.Healthy("comfy_h3") {
		t.Skip("8189 未启动，跳过真机验证")
	}

	res, err := s.Unload("comfy_h3")
	if err != nil {
		t.Fatalf("真机卸载失败：%v", err)
	}
	if !res.Supported {
		t.Fatalf("ComfyUI 应支持卸载，实际 supported=%v note=%s", res.Supported, res.Note)
	}
	t.Logf("真机结果：freed=%dMB free=%dMB note=%s", res.FreedMB, res.FreeMB, res.Note)
}
