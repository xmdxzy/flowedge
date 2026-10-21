package service

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"

	"flowedge/backend/model"
)

// 产物血缘（Lineage）
//
// 解决的问题：一条 mp4 出来之后，没人说得清它由哪张图、哪段配音、哪个提示词构成。
// 以前只能靠文件名猜。这里在运行结束时把「产物 → 节点 → 上游产物」写进表，
// 画廊点开任何一件产物都能看到完整来源链。

// ArtifactLineage 一件产物的来源记录
type ArtifactLineage struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	Name       string    `gorm:"size:255;uniqueIndex" json:"name"` // basename，与画廊条目对齐
	Kind       string    `gorm:"size:16" json:"kind"`              // image / audio / video
	WorkflowID uint      `gorm:"index" json:"workflow_id"`
	RunID      uint      `gorm:"index" json:"run_id"`
	NodeID     string    `gorm:"size:64" json:"node_id"`
	NodeType   string    `gorm:"size:32" json:"node_type"`
	Parents    string    `gorm:"type:text" json:"parents"` // JSON []string（直接上游产物文件名）
	Meta       string    `gorm:"type:text" json:"meta"`    // JSON：prompt / text / 参数
	CreatedAt  time.Time `json:"created_at"`
}

// LineageStore 血缘读写
type LineageStore struct {
	db *gorm.DB
}

func NewLineageStore(db *gorm.DB) *LineageStore {
	if db == nil {
		return nil
	}
	return &LineageStore{db: db}
}

// mediaOutputs 从节点输出里挑出产物字段（image/audio/video）
func mediaOutputs(out interface{}) map[string]string {
	res := map[string]string{}
	m, ok := out.(map[string]interface{})
	if !ok {
		return res
	}
	for _, k := range []string{"image", "audio", "video"} {
		if v, ok := m[k].(string); ok && v != "" {
			res[k] = v
		}
	}
	return res
}

func lineageKind(field string) string {
	switch field {
	case "audio":
		return "audio"
	case "video":
		return "video"
	default:
		return "image"
	}
}

// passthroughOfUpstream：白名单也可能写错（新节点把上游文件原样返回）。
// 这里加一道结构判断——如果本节点产出的媒体文件名全部已出现在上游生产者的产出里，
// 说明它没造新文件，只是搬运，不该覆盖真正的生产者记录。
func passthroughOfUpstream(media map[string]string, preds []string, outputs map[string]interface{}, types map[string]string) bool {
	upstream := map[string]bool{}
	for _, pid := range preds {
		if !mediaProducerTypes[types[pid]] {
			continue
		}
		for _, p := range mediaOutputs(outputs[pid]) {
			upstream[filepath.Base(p)] = true
		}
	}
	if len(upstream) == 0 {
		return false
	}
	for _, p := range media {
		if !upstream[filepath.Base(p)] {
			return false
		}
	}
	return true
}

// RecordRun 在一次运行结束后落库血缘（幂等：同名产物覆盖更新）
func (s *LineageStore) RecordRun(workflowID, runID uint, nodes []model.NodeInstance, edges []model.EdgeInstance) int {
	if s == nil {
		return 0
	}
	preds := make(map[string][]string, len(nodes))
	for _, e := range edges {
		preds[e.Target] = append(preds[e.Target], e.Source)
	}
	outputs := make(map[string]interface{}, len(nodes))
	types := make(map[string]string, len(nodes))
	for _, n := range nodes {
		types[n.ID] = n.Type
		if n.Status == "success" {
			outputs[n.ID] = n.Output
		}
	}

	written := 0
	for _, n := range nodes {
		if n.Status != "success" || !mediaProducerTypes[n.Type] {
			continue
		}
		media := mediaOutputs(n.Output)
		if len(media) == 0 {
			continue
		}
		if passthroughOfUpstream(media, preds[n.ID], outputs, types) {
			continue
		}
		// 直接上游里「真正产出媒体」的节点才算父节点（跳过透传节点）
		var parents []string
		for _, pid := range preds[n.ID] {
			if !mediaProducerTypes[types[pid]] {
				continue
			}
			for _, p := range mediaOutputs(outputs[pid]) {
				parents = append(parents, filepath.Base(p))
			}
		}
		meta := map[string]interface{}{}
		for _, k := range []string{"prompt", "text", "width", "height", "image_duration", "voice"} {
			if v, ok := n.Data[k]; ok {
				meta[k] = v
			}
		}
		parentsJSON, _ := json.Marshal(parents)
		meta["node_id"] = n.ID
		meta["node_type"] = n.Type
		metaJSON, _ := json.Marshal(meta)

		for field, p := range media {
			rec := ArtifactLineage{
				Name:       filepath.Base(p),
				Kind:       lineageKind(field),
				WorkflowID: workflowID,
				RunID:      runID,
				NodeID:     n.ID,
				NodeType:   n.Type,
				Parents:    string(parentsJSON),
				Meta:       string(metaJSON),
				CreatedAt:  time.Now(),
			}
			// 同名覆盖：重跑同一工作流不该在血缘表里堆出一堆重复行
			var existing ArtifactLineage
			// 用 Find 而不是 First：First 找不到会打一条 record not found 噪音日志
			res := s.db.Where("name = ?", rec.Name).Limit(1).Find(&existing)
			if res.Error == nil && existing.ID != 0 {
				rec.ID = existing.ID
				rec.CreatedAt = existing.CreatedAt
				s.db.Save(&rec)
			} else {
				s.db.Create(&rec)
			}
			written++
		}
	}
	return written
}

// Get 查一件产物的血缘：自身 + 逐层上游
func (s *LineageStore) Get(name string) (ArtifactLineage, []ArtifactLineage, bool) {
	var self ArtifactLineage
	if s == nil || s.db == nil {
		return self, nil, false
	}
	if res := s.db.Where("name = ?", name).Limit(1).Find(&self); res.Error != nil || self.ID == 0 {
		return self, nil, false
	}
	ancestors := make([]ArtifactLineage, 0, 4)
	seen := map[string]bool{self.Name: true}
	queue := decodeParents(self.Parents)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		var rec ArtifactLineage
		if res := s.db.Where("name = ?", cur).Limit(1).Find(&rec); res.Error == nil && rec.ID != 0 {
			ancestors = append(ancestors, rec)
			queue = append(queue, decodeParents(rec.Parents)...)
		}
	}
	return self, ancestors, true
}

// List 最近的血缘记录（画廊可批量展示来源）
func (s *LineageStore) List(limit int) []ArtifactLineage {
	out := []ArtifactLineage{}
	if s == nil || s.db == nil {
		return out
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	s.db.Order("id desc").Limit(limit).Find(&out)
	return out
}

func decodeParents(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}
