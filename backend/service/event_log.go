package service

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	"flowedge/backend/model"
)

// skippedEventTypes 高频事件不入库：node_token 每字一个事件，
// 同步写库会把 SQLite 写爆并拖慢引擎，导致流式测试时序抖动。
var skippedEventTypes = map[string]bool{"node_token": true}

// TokenUsage 一次运行的 token 消耗
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// EventLogger 执行事件持久化。
//
// 设计要点：
//  1. Emit 只入内存缓冲（同步代价极低），后台协程每 100ms / 每 64 条批量落库；
//  2. 落库失败只记日志不阻塞引擎；
//  3. node_token 直接跳过，避免写放大；
//  4. WS 广播保持原有非阻塞语义（广播不因落库而丢）。
type EventLogger struct {
	db   *gorm.DB
	mu   sync.Mutex
	buf  []model.RunEvent
	seq  map[uint]int
	stop chan struct{}
	once sync.Once
}

func NewEventLogger(db *gorm.DB) *EventLogger {
	l := &EventLogger{
		db:   db,
		buf:  make([]model.RunEvent, 0, 128),
		seq:  make(map[uint]int),
		stop: make(chan struct{}),
	}
	go l.loop()
	return l
}

// Emit 记录一条事件（runID=0 表示与运行无关的独立节点执行，不入库）
func (l *EventLogger) Emit(runID uint, ev Event) {
	if runID == 0 || skippedEventTypes[ev.Type] {
		return
	}
	var data json.RawMessage
	if ev.Data != nil {
		if b, err := json.Marshal(ev.Data); err == nil {
			data = b
		}
	}

	l.mu.Lock()
	l.seq[runID]++
	l.buf = append(l.buf, model.RunEvent{
		RunID:     runID,
		Seq:       l.seq[runID],
		Type:      ev.Type,
		NodeID:    ev.NodeID,
		Message:   ev.Message,
		Data:      data,
		CreatedAt: time.Now(),
	})
	n := len(l.buf)
	l.mu.Unlock()

	if n >= 64 {
		go l.Flush()
	}
}

func (l *EventLogger) loop() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.Flush()
		case <-l.stop:
			l.Flush()
			return
		}
	}
}

// Flush 把缓冲批量写入数据库
func (l *EventLogger) Flush() {
	l.mu.Lock()
	if len(l.buf) == 0 {
		l.mu.Unlock()
		return
	}
	batch := l.buf
	l.buf = make([]model.RunEvent, 0, 128)
	l.mu.Unlock()

	if l.db == nil {
		return
	}
	if err := l.db.CreateInBatches(batch, 64).Error; err != nil {
		log.Printf("[EventLog] flush %d events failed: %v", len(batch), err)
	}
}

// Close 停止后台协程（写入前会做最后一次 flush）
func (l *EventLogger) Close() {
	l.once.Do(func() { close(l.stop) })
}

// ListByRun 按顺序取某次运行的事件（trace 视图数据源）
func (l *EventLogger) ListByRun(runID uint, limit int) ([]model.RunEvent, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	var out []model.RunEvent
	err := l.db.Where("run_id = ?", runID).Order("seq ASC").Limit(limit).Find(&out).Error
	return out, err
}

// UsageOfEvents 从事件流累加 token 消耗（node_finished 的 output._usage）
func UsageOfEvents(events []model.RunEvent) TokenUsage {
	var u TokenUsage
	for _, e := range events {
		if e.Type != "node_finished" || len(e.Data) == 0 {
			continue
		}
		var m struct {
			Output struct {
				Usage struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"_usage"`
			} `json:"output"`
		}
		if err := json.Unmarshal(e.Data, &m); err != nil {
			continue
		}
		u.PromptTokens += m.Output.Usage.PromptTokens
		u.CompletionTokens += m.Output.Usage.CompletionTokens
	}
	return u
}
