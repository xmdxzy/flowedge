#!/usr/bin/env python3
"""出片前自检：把《3D 爆炸图提示词规范》变成确定性检查。

为什么要有它：上一支腕表片栽在两件事上——① 写了否定清单（cfg=1 读不到负分支），
② 画面里排了两行文字（官方产品广告 Skill 明令禁止）。两条都是**可静态检出的**，
当时却没任何一道门拦它，等 40 分钟烧完卡才在抽帧里看见。规则不落检 = 规则会再犯。

判据来源（逐条可指到原文）：
  · HF MiniMax-H3 docs/VIDEO_PROMPT_WRITING_GUIDE_base_en.md §2.2/§4.2/§4.5/§4.6/§4.7
  · MiniMax-AI/MiniMax-H3 skills/minimalist-product-ad-generator/SKILL.cn.md 文字规范
  · 本地 ComfyUI cfg=1 无负向分支（docs.comfy.org minimax-h3-prompt-guide 第 8 条）
"""
import json
import re
import sys

PLAN = sys.argv[1] if len(sys.argv) > 1 else ".tmp/pc-plan.json"

# 否定清单：cfg=1 没有负分支，这些词会把被否定的概念**注册成内容**
NEGATION = [
    "no ", "not ", "without ", "never ", "禁止", "不要", "不得",
    "无品牌", "无任何", "不含", "没有", "绝无", "避免",
    "no random letters", "no watermark",
]
# 但工程上必须允许的「正向否定」：描述物理状态的形容词
NEGATION_ALLOW = [
    "nothing else rotates",   # 约束运动源数量，指向已列出的部件
    "no component is duplicated",
    "never overlap",
]
FIELDS = ["integrated_multimodal_description", "overall_soundscape", "non_diegetic_music"]
# 官方 §4.6/§4.7：soundscape 1–4 句，music 1–3 句
SENT_MAX = {"overall_soundscape": 4, "non_diegetic_music": 3}


def sentences(t: str) -> int:
    t = t.strip()
    if not t or t.upper() == "N/A":
        return 0
    return len([s for s in re.split(r"(?<=[.!?])\s+", t) if s.strip()])


def main() -> int:
    plan = json.loads(open(PLAN, encoding="utf-8").read())
    fails, warns = [], []
    # 时间码检查用：整片时长按各镜帧数折算（24fps）；一处 frames 都没给就不判越界。
    total = sum(int(s.get("frames") or 0) for s in plan["shots"]) / 24.0
    last_cut = 0.0
    for sh in plan["shots"]:
        sid = sh["id"]
        p = sh["prompt"]
        # 1) 三段字段必须都在（§2.2）
        for f in FIELDS:
            if f not in p:
                fails.append(f"镜{sid}: 缺字段 {f}（空/缺会让模型自行脑补）")
        # 2) 正文必须英文（ref guide 首段 + Comfy 官方文档）
        body = p.split("overall_soundscape")[0]
        cn = len(re.findall(r"[一-鿿]", body))
        if cn:
            fails.append(f"镜{sid}: 正文含 {cn} 个汉字（H3 正文须英文）")
        # 3) 首镜不加时间戳（§4.2）
        if re.search(r"\[Shot 1\]\s*(在|At)\s*\d", p):
            fails.append(f"镜{sid}: [Shot 1] 带了时间戳（官方禁止）")
        if re.search(r"在\s*00:\d\d", p):
            fails.append(f"镜{sid}: 用了中文「在 00:xx」，应为 At 00:xx.xxx,")
        # 4) 否定清单（本地 cfg=1 无负分支）
        low = p.lower()
        for n in NEGATION:
            for ok in NEGATION_ALLOW:
                low = low.replace(ok, "")
        hits = [n.strip() for n in NEGATION if n in low]
        if hits:
            fails.append(f"镜{sid}: 出现否定清单 {hits} → 改写成可见事实")
        # 5) 画面内文字：双引号包裹 + 同屏只允许一条单行
        quoted = re.findall(r'"([^"]{1,40})"', p)
        if len(quoted) > 1:
            fails.append(f"镜{sid}: 画面文字有 {len(quoted)} 处 {quoted}（官方：同一时间只允许一条单行文案）")
        for q in quoted:
            if len(q.split()) < 3 or len(q) > 32:
                warns.append(f"镜{sid}: 文案 \"{q}\" 不符 3–5 词 / ≤32 字符")
        # 6) 字段句数上限（§4.6/§4.7）
        for f, lim in SENT_MAX.items():
            m = re.search(rf"{f}:\s*(.+?)(?:\n\n|$)", p, re.S)
            if m and (n := sentences(m.group(1))) > lim:
                fails.append(f"镜{sid}: {f} 写了 {n} 句，官方上限 {lim} 句")
        # 7) 运动源数量（社区公认：相机环绕 + 部件自转 叠加必抖）
        #    词边界必须有：`arc` 曾命中 "charcoal"，把每个镜头都误判成多一个运动源。
        movers = sum([
            bool(re.search(r"\b(orbit|arcing|arc shot|环绕)\b", low)),
            bool(re.search(r"\b(rotat|spin)[a-z]*\b", low)),
            bool(re.search(r"\b(push(es|ing)? in|pull(s|ing)? out|dolly|track(s|ing))\b", low)),
        ])
        if movers >= 3:
            warns.append(f"镜{sid}: {movers} 个运动源叠加（建议 ≤2）")
        # 8) 件数必须显式钉死——只在**真的发生分离**的镜子上要求。
        #    判据是「有没有明确数字」，不是「有没有 exactly」：run 431 镜 3 写
        #    `The seven separated components … draw inward` 件数同样是钉死的，
        #    旧正则只认 exactly/all 会误报（噪声门会被忽略，见 §0.12②）。
        separates = bool(re.search(r"separat|explod|stack|layers rise|split", low))
        pinned = bool(re.search(
            r"\b(one|two|three|four|five|six|seven|eight|nine|ten|\d+)\s+(\w+\s+){0,2}components?\b", low))
        if separates and not pinned:
            warns.append(f"镜{sid}: 有分离动作却没钉死件数（N components），件数会漂")
        # 9) 编号必须逐字枚举：run 431 实测 `01 through 07` 渲染成 01 02 04 05 06 07（漏 03）。
        #    序列是概率的，枚举是查表的——这条是 lint 写法，不是 lint 产物。
        if re.search(r"\b0?1\s*(through|to|–|-)\s*0?7\b", low) and not re.search(r"01,\s*02,\s*03", low):
            fails.append(f"镜{sid}: 编号写成区间（01 through 07）→ 实测会漏号；改成逐字枚举 01, 02, 03, …")
        # 10) 归位段的真根因是**动作密度**，不是措辞。
        #     run 431 实测：镜 3 已写 "stands whole" + "every panel flush"，
        #     73 帧（3.04s）里塞「特写起幅 + 7 层归位 + 面板闭合 + 拉远 + 淡出」五拍，
        #     结果拍成开口机箱——**措辞齐备的假通过**。所以这条查帧数预算，不查词。
        #     经验值：一镜可靠承载的动作拍 ≈ 帧数/35（107 帧≈3 拍，124 帧≈3.5 拍）。
        reassembles = bool(re.search(r"reassembl|draw inward|seats? first|panels? close", low))
        if reassembles:
            # 按**类别**计数（同类只算一次）：`seats/seating/seat` 数三遍会把 5 拍报成 14 拍，
            # 而虚高的门给出的建议必然荒谬（曾算出"加帧到 490"，H3 上限才 362）。
            groups = {
                "层归位": r"reassembl|draw inward|seats? (first|beneath)|returns? to the chassis",
                "面板闭合": r"panels? close|latch|side panel returns|flush",
                "相机动作": r"pulls? out|push(es)? in|pans? |tilts? |arc shot|tracks? ",
                "光线扫过": r"highlight (sweeps|travels)|sweeps? once",
                "淡出": r"fades? (in|to)|fade to black",
            }
            beats = sum(1 for pat in groups.values() if re.search(pat, low))
            budget = max(1, int(sh.get("frames", 0) / 35))
            if beats > budget:
                cap = min(beats * 35, 362)  # 362 = 17×21+5，H3 训练上限 ≈15.08s
                fix = f"加帧到 {cap}" if cap > sh.get("frames", 0) else "已到帧数上限，只能砍拍"
                fails.append(f"镜{sid}: 归位段 {beats} 类动作 > 帧数预算 {budget}"
                             f"（{sh.get('frames')} 帧）→ 拍不完，结尾会是开口半成品；{fix}，"
                             f"或把淡出/高光扫过交给后期 ffmpeg")

        # 11) 时间码必须严格递增（四家族通用，见《视频提示词编排规范》§三.5）。
        #     越界只报 WARN：plan 有两种读法——整片一次生成（时间码=全片绝对时间）
        #     与逐镜生成（时间码=镜内时间），拿总时长硬判会把后者全部误杀。
        for m in re.finditer(r"(?i)\b(?:at|在)\s*(\d{1,2}):(\d{2})(?:\.(\d{1,3}))?", p):
            sec = int(m.group(1)) * 60 + int(m.group(2)) + (
                int((m.group(3) or "").ljust(3, "0")) / 1000 if m.group(3) else 0.0)
            if sec <= last_cut:
                fails.append(f"镜{sid}: 时间码 {sec:.3f}s 未严格递增（上一处 {last_cut:.3f}s）")
            last_cut = max(last_cut, sec)
            if total and sec >= total:
                warns.append(f"镜{sid}: 时间码 {sec:.3f}s ≥ 总时长 {total:.3f}s（逐镜生成时属正常）")
        # 12) 台词标签成对；语言标签在前。唇形对齐靠它，漏一个 </d> 就退化成画外音。
        if p.count("<d>") != p.count("</d>"):
            fails.append(f"镜{sid}: <d>/</d> 不配对（{p.count('<d>')} vs {p.count('</d>')}）")
        for m in re.finditer(r"<d>", p):
            if not re.search(r"\[[A-Za-z]+\]\s*$", p[max(0, m.start() - 24):m.start()]):
                warns.append(f"镜{sid}: <d> 前缺语言标签（应为 <d>[Chinese] …</d>）")
        # 13) 详细度下限：正文 <100 词 = 信息位不足，模型会脑补（规范 §三.1）。
        words = len(re.findall(r"[A-Za-z][A-Za-z'’-]*", body))
        if words < 100:
            warns.append(f"镜{sid}: 正文 {words} 词 <100，细节偏薄（主体段/动作链段最常缺）")

    print(f"检查 {len(plan['shots'])} 镜")
    for w in warns:
        print("  WARN", w)
    for f in fails:
        print("  FAIL", f)
    print("结论：" + ("OK 可以提交" if not fails else f"{len(fails)} 项不过，先改再烧卡"))
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
