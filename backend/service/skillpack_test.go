package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 项目根：go test 的工作目录是 backend/service
func projectRootForTest(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestParseSkillMetaPlain(t *testing.T) {
	raw := []byte("---\nname: demo\ndescription: 一句话说明\nread_when:\n  - 场景一\n  - 场景二\n---\n\n# 正文\n")
	m := parseSkillMeta(raw)
	if m.name != "demo" {
		t.Fatalf("name = %q", m.name)
	}
	if m.description != "一句话说明" {
		t.Fatalf("description = %q", m.description)
	}
	if len(m.readWhen) != 2 || m.readWhen[0] != "场景一" {
		t.Fatalf("read_when = %v", m.readWhen)
	}
}

// 真实文件普遍带 BOM，且有块标量 description: |。两种都必须过得去，
// 否则扫描会把一半技能包读成"没描述"。
func TestParseSkillMetaBOMAndBlockScalar(t *testing.T) {
	raw := []byte("\ufeff---\nname: blk\ndescription: |\n  第一行\n  第二行\ntitle: x\n---\n")
	m := parseSkillMeta(raw)
	if m.name != "blk" {
		t.Fatalf("name = %q（BOM 没去掉）", m.name)
	}
	if !strings.Contains(m.description, "第一行") || !strings.Contains(m.description, "第二行") {
		t.Fatalf("块标量没拼全: %q", m.description)
	}
}

func TestParseSkillMetaNoFrontmatter(t *testing.T) {
	m := parseSkillMeta([]byte("# 只有正文\n"))
	if m.name != "" || m.description != "" {
		t.Fatalf("没有 frontmatter 却解析出内容: %+v", m)
	}
}

// 扫描：正常包、已停用包（只有 _DEPRECATED.md）、隐藏目录都要处理对。
func TestScanSkillPacks(t *testing.T) {
	root := t.TempDir()
	mk := func(name, file, body string) {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if file != "" {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("alpha", "SKILL.md", "---\nname: alpha\ndescription: A\n---\n")
	mk("beta", "_DEPRECATED.md", "已停用")
	mk(".hidden", "SKILL.md", "---\nname: hidden\n---\n")
	mk("empty", "", "")

	packs := ScanSkillPacks([]string{root}, nil)
	if len(packs) != 2 {
		t.Fatalf("扫到 %d 个，期望 2（alpha + beta）: %+v", len(packs), packs)
	}
	byName := map[string]SkillPack{}
	for _, p := range packs {
		byName[p.Name] = p
	}
	if byName["alpha"].Description != "A" {
		t.Fatalf("alpha 描述没读到: %+v", byName["alpha"])
	}
	if byName["beta"].Verdict != SkillUnclassified {
		// beta 不在判定表里，没有 SKILL.md。这里不锁死结论，只要求它被登记且被标出来
		if byName["beta"].Reason == "" {
			t.Fatalf("beta 未判定却没有 Reason")
		}
	}
	if _, ok := byName[".hidden"]; ok {
		t.Fatal("隐藏目录不该被扫进来")
	}
}

// 同名包先到先得：用户级在前，不能让项目里的旧副本顶掉。
func TestScanSkillPacksDedupFirstWins(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(filepath.Join(d, "same"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(a, "same", "SKILL.md"), []byte("---\nname: same\ndescription: 先\n---\n"), 0o644)
	os.WriteFile(filepath.Join(b, "same", "SKILL.md"), []byte("---\nname: same\ndescription: 后\n---\n"), 0o644)

	packs := ScanSkillPacks([]string{a, b}, nil)
	if len(packs) != 1 {
		t.Fatalf("同名没去重，得到 %d 个", len(packs))
	}
	if packs[0].Description != "先" {
		t.Fatalf("去重取了后来的: %q", packs[0].Description)
	}
}

// 判定表是最容易被写成"看着挺像"的地方。三条硬要求：
// 有理由、mirrored 有证据、adopted 指得到卡。
func TestSkillVerdictTableIsComplete(t *testing.T) {
	for name, v := range skillVerdicts {
		if strings.TrimSpace(v.Reason) == "" {
			t.Errorf("%s 没写 Reason", name)
		}
		switch v.Verdict {
		case SkillMirrored:
			if len(v.Evidence) == 0 {
				t.Errorf("%s 判为 mirrored 却没给证据", name)
			}
		case SkillAdopted, SkillPartial:
			if v.Card == "" {
				t.Errorf("%s 判为 %s 却没指卡", name, v.Verdict)
			} else if !cardExists(v.Card) {
				t.Errorf("%s 指的卡 %q 不存在", name, v.Card)
			}
		case SkillHarness, SkillProject, SkillDeprecated:
		default:
			t.Errorf("%s 的判定值非法: %q", name, v.Verdict)
		}
	}
}

// 真实证据必须指得准：文件在、行在、锚点对得上。
// 这条测试会随着被指向文件的编辑而变红——那是设计意图，不是噪音。
func TestSkillEvidenceIsVerifiable(t *testing.T) {
	root := projectRootForTest(t)
	for name, v := range skillVerdicts {
		for _, ev := range v.Evidence {
			if err := VerifyEvidence(root, ev); err != nil {
				t.Errorf("%s 的证据 %q 核验失败: %v", name, ev, err)
			}
		}
	}
}

func TestVerifyEvidenceRejectsBadInput(t *testing.T) {
	root := projectRootForTest(t)
	cases := []struct {
		ev  string
		why string
		ok  bool
	}{
		{"backend/service/cards.go:1#package service", "真实证据", true},
		{"backend/service/not-exist.go:1", "文件不存在", false},
		{"backend/service/cards.go:0", "行号非法", false},
		{"backend/service/cards.go:999999#x", "行号越界", false},
		{"backend/service/cards.go:1#绝对不存在的锚点", "锚点漂移", false},
		{"no-line-number", "没有行号", false},
	}
	for _, c := range cases {
		err := VerifyEvidence(root, c.ev)
		if c.ok && err != nil {
			t.Errorf("%s（%s）应通过，得到 %v", c.ev, c.why, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s（%s）应失败，却通过了", c.ev, c.why)
		}
	}
}

// 核验不过的条目必须降级为 unclassified 且保留原判据，不许悄悄放过。
func TestVerifySkillPacksDowngrades(t *testing.T) {
	packs := []SkillPack{
		{Name: "fake-mirror", Verdict: SkillMirrored, Reason: "自称已实现", Evidence: []string{"backend/service/nope.go:1"}},
		{Name: "fake-adopt", Verdict: SkillAdopted, Reason: "自称收录", Card: "no-such-card"},
		{Name: "no-reason", Verdict: SkillHarness},
	}
	out, problems := VerifySkillPacks(packs, projectRootForTest(t))
	if len(problems) != 3 {
		t.Fatalf("应报 3 个问题，得到 %d: %v", len(problems), problems)
	}
	for _, p := range out {
		if p.Verdict != SkillUnclassified {
			t.Errorf("%s 没被降级，仍是 %q", p.Name, p.Verdict)
		}
		if !strings.Contains(p.Reason, "核验未通过") {
			t.Errorf("%s 降级时丢了原判据说明: %q", p.Name, p.Reason)
		}
	}
}

// 真实判定表过一遍核验必须零问题——否则说明内置判定是假的。
func TestRealVerdictsPassVerification(t *testing.T) {
	var packs []SkillPack
	for name, v := range skillVerdicts {
		packs = append(packs, SkillPack{
			Name: name, Verdict: v.Verdict, Reason: v.Reason,
			Evidence: v.Evidence, Card: v.Card, Tier: v.Tier,
		})
	}
	_, problems := VerifySkillPacks(packs, projectRootForTest(t))
	if len(problems) > 0 {
		t.Fatalf("内置判定核验不过：%v", strings.Join(problems, " | "))
	}
}

func TestVerdictCategoryCoversAll(t *testing.T) {
	for _, v := range []string{SkillMirrored, SkillAdopted, SkillPartial, SkillHarness, SkillProject, SkillDeprecated, SkillUnclassified} {
		if VerdictCategory(v) == "" {
			t.Errorf("%q 没有分组名", v)
		}
	}
	if VerdictCategory("whatever") != "未判定" {
		t.Error("未知判定应归入未判定")
	}
}

// 定位修正后（通用超级智能体工作流平台），判定表的保留清单必须满足两条：
// ① 保留的包都能说出"变成了什么能力"（卡 or 引擎已实现）
// ② 被删掉的包一条都不能带 retain=true 的卡关系含糊
func TestCorePacksHaveDestinations(t *testing.T) {
	core := 0
	for name, v := range skillVerdicts {
		if v.Tier != TierCore {
			continue
		}
		core++
		switch v.Verdict {
		case SkillAdopted, SkillPartial:
			if v.Card == "" {
				t.Errorf("%s 是通用能力，但没指向任何卡", name)
			} else if !cardExists(v.Card) {
				t.Errorf("%s 指的卡 %q 不存在", name, v.Card)
			}
		case SkillMirrored:
			if len(v.Evidence) == 0 {
				t.Errorf("%s 是通用能力且判为 mirrored，必须给实现证据", name)
			}
		default:
			t.Errorf("%s 是通用能力，判定却是 %q —— 通用档只放 adopted/partial/mirrored", name, v.Verdict)
		}
	}
	if core < 5 {
		t.Fatalf("通用能力只剩 %d 个，判定表被删坏了", core)
	}
}

// 领域包不要求有卡（它就是资料库），但**必须至少有一个**——否则通用平台就变成了
// "只会出片的工作站"，用户编 UE5/训练/部署的图时手上什么都没有。
// 2026-10-23 扩根后「不收录」不再是孤例：冷存储/DSH 里真有已退役与纯私人的包。
// 守卫意图从「只能有一个」改为「每一个都必须是显式判定且说得出理由」——
// 防的是把领域包悄悄改判成 none，不是禁止 none 存在。
func TestDomainTierIsNotEmpty(t *testing.T) {
	domain, none := 0, 0
	for name, v := range skillVerdicts {
		switch v.Tier {
		case TierDomain:
			domain++
			if strings.TrimSpace(v.Reason) == "" {
				t.Errorf("%s 是领域能力，必须写清什么场景会用", name)
			}
		case TierNone:
			none++
			if strings.TrimSpace(v.Reason) == "" {
				t.Errorf("%s 不收录，必须说清为什么不收", name)
			}
			switch v.Verdict {
			case SkillDeprecated, SkillProject:
			default:
				t.Errorf("%s 不收录但判定是 %q —— none 只放已停用/纯业务私货", name, v.Verdict)
			}
		case TierCore:
		default:
			t.Errorf("%s 的层级非法：%q", name, v.Tier)
		}
	}
	if domain < 5 {
		t.Fatalf("领域能力只有 %d 个 —— 通用平台不该只留通用包", domain)
	}
	if none < 1 {
		t.Fatalf("不收录的一个都没有 —— 判定表被删坏了")
	}
}

// 层级必须覆盖到所有判定表条目：漏一个 Tier 就等于把它静默降级。
func TestEveryVerdictHasTier(t *testing.T) {
	for name, v := range skillVerdicts {
		if v.Tier == "" {
			t.Errorf("%s 没标层级", name)
		}
	}
}

// 四张新的通用角色卡必须在册：它们是这次「收录」的实际产物，
// 少了任何一张，对应的 skill 包就变成了"判为提炼但卡不存在"。
func TestGeneralAgentCardsExist(t *testing.T) {
	for _, id := range []string{"long-run-executor", "job-orchestrator", "env-doctor", "eval-designer"} {
		c, ok := builtinAgentByID(id)
		if !ok {
			t.Errorf("缺少通用角色卡 %q", id)
			continue
		}
		if strings.TrimSpace(c.System) == "" {
			t.Errorf("%q 没有 system", id)
		}
		if c.Category == "" {
			t.Errorf("%q 没有分类", id)
		}
	}
}

// 2026-10-23 扩根：默认根必须覆盖本机技能生态的全部五处，
// 少一处用户就会以为「只装了这么点」（原 bug：只扫 ~/.workbuddy/skills）。
func TestSkillRootsCoverAllEcosystemLocations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	base := t.TempDir()

	roots := SkillRoots(base)
	want := []string{
		filepath.Join(home, ".workbuddy", "skills"),
		filepath.Join(home, ".workbuddy", "skills-cold"),
		filepath.Join(home, ".dsh", "skills"),
		filepath.Join(home, ".agents", "skills"),
	}
	for _, w := range want {
		found := false
		for _, r := range roots {
			if r == w {
				found = true
			}
		}
		if !found {
			t.Errorf("默认根缺少 %s（全量清单：%v）", w, roots)
		}
	}
	// 项目级根：baseDir 的上一级 .workbuddy/skills
	projWant := filepath.Join(filepath.Dir(base), ".workbuddy", "skills")
	if !containsStr(roots, projWant) {
		t.Errorf("默认根缺少项目级 %s", projWant)
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// data/skill-roots.json 登记额外根（别的工作区的项目级技能）。
// 文件不存在不算错；~ 开头要展开成 home。
func TestExtraSkillRootsFromFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"roots":["~/ws-a/.workbuddy/skills","` + filepath.ToSlash(filepath.Join(base, "ws-b")) + `",""]}`
	if err := os.WriteFile(filepath.Join(dataDir, "skill-roots.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := extraSkillRoots(base)
	if len(got) != 2 {
		t.Fatalf("额外根应解析出 2 个（空串要跳过），得到 %v", got)
	}
	if got[0] != filepath.Join(home, "ws-a", ".workbuddy", "skills") {
		t.Errorf("~ 没展开: %q", got[0])
	}
	// 没有文件时返回 nil，不算错
	if extraSkillRoots(t.TempDir()) != nil {
		t.Error("无文件时应返回 nil")
	}
}

// 专家插件市场的两级布局 plugins/<插件>/skills/<技能> 必须扫得到，
// 且 Source 标插件名、默认判定为 project/domain（有理由、可被表覆盖）。
func TestScanPluginPacksTwoLevelLayout(t *testing.T) {
	root := t.TempDir()
	mk := func(plugin, skill, desc string) {
		dir := filepath.Join(root, "plugins", plugin, "skills", skill)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + skill + "\ndescription: " + desc + "\n---\n# 正文\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("game-art-expert", "game-character-design", "角色设计")
	mk("team-novel-studio", "novel-slicer", "切章")
	// 没有 skills/ 子目录的插件要安静跳过，不是错误
	if err := os.MkdirAll(filepath.Join(root, "plugins", "empty-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}

	packs := ScanPluginPacks([]string{root}, nil)
	if len(packs) != 2 {
		t.Fatalf("应扫到 2 个插件技能，得到 %d: %+v", len(packs), packs)
	}
	byName := map[string]SkillPack{}
	for _, p := range packs {
		byName[p.Name] = p
	}
	cd := byName["game-character-design"]
	if cd.Source != "插件·game-art-expert" {
		t.Errorf("Source 没标插件名: %q", cd.Source)
	}
	if cd.Verdict != SkillProject || cd.Tier != TierDomain {
		t.Errorf("插件技能默认判定应为 project/domain，得到 %s/%s", cd.Verdict, cd.Tier)
	}
	if !strings.Contains(cd.Reason, "game-art-expert") || !strings.Contains(cd.Reason, "游戏美术") {
		t.Errorf("默认判定理由要说清来自哪个插件、是干什么的: %q", cd.Reason)
	}
	if cd.Description != "角色设计" {
		t.Errorf("描述没读到: %q", cd.Description)
	}
}

// 判定表覆盖优先于插件默认判定：表里写了就以表为准。
func TestScanPluginPacksVerdictOverride(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "plugins", "p", "skills", "s")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: s\ndescription: d\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := map[string]skillVerdict{
		"s": {Verdict: SkillAdopted, Tier: TierCore, Card: "job-orchestrator", Reason: "表覆盖"},
	}
	packs := ScanPluginPacks([]string{root}, policy)
	if len(packs) != 1 {
		t.Fatalf("应扫到 1 个，得到 %d", len(packs))
	}
	if packs[0].Verdict != SkillAdopted || packs[0].Tier != TierCore || packs[0].Card != "job-orchestrator" {
		t.Errorf("表覆盖没生效: %+v", packs[0])
	}
}

// 来源标签：用户看到「扫到 N 个」时必须能回答「从哪扫的」。
func TestSourceLabel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	ws := t.TempDir() // 假工作区，取其末级名
	wsName := filepath.Base(ws)
	cases := map[string]string{
		filepath.Join(home, ".workbuddy", "skills"):                     "用户级",
		filepath.Join(home, ".workbuddy", "skills-cold"):                "冷存储",
		filepath.Join(home, ".dsh", "skills"):                           "DSH 侧",
		filepath.Join(home, ".agents", "skills"):                        "agents",
		filepath.Join(ws, ".workbuddy", "skills"):                       "项目级·" + wsName,
		filepath.Join(t.TempDir(), "AIStarter", ".workbuddy", "skills"): "项目级·AIStarter",
		filepath.Join(t.TempDir(), "AI", "somewhere", "skills"):         "额外·skills",
	}
	for root, want := range cases {
		if got := sourceLabel(root); got != want {
			t.Errorf("sourceLabel(%q) = %q，期望 %q", root, got, want)
		}
	}
}
