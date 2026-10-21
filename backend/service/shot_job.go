package service

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"flowedge/backend/model"
)

// ShotJobStore 镜头级作业的持久化（见 model.ShotJob 的背景说明）。
//
// db 不可用时返回 nil，调用方必须 nil-safe 降级为「不落盘」：
// 单测与「独立执行一个节点」这种没有 run 的路径不应该依赖数据库。
type ShotJobStore struct{ db *gorm.DB }

func NewShotJobStore(db *gorm.DB) *ShotJobStore {
	if db == nil {
		return nil
	}
	return &ShotJobStore{db: db}
}

// SetShotJobs 注入镜头级作业仓库（db 为 nil 时传 nil，自动降级为「不落盘」）
func (e *EngineV2) SetShotJobs(store *ShotJobStore) {
	if e != nil {
		e.shotJobs = store
	}
}

// Shots 某次运行某节点的镜头作业（无 store 时返回 nil，调用方按空处理）
func (e *EngineV2) Shots(runID uint, nodeID string) []model.ShotJob {
	if e == nil {
		return nil
	}
	return e.shotJobs.ListByNode(runID, nodeID)
}

// interruptComfy 让 ComfyUI 立刻停手。
//
// 只为「取消」服务：引擎的取消原先只在层边界检查 ctx，而长片单镜要跑 3–5 分钟，
// 用户点了停止仍得等当前镜跑完。POST /interrupt 能让正在采样的任务立刻中断。
// 主实例（8188）与 H3（8189）各发一份、尽力而为：打断失败只记日志，
// 不能因为「打断失败」让取消本身失败。
func (e *EngineV2) interruptComfy() {
	if e == nil || e.t2i == nil {
		return
	}
	client := &http.Client{Timeout: 3 * time.Second}
	for _, base := range []string{e.t2i.BaseURL, e.t2i.H3BaseURL} {
		if strings.TrimSpace(base) == "" {
			continue
		}
		go func(b string) {
			url := strings.TrimRight(b, "/") + "/interrupt"
			resp, err := client.Post(url, "application/json", nil)
			if err != nil {
				log.Printf("[cancel] %s 未响应：%v（实例可能已停）", url, err)
				return
			}
			resp.Body.Close()
			log.Printf("[cancel] 已请求 %s → %d", url, resp.StatusCode)
		}(base)
	}
}

// shotPromptHash 复用判据：提示词 + 影响画面的关键参数。
// 任何一项变了都必须重生成，否则会出现「改了提示词却拿到旧画面」这种最坏的静默错误。
func shotPromptHash(prompt string, frames, width, height, steps int, turbo bool) string {
	raw := fmt.Sprintf("%s|%d|%d|%d|%d|%t", prompt, frames, width, height, steps, turbo)
	sum := sha1.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Lookup 找一条可复用的成功镜头。
// 除状态外还要求文件仍在盘上——产物被清理过就不能复用，否则会装配出空片段。
func (s *ShotJobStore) Lookup(runID uint, nodeID string, idx int, hash string) (*model.ShotJob, bool) {
	if s == nil || runID == 0 {
		return nil, false
	}
	var job model.ShotJob
	// 用 Limit(1).Find 而不是 First：未命中是正常情况（首次运行/改了提示词），
	// First 会把 ErrRecordNotFound 打到日志，每次复用检查都喷一行红字。
	s.db.
		Where("run_id = ? AND node_id = ? AND shot_index = ? AND prompt_hash = ? AND status = ?",
			runID, nodeID, idx, hash, "success").
		Order("id DESC").Limit(1).Find(&job)
	if job.ID == 0 {
		return nil, false
	}
	if _, statErr := os.Stat(job.File); statErr != nil {
		return nil, false
	}
	return &job, true
}

// Record 写入/覆盖一条镜头作业（同一 run+node+index 只留最新一条）
func (s *ShotJobStore) Record(job model.ShotJob) {
	if s == nil || job.RunID == 0 {
		return
	}
	var existing model.ShotJob
	s.db.
		Where("run_id = ? AND node_id = ? AND shot_index = ?", job.RunID, job.NodeID, job.ShotIndex).
		Order("id DESC").Limit(1).Find(&existing)
	if existing.ID != 0 {
		job.ID = existing.ID
		s.db.Model(&model.ShotJob{}).Where("id = ?", existing.ID).Updates(map[string]interface{}{
			"shot_id": job.ShotID, "prompt_hash": job.PromptHash, "prompt": job.Prompt,
			"file": job.File, "frames": job.Frames, "seconds": job.Seconds,
			"transition": job.Transition, "seed": job.Seed, "status": job.Status,
			"error": job.Error, "note": job.Note, "elapsed_ms": job.ElapsedMS,
		})
		return
	}
	_ = s.db.Create(&job).Error
}

// ListByNode 取本次运行该节点的全部镜头（写进节点输出，供界面按镜查看/挑选）
func (s *ShotJobStore) ListByNode(runID uint, nodeID string) []model.ShotJob {
	if s == nil || runID == 0 {
		return nil
	}
	var out []model.ShotJob
	_ = s.db.Where("run_id = ? AND node_id = ?", runID, nodeID).Order("shot_index ASC").Find(&out).Error
	return out
}

// ShotBudget 单镜时间预算。
//
// 基准来自 AGENTS §5.10 实测：608×352 / 12 步 / 2 张参考图，单帧 0.85–0.93 秒。
// 这里按 1.2 秒/帧（约 30% 余量）+ 90 秒固定开销（上传参考图/实例切换/音频治理），
// 不再用「3 分钟/镜」这种过时常数——那是 12 步 169s 时代的估算，
// 而 §5.10 已经出现单段 556s 的异常争用，固定常数会把合法任务砍断。
func ShotBudget(frames int) time.Duration {
	if frames <= 0 {
		frames = 124
	}
	return time.Duration(float64(frames)*1.2)*time.Second + 90*time.Second
}

// ShotListBudget 整节点预算 = 各镜预算之和 + 装配/自检/插帧。
func ShotListBudget(shotCount, totalFrames int) time.Duration {
	if shotCount <= 0 {
		return 2 * time.Hour
	}
	d := time.Duration(float64(totalFrames)*1.2)*time.Second +
		time.Duration(shotCount)*90*time.Second +
		10*time.Minute
	if d < 15*time.Minute {
		d = 15 * time.Minute
	}
	return d
}

// parseReroll 解析 reroll 字段：支持 "3" / "1,3" / "1-3"。镜号从 1 开始（与分镜表一致）。
func parseReroll(node *model.NodeInstance) map[int]bool {
	out := map[int]bool{}
	if node == nil {
		return out
	}
	raw := ""
	switch v := node.Data["reroll"].(type) {
	case string:
		raw = v
	case float64:
		raw = strconv.Itoa(int(v))
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, err1 := strconv.Atoi(strings.TrimSpace(lo))
			b, err2 := strconv.Atoi(strings.TrimSpace(hi))
			if err1 == nil && err2 == nil {
				for i := a; i <= b; i++ {
					out[i-1] = true // 内部下标从 0 起
				}
			}
			continue
		}
		if n, err := strconv.Atoi(part); err == nil {
			out[n-1] = true
		}
	}
	return out
}

// emitShotProgress 每镜广播一次进度：界面终于能显示「第 3/6 镜」而不是一个转圈的点。
func (e *EngineV2) emitShotProgress(runID uint, nodeID string, idx, total, shotID int, phase string, elapsed time.Duration) {
	if e == nil {
		return
	}
	ev := Event{
		Type:   "shot_progress",
		NodeID: nodeID,
		RunID:  runID,
		Data: map[string]interface{}{
			"index":      idx + 1,
			"total":      total,
			"shot_id":    shotID,
			"phase":      phase, // generating / reused / failed
			"elapsed_ms": elapsed.Milliseconds(),
		},
	}
	if e.wsHub != nil {
		e.wsHub.Broadcast(ev)
	}
	if e.eventLog != nil {
		e.eventLog.Emit(runID, ev)
	}
}
