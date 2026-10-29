package handler

import (
	"encoding/json"
	"testing"
	"time"

	"flowedge/backend/model"
)

// 同一条指令跑第二遍是很常见的浪费：实测 208/209 就是同一份指纹连跑两次，
// 各约 4 分钟 GPU。这里钉住「认得出、且不误认」。
func TestFindDuplicateRun(t *testing.T) {
	db := newTestDB(t)
	p := &Producer{db: db}

	now := time.Now()
	mk := func(id uint, pipeline, status, params string, at time.Time) {
		t.Helper()
		r := &model.WorkflowRun{
			ID: id, PipelineID: pipeline, Status: status,
			Params: json.RawMessage(params), StartedAt: &at,
		}
		if err := db.Create(r).Error; err != nil {
			t.Fatalf("create run %d: %v", id, err)
		}
	}
	// 键序故意写反：语义相同就算同一条指令
	mk(1, "film_from_plan", "success", `{"prompt":"船","width":608,"height":352}`, now.Add(-10*time.Minute))
	mk(2, "film_from_plan", "success", `{"prompt":"别的东西","width":608}`, now.Add(-5*time.Minute))
	mk(3, "video", "success", `{"prompt":"船","width":608,"height":352}`, now.Add(-4*time.Minute))
	mk(4, "film_from_plan", "failed", `{"prompt":"船2","width":608,"height":352}`, now.Add(-3*time.Minute))

	same := marshalParams(map[string]interface{}{
		"height": 352, "width": 608, "prompt": "船",
	})
	id, when := p.findDuplicateRun("film_from_plan", same)
	if id != 1 {
		t.Fatalf("应认出 run 1，实际 %d（when=%s）", id, when)
	}
	if when == "" {
		t.Fatal("要带上时间，否则用户不知道是刚才还是昨天跑的")
	}

	// 不同流水线的同样参数不算重复
	if id, _ := p.findDuplicateRun("image", same); id != 0 {
		t.Fatalf("跨流水线不该算重复，实际 %d", id)
	}
	// 失败的运行不算「已出片」
	failedParams := marshalParams(map[string]interface{}{"prompt": "船2", "width": 608, "height": 352})
	if id, _ := p.findDuplicateRun("film_from_plan", failedParams); id != 0 {
		t.Fatalf("失败的运行不该算已成功出片，实际 %d", id)
	}
	// 空参数不查（避免把所有空参数的运行误判成重复）
	if id, _ := p.findDuplicateRun("film_from_plan", nil); id != 0 {
		t.Fatalf("空参数不该命中，实际 %d", id)
	}
}

// 参数比较必须无视键序与空白，否则「同一条指令」会被判成不同。
func TestJSONEqualRaw(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{`{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{`{"a":1}`, ` { "a" : 1 } `, true},
		{`{"a":1}`, `{"a":2}`, false},
		{`{"a":1}`, ``, false},
		{`not-json`, `{"a":1}`, false},
	}
	for _, c := range cases {
		if got := jsonEqualRaw([]byte(c.a), []byte(c.b)); got != c.want {
			t.Fatalf("%q vs %q = %v，want %v", c.a, c.b, got, c.want)
		}
	}
}

// 同一份 Values 序列化必须稳定（map 键序由 encoding/json 排序保证）；
// 不稳定的话「同参数」每天都会被判成不同。
func TestMarshalParamsStable(t *testing.T) {
	v := map[string]interface{}{"z": 1, "a": 2, "m": map[string]interface{}{"y": 1, "b": 2}}
	first := marshalParams(v)
	for i := 0; i < 20; i++ {
		if got := marshalParams(v); string(got) != string(first) {
			t.Fatalf("第 %d 次序列化结果不同：%s vs %s", i, got, first)
		}
	}
}
