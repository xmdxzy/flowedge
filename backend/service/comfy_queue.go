package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// ComfyUI 队列卫生：**中止必须真的中止**。
//
// 由来（2026-10-26 实测，两次都是白烧 GPU）：
//   - run 224：看门狗在 5m35s 触发 /interrupt，但那条 prompt 其实还在**排队**没开始执行，
//     /interrupt 只作用于"正在跑"的那一条 → 什么都没停。它 08:47:27 才开始跑，
//     21 分钟后才返回；引擎 15 分钟节点超时先到，那一整段 GPU 时间全白烧。
//   - run 225：08:56 提交，排在上一轮的幽灵 prompt 后面，09:02:43 才开始执行 → 先排 6.4 分钟，
//     再被 12 分钟节点超时掐掉。两次提交都撞在"队列里有别人"上。
//
// 所以"中止"这个动作要拆成三件事，缺一不可：
//  1. 删掉还没开始跑的那条（POST /queue {"delete":[id]}）
//  2. 停掉正在跑的那条（POST /interrupt）
//  3. 复核（再读 /queue，确认 id 真的不在了；不在了才算中止成功）
//
// 队列操作单独用一个 3 秒超时的客户端：取消/超时路径上绝不能被 HTTP 拖住。

var queueHTTPClient = &http.Client{Timeout: 3 * time.Second}

// comfyQueueItem 是 /queue 里的一个元素：[number, prompt_id, prompt, extra, outputs]。
// 只需要第 2 个字段（prompt_id），其余用 RawMessage 兜住，避免因版本差异解析失败。
type comfyQueueItem []json.RawMessage

func (it comfyQueueItem) promptID() string {
	if len(it) < 2 {
		return ""
	}
	var id string
	if err := json.Unmarshal(it[1], &id); err != nil {
		return ""
	}
	return id
}

type comfyQueueState struct {
	Running []comfyQueueItem `json:"queue_running"`
	Pending []comfyQueueItem `json:"queue_pending"`
}

func (st *comfyQueueState) ids(items []comfyQueueItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if id := it.promptID(); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// QueueIDs 返回（运行中, 排队中）的 prompt_id 列表。
func (s *Text2ImageService) QueueIDs(base string) (running, pending []string, err error) {
	if strings.TrimSpace(base) == "" {
		return nil, nil, fmt.Errorf("no comfyui base")
	}
	resp, err := queueHTTPClient.Get(strings.TrimRight(base, "/") + "/queue")
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("GET /queue → %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var st comfyQueueState
	if err := json.Unmarshal(body, &st); err != nil {
		return nil, nil, fmt.Errorf("/queue 解析失败：%w", err)
	}
	return st.ids(st.Running), st.ids(st.Pending), nil
}

// QueueBusy 只回答"这个实例现在有几条在跑 / 几条在排"。探不到一律返回 0,0——
// 调用方把这个数当提示用，不能因为探活失败影响出片主流程。
func (s *Text2ImageService) QueueBusy(base string) (running, pending int) {
	r, p, err := s.QueueIDs(base)
	if err != nil {
		return 0, 0
	}
	return len(r), len(p)
}

// postQueue 给 /queue 发一条指令（clear / delete）。
func postQueue(base string, payload map[string]interface{}) error {
	raw, _ := json.Marshal(payload)
	resp, err := queueHTTPClient.Post(strings.TrimRight(base, "/")+"/queue", "application/json", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST /queue → %d", resp.StatusCode)
	}
	return nil
}

// DeleteQueued 把一条**还没开始执行**的提交从队列里删掉。
// 返回 true 表示删完复核确认它已经不在了（或本来就不在）。
func (s *Text2ImageService) DeleteQueued(base, promptID string) bool {
	if strings.TrimSpace(promptID) == "" {
		return false
	}
	if err := postQueue(base, map[string]interface{}{"delete": []string{promptID}}); err != nil {
		log.Printf("[queue] 删除 pending %s 失败：%v", shortID(promptID), err)
		return false
	}
	// 复核：真正删掉才算数（ComfyUI 对不存在的 id 也回 200）
	_, pending, err := s.QueueIDs(base)
	if err != nil {
		return false
	}
	for _, id := range pending {
		if id == promptID {
			return false
		}
	}
	return true
}

// ClearPendingQueue 丢弃该实例里**所有还没开始跑**的提交（孤儿）。
//
// 什么时候用：引擎启动时（上一轮进程崩了，ComfyUI 里的 pending 是孤儿），
// 以及节点超时/取消后（我们只关心自己的那条，但队列里剩下的同样没人要）。
// 返回丢弃条数。
func (s *Text2ImageService) ClearPendingQueue(base string) int {
	if strings.TrimSpace(base) == "" {
		return 0
	}
	_, before, err := s.QueueIDs(base)
	if err != nil {
		return 0
	}
	if len(before) == 0 {
		return 0
	}
	if err := postQueue(base, map[string]interface{}{"clear": true}); err != nil {
		log.Printf("[queue] 清空 pending 失败：%v", err)
		return 0
	}
	_, after, verr := s.QueueIDs(base)
	if verr != nil {
		return len(before)
	}
	return len(before) - len(after)
}

// Interrupt 停掉正在执行的那一条。
//
// ComfyUI 的 /interrupt 语义：停"当前执行中"的 prompt。带 prompt_id 参数是较新版本的
// 精确中止，老版本会忽略参数——两种情况都能接受，因为调用方之后会复核队列。
func (s *Text2ImageService) Interrupt(base, promptID string) bool {
	if strings.TrimSpace(base) == "" {
		return false
	}
	payload := map[string]interface{}{}
	if promptID != "" {
		payload["prompt_id"] = promptID
	}
	raw, _ := json.Marshal(payload)
	resp, err := queueHTTPClient.Post(strings.TrimRight(base, "/")+"/interrupt", "application/json", bytes.NewReader(raw))
	if err != nil {
		log.Printf("[queue] interrupt 失败：%v", err)
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode == http.StatusOK
}

// AbortPrompt 精确中止一次提交，并**复核**它真的没了。
//
// 顺序不能反：先删 pending（还在排的话 /interrupt 对它无效），再停 running。
// 返回（是否已不在队列, 是否曾观察到在跑）。
func (s *Text2ImageService) AbortPrompt(base, promptID string) (gone bool, wasRunning bool) {
	if strings.TrimSpace(promptID) == "" {
		return false, false
	}
	running, pending, err := s.QueueIDs(base)
	if err != nil {
		// 队列读不到也要尽力而为：至少把 interrupt 打过去
		s.Interrupt(base, promptID)
		return false, false
	}
	inPending, inRunning := false, false
	for _, id := range pending {
		if id == promptID {
			inPending = true
		}
	}
	for _, id := range running {
		if id == promptID {
			inRunning = true
		}
	}
	if inPending {
		s.DeleteQueued(base, promptID)
	}
	if inRunning || inPending {
		s.Interrupt(base, promptID)
	}
	// 复核一遍：最多等 6 秒，看它是否真的离开队列
	for i := 0; i < 3; i++ {
		time.Sleep(2 * time.Second)
		r2, p2, err := s.QueueIDs(base)
		if err != nil {
			return false, inRunning
		}
		still := false
		for _, id := range append(append([]string{}, r2...), p2...) {
			if id == promptID {
				still = true
			}
		}
		if !still {
			return true, inRunning
		}
		if i == 2 {
			log.Printf("[queue] 中止未生效：prompt %s 仍在队列里（interrupt 被忽略或正在收尾）", shortID(promptID))
		}
	}
	return false, inRunning
}

// shortID 日志里只打前 8 位，够定位又不刷屏。
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// containsID 判断 prompt_id 是否在列表里。
func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
