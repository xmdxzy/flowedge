package service

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// 剧本创意层（2026-10-30）：两段式生成的「结构层」——先有故事，再有分镜。
//
// 之前的剧本智能 = 一份固定系统模板 + 一轮 LLM 调用（storyboardInstruction），
// 无结构层、无词表，角色只有一句话；Shot.Beat 字段定义了但从未生成和消费。
// GitHub 检索后择优移植（出处与取舍见 docs/剧本生成与创意优化-20261030.md）：
//   - google-deepmind/dramatron：logline→角色→节拍→分镜的级联生成，上层作下层上下文
//   - shuohao-skills：大纲→角色圣经→节拍剧本→分镜，冷开场钩子前置
//   - thoxakihiko/shotlist-forge：节奏型 = 景别序列 + 运镜序列
//   - crawfordxx/xiaoma-director：片种卡，按观众期待安排节拍
//
// 降级策略：结构层失败（LLM 挂 / JSON 坏）回退单段式——旧的直接分镜仍是
// 完整产品，不是换替代品；回退可观测（返回值结构为 nil，引擎日志有记录）。

// BeatCharacter 结构层产出的角色卡：一句话写全外貌/衣着/道具（具体到可画）
type BeatCharacter struct {
	Name string `json:"name"`
	Look string `json:"look"`
}

// StoryBeat 一个叙事节拍（多镜共享同一节拍）
type StoryBeat struct {
	Beat    string `json:"beat"`    // hook/setup/develop/clash/turn/finish
	Purpose string `json:"purpose"` // 这一拍要让观众感受到/知道什么
	Summary string `json:"summary"` // 画面概括（可拍的画面，不写心理活动）
}

// BeatPlan 结构层输出：片名/一句话故事/风格锚/角色圣经/节拍表
type BeatPlan struct {
	Title      string          `json:"title"`
	Logline    string          `json:"logline"`
	Style      string          `json:"style"`
	Characters []BeatCharacter `json:"characters"`
	Beats      []StoryBeat     `json:"beats"`
}

const storyBeatInstruction = `你是短剧编剧。把用户的内容需求先写成「故事结构」，只输出 JSON，不要解释。
这一步只定结构不定镜头：后面由分镜师把每一拍展开成镜头。

创作方法（必须遵守）：
1. 片种先行：从下面选最贴合的一个，按它的观众期待安排节拍，logline 里点明——
   悬疑反转（信息差+末拍翻案）/ 热血逆袭（压制→爆发）/ 温情日常（细节+情绪落点）/
   都市奇遇（反常事件+错位喜感）/ 科幻惊悚（未知威胁+代价）/ 古风唯美（意境+留白）/
   荒诞寓言（夸张设定+现实讽刺）/ 悬念惊悚（威胁渐进+安全错觉）/ 动作追逐（目标+障碍+反转）/
   治愈成长（缺口→修复）
2. 冷开场：第一拍必须是 hook——悬念、奇观或冲突预告，不许平铺直叙交代背景
3. 结构完整：hook(钩子)→setup(建立)→develop(发展)→clash(冲突)→turn(转折)→finish(收束)
   至少取四拍；turn 是创意所在，要有第二层信息或反转，不许直线叙事
4. 角色圣经：每个角色「名字+一句话写全外貌/衣着/道具」，具体到可画（发色/服装款式/随身物），
   不写性格形容词——画面锁不住「坚强」，锁得住「灰白短发+军绿帆布外套+旧相机」
5. 风格锚：一句话写全画面质感/色调/年代（如「黄昏渔村，青灰冷调，地面潮湿反光」）；
   用户给了风格就原样采用
6. purpose 写「这一拍要让观众感受到/知道什么」，summary 写画面概括，必须是可拍到的画面

输出 JSON 结构（严格照此，不要多字段）：
{
  "title": "片名",
  "logline": "一句话故事（含片种）",
  "style": "全局风格锚，一句话",
  "characters": [{"name": "名字", "look": "外貌+衣着+道具，一句话写全"}],
  "beats": [{"beat": "hook", "purpose": "这一拍完成什么", "summary": "画面概括"}]
}`

// storyboardLLM 分镜规划对 LLM 的最小依赖面（*LLMService 满足它）。
// 收窄成接口是为了让两段式/回退逻辑能拿假 LLM 单测，不用起真 Ollama。
type storyboardLLM interface {
	ChatWithModel(prompt, model string) (string, error)
}

// parseBeatPlan 解析结构层输出。LLM 可能裹代码围栏，照 ParseShotPlan 的办法
// 切首个 { 到最后一个 }。
func parseBeatPlan(raw string) (*BeatPlan, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndex(s, "}"); j >= 0 {
		s = s[:j+1]
	}
	var bp BeatPlan
	if err := json.Unmarshal([]byte(s), &bp); err != nil {
		return nil, fmt.Errorf("故事结构 JSON 解析失败：%w", err)
	}
	if len(bp.Beats) == 0 {
		return nil, fmt.Errorf("故事结构没有节拍（beats 为空）")
	}
	return &bp, nil
}

// planShotCreatively 两段式剧本生成：结构层（故事）→ 分镜层（镜头表）。
// bp 非 nil 表示用上了结构层；nil = 结构层失败已回退单段式（不影响出片）。
func planShotCreatively(llm storyboardLLM, model, brief, style, characters string, target, maxShots int) (*ShotPlan, *BeatPlan, error) {
	var bp *BeatPlan
	raw, err := llm.ChatWithModel(storyBeatInstruction+"\n\n"+beatUserPrompt(brief, style, characters, target, maxShots), model)
	if err != nil {
		log.Printf("[storyboard] 结构层调用失败，回退单段式：%v", err)
	} else if parsed, perr := parseBeatPlan(raw); perr != nil {
		log.Printf("[storyboard] 结构层输出不可用（%v），回退单段式", perr)
	} else {
		bp = parsed
	}

	structure := ""
	if bp != nil {
		structure = beatStructureBlock(bp, style, characters)
	}
	shotRaw, err := llm.ChatWithModel(storyboardInstruction+"\n\n"+shotUserPrompt(brief, structure, target, maxShots), model)
	if err != nil {
		return nil, bp, err
	}
	plan, perr := ParseShotPlan(shotRaw)
	if perr != nil {
		return nil, bp, fmt.Errorf("%w（原始输出片段：%.200s）", perr, shotRaw)
	}
	// 用户点名的风格必须原样落表：LLM 转述会漂，「用户输入」优先级高于模型创作
	if s := strings.TrimSpace(style); s != "" {
		plan.Style = s
	}
	return plan, bp, nil
}

// beatUserPrompt 结构层的用户提示词
func beatUserPrompt(brief, style, characters string, target, maxShots int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "目标总时长：%d 秒。预计 %d 个镜头左右（每拍展开 1–3 镜，节拍取 3–6 个）。\n内容需求：%s", target, maxShots, brief)
	if s := strings.TrimSpace(style); s != "" {
		fmt.Fprintf(&b, "\n用户指定风格（必须原样采用，不得更改）：%s", s)
	}
	if s := strings.TrimSpace(characters); s != "" {
		fmt.Fprintf(&b, "\n用户指定角色（必须原样采用，不得增删改名）：\n%s", s)
	}
	return b.String()
}

// beatStructureBlock 把结构层产物压成分镜层的上下文块。
// 用户给过的风格/角色原样透传（用户输入 > 模型创作）。
func beatStructureBlock(bp *BeatPlan, userStyle, userCharacters string) string {
	var b strings.Builder
	if bp.Logline != "" {
		fmt.Fprintf(&b, "故事一句话：%s\n", bp.Logline)
	}
	style := strings.TrimSpace(userStyle)
	if style == "" {
		style = strings.TrimSpace(bp.Style)
	}
	if style != "" {
		fmt.Fprintf(&b, "全局风格锚（逐镜重复，不得改动）：%s\n", style)
	}
	if chars := strings.TrimSpace(userCharacters); chars != "" {
		fmt.Fprintf(&b, "角色卡（逐镜锁定，不得改动）：\n%s\n", chars)
	} else if len(bp.Characters) > 0 {
		fmt.Fprintf(&b, "角色卡（逐镜锁定，不得改动）：\n")
		for _, c := range bp.Characters {
			if strings.TrimSpace(c.Name) == "" {
				continue
			}
			fmt.Fprintf(&b, "- %s：%s\n", c.Name, c.Look)
		}
	}
	fmt.Fprintf(&b, "节拍表（每个镜头的 beat 必须取自这里，首拍即钩子）：")
	for i, bt := range bp.Beats {
		fmt.Fprintf(&b, "\n%d. [%s] %s——%s", i+1, bt.Beat, bt.Purpose, bt.Summary)
	}
	return b.String()
}

// shotUserPrompt 分镜层的用户提示词（structure 为空 = 单段式，与旧版等价）
func shotUserPrompt(brief, structure string, target, maxShots int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "目标总时长：%d 秒。镜头数量不超过 %d 个（时长按内容给，3–8 秒/镜）。\n内容需求：%s", target, maxShots, brief)
	if structure != "" {
		fmt.Fprintf(&b, "\n\n已确定的故事结构（分镜必须在此结构内展开，不得另起炉灶）：\n%s", structure)
	}
	return b.String()
}
