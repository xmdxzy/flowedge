package handler

// 技能包索引端点。
//
// 与 /api/cards 的分工必须说清，否则前端会混着用：
//
//	/api/cards  能给节点**贴上去**的能力（agent / skill / workflow），贴了就改输出
//	/api/skills 技能包的**归属审计**：哪些内容其实已在引擎里、哪些该留在 agent 侧、
//	            哪些是具体业务私货。它只读，不产生可执行能力。
//
// 为什么要有只读的这一层：用户机器上那批 SKILL.md 是方法论包，压不进技能卡。
// 但"压不进"不能变成"不闻不问"——每一条都得有判定、有理由、有能核验的证据，
// 否则下次又会出现"我的 skill 到底有没有进引擎"这种只能靠人记的问题。

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"

	"flowedge/backend/service"
)

type SkillPackHandler struct {
	baseDir string
	// projectRoot 仓库根。空 = 本机没源码（分发的 exe），证据无法核验，
	// 此时**不降级**判定，只在响应里标 verified=false——不能因为没源码就说判定假。
	projectRoot string
	policyPath  string
}

func NewSkillPackHandler(baseDir string) *SkillPackHandler {
	return &SkillPackHandler{
		baseDir:     baseDir,
		projectRoot: resolveProjectRoot(baseDir),
		policyPath:  filepath.Join(baseDir, "data", "skill-policy.json"),
	}
}

// resolveProjectRoot 找仓库根：要求 backend/service/prompt_opt.go 真实存在，
// 因为证据多半指向那里。找不到就返回空，让端点降级为"不可核验"。
func resolveProjectRoot(baseDir string) string {
	if p := os.Getenv("FLOWEDGE_PROJECT_ROOT"); p != "" {
		return p
	}
	abs, err := filepath.Abs(baseDir)
	if err != nil {
		return ""
	}
	// 开发期 baseDir 是 运行/ 或 backend/，两种都往上找两级
	for _, cand := range []string{filepath.Dir(abs), filepath.Dir(filepath.Dir(abs)), abs} {
		if _, err := os.Stat(filepath.Join(cand, "backend", "service", "prompt_opt.go")); err == nil {
			return cand
		}
	}
	return ""
}

type skillPackView struct {
	service.SkillPack
	// Category 判定的人话分组名，前端直接按它分组，不用自己映射
	Category string `json:"category"`
}

// List GET /api/skills
//
// 默认返回**收录在平台里**的全部包（tier=core + domain），`?tier=core` 只看通用能力，
// `?all=1` 连不收录的（已停用/不承载）一起给。三种视图对应三个问题：
//
//	「平台默认给我什么」→ core
//	「我要做某个领域的图，有没有现成经验」→ 默认（含 domain，可搜）
//	「某个包为什么不在」→ all
//
// 两类来源一起返：本机技能根（用户级/冷存储/DSH/agents/项目级/额外根）与专家插件市场
// （plugins/<插件>/skills/<技能>）。少返一类，用户就会以为「只装了这么点」——
// 2026-10-23 实测踩过：只扫用户级根时报告 14 个，全盘其实有五处本机生态 + 插件市场。
func (h *SkillPackHandler) List(c *gin.Context) {
	roots := service.SkillRoots(h.baseDir)
	packs := service.ScanSkillPacks(roots, service.LoadSkillPolicy(h.policyPath))
	pluginPacks := service.ScanPluginPacks(service.PluginSkillRoots(h.baseDir), service.LoadSkillPolicy(h.policyPath))
	packs = append(packs, pluginPacks...)

	problems := []string{}
	verified := h.projectRoot != ""
	if verified {
		packs, problems = service.VerifySkillPacks(packs, h.projectRoot)
	}

	want := c.Query("tier")
	showAll := c.Query("all") == "1"
	byVerdict := map[string]int{}
	byTier := map[string]int{}
	bySource := map[string]int{}
	cardIDs := map[string]bool{}
	views := make([]skillPackView, 0, len(packs))
	for _, p := range packs {
		byVerdict[p.Verdict]++
		byTier[p.Tier]++
		bySource[p.Source]++
		if p.Card != "" {
			cardIDs[p.Card] = true
		}
		show := true
		switch {
		case want != "" && want != "all":
			show = p.Tier == want
		case showAll:
			show = true
		default:
			show = p.Tier != service.TierNone
		}
		if show {
			views = append(views, skillPackView{SkillPack: p, Category: service.VerdictCategory(p.Verdict)})
		}
	}

	// roots 一起返回：扫不到东西时，第一件要确认的事就是"我到底扫了哪几个目录"。
	c.JSON(http.StatusOK, gin.H{
		"packs":        views,
		"roots":        roots,
		"plugin_roots": service.PluginSkillRoots(h.baseDir),
		"verified":     verified,
		"problems":     problems,
		"show_all":     showAll,
		"summary": gin.H{
			"total":      len(packs),
			"core":       byTier[service.TierCore],
			"domain":     byTier[service.TierDomain],
			"none":       byTier[service.TierNone],
			"retained":   len(packs) - byTier[service.TierNone],
			"plugins":    len(pluginPacks),
			"by_verdict": byVerdict,
			"by_tier":    byTier,
			"by_source":  bySource,
			// cards 是**去重后的卡数**，不是包数：两张包可以提炼成同一张卡
			// （env-doctor 就吸收了 local-multimodal-ops 与 windows-dev-env）。
			"cards":    len(cardIDs),
			"mirrored": byVerdict[service.SkillMirrored],
		},
	})
}

// ReadDoc GET /api/skills/:name —— 返回技能包正文（只读已索引的包）
func (h *SkillPackHandler) ReadDoc(c *gin.Context) {
	name := c.Param("name")
	packs := service.ScanSkillPacks(service.SkillRoots(h.baseDir), service.LoadSkillPolicy(h.policyPath))
	packs = append(packs, service.ScanPluginPacks(service.PluginSkillRoots(h.baseDir), service.LoadSkillPolicy(h.policyPath))...)
	doc, err := service.ReadSkillDoc(skillPackSource{packs}, name)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "没有这个技能包", "name": name})
		return
	}
	c.JSON(http.StatusOK, gin.H{"name": name, "doc": doc})
}

// skillPackSource 把切片适配成 service.PathGetter。
type skillPackSource struct{ packs []service.SkillPack }

func (s skillPackSource) Packs() []service.SkillPack { return s.packs }
