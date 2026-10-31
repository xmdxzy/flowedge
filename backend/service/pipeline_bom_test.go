package service

import (
	"os"
	"path/filepath"
	"testing"
)

// 带 UTF-8 BOM 的 pipelines.json 必须能加载（2026-10-31 实测：Windows 编辑器会写 BOM，
// encoding/json 不认 → 整条产品线起不来，报错还很有误导性「invalid character 'ï'」）。
// 同时要求：去 BOM 后**内容语义不变**（不是宽松到接受任何脏数据）。
func TestLoadPipelines_ToleratesBOM(t *testing.T) {
	src := filepath.Join("..", "..", "data", "pipelines.json")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读样例失败：%v", err)
	}
	dir := t.TempDir()

	clean := filepath.Join(dir, "clean.json")
	if err := os.WriteFile(clean, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	withBOM := filepath.Join(dir, "bom.json")
	if err := os.WriteFile(withBOM, append([]byte{0xEF, 0xBB, 0xBF}, raw...), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := LoadPipelines(clean)
	if err != nil {
		t.Fatalf("无 BOM 应加载成功：%v", err)
	}
	b, err := LoadPipelines(withBOM)
	if err != nil {
		t.Fatalf("带 BOM 应加载成功（去 BOM 后解析）：%v", err)
	}
	la, lb := a.List(), b.List()
	if len(la) != len(lb) {
		t.Fatalf("流水线条数不一致：%d vs %d", len(la), len(lb))
	}
	for i := range la {
		if la[i].ID != lb[i].ID {
			t.Errorf("第 %d 条流水线 id 不一致：%s vs %s", i, la[i].ID, lb[i].ID)
		}
	}

	// 真正坏掉的 JSON 仍必须报错（容错不能变成吞错）
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{\"pipelines\": [ }"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPipelines(broken); err == nil {
		t.Errorf("坏 JSON 必须报错，不能静默通过")
	}
}
