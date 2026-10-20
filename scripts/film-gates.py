#!/usr/bin/env python3
"""控制门（gates）：把「每条片子该怎么拍」从经验变成可声明、可校验的一组参数。

为什么要有这一层
----------------
「每个镜头多少秒」没有恒定答案——30 秒斗法和 15 秒广告、8 秒情绪特写的节奏完全不同。
以前我把结论写成固定表格（建立 6–8s、交锋 11–15s），下次换题材就不好用了；
更糟的是，它会让执行者跳过「先想清楚再开机」这一步。

所以这里把判断本身**参数化**：片子开工前必须先把这组门写出来（plan.gates），
执行器按门生成与拦截。门可以改、可以换、可以只为这一条片子存在。

门的五个层（每层都是「规则 + 阈值 + 违约动作」）
------------------------------------------------
  timing     节奏：总时长、网格吸附、每镜最短/最长停留、各段权重
  identity   角色：定妆图数量、每段必须带参考图、prompt 里 <Picture N> 不得越界
  audio      声音：台词段必须转出原话、氛围段必须无人声、响度区间、静音缺口
  technical  技术：分辨率、帧率、音画漂移、接缝时长
  budget     成本：总墙钟上限、单段上限、超了是告警还是拦截

两个子命令
----------
  plan   按「总时长 + 各段叙事权重 + 模式 + 预算」生成一份门草案（打印到 stdout）
  check  读取 plan.json（含 gates）做静态自洽校验：能算的错，开机前就拦掉

用法
----
    # 生成草案（先看清再落盘，不要盲抄）
    python scripts/film-gates.py plan --seconds 30 \
        --beats setup=1.0,clash=1.35,finish=1.2 \
        --modes dialogue,ambience,dialogue \
        --lines-per-seg 1,0,2 --budget 1500

    # 开机前自检（shot-film.py 会自动调用它）
    python scripts/film-gates.py check --plan data/film/jindan/plan.json
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys

FPS = 24
GRID_STEP = 17
GRID_OFFSET = 5
DEFAULT_FADE = 0.2
# 单帧出片成本的保守估计（秒/帧，608×352 / 12 步 / 2 参考图实测 0.85–0.93）
SEC_PER_FRAME = 0.95

# 预置 profile：常见任务类型的门起点。**不是标准答案**，是"从哪儿开始改"。
# 用法：film-gates.py plan --profile quick-cut …
# 覆盖顺序：DEFAULT < PROFILE < 本片 gates < 命令行参数
PROFILES: dict[str, dict] = {
    # 叙事短片（默认）：一镜讲一件事，1.2s 是"读得到信息"的下限
    "narrative": {
        "timing": {"min_shot_seconds": 1.2, "max_shot_seconds": 4.5},
        "audio": {"segment_retry": 1},
    },
    # 短视频快切：15s 内 4–6 段，允许 0.8s 闪切，总长容差收紧
    "quick-cut": {
        "timing": {"min_shot_seconds": 0.8, "max_shot_seconds": 2.5,
                   "tolerance_seconds": 0.4},
        "audio": {"segment_retry": 1, "max_silence_gap": 0.6},
    },
    # 情绪长镜 / 独白：一镜停久一点，别切碎；总长容差放宽
    "long-take": {
        "timing": {"min_shot_seconds": 2.0, "max_shot_seconds": 0,
                   "tolerance_seconds": 1.0},
        "audio": {"max_silence_gap": 1.2},
    },
    # 对话戏：口型与台词是命门，重试两次；响度收紧
    "dialogue": {
        "timing": {"min_shot_seconds": 1.5, "max_shot_seconds": 6.0},
        "audio": {"segment_retry": 2, "loudness_db": [-19, -13],
                  "max_silence_gap": 0.8},
    },
    # 夜间批跑：宁可靠门拦住也不要拖到天亮；不重试，超预算直接 fail
    "batch-night": {
        "timing": {"min_shot_seconds": 1.0, "max_shot_seconds": 5.0},
        "audio": {"segment_retry": 0},
        "budget": {"on_exceed": "fail"},
    },
    # 纯氛围（无台词）：不要求台词转写，响度放宽，静音缺口是主要敌人
    "ambience-only": {
        "timing": {"min_shot_seconds": 1.5, "max_shot_seconds": 6.0},
        "audio": {"loudness_db": [-24, -13], "max_silence_gap": 0.7},
        "identity": {"require_picture_tags": True},
    },
}

# 默认门：只写「通用规则」，不写「这类片子该多少秒」。
# 每个字段都可以被 plan.gates 覆盖——这正是「每次任务前先设计门」的入口。
DEFAULT_GATES: dict = {
    "version": 1,
    "profile": "default",
    "timing": {
        "fps": FPS,
        "grid": [GRID_OFFSET, GRID_STEP],
        "target_seconds": 0,          # 0 = 不强制总长
        "tolerance_seconds": 0.75,    # 总长容差
        "min_shot_seconds": 1.2,      # 单镜最短停留：低于这个数观众读不到信息
        "max_shot_seconds": 0,        # 0 = 不限制
        "beat_weights": {},           # 段 -> 权重；用于自动分配帧数
        "require_beats": True,        # 段必须声明 beat，否则无法谈节奏
    },
    "identity": {
        "stills": 0,                  # 需要几张定妆图（0 = 本片无重复角色）
        "require_ref_images_every_segment": True,
        "max_ref_images": 9,
        "require_picture_tags": True,  # prompt 必须用 <Picture N> 指认，而不是"那个男的"
    },
    "audio": {
        "dialogue": {"must_transcribe": True, "min_chars": 2},
        "ambience": {"must_be_silent": True, "max_chars": 0},
        "loudness_db": [-22, -11],    # 平均响度区间
        "max_silence_gap": 1.0,       # 片中允许的最长静音缺口（秒）
        # 段级失败策略：一段不合门时先重跑（采样有随机性），仍不过才放弃整片
        "segment_retry": 1,
        "on_segment_fail": "retry",   # retry | fail
    },
    "technical": {
        "min_width": 608,
        "min_height": 352,
        "must_have_audio": True,
        "av_drift_max": 0.2,
        "fade": DEFAULT_FADE,
    },
    "budget": {
        "wall_seconds_max": 0,        # 0 = 不限制
        "per_segment_wall_max": 0,
        "on_exceed": "warn",          # warn | fail
    },
}


# ---------------------------------------------------------------- 工具函数


def snap_frames(seconds: float, fps: int = FPS) -> int:
    """把秒数吸附到最近的 17k+5 帧。返回 None 表示超出可达范围。"""
    target = seconds * fps
    k = round((target - GRID_OFFSET) / GRID_STEP)
    if k < 0:
        return None
    return GRID_STEP * k + GRID_OFFSET


def frames_in_grid(n: int) -> bool:
    return n > 0 and (n - GRID_OFFSET) % GRID_STEP == 0


def seg_seconds(frames: int, fps: int = FPS) -> float:
    return round(frames / float(fps), 3)


def total_seconds(frames: list[int], fade: float = DEFAULT_FADE, fps: int = FPS) -> float:
    """装配后总长：逐段相加，每处接缝吃掉 fade。"""
    if not frames:
        return 0.0
    return round(sum(frames) / float(fps) - fade * max(0, len(frames) - 1), 2)


def count_shots(prompt: str) -> list[int]:
    """数「镜头起点」标记。

    不能简单数 `[Shot N]` 出现次数：retention_analysis 里会写
    `<Picture 1> (appears in [Shot 1], [Shot 2])`，那是「出现于哪几镜」的引用，
    不是新镜头。区别在于镜头起点后面跟的是内容，引用后面跟的是逗号或右括号。
    """
    pat = r"\[\s*Shot\s+(\d+)\s*\](?!\s*[,)])"
    return [int(m) for m in re.findall(pat, prompt or "", re.I)]


def shot_timestamps(prompt: str) -> list[float]:
    """取 [Shot k] 之前声明的时间戳（形如 At 00:04.100）。"""
    out = []
    for m in re.finditer(r"At\s+(\d{1,2}):(\d{2})\.(\d{3})", prompt or "", re.I):
        out.append(int(m.group(1)) * 60 + int(m.group(2)) + int(m.group(3)) / 1000.0)
    return out


def picture_tags(prompt: str) -> list[int]:
    return [int(m) for m in re.findall(r"<\s*Picture\s+(\d+)\s*>", prompt or "", re.I)]


def dialogue_blocks(prompt: str) -> list[str]:
    return [m.strip() for m in re.findall(r"<d>(.*?)</d>", prompt or "", re.S | re.I)]


def deep_merge(base: dict, override: dict) -> dict:
    """门的合并：任务级 gates 覆盖默认门，未声明的继承默认。"""
    out = dict(base)
    for k, v in (override or {}).items():
        if isinstance(v, dict) and isinstance(out.get(k), dict):
            out[k] = deep_merge(out[k], v)
        else:
            out[k] = v
    return out


# ---------------------------------------------------------------- 门草案


def build_plan_gates(args) -> dict:
    """按任务参数生成门草案。

    关键：**不写死段数**，段数来自 --beats 的个数；每段帧数由权重分配后吸附网格。
    """
    beats = []
    for item in (args.beats or "").split(","):
        item = item.strip()
        if not item:
            continue
        if "=" in item:
            name, w = item.split("=", 1)
            beats.append((name.strip(), float(w)))
        else:
            beats.append((item, 1.0))
    if not beats:
        beats = [("whole", 1.0)]

    modes = [m.strip() for m in (args.modes or "").split(",") if m.strip()]
    while len(modes) < len(beats):
        modes.append("ambience")
    modes = modes[:len(beats)]

    lines = [int(x) for x in (args.lines_per_seg or "").split(",") if x.strip()]
    while len(lines) < len(beats):
        lines.append(0)
    lines = lines[:len(beats)]

    fade = float(args.fade)
    # 目标视频时长 = 段时长之和 - 接缝数 × fade
    video_budget = float(args.seconds) + fade * max(0, len(beats) - 1)
    total_weight = sum(w for _, w in beats) or 1.0
    raw = [video_budget * w / total_weight for _, w in beats]

    snapped = []
    for r in raw:
        f = snap_frames(r)
        if f is None:
            f = GRID_OFFSET
        snapped.append(f)

    gates = json.loads(json.dumps(DEFAULT_GATES))  # deep copy
    # 覆盖顺序：DEFAULT < PROFILE < 命令行显式参数。
    # 所以这些参数必须是 None 默认——若给 1.2/0.75 这类默认值，会把 profile 静默盖掉。
    prof = (args.profile or "").strip()
    if prof in PROFILES:
        gates = deep_merge(gates, PROFILES[prof])
    gates["profile"] = prof or "custom"
    gates["timing"]["fps"] = FPS
    gates["timing"]["target_seconds"] = float(args.seconds)
    gates["timing"]["beat_weights"] = {name: w for name, w in beats}
    gates["timing"]["require_beats"] = True
    if args.tolerance is not None:
        gates["timing"]["tolerance_seconds"] = float(args.tolerance)
    if args.min_shot is not None:
        gates["timing"]["min_shot_seconds"] = float(args.min_shot)
    if args.max_shot is not None:
        gates["timing"]["max_shot_seconds"] = float(args.max_shot)
    if args.stills is not None:
        gates["identity"]["stills"] = int(args.stills)
    if args.budget is not None:
        gates["budget"]["wall_seconds_max"] = int(args.budget)
    if args.per_seg_budget:
        gates["budget"]["per_segment_wall_max"] = int(args.per_seg_budget)
    gates["technical"]["fade"] = fade

    plan = {
        "note": "先读这份草案，按本条片子的内容改完再落进 plan.json；不要直接照抄。",
        "gates": gates,
        "suggested": {
            "segment_beats": [name for name, _ in beats],
            "segment_modes": modes,
            "segment_lines": lines,
            "segment_frames": snapped,
            "segment_seconds": [seg_seconds(f) for f in snapped],
            "total_seconds": total_seconds(snapped, fade),
            "est_wall_seconds": round(sum(snapped) * SEC_PER_FRAME
                                      + 50 * len(snapped), 0),
        },
    }
    return plan


# ---------------------------------------------------------------- 静态校验


class Verdict:
    def __init__(self):
        self.items: list[dict] = []

    def add(self, level: str, gate: str, msg: str):
        self.items.append({"level": level, "gate": gate, "msg": msg})

    def ok(self) -> bool:
        return not any(i["level"] == "FAIL" for i in self.items)

    def worst(self) -> str:
        if any(i["level"] == "FAIL" for i in self.items):
            return "FAIL"
        if any(i["level"] == "WARN" for i in self.items):
            return "WARN"
        return "OK"


def check_plan(plan: dict, require_gates: bool = True) -> tuple[Verdict, dict]:
    v = Verdict()
    raw_gates = plan.get("gates")

    if not raw_gates:
        if require_gates:
            v.add("FAIL", "gates_missing",
                  "plan 没有 gates：本片子的节奏/一致性/验收标准还没设计。"
                  "先跑 film-gates.py plan 生成草案并改写（确需跳过加 --no-gates）")
        else:
            v.add("WARN", "gates_missing", "未声明 gates，全部按默认门执行")
        raw_gates = {}

    g = deep_merge(DEFAULT_GATES, raw_gates)
    timing, ident, audio, tech, budget = (g["timing"], g["identity"],
                                          g["audio"], g["technical"], g["budget"])
    fps = int(timing.get("fps") or FPS)
    fade = float(tech.get("fade") or DEFAULT_FADE)

    segments = plan.get("segments") or []
    if not segments:
        v.add("FAIL", "segments_empty", "plan.segments 为空")
        return v, g, {}

    # ---------- timing：网格 / 每镜停留 / 总长 ----------
    frames: list[int] = []
    for i, seg in enumerate(segments):
        sid = seg.get("id") or (i + 1)
        f = seg.get("frames")
        if f is None:
            v.add("WARN", "frames_missing", f"seg{sid} 未声明 frames，将由权重推导")
            frames.append(0)
            continue
        f = int(f)
        if not frames_in_grid(f):
            v.add("FAIL", "grid", f"seg{sid} frames={f} 不在 17k+5 网格")
        frames.append(f)

        secs = seg_seconds(f, fps)
        prompt = seg.get("prompt") or ""
        shots = count_shots(prompt)
        min_shot = float(timing.get("min_shot_seconds") or 0)
        max_shot = float(timing.get("max_shot_seconds") or 0)

        if shots:
            stamps = shot_timestamps(prompt)
            # 第 k 镜的起点：第 1 镜是 0，其余取声明的时间戳
            starts = [0.0] + stamps
            starts = starts[:len(shots)]
            if len(stamps) >= len(shots) - 1:
                spans = []
                for k in range(len(shots)):
                    end = starts[k + 1] if k + 1 < len(starts) else secs
                    spans.append(round(end - starts[k], 3))
                for k, span in enumerate(spans):
                    if span < min_shot:
                        v.add("FAIL", "shot_too_short",
                              f"seg{sid} 第 {k + 1} 镜只有 {span}s（门要求 ≥{min_shot}s）"
                              f"——切点要前移或减少镜头数")
                    if max_shot and span > max_shot:
                        v.add("WARN", "shot_too_long",
                              f"seg{sid} 第 {k + 1} 镜 {span}s 超过 {max_shot}s")
                if stamps:
                    if stamps != sorted(stamps):
                        v.add("FAIL", "shot_order", f"seg{sid} 镜头时间戳不递增")
                    last = stamps[-1]
                    if last >= secs - min_shot:
                        v.add("FAIL", "shot_tail",
                              f"seg{sid} 最后一个切点 {last}s 距片尾 {secs}s 不足 {min_shot}s")
            else:
                v.add("WARN", "shot_stamps",
                      f"seg{sid} 有 {len(shots)} 个镜头但只声明了 {len(stamps)} 个时间戳，"
                      f"无法校验每镜停留时长")
        elif timing.get("require_beats"):
            v.add("WARN", "no_shot_tags",
                  f"seg{sid} 提示词里没有 [Shot N] 标记，镜内节奏不可控")

    if all(f > 0 for f in frames):
        got = total_seconds(frames, fade, fps)
        target = float(timing.get("target_seconds") or 0)
        tol = float(timing.get("tolerance_seconds") or 0.75)
        if target > 0 and abs(got - target) > tol:
            v.add("FAIL", "total_seconds",
                  f"装配后 {got}s 与目标 {target}s 相差超过容差 {tol}s"
                  f"（段帧数 {frames}）")
        elif target > 0:
            v.add("OK", "total_seconds", f"{got}s ↔ 目标 {target}s（容差 {tol}s）")

    # ---------- identity：定妆图 / Picture 标签 ----------
    chars = plan.get("characters") or []
    need = int(ident.get("stills") or 0)
    if need and len(chars) < need:
        v.add("FAIL", "stills", f"门要求 {need} 张定妆图，plan 只给了 {len(chars)} 张")
    max_ref = int(ident.get("max_ref_images") or 9)
    if len(chars) > max_ref:
        v.add("FAIL", "ref_images", f"参考图 {len(chars)} 张超过上限 {max_ref}")

    for i, seg in enumerate(segments):
        sid = seg.get("id") or (i + 1)
        prompt = seg.get("prompt") or ""
        tags = picture_tags(prompt)
        if tags:
            over = [t for t in tags if t > len(chars)]
            if over:
                v.add("FAIL", "picture_overflow",
                      f"seg{sid} 引用了 <Picture {max(over)}> 但只有 {len(chars)} 张定妆图")
        elif chars and ident.get("require_picture_tags"):
            v.add("WARN", "picture_tags",
                  f"seg{sid} 有定妆图却没用 <Picture N> 指认角色，一致性只能靠文字描述")

        if chars and ident.get("require_ref_images_every_segment"):
            if seg.get("ref_images") is False:
                v.add("FAIL", "ref_images_segment",
                      f"seg{sid} 显式关闭了参考图：这一段会换脸")

    # ---------- audio：台词表 ↔ 提示词 ----------
    total_lines = 0
    for i, seg in enumerate(segments):
        sid = seg.get("id") or (i + 1)
        lines = [x for x in (seg.get("lines") or []) if str(x.get("text", "")).strip()]
        total_lines += len(lines)
        blocks = dialogue_blocks(seg.get("prompt") or "")

        if lines:
            if not blocks:
                v.add("FAIL", "dialogue_not_in_prompt",
                      f"seg{sid} 有 {len(lines)} 句台词，但提示词里没有 <d>…</d> 标签，"
                      f"H3 不会照念")
            for ln in lines:
                text = re.sub(r"\s+", "", str(ln.get("text", "")))
                if text and not any(text in re.sub(r"\s+", "", b) for b in blocks):
                    # 这是硬错误：TTS 念的是台词表，提示词里写的是另一句，
                    # 成片一定念错。必须在开机前拦，不能等交付时才发现。
                    v.add("FAIL", "line_mismatch",
                          f"seg{sid} 台词「{ln.get('text')}」没出现在 <d> 块里："
                          f"TTS 会念台词表，画面按提示词走，两者对不上")
            if audio.get("dialogue", {}).get("must_transcribe"):
                v.add("OK", "dialogue_policy", f"seg{sid} 台词段：出片后必须 ASR 转出原话")
        else:
            if audio.get("ambience", {}).get("must_be_silent"):
                v.add("OK", "ambience_policy", f"seg{sid} 氛围段：出片后必须 ASR 无人声")

    if len(chars) and total_lines and not any(
            picture_tags((s.get("prompt") or "")) for s in segments):
        v.add("WARN", "no_picture_with_dialogue", "有台词又有定妆图，却没有 <Picture N> 指认")

    # ---------- technical ----------
    w = int(plan.get("width") or 608)
    h = int(plan.get("height") or 352)
    if w < int(tech.get("min_width") or 0) or h < int(tech.get("min_height") or 0):
        v.add("WARN", "resolution",
              f"{w}×{h} 低于门要求 {tech.get('min_width')}×{tech.get('min_height')}")
    if tech.get("must_have_audio"):
        for i, seg in enumerate(segments):
            if seg.get("mute") is True:
                v.add("FAIL", "audio_mute", f"seg{seg.get('id') or (i + 1)} 被设为静音")

    # ---------- budget ----------
    if all(f > 0 for f in frames):
        est = round(sum(frames) * SEC_PER_FRAME + 50 * len(frames), 0)
        cap = int(budget.get("wall_seconds_max") or 0)
        if cap and est > cap:
            level = "FAIL" if budget.get("on_exceed") == "fail" else "WARN"
            v.add(level, "budget",
                  f"预计 {est:.0f}s 超过预算 {cap}s（{sum(frames)} 帧）；"
                  f"要么加预算，要么压帧数/分段数")
    stats = {
        "frames": frames,
        "total_seconds": total_seconds(frames, fade, fps) if all(f > 0 for f in frames) else None,
        "shots": [len(count_shots(s.get("prompt") or "")) for s in segments],
        "lines": total_lines,
        "stills": len(chars),
    }
    return v, g, stats


# ---------------------------------------------------------------- CLI


def main() -> int:
    ap = argparse.ArgumentParser(description="控制门：为一条片子设计并校验节奏/一致性/验收标准")
    sub = ap.add_subparsers(dest="cmd", required=True)

    p1 = sub.add_parser("plan", help="生成门草案")
    p1.add_argument("--seconds", type=float, required=True, help="目标成片时长")
    p1.add_argument("--beats", required=True,
                    help="各段叙事功能与权重，如 setup=1.0,clash=1.35,finish=1.2")
    p1.add_argument("--modes", default="", help="各段模式 dialogue|ambience，逗号分隔")
    p1.add_argument("--lines-per-seg", default="", help="各段台词句数，如 1,0,2")
    p1.add_argument("--stills", type=int, default=None, help="定妆图数量")
    p1.add_argument("--min-shot", type=float, default=None,
                    help="单镜最短停留秒数（不给则用 profile 的值）")
    p1.add_argument("--max-shot", type=float, default=None,
                    help="单镜最长停留秒数（0 不限；不给则用 profile 的值）")
    p1.add_argument("--tolerance", type=float, default=None,
                    help="总长容差（不给则用 profile 的值）")
    p1.add_argument("--fade", type=float, default=DEFAULT_FADE, help="接缝淡化秒数")
    p1.add_argument("--budget", type=int, default=None, help="总墙钟预算秒数（0 不限）")
    p1.add_argument("--per-seg-budget", type=int, default=0)
    p1.add_argument("--profile", default="", help="给这组门起个名字，便于复用")
    p1.add_argument("--out", default="", help="写入文件（默认打印）")

    p2 = sub.add_parser("check", help="静态校验 plan（含 gates）")
    p2.add_argument("--plan", required=True)
    p2.add_argument("--allow-missing-gates", action="store_true",
                    help="允许没有 gates（降级为告警）")
    p2.add_argument("--json", action="store_true")

    p3 = sub.add_parser("show", help="打印合并后的门（默认门 + 本片覆盖）")
    p3.add_argument("--plan", required=True)

    args = ap.parse_args()

    if args.cmd == "plan":
        data = build_plan_gates(args)
        text = json.dumps(data, ensure_ascii=False, indent=2)
        if args.out:
            os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
            with open(args.out, "w", encoding="utf-8") as f:
                f.write(text + "\n")
            print(f"写到 {args.out}")
        else:
            print(text)
        return 0

    with open(args.plan, encoding="utf-8") as f:
        plan = json.load(f)

    if args.cmd == "show":
        _, g, _ = check_plan(plan, require_gates=not args.allow_missing_gates)
        print(json.dumps(g, ensure_ascii=False, indent=2))
        return 0

    v, g, stats = check_plan(plan, require_gates=not args.allow_missing_gates)
    if args.json:
        print(json.dumps({"verdict": v.worst(), "ok": v.ok(), "items": v.items,
                          "stats": stats}, ensure_ascii=False, indent=2))
    else:
        print(f"门检结论：{v.worst()}")
        print(f"  段帧数 {stats['frames']}  总长 {stats['total_seconds']}s "
              f"镜头数 {stats['shots']}  台词 {stats['lines']} 句  定妆图 {stats['stills']} 张")
        for it in v.items:
            print(f"  [{it['level']}] {it['gate']}: {it['msg']}")
    return 0 if v.ok() else 2


if __name__ == "__main__":
    sys.exit(main())
