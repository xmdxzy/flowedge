package service

import (
	"strings"
	"testing"

	"gorm.io/gorm"

	"flowedge/backend/model"
)

// 晋升台账的单测。判据不是"能存能取"，而是**牙齿在不在**：
// 非法记录必须被拒绝（而不是存进去等着审计时猜），
// 合法记录必须原样读得回来（数值快照不许丢）。

func newPromoTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := memDB(t)
	if err := db.AutoMigrate(&model.Promotion{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestPromotionRecord_ListLatest(t *testing.T) {
	db := newPromoTestDB(t)
	st := NewPromotionStore(db)

	// 空台账：Latest 必须明确说"没有"，而不是返回零值记录
	if _, ok := st.Latest(1); ok {
		t.Errorf("空台账的 Latest 应返回 ok=false")
	}
	if got := st.List(1, 0); len(got) != 0 {
		t.Errorf("空台账的 List 应返回空切片，收到 %d 条", len(got))
	}

	recs := []model.Promotion{
		{WorkflowID: 7, Action: model.PromotionActionArchive, Source: model.PromotionSourceDecide,
			Verdict: "keep_baseline", Pairs: 9, GateScoreDelta: -0.01,
			Reason: "decide 归档：verdict=keep_baseline", Actor: "engine"},
		{WorkflowID: 7, Action: model.PromotionActionPromote, Source: model.PromotionSourcePromote,
			FromVersion: 3, ToVersion: 4, Pairs: 9, GateScoreDelta: 0.041,
			TokenDeltaPct: -2.5, LatencyDeltaPct: 1.2, CriticalRegressions: 0,
			Reason: "门分提升 4.1%，9 对 8 胜", Actor: "张越"},
		{WorkflowID: 9, Action: model.PromotionActionKeep, Source: model.PromotionSourcePromote,
			Reason: "token 涨幅超上限，维持基线"},
	}
	for i := range recs {
		if err := st.Record(recs[i]); err != nil {
			t.Fatalf("Record[%d] 失败：%v", i, err)
		}
	}

	// 别的 workflow 的记录不许混进来
	got := st.List(7, 0)
	if len(got) != 2 {
		t.Fatalf("workflow 7 应有 2 条记录，收到 %d", len(got))
	}
	// 新的在前
	if got[0].Action != model.PromotionActionPromote {
		t.Errorf("最新一条应是 promote，收到 %s", got[0].Action)
	}
	// 数值快照必须原样读回（复核时靠这些数，丢一个都等于没记）
	if got[0].GateScoreDelta != 0.041 || got[0].Pairs != 9 || got[0].TokenDeltaPct != -2.5 {
		t.Errorf("数值快照丢失：%+v", got[0])
	}
	if got[0].Actor != "张越" {
		t.Errorf("Actor 应原样读回，收到 %q", got[0].Actor)
	}

	latest, ok := st.Latest(7)
	if !ok || latest.ToVersion != 4 {
		t.Errorf("Latest(7) 应是 promote 到 v4，收到 ok=%v to=%d", ok, latest.ToVersion)
	}
}

// 校验牙齿：每一条都对应一种真实会发生的事故。
func TestPromotionRecord_Validation(t *testing.T) {
	db := newPromoTestDB(t)
	st := NewPromotionStore(db)

	cases := []struct {
		name string
		rec  model.Promotion
		want string // 错误里必须出现的子串（空 = 必须通过）
	}{
		{
			name: "action 不在枚举内",
			rec:  model.Promotion{WorkflowID: 1, Action: "promotee", Source: model.PromotionSourcePromote, Reason: "x"},
			want: "action 必须是",
		},
		{
			name: "source 不在枚举内",
			rec:  model.Promotion{WorkflowID: 1, Action: model.PromotionActionKeep, Source: "webhook", Reason: "x"},
			want: "source 必须是",
		},
		{
			name: "空理由",
			rec:  model.Promotion{WorkflowID: 1, Action: model.PromotionActionKeep, Source: model.PromotionSourcePromote, Reason: "   "},
			want: "reason 必填",
		},
		{
			name: "promote 缺 toVersion",
			rec:  model.Promotion{WorkflowID: 1, Action: model.PromotionActionPromote, Source: model.PromotionSourcePromote, Reason: "x"},
			want: "toVersion",
		},
		{
			name: "rollback 缺 toVersion",
			rec:  model.Promotion{WorkflowID: 1, Action: model.PromotionActionRollback, Source: model.PromotionSourceRollback, FromVersion: 5, Reason: "x"},
			want: "toVersion",
		},
		{
			name: "workflowId=0",
			rec:  model.Promotion{Action: model.PromotionActionKeep, Source: model.PromotionSourcePromote, Reason: "x"},
			want: "workflowId 必填",
		},
		{
			name: "负版本号",
			rec:  model.Promotion{WorkflowID: 1, Action: model.PromotionActionKeep, Source: model.PromotionSourcePromote, Reason: "x", FromVersion: -1},
			want: "不能为负",
		},
		{
			name: "合法 keep（零版本号放行）",
			rec:  model.Promotion{WorkflowID: 1, Action: model.PromotionActionKeep, Source: model.PromotionSourcePromote, Reason: "维持基线"},
		},
		{
			name: "合法 archive（引擎归档，零版本号放行）",
			rec:  model.Promotion{WorkflowID: 1, Action: model.PromotionActionArchive, Source: model.PromotionSourceDecide, Verdict: "inconclusive", Reason: "样本不足"},
		},
	}
	for _, c := range cases {
		err := st.Record(c.rec)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s：应通过，收到 %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s：应被拒绝，却落库了", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：错误信息应包含 %q，收到 %v", c.name, c.want, err)
		}
	}

	// 被拒绝的记录不许落库：台账里只应有 2 条合法的
	if got := st.List(1, 0); len(got) != 2 {
		t.Errorf("拒绝的记录不应落库：台账应有 2 条，收到 %d", len(got))
	}
}

// Actor 为空按 manual 记；CreatedAt 由存储层打。
func TestPromotionRecord_Defaults(t *testing.T) {
	db := newPromoTestDB(t)
	st := NewPromotionStore(db)
	if err := st.Record(model.Promotion{
		WorkflowID: 3, Action: model.PromotionActionKeep, Source: model.PromotionSourcePromote,
		Reason: "没理由不动",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rec, ok := st.Latest(3)
	if !ok {
		t.Fatal("应能读回")
	}
	if rec.Actor != "manual" {
		t.Errorf("Actor 空值应按 manual 记，收到 %q", rec.Actor)
	}
	if rec.CreatedAt.IsZero() {
		t.Errorf("CreatedAt 应由存储层打上")
	}
}

// nil store（db 没装配）不许 panic：进化是旁路，不能因为它把主链路打挂。
func TestPromotionStore_NilSafe(t *testing.T) {
	var st *PromotionStore
	if err := st.Record(model.Promotion{WorkflowID: 1, Action: model.PromotionActionKeep, Source: model.PromotionSourcePromote, Reason: "x"}); err == nil {
		t.Errorf("nil store 的 Record 应返回 error，不是 panic")
	}
	if got := st.List(1, 0); len(got) != 0 {
		t.Errorf("nil store 的 List 应返回空")
	}
	if _, ok := st.Latest(1); ok {
		t.Errorf("nil store 的 Latest 应 ok=false")
	}
}

// Decide 归档：引擎说 promote 也只记 archive —— 人没按的那一下，
// 台账不许替人按。
func TestFromDecision_ArchivesEngineVerdict(t *testing.T) {
	d := Decision{Verdict: "promote", Pairs: 9, Better: 8, Worse: 0, Tie: 1,
		GateScoreDelta: 0.041, CriticalRegressions: 0}
	rec := FromDecision(113, d, "门分提升")
	if rec.Action != model.PromotionActionArchive {
		t.Errorf("decide 归档必须记 archive，收到 %s", rec.Action)
	}
	if rec.Verdict != "promote" {
		t.Errorf("verdict 应原样搬，收到 %s", rec.Verdict)
	}
	if rec.Actor != "engine" {
		t.Errorf("引擎归档的 actor 应是 engine，收到 %s", rec.Actor)
	}
	if rec.Source != model.PromotionSourceDecide {
		t.Errorf("source 应是 decide，收到 %s", rec.Source)
	}
}

// Monitor 归档：退化结论里的 RollbackTo 必须进台账（那是"该滚回哪版"的唯一记录）。
func TestFromRegression_KeepsRollbackTarget(t *testing.T) {
	rep := RegressionReport{
		Verdict: "regressed", CandidateVersion: 12, RollbackTo: 10,
		Decision: Decision{Verdict: "keep_baseline", Pairs: 9, GateScoreDelta: -0.02},
	}
	rec := FromRegression(113, rep, "退化")
	if rec.FromVersion != 12 || rec.ToVersion != 10 {
		t.Errorf("from/to 应搬 12→10，收到 %d→%d", rec.FromVersion, rec.ToVersion)
	}
	if rec.Action != model.PromotionActionArchive || rec.Verdict != "regressed" {
		t.Errorf("monitor 归档口径不对：action=%s verdict=%s", rec.Action, rec.Verdict)
	}
}
