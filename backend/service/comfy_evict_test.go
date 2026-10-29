package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGPU 一张假的 24GB 卡：各实例各自驻留一部分，/system_stats 报**设备级**空闲。
//
// 这一点必须仿真：真实 ComfyUI 的 vram_free 是整卡可用量（别的实例占掉的在这里
// 就体现成「少了」），而腾显存的效果也正是靠这个数变化来确认的。
// 早期版本让每个假实例各报各的空闲，结果「释放兄弟后目标仍报 4800」——
// 那是假模型错了，不是被测逻辑错了。
type fakeGPU struct {
	total int64
	mu    sync.Mutex
	held  map[string]int64
}

func newFakeGPU(totalMB int) *fakeGPU {
	return &fakeGPU{total: int64(totalMB), held: map[string]int64{}}
}

func (g *fakeGPU) freeMB() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	used := int64(0)
	for _, v := range g.held {
		used += v
	}
	return g.total - used
}

func (g *fakeGPU) release(id string) {
	g.mu.Lock()
	g.held[id] = 0
	g.mu.Unlock()
}

// fakeComfy 一个只会 /system_stats 与 /free 的假实例。
type fakeComfy struct {
	srv       *httptest.Server
	gpu       *fakeGPU
	freeCalls atomic.Int32
	statsUp   atomic.Bool
}

func newFakeComfy(t *testing.T, gpu *fakeGPU, heldMB int) *fakeComfy {
	t.Helper()
	f := &fakeComfy{gpu: gpu}
	f.statsUp.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("/system_stats", func(w http.ResponseWriter, r *http.Request) {
		if !f.statsUp.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		st := map[string]interface{}{
			"devices": []map[string]interface{}{
				{"vram_free": f.gpu.freeMB() * (1 << 20)},
			},
		}
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("/free", func(w http.ResponseWriter, r *http.Request) {
		f.freeCalls.Add(1)
		f.gpu.release(f.srvID())
		_, _ = w.Write([]byte(`{}`))
	})
	f.srv = httptest.NewServer(mux)
	// 服务起来后才有地址可当身份，此时再登记驻留量。
	f.gpu.mu.Lock()
	f.gpu.held[f.srv.URL] = int64(heldMB)
	f.gpu.mu.Unlock()
	t.Cleanup(f.srv.Close)
	return f
}

// srvID 用地址当实例身份。
func (f *fakeComfy) srvID() string { return f.srv.URL }

// registryFor 造一份只含测试实例的注册表（名字不与内置重名，避免被 merge 补条目）。
func registryFor(t *testing.T, items map[string]string) *InstanceRegistry {
	t.Helper()
	type inst struct {
		Name    string `json:"name"`
		BaseURL string `json:"base_url"`
		Enabled bool   `json:"enabled"`
	}
	var list []inst
	for name, url := range items {
		list = append(list, inst{Name: name, BaseURL: url, Enabled: true})
	}
	p := filepath.Join(t.TempDir(), "instances.json")
	raw, _ := json.Marshal(map[string]interface{}{"default": "t-main", "instances": list})
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatalf("write instances: %v", err)
	}
	return LoadInstanceRegistry(p, "")
}

func withEvictEnabled(t *testing.T) {
	t.Helper()
	old := VRAMEvictEnabled()
	SetVRAMEvict(true)
	t.Cleanup(func() { SetVRAMEvict(old) })
}

// 空闲显存够的时候，一个 /free 都不许发。
//
// 这条是本次优化的核心收益：同实例连续跑（image→image、shot→shot）时
// 热权重必须留着，否则每张图都要重付 183 秒冷加载——那比 OOM 更常见也更贵。
func TestEvictSiblings_SkipWhenEnoughFreeVRAM(t *testing.T) {
	withEvictEnabled(t)
	gpu := newFakeGPU(24576)
	target := newFakeComfy(t, gpu, 1000)
	sib := newFakeComfy(t, gpu, 1000)

	s := &Text2ImageService{Instances: registryFor(t, map[string]string{
		"t-main":  target.srv.URL,
		"t-other": sib.srv.URL,
	})}

	freed, free := s.evictSiblingsFor(target.srv.URL)
	if len(freed) != 0 {
		t.Fatalf("空闲充足时不该驱逐任何实例，实际 %v", freed)
	}
	if free != 22576 {
		t.Fatalf("应回报实测设备级空闲 22576MB，实际 %d", free)
	}
	if n := sib.freeCalls.Load(); n != 0 {
		t.Fatalf("空闲够时不该调 /free，实际调用 %d 次", n)
	}
}

// 空闲不够时：驱逐兄弟实例，但绝不驱逐目标自己；腾完要等显存真的回来。
func TestEvictSiblings_EvictsOthersNotTarget(t *testing.T) {
	withEvictEnabled(t)
	gpu := newFakeGPU(24576)
	// 目标自己只占 4GB，另两个兄弟占掉 17GB —— 正是「主实例残留在 8188、
	// 现在要跑 H3」的现场。
	target := newFakeComfy(t, gpu, 4000)
	sibA := newFakeComfy(t, gpu, 12000)
	sibB := newFakeComfy(t, gpu, 5000)

	s := &Text2ImageService{Instances: registryFor(t, map[string]string{
		"t-main": target.srv.URL,
		"t-a":    sibA.srv.URL,
		"t-b":    sibB.srv.URL,
	})}

	freed, free := s.evictSiblingsFor(target.srv.URL)
	if len(freed) != 2 {
		t.Fatalf("应驱逐两个兄弟实例，实际 %v", freed)
	}
	if n := sibA.freeCalls.Load(); n != 1 {
		t.Fatalf("t-a 应被 /free 一次，实际 %d", n)
	}
	if n := sibB.freeCalls.Load(); n != 1 {
		t.Fatalf("t-b 应被 /free 一次，实际 %d", n)
	}
	if n := target.freeCalls.Load(); n != 0 {
		t.Fatalf("目标实例不能驱逐自己（正在跑的就是它），实际调用 %d 次", n)
	}
	// 腾完之后应等显存真的回来，而不是 /free 一返回就去提交。
	if free != 20576 {
		t.Fatalf("腾完应有 20576MB 空闲（24576-4000），实际 %d", free)
	}
}

// 探不到目标实例的显存时不动手：不知道卡上有多少东西，驱逐谁都是猜。
func TestEvictSiblings_NoProbeNoAction(t *testing.T) {
	withEvictEnabled(t)
	gpu := newFakeGPU(24576)
	target := newFakeComfy(t, gpu, 1000)
	target.statsUp.Store(false)
	sib := newFakeComfy(t, gpu, 18000)

	s := &Text2ImageService{Instances: registryFor(t, map[string]string{
		"t-main":  target.srv.URL,
		"t-other": sib.srv.URL,
	})}

	freed, free := s.evictSiblingsFor(target.srv.URL)
	if len(freed) != 0 || free != 0 {
		t.Fatalf("探不到显存时不该动手，实际 freed=%v free=%d", freed, free)
	}
	if n := sib.freeCalls.Load(); n != 0 {
		t.Fatalf("探不到显存时不该 /free，实际 %d 次", n)
	}
}

// 兄弟实例没起（连接失败）只当它不占显存，不影响整条路径。
func TestEvictSiblings_ToleratesDeadSibling(t *testing.T) {
	withEvictEnabled(t)
	gpu := newFakeGPU(24576)
	target := newFakeComfy(t, gpu, 4000)
	sib := newFakeComfy(t, gpu, 9000)
	sibURL := sib.srv.URL
	sib.srv.Close() // 变成连不上的地址

	s := &Text2ImageService{Instances: registryFor(t, map[string]string{
		"t-main": target.srv.URL,
		"t-dead": sibURL,
	})}

	freed, _ := s.evictSiblingsFor(target.srv.URL)
	if len(freed) != 0 {
		t.Fatalf("连不上的实例不该计成已释放，实际 %v", freed)
	}
}

// 逃生阀：关掉之后一个请求都不发。
func TestEvictSiblings_KillSwitch(t *testing.T) {
	SetVRAMEvict(false)
	t.Cleanup(func() { SetVRAMEvict(true) })
	gpu := newFakeGPU(24576)
	target := newFakeComfy(t, gpu, 4000)
	sib := newFakeComfy(t, gpu, 18000)

	s := &Text2ImageService{Instances: registryFor(t, map[string]string{
		"t-main":  target.srv.URL,
		"t-other": sib.srv.URL,
	})}

	freed, free := s.evictSiblingsFor(target.srv.URL)
	if len(freed) != 0 || free != 0 {
		t.Fatalf("关闭后不该有任何动作，实际 freed=%v free=%d", freed, free)
	}
	if n := sib.freeCalls.Load(); n != 0 {
		t.Fatalf("关闭后不该 /free，实际 %d 次", n)
	}
}

// 同一实例连续提交必须零开销：不发探测、不发 /free、不等待。
//
// 场景：film 8 镜都在 8189 上跑，而 H3 自己就占着 20GB，设备级空闲永远
// 到不了 21GB 阈值。若每次提交都去探测+等显存涨回来，会在 8 个镜头上白等
// 8×N 秒——耗时表上看不出来，只是「今天怎么慢了」。
func TestEvictSiblings_SameBaseCostsNothing(t *testing.T) {
	withEvictEnabled(t)
	gpu := newFakeGPU(24576)
	target := newFakeComfy(t, gpu, 20000) // H3 热着
	sib := newFakeComfy(t, gpu, 0)

	s := &Text2ImageService{Instances: registryFor(t, map[string]string{
		"t-main":  target.srv.URL,
		"t-other": sib.srv.URL,
	})}

	// 第一发：换实例，可能动手（这里空闲 4576MB 会触发一次驱逐尝试）
	// 第二发起：同一实例，必须什么都不做。
	done := make(chan struct{})
	go func() { s.evictSiblingsFor(target.srv.URL); close(done) }()
	<-done
	before := sib.freeCalls.Load()

	start := time.Now()
	freed, free := s.evictSiblingsFor(target.srv.URL)
	elapsed := time.Since(start)

	if len(freed) != 0 || free != 0 {
		t.Fatalf("同实例连跑不该有动作，实际 freed=%v free=%d", freed, free)
	}
	if n := sib.freeCalls.Load(); n != before {
		t.Fatalf("同实例连跑不该再发 /free，实际多了 %d 次", n-before)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("同实例连跑应即时返回，实际耗时 %v", elapsed)
	}
}

// 腾完之后显存若到不了阈值（目标自己占着大头），不能等满 12 秒。
func TestEvictSiblings_StopsWhenVRAMStopsRising(t *testing.T) {
	withEvictEnabled(t)
	gpu := newFakeGPU(24576)
	target := newFakeComfy(t, gpu, 18000) // 目标自己就占 18GB，腾完仍到不了 21GB
	sib := newFakeComfy(t, gpu, 5000)

	s := &Text2ImageService{Instances: registryFor(t, map[string]string{
		"t-main":  target.srv.URL,
		"t-other": sib.srv.URL,
	})}

	start := time.Now()
	freed, free := s.evictSiblingsFor(target.srv.URL)
	elapsed := time.Since(start)

	if len(freed) != 1 {
		t.Fatalf("应驱逐那个 5GB 的兄弟实例，实际 %v", freed)
	}
	if free != 6576 {
		t.Fatalf("腾完应有 6576MB（24576-18000），实际 %d", free)
	}
	// 阈值到不了，靠「停止上涨」退出；必须远快于 12 秒上限。
	if elapsed > 5*time.Second {
		t.Fatalf("显存停止上涨后应提前返回，实际等了 %v", elapsed)
	}
}
