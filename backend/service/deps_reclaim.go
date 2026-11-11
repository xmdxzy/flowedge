package service

// 显存回收（reclaim）：把「任务结束后显存还不还」变成一条可执行的动作
//
// 由来（2026-10-31 实测）：一条 H3 任务超时后，ComfyUI 侧**并没有停**——
// 它卡在 VAE decode 里，`/interrupt` 返回 200 但队列里那条仍是 running，
// `POST /queue {"delete":[...]}` 也删不掉，`/free` 更是无效（还在跑就不会卸载）。
// 结果：任务早就失败了，显存仍被占 21GB，用户看到的是「跑完了显存不还」。
//
// 所以「释放显存」不能只是一次 /free，必须是三步：
//   1) 清队列（把 pending 和 running 的 id 都删掉）
//   2) /free（unload_models + free_memory）
//   3) **测量**：显存真的涨了吗？没涨就如实说「没释放」，而不是报成功
//
// 第 3 步是这条路径存在的意义：AGENTS 已记过「假卸载」（返回 ok 但显存没还），
// 同样的坑不能再踩一次。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// VramReclaimResult 一次回收的结果（前后对比，便于判断「到底释放了没有」）。
type VramReclaimResult struct {
	Name         string `json:"name"`
	QueueCleared int    `json:"queue_cleared"` // 删掉的队列项数
	FreeBeforeMB int    `json:"free_before_mb"`
	FreeAfterMB  int    `json:"free_after_mb"`
	GainedMB     int    `json:"gained_mb"`
	// Released 只有「显存确实涨了」才为 true。请求成功但显存没动 = 没释放。
	Released bool   `json:"released"`
	Note     string `json:"note,omitempty"`
}

// IsReleased 判定「是否真的释放了」：只看显存涨没涨，不看请求成没成功。
//
// 单列成方法而不是让调用方各自写 `GainedMB > 256`：阈值只该有一个定义，
// 否则「多少算释放」会漂成两套口径（AGENTS §0.12 ③ 的同源问题）。
func (r VramReclaimResult) IsReleased() bool { return r.GainedMB > 256 }

// comfyQueueIDs 读出 ComfyUI 队列里所有 prompt id（running + pending）。
func comfyQueueIDs(base string) []string {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(base + "/queue")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	// ComfyUI 的队列结构：{"queue_running":[[num, id, ...]], "queue_pending":[...]}
	var q struct {
		Running [][]interface{} `json:"queue_running"`
		Pending [][]interface{} `json:"queue_pending"`
	}
	if json.Unmarshal(body, &q) != nil {
		return nil
	}
	var ids []string
	for _, group := range [][][]interface{}{q.Running, q.Pending} {
		for _, item := range group {
			if len(item) > 1 {
				if id, ok := item[1].(string); ok && id != "" {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}

// deleteComfyQueue 删除指定队列项。返回实际发出的删除条数。
func deleteComfyQueue(base string, ids []string) int {
	if len(ids) == 0 {
		return 0
	}
	payload, _ := json.Marshal(map[string]interface{}{"delete": ids})
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Post(base+"/queue", "application/json", bytes.NewReader(payload))
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	return len(ids)
}

// Reclaim 回收某个依赖占用的显存：清队列 → /free（或 /unload）→ 测量。
//
// 对 comfy 类：清队列 + /free。对 tts：/unload（它没有队列概念）。
func (s *Supervisor) ReclaimVRAM(name string) (VramReclaimResult, error) {
	res := VramReclaimResult{Name: name}
	spec, ok := s.Spec(name)
	if !ok {
		return res, fmt.Errorf("未知依赖 %q", name)
	}
	base := spec.Endpoint
	if base == "" {
		return res, fmt.Errorf("%s 没有配置 base_url，无法回收", name)
	}

	res.FreeBeforeMB = s.deviceFreeVRAMMB()

	if isComfySpec(name) {
		// 先 interrupt（尽力而为：卡在 VAE decode 时它不会停，但不试就不知道）
		_ = postEmpty(base, "/interrupt")
		ids := comfyQueueIDs(base)
		res.QueueCleared = deleteComfyQueue(base, ids)
		// 再 /free：带 unload_models，才会真正卸载权重
		_ = postJSON(base, "/free", map[string]interface{}{
			"unload_models": true,
			"free_memory":   true,
		})
	} else {
		// TTS 这类走 /unload（进程保留、权重释放）
		_ = postEmpty(base, "/unload")
	}

	// 测量：给驱动 3 秒把显存还回来（CUDA 释放是异步的）
	time.Sleep(3 * time.Second)
	res.FreeAfterMB = s.deviceFreeVRAMMB()
	res.GainedMB = res.FreeAfterMB - res.FreeBeforeMB
	res.Released = res.IsReleased()

	switch {
	case res.Released:
		res.Note = fmt.Sprintf("已释放 %d MB", res.GainedMB)
	case res.QueueCleared > 0:
		res.Note = "队列已清，但显存没动：任务可能卡在不可中断的阶段（VAE decode）。" +
			"确认没有别的任务在跑后，直接杀掉该依赖的进程再 start（`taskkill /PID <pid> /T /F`）"
	default:
		res.Note = "显存没动，且队列本来就是空的：可能权重已被别的进程持有，" +
			"或该服务当前没有加载模型（这种情况不需要回收）"
	}
	return res, nil
}

// isComfySpec ComfyUI 系（有队列、走 /free）。
func isComfySpec(name string) bool {
	return name == "comfyui" || name == "comfy_h3" || name == "comfy_moss"
}

// postJSON POST 一个 JSON 体（失败忽略：回收是尽力而为，结果由测量决定）。
func postJSON(base, path string, body interface{}) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(base+path, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}
