"""FlowEdge 本地 TTS 服务 —— Breeze TTS 2（3.47B，BF16，CUDA）

为什么是它：Artificial Analysis 开源权重榜第 2（Elo 1233），支持
「音色设计（不给参考音频，用一句话描述造音色）」「导演（保持音色改表演）」
「零样本克隆」三种模式，显存只要 ~7.7GiB（eager）。

与 Audio8 版本的关键差别（别照抄老配置）：
  * 权重 3.47B / 7.7GB，显存 ~8GB（eager）；--fast-all 会到 ~14.4GB 但要 flash-attn，
    Windows 装不了 flash-attn，所以本服务**只走 eager 路径**。
  * 克隆**必须**同时给 ref_audio 与 ref_text（逐字稿），缺一个官方就拒；
    服务端的取舍是：缺 ref_text 时先去同目录找 `<同名>.txt`，再退 `voices-breeze/<name>/ref.txt`，
    都找不到**显式报错 400**——绝不猜稿子（猜错等于换了个音色）。
  * 没有语速控制（和 Audio8 一样）：speed/rate 收到就忽略，并在响应头写下事实。
  * 采样率 24kHz 单声道；返回的是**完整 WAV**（Go 端直接落盘 .wav）。

接口（与 tts_audio8_pt_server.py 保持同一套契约）：
  GET  /health            -> 轻量探活（不加载模型）
  GET  /health?deep=1     -> 真合成一小段，验证「跑着但坏了」
  GET  /voices            -> 已注册音色 + 内置音色设计预设
  POST /voices/register   -> {"name","audio_path","text"} 注册克隆音色
  POST /unload            -> 释放模型与显存（配合单侧驻留策略）
  POST /tts               -> {"text","voice","rate","speed","ref_audio","ref_text",
                              "instruction","cfg_scale","seed"} -> wav 二进制

环境变量：
  BREEZE_MODEL_DIR      权重目录（默认 D:/AI/Models/breeze_tts_2）
  BREEZE_CODE_DIR       官方推理代码目录（默认 D:/AI/breeze-tts，内含 breeze_infer/ 与 models/）
  BREEZE_PORT           端口（默认 8104）
  BREEZE_VOICES_DIR     克隆音色目录（默认 ./voices-breeze）
  BREEZE_IDLE_UNLOAD    空闲多少秒后卸载显存（0 = 不自动卸载，默认 600）
  BREEZE_DEFAULT_INSTRUCTION  无声色时的默认音色描述
  BREEZE_CFG_SCALE      设计/导演模式的默认 cfg_scale（默认 4.0）
  BREEZE_PRELOAD        1 = 启动即加载（默认懒加载）
"""

from __future__ import annotations

import io
import json
import os
import re
import sys
import threading
import time
import traceback
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent

# 必须在 import torch 之前设！torch 在导入时读这些开关。
# 为什么关静态启动器：Windows 上 inductor 的 StaticallyLaunchedCudaKernel 会把超过
# 2^31 的整数塞给 C 的 long（Windows long 是 32 位）→ OverflowError。
# 代价是退回 Python 启动器（慢），换来快路径可用。
# 实测（RTX 3090）：不开快路径 RTF≈10.8；开快路径+关静态启动器 RTF≈0.66。
os.environ.setdefault("TORCHINDUCTOR_USE_STATIC_CUDA_LAUNCHER", "0")

# 必须离线！权重全在本地，但引擎（deps.go）会传 HF_ENDPOINT=https://hf-mirror.com。
# 不设离线时，Qwen3TTSTokenizer.from_pretrained 仍会走一次 hub 解析，而镜像不可达时
# 没有任何超时——实测卡 280s+ 还停在 load_runtime 里（日志只剩 phase imports），
# 表现为「引擎托管永远起不来、手动跑 18 秒就好」。这两个开关一起设。
os.environ.setdefault("HF_HUB_OFFLINE", "1")
os.environ.setdefault("TRANSFORMERS_OFFLINE", "1")

# numba 的缓存目录：默认落在 site-packages 的 __pycache__ 里，路径又深又带中文，
# 实测在引擎托管的进程里会卡死在 numba.core.caching.ensure_cache_path 的
# NamedTemporaryFile 上死循环（CPU 100%、堆栈不动、13 分钟不返回）。
# 指到一个短的纯 ASCII 目录，规避 Windows 路径长度/临时文件问题。
_numba_cache = Path(os.environ.get("NUMBA_CACHE_DIR", "D:/AI/cache/numba"))
_numba_cache.mkdir(parents=True, exist_ok=True)
os.environ.setdefault("NUMBA_CACHE_DIR", str(_numba_cache))

# TMP/TEMP 也要钉成纯 ASCII 短路径。引擎（外壳）会把子进程的 TMP 指到
# `…\Desktop\flowedge\运行\data\tmp`——带中文的深路径。实测这种环境下
# numba 的 ensure_cache_path 会在 tempfile.NamedTemporaryFile 上空转
# （CPU 100%、堆栈不动、十几分钟不返回），表现为「引擎托管永远起不来」。
# 手动跑（TMP=%LOCALAPPDATA%\Temp）18–70 秒就绪，对照组明确。
_tmp_dir = Path(os.environ.get("BREEZE_TMP_DIR", "D:/AI/cache/tmp"))
_tmp_dir.mkdir(parents=True, exist_ok=True)
os.environ["TMP"] = str(_tmp_dir)
os.environ["TEMP"] = str(_tmp_dir)

# 最后一层保险：关掉 numba 的 JIT。它是被 qwen_tts → librosa 拖进来的，
# 只用于音频重采样这类小函数；开着 JIT 时 numba 会去 ensure_cache_path 建缓存，
# 在引擎托管的进程里会空转（见上，TMP/缓存目录都不是原因，19–21 点实测 6 次复现）。
# 关掉后走纯 Python 实现，功能不变、这条死路直接绕过。
# 想恢复 JIT（性能优先）就设 BREEZE_NUMBA_JIT=1。
if os.environ.get("BREEZE_NUMBA_JIT") != "1":
    os.environ.setdefault("NUMBA_DISABLE_JIT", "1")

MODEL_DIR = Path(os.environ.get("BREEZE_MODEL_DIR", "D:/AI/Models/breeze_tts_2"))
CODE_DIR = Path(os.environ.get("BREEZE_CODE_DIR", "D:/AI/breeze-tts"))
VOICES_DIR = Path(os.environ.get("BREEZE_VOICES_DIR", str(HERE / "voices-breeze")))
# 引擎（deps.go）只认它自己那套变量名：TTS_PORT / AUDIO8_IDLE_UNLOAD。
# 不认这两个就会「端口靠默认值刚好对上、idle_unload 却退回 600s」——那意味着
# 闲置 10 分钟卸载，下次合成再等 225s 预热，诡异到查不出来。两个都认。
IDLE_UNLOAD = int(
    os.environ.get("BREEZE_IDLE_UNLOAD")
    or os.environ.get("AUDIO8_IDLE_UNLOAD")
    or "0"
)
PORT = int(
    os.environ.get("BREEZE_PORT")
    or os.environ.get("TTS_PORT")
    or "8104"
)
DEFAULT_INSTRUCTION = os.environ.get(
    "BREEZE_DEFAULT_INSTRUCTION",
    "一位温柔自信的年轻女性，声音清晰，语气亲切，表达轻快而富有感染力。",
)
DEFAULT_CFG = float(os.environ.get("BREEZE_CFG_SCALE", "4.0"))
# 快路径（CUDA Graph）。默认开：本机实测 eager RTF≈10.8，快到 1 以下才算能用。
FAST = os.environ.get("BREEZE_FAST", "1") == "1"
# 深度解码器的全图编译默认开：配合上面的"关静态启动器"实测可用（RTF 0.66）。
# 注意它让启动预热从 ~20s 变成 ~225s（显存有竞争时实测到 425s），换来生成快 16 倍。
FAST_DEPTH = os.environ.get("BREEZE_FAST_DEPTH", "1") == "1"

# 抢合成锁的上限。Breeze 是单并发自回归，一个卡住的请求会把后面全部堵死
# （2026-10-27 实测：tts 节点 3 分钟超时、客户端放弃，服务端仍在跑，后续请求
# 全排在它后面一起超时，看起来像「服务死了」）。等不到锁就立刻回 503。
SRV_BUSY_TIMEOUT = float(os.environ.get("BREEZE_BUSY_TIMEOUT", "180"))


class BusyError(RuntimeError):
    """服务忙（上一个合成还没结束），映射成 HTTP 503 而不是 500。"""

# 内置「音色设计」预设：FlowEdge 的 voice 字段传这些名字就能直接选声线。
# 值就是发给模型的自然语言指令（中文，与目标文本语言一致）。
BUILTIN_VOICES = {
    "zh_female_warm": "一位温柔自信的年轻女性，声音清晰，语气亲切，表达轻快而富有感染力。",
    "zh_female_calm": "一位沉稳冷静的成年女性，中低音，语速平稳，吐字清晰，像纪录片旁白。",
    "zh_male_deep": "一位成熟稳重的成年男性，低沉厚实的嗓音，语速从容，语气可信。",
    "zh_male_young": "一位二十出头的年轻男性，声音清亮，语气自然松弛，像日常聊天。",
    # 二次元 / 年轻女声声线（2026-10-27 加）：音色设计模式，直接选名字即可。
    "anime_moe": "一位十六七岁的二次元少女，声音又软又甜，音调偏高，咬字带一点撒娇的拖音，情绪明亮可爱，像动画里的萌系女主角。",
    "anime_genki": "一位十七八岁的元气少女，声音明亮有弹性，音调偏高，语速偏快，情绪饱满上扬，像热血动画的女主角在给同伴打气。",
    "anime_healing": "一位温柔的少女，声音轻柔细腻略带气声，音调适中偏高，语速舒缓，像安静动画里的治愈系旁白。",
    "anime_tsundere": "一位十五六岁的少女，声音清亮略冲，语气带着不服输和一点撒娇的起伏，尾音上扬，像动画里的傲娇角色。",
    "anime_idol": "一位年轻的二次元偶像歌手，声音清亮有穿透力，语气元气满满、带笑意，像舞台上的自我介绍。",
    # 调和版（2026-10-27 用户要求「1 和 3 调和」）：把 anime_moe(296Hz 软甜) 与
    # anime_healing(239Hz 温柔气声) 按三种配比重新描述成一条声线，标尺是中位基频落在两者之间。
    "anime_mix_moe70": "一位十六七岁的二次元少女，声音软甜清亮，收住撒娇的拖音，音调偏高但柔和，语速中等，语气温柔可爱，像治愈系动画里的萌系女主角。",
    "anime_mix_5050": "一位年轻少女，声音清甜而柔和，音调中等偏高，语速舒缓，语气温柔里带一点明快的笑意，既软又安静。",
    "anime_mix_heal70": "一位温柔的年轻少女，声音轻柔清甜，略带气声，音调适中偏高，语速缓慢，尾音轻轻收住，安静又可爱。",
    "zh_narrator": "一位专业的中文旁白播音员，中性、端正、标准普通话，节奏均匀，无夸张情绪。",
    "en_female_warm": "A warm, thoughtful young woman with a clear voice and a calm, reflective delivery.",
    "en_male_narrator": "A professional male narrator with a deep, resonant voice and an even, measured pace.",
}
DEFAULT_VOICE = "zh_female_warm"

# 模型状态：进程内单例，单并发（Breeze 是自回归，同一时刻只服务一个请求）
_state = {
    "tokenizer": None,
    "model": None,
    "audio_tokenizer": None,
    "runtime": None,
    "sr": 24000,
    "loaded_at": None,
    "last_used": None,
    "device": None,
}
_lock = threading.Lock()
_synth_lock = threading.Lock()
# 加载锁：预热线程和第一个 /tts 请求可能同时进来，必须保证只加载一次
_load_lock = threading.Lock()


# ---------------------------------------------------------------- 模型管理


def _imports():
    """把官方代码目录插进 sys.path 后再导入（官方代码用 `from models.breeze import ...` 这种顶层路径）。"""
    code = str(CODE_DIR)
    if code not in sys.path:
        sys.path.insert(0, code)
    import numpy as np
    import soundfile as sf
    import torch

    from breeze_infer.runtime import (  # noqa: E402
        load_runtime,
        resolve_device,
        set_all_seeds,
        update_generation_config_for_breeze,
    )
    from breeze_infer.templates import (  # noqa: E402
        get_template,
        prepare_inputs,
        select_template_name,
    )
    from models.fast_streaming import FastBreezeStreamingRuntime, FastStreamingConfig  # noqa: E402

    return {
        "np": np,
        "sf": sf,
        "torch": torch,
        "load_runtime": load_runtime,
        "resolve_device": resolve_device,
        "set_all_seeds": set_all_seeds,
        "update_generation_config_for_breeze": update_generation_config_for_breeze,
        "get_template": get_template,
        "prepare_inputs": prepare_inputs,
        "select_template_name": select_template_name,
        "FastBreezeStreamingRuntime": FastBreezeStreamingRuntime,
        "FastStreamingConfig": FastStreamingConfig,
    }


def load():
    """加载模型（幂等）。返回 (deps, runtime)。"""
    with _load_lock:
        if _state["runtime"] is not None:
            return _state["deps"], _state["runtime"]
        try:
            return _load_locked()
        except BaseException:
            # 加载失败必须把已经吃进去的显存退回来。
            #
            # 2026-10-27 实测的病灶：加载在 `phase runtime`（权重已进显存、CUDA Graph
            # 还没捕获完）时失败，进程 loaded=false 却占着 16GB 不下来。
            # 于是「再试一次」装不下、别的大模型也装不下 —— 两个都用不了。
            # 现在失败即回收，代价只是这次加载白等，卡不会留下死块。
            try:
                before = _vram_snapshot()
                after = _release_vram()
                print(
                    f"[breeze] 加载失败，已回收显存："
                    f"allocated {before.get('allocated_mb')} -> {after.get('allocated_mb')} MiB",
                    flush=True,
                )
            except Exception:
                traceback.print_exc()
            raise


def _load_locked():
    t0 = time.time()
    print(
        f"[breeze] load start pid={os.getpid()} ppid={os.getppid()} port={PORT} "
        f"fast={FAST} fast_depth={FAST_DEPTH} idle={IDLE_UNLOAD} "
        f"static_launcher={os.environ.get('TORCHINDUCTOR_USE_STATIC_CUDA_LAUNCHER')} "
        f"hf_endpoint={os.environ.get('HF_ENDPOINT')} offline={os.environ.get('HF_HUB_OFFLINE')} "
        f"cwd={os.getcwd()} tmp={os.environ.get('TMP')} temp={os.environ.get('TEMP')} "
        f"numba={os.environ.get('NUMBA_CACHE_DIR')}",
        flush=True,
    )
    deps = _imports()
    print(f"[breeze] phase imports +{time.time() - t0:.1f}s", flush=True)
    torch = deps["torch"]

    if not MODEL_DIR.is_dir():
        raise FileNotFoundError(f"权重目录不存在：{MODEL_DIR}")
    if not (CODE_DIR / "breeze_infer").is_dir():
        raise FileNotFoundError(f"官方代码目录不对（缺 breeze_infer/）：{CODE_DIR}")

    # 设备可用 FLOWEDGE_TTS_DEVICE 强制（cpu / cuda）；不设就走官方 resolve_device()。
    # 为什么要这个开关：Breeze 常驻显存 8~16GB，和 H3 的 20.5GB 在同卡上不能共存，
    # 每次出片前都得手工停一次。切到 CPU 就不抢卡，代价是 RTF 大幅变慢（见文档实测）。
    # 优先级：环境变量 > data/deps.json 的 tts.device > 官方自动探测。
    #
    # 为什么要读 deps.json 而不是只认环境变量：引擎是**工作站进程**派生的，
    # 继承的是工作站启动时的环境；用户后来 setx 的变量要连工作站一起重启才生效
    # （2026-10-26 实测：setx 后重启引擎，TTS 仍然 device=cuda:0）。
    # 配置走文件就不依赖谁先启动。
    _forced = os.environ.get("FLOWEDGE_TTS_DEVICE", "").strip().lower()
    _forced = _forced.strip().lower()
    if not (_forced == "cpu" or _forced.startswith("cuda")):
        try:
            import json as _json
            _cfg = _json.loads((Path(__file__).resolve().parent.parent / "data" / "deps.json")
                               .read_text(encoding="utf-8-sig"))
            _forced = str((_cfg.get("tts") or {}).get("device", "")).strip().lower()
        except Exception:
            _forced = ""
    device = _forced if (_forced == "cpu" or _forced.startswith("cuda")) else deps["resolve_device"]()
    # 配置里写 "cuda"（人话）要被翻译成 "cuda:0"：torch.cuda.set_device("cuda") 会抛
    # ValueError: Expected a torch.device with a specified index or an integer, but got:cuda
    # （2026-10-27 实测，表面上变成「预加载失败，转为懒加载」+ loaded 永远 false）。
    if str(device) == "cuda":
        device = "cuda:0"
    # CPU 上没有 CUDA Graph：快路径全部关掉，否则 torch.cuda.CUDAGraph 直接报错。
    fast_path = bool(FAST) and str(device).startswith("cuda")
    print(f"[breeze] device={device} fast_path={fast_path}", flush=True)

    # 显存预检：不够就当场说清楚，不要「沉默地慢慢磨」。
    # 实测 2026-10-26：和 H3 抢卡时启动会卡在预热里 300s+ 最后超时，日志只停在
    # `phase runtime`，全程不报错——这种失败最难查（我因此误判过两次「引擎托管坏了」）。
    # 宁可 2 秒失败、并给出可执行的下一步。
    # BREEZE_MIN_FREE_GB 可覆盖阈值；BREEZE_ALLOW_LOW_VRAM=1 可强行跳过（排障用）。
    if str(device).startswith("cuda") and os.environ.get("BREEZE_ALLOW_LOW_VRAM") != "1":
        # 阈值按**峰值**给，不是按权重给：权重吃 8.3GB，但 CUDA Graph 捕获会把整卡吃满。
        # 2026-10-27 重测（干净卡、无 H3/ComfyUI）：驻留 15.2GB、加载完峰值 19.9GB，
        # 且当天在 ~20.5GiB 空闲时真实加载成功过 ⇒ 阈值取 20.3，不再用 2026-10-26
        # 那张「H3 与 TTS 抢卡」时采的 22.0（它会把干净卡也拒掉，白等一晚上）。
        # H3(20.5GB)/ComfyUI 占着时空闲仍在 10GB 以下，照样会被这条拦住——语义没变。
        _need = float(os.environ.get("BREEZE_MIN_FREE_GB") or (20.3 if fast_path else 9.5))
        # 采样 3 次取最小：ComfyUI/H3 是异步加载的，单次采样很容易撞上它们「刚好卸完」
        # 的空隙 —— 2026-10-27 实测过一次 22.0GiB 放行、随后权重加载花了 334s（正常 11-25s）。
        _samples = []
        for _ in range(3):
            try:
                _samples.append(torch.cuda.mem_get_info()[0] / (1024 ** 3))
            except Exception:
                _samples = []
                break
            time.sleep(1.5)
        if _samples:
            _free = min(_samples)
            print(
                f"[breeze] vram free={_free:.1f}GiB (min of {len(_samples)} samples) "
                f"need≈{_need:.1f}GiB",
                flush=True,
            )
            if _free < _need:
                raise RuntimeError(
                    f"显存不足：空闲 {_free:.1f}GiB < 需要 ~{_need:.1f}GiB。"
                    "Breeze 快路径峰值约 23.4GB（权重 8.3 + CUDA Graph 捕获吃满整卡），"
                    "和 H3(20.5GB) / ComfyUI 装不下同一张 24GB 卡。"
                    "先腾显存再启动：POST http://127.0.0.1:8189/free "
                    '{"unload_models":true,"free_memory":true}（主实例 :8188 同理），'
                    "确认 nvidia-smi 空闲 ≥22000 MiB 后重新 /api/deps/tts/start。"
                    "确要硬上可设 BREEZE_ALLOW_LOW_VRAM=1。"
                )
    # 关键：eager，不用 flash_attention_2。Windows 没有 flash-attn，官方
    # 的 --fast-all 需要它；eager 路径官方实测 ~7.7GiB，够用。
    tokenizer, model, audio_tokenizer = deps["load_runtime"](
        MODEL_DIR, device=device, attn_implementation="eager"
    )
    print(f"[breeze] phase weights +{time.time() - t0:.1f}s", flush=True)
    deps["update_generation_config_for_breeze"](model)

    config = deps["FastStreamingConfig"](
        max_new_tokens=1500,
        max_seq_len=2048,
        # 分阶段开关（不能只给 fast_all，那会把做过头的阶段也打开）：
        #   fast_text_encoder / backbone_prefill / backbone_decode / codec
        #     都只用 torch.cuda.CUDAGraph 静态捕获，Windows 可用；
        #   fast_depth_decoder 走 torch.compile→inductor→triton，Windows 上
        #     会炸在 _StaticCUDA_LAUNCHER 的 int→C long 溢出（实测 triton 3.5.1/3.8 都炸），
        #     所以默认关掉，用 BREEZE_FAST_DEPTH=1 才开。
        fast_all=None,
        fast_text_encoder=fast_path,
        fast_backbone_prefill=fast_path,
        fast_backbone_decode=fast_path,
        fast_codec=fast_path,
        fast_depth_decoder=bool(FAST_DEPTH) and fast_path,
        repetition_penalty=1.1,
    )
    runtime = deps["FastBreezeStreamingRuntime"](
        model, audio_tokenizer, config, tokenizer=tokenizer
    )
    print(f"[breeze] phase runtime +{time.time() - t0:.1f}s", flush=True)

    # 快路径必须按官方做法先预热，否则第一个请求里现场捕获图（几十秒）会算进 RTF。
    if fast_path and getattr(runtime, "fast_enabled", False):
        try:
            from dataclasses import replace as _replace

            from models.warmup_profile import load_warmup_profile

            profile = load_warmup_profile(CODE_DIR / "configs" / "fast.json")
            profile = _replace(profile, codec_chunk_frames=runtime.codec_chunk_frames)
            manifest = runtime.warmup_from_profile(profile)
            print(f"[breeze] fast warmup {manifest['total_elapsed_ms']:.0f} ms", flush=True)
        except Exception:
            traceback.print_exc()
            print("[breeze] 预热失败，继续（首个请求会慢一次）", flush=True)

    sr = int(getattr(runtime, "sample_rate", 24000) or 24000)
    vram = 0.0
    if torch.cuda.is_available():
        vram = torch.cuda.memory_allocated() / (1024 ** 3)

    _state.update(
        deps=deps,
        tokenizer=tokenizer,
        model=model,
        audio_tokenizer=audio_tokenizer,
        runtime=runtime,
        sr=sr,
        device=device,
        loaded_at=time.time(),
        last_used=time.time(),
    )
    print(
        f"[breeze] ready in {time.time() - t0:.1f}s device={device} sr={sr} vram={vram:.2f}GiB",
        flush=True,
    )
    return deps, runtime


def _vram_snapshot() -> dict:
    """本进程的显存读数（MiB）。拿不到就返回空 dict，绝不抛异常。"""
    try:
        import torch

        if not torch.cuda.is_available():
            return {}
        free, total = torch.cuda.mem_get_info()
        return {
            "allocated_mb": round(torch.cuda.memory_allocated() / (1024 ** 2)),
            "reserved_mb": round(torch.cuda.memory_reserved() / (1024 ** 2)),
            "free_mb": round(free / (1024 ** 2)),
            "total_mb": round(total / (1024 ** 2)),
        }
    except Exception:
        return {}


def _release_vram() -> dict:
    """断开 model/runtime 的引用环，真正把显存还给驱动。返回释放后的读数。

    ★ 为什么「把 _state 里的字段置 None」不够（2026-10-28 实测，这是假释放的根因）：
      `FastBreezeStreamingRuntime.__init__` 里 `self.model = model`，
      同时 `model._fast_text_encoder_cudagraph = ...`；
      runtime 还持有 `_backbone_graphs` / `_backbone_prefill_graphs`
      （CUDAGraph + 静态输入张量，捕获时把整卡吃满）。
      于是 runtime ↔ model 是**循环引用**：CPython 的引用计数永远归不到零，
      只置 None 时对象仍然活着，PyTorch caching allocator 认为这些块是
      *allocated* 而不是 *cached*，`torch.cuda.empty_cache()` 无权动它们。

      实测代价：POST /unload 返回 {"ok":true,"unloaded":true}，
      而该进程仍占 16274 MiB（15.9 GiB），设备空闲只从 6737 变成 6730 MiB。
      卸载是**假的**，更糟的是它会骗过上游——引擎以为「腾好了」，下一次出片照样 OOM。

    正确顺序（顺序错了就白做）：
      ① 把强引用挪到局部并置空 _state
      ② 打断 runtime ↔ model 的环（runtime 侧 setattr None、清 graph 表）
      ③ gc.collect() ×2 —— 循环引用只有 gc 收得掉；第二轮收拾析构中新生成的环
      ④ synchronize → empty_cache → ipc_collect —— 块先变 cached，才能还给驱动
    """
    runtime = _state.get("runtime")
    model = _state.get("model")
    audio_tokenizer = _state.get("audio_tokenizer")
    tokenizer = _state.get("tokenizer")

    _state.update(
        tokenizer=None,
        model=None,
        audio_tokenizer=None,
        runtime=None,
        loaded_at=None,
    )

    # ② 断环
    if runtime is not None:
        for attr in ("model", "audio_tokenizer", "tokenizer", "_codec_runtime"):
            try:
                setattr(runtime, attr, None)
            except Exception:
                pass
        for attr in ("_backbone_graphs", "_backbone_prefill_graphs"):
            pool = getattr(runtime, attr, None)
            if isinstance(pool, dict):
                pool.clear()  # 里面的 CUDAGraph 引用一断才能析构
        for attr in ("_backbone_graph", "_backbone_prefill_graph", "_depth_decoder_graph"):
            try:
                setattr(runtime, attr, None)
            except Exception:
                pass
    if model is not None:
        try:
            model._fast_text_encoder_cudagraph = None
        except Exception:
            pass

    del runtime, model, audio_tokenizer, tokenizer

    # ③ 循环引用只能靠 gc；引用计数在这里完全无效。
    import gc

    gc.collect()
    gc.collect()

    # ④ 同步 + 归还。synchronize 必须在 empty_cache 之前：流上没跑完的 kernel
    #    还引用着这些块，此时 free 会被跳过。
    try:
        import torch

        if torch.cuda.is_available():
            try:
                torch.cuda.synchronize()
            except Exception:
                pass
            torch.cuda.empty_cache()
            try:
                torch.cuda.ipc_collect()
            except Exception:
                pass
    except Exception:
        traceback.print_exc()

    return _vram_snapshot()


def unload() -> bool:
    """释放模型与显存。返回是否真的卸载了。"""
    if _state["runtime"] is None and _state["model"] is None:
        return False
    with _synth_lock:
        try:
            before = _vram_snapshot()
            after = _release_vram()
            print(
                f"[breeze] unloaded  allocated {before.get('allocated_mb')} -> "
                f"{after.get('allocated_mb')} MiB, reserved {before.get('reserved_mb')} -> "
                f"{after.get('reserved_mb')} MiB, device free {before.get('free_mb')} -> "
                f"{after.get('free_mb')} MiB",
                flush=True,
            )
            return True
        except Exception:
            traceback.print_exc()
            return False


def _idle_watcher():
    while True:
        time.sleep(10)
        if IDLE_UNLOAD <= 0:
            continue
        last = _state.get("last_used")
        if _state["runtime"] is not None and last and (time.time() - last) > IDLE_UNLOAD:
            print(f"[breeze] idle {IDLE_UNLOAD}s -> unload", flush=True)
            unload()


# ---------------------------------------------------------------- 文本与音色


_SENT_SPLIT = re.compile(r"(?<=[。！？；!?;\n])")


def split_text(text: str, limit: int = 180) -> list[str]:
    """按句切到 limit 字以内。自回归模型对超长文本容易跑飞，切开更稳。"""
    text = (text or "").strip()
    if not text:
        return []
    if len(text) <= limit:
        return [text]
    parts, buf = [], ""
    for seg in _SENT_SPLIT.split(text):
        if not seg:
            continue
        if len(buf) + len(seg) <= limit:
            buf += seg
            continue
        if buf:
            parts.append(buf)
        while len(seg) > limit:          # 没有标点的超长句，硬切
            parts.append(seg[:limit])
            seg = seg[limit:]
        buf = seg
    if buf:
        parts.append(buf)
    return [p for p in parts if p.strip()]


def _listed_voices() -> dict:
    """内置预设 + 目录里注册的克隆音色。"""
    out = {k: {"type": "design", "instruction": v} for k, v in BUILTIN_VOICES.items()}
    if VOICES_DIR.is_dir():
        for d in sorted(VOICES_DIR.iterdir()):
            meta = d / "meta.json"
            if d.is_dir() and meta.is_file():
                try:
                    m = json.loads(meta.read_text(encoding="utf-8"))
                except Exception:
                    continue
                out[d.name] = {"type": "clone", "ref_text": m.get("text", "")}
    return out


def _resolve_voice(voice: str | None) -> tuple[str | None, str | None]:
    """把 voice 名字解析成 (instruction, 克隆参考音频路径)。

    - 是内置预设名 -> (对应指令, None)
    - 是已注册克隆名 -> (None, ref.wav 路径)
    - 没给/不认识 -> 如果是像「描述」的长句就当指令用，否则回落默认音色
    """
    name = (voice or "").strip()
    if not name:
        return BUILTIN_VOICES[DEFAULT_VOICE], None
    if name in BUILTIN_VOICES:
        return BUILTIN_VOICES[name], None

    clone_dir = VOICES_DIR / name
    if clone_dir.is_dir() and (clone_dir / "ref.wav").is_file():
        return None, str(clone_dir / "ref.wav")

    # 不是名字而是一句描述（含空格或中文长句）：直接当设计指令
    if len(name) >= 8 or " " in name:
        return name, None
    print(f"[breeze] unknown voice '{name}' -> default {DEFAULT_VOICE}", flush=True)
    return BUILTIN_VOICES[DEFAULT_VOICE], None


def _find_ref_text(ref_audio: str, voice: str | None, given: str | None) -> str | None:
    """找参考音频的逐字稿。顺序：请求给的 -> 音频同名 .txt -> voices/<name>/ref.txt。"""
    if given and given.strip():
        return given.strip()
    p = Path(ref_audio)
    for cand in (p.with_suffix(".txt"), p.with_name(p.stem + ".ref.txt")):
        if cand.is_file():
            return cand.read_text(encoding="utf-8").strip()
    if voice:
        cand = VOICES_DIR / voice / "ref.txt"
        if cand.is_file():
            return cand.read_text(encoding="utf-8").strip()
    return None


# ---------------------------------------------------------------- 合成


def synth(text, voice=None, ref_audio=None, ref_text=None,
          instruction=None, cfg_scale=None, seed=42):
    """合成语音，返回 (wav 字节, 元信息)。"""
    if not text or not text.strip():
        raise ValueError("text 为空")

    deps, runtime = load()
    np = deps["np"]
    sf = deps["sf"]
    torch = deps["torch"]

    # 音色与模式判定（顺序即优先级：显式参考音频 > voice 解析结果）
    design_ins, clone_ref = _resolve_voice(voice)
    if ref_audio:
        clone_ref = ref_audio
        design_ins = None
    if clone_ref:
        design_ins = None

    ref_text_found = None
    if clone_ref:
        if not Path(clone_ref).is_file():
            raise FileNotFoundError(f"参考音频不存在：{clone_ref}")
        ref_text_found = _find_ref_text(clone_ref, voice, ref_text)
        if not ref_text_found:
            raise ValueError(
                "克隆模式必须提供参考音频的逐字稿（ref_text），否则音色会漂。"
                f"已尝试：请求字段 / {Path(clone_ref).with_suffix('.txt')} / "
                f"voices-breeze/{voice or '<voice>'}/ref.txt —— 都没有。"
            )

    ins = (instruction or "").strip() or design_ins
    if clone_ref:
        mode = "direction" if ins else "clone"
    else:
        mode = "design" if ins else "plain"

    if cfg_scale is None:
        cfg_scale = 1.0 if mode in ("clone", "plain") else DEFAULT_CFG
    if not (cfg_scale > 0):
        raise ValueError("cfg_scale 必须大于 0")

    pieces = split_text(text)
    if not pieces:
        raise ValueError("text 切分后为空")

    wavs = []
    notes = []
    t0 = time.time()
    for idx, piece in enumerate(pieces):
        request = {"id": f"fe-{uuid.uuid4().hex[:8]}-{idx}", "text": piece, "speaker": "S0"}
        if ins:
            request["instruction"] = ins
        if clone_ref:
            request["ref_audio_path"] = str(clone_ref)
            request["ref_text"] = ref_text_found

        template_name = deps["select_template_name"](request)
        deps["set_all_seeds"](seed)
        inputs = deps["prepare_inputs"](
            _state["tokenizer"], _state["audio_tokenizer"], _state["model"],
            [request], deps["get_template"](template_name),
            guidance_scale=cfg_scale,
            guidance_scale_ref=None,
            guidance_scale_ins=None,
        )
        buf = []
        for chunk in runtime.iter_audio_chunks(
            inputs, request_id=request["id"], seed=seed
        ):
            a = chunk.audio
            if hasattr(a, "detach"):
                a = a.detach().float().cpu().numpy()
            buf.append(np.asarray(a).reshape(-1))
        if not buf:
            raise RuntimeError(f"第 {idx + 1} 段没有产出音频（文本：{piece[:30]}…）")
        merged = np.concatenate(buf).astype(np.float32)
        # 段间留 120ms 静音，避免拼接处爆音
        if idx > 0:
            merged = np.concatenate(
                [np.zeros(int(_state["sr"] * 0.12), dtype=np.float32), merged]
            )
        wavs.append(merged)
        peak = float(np.abs(merged).max()) if merged.size else 0.0
        if peak > 1.0:
            notes.append(f"seg{idx + 1} peak={peak:.2f} (clipped)")

    audio = np.concatenate(wavs)
    bio = io.BytesIO()
    sf.write(bio, audio, _state["sr"], format="WAV", subtype="PCM_16")
    wav = bio.getvalue()

    info = {
        "mode": mode,
        "voice": voice or "",
        "segments": len(pieces),
        "seconds": round(len(audio) / _state["sr"], 3),
        "rtf": round((time.time() - t0) / max(len(audio) / _state["sr"], 1e-6), 3),
        "sample_rate": _state["sr"],
        "cfg_scale": cfg_scale,
        "notes": notes,
        "finished": not notes,
    }
    return wav, info


def health(deep: bool = False) -> dict:
    body = {
        "status": "ok",
        "backend": "breeze-tts-2",
        "model": "Breeze-TTS-2 (3.47B BF16, eager)",
        "model_dir": str(MODEL_DIR),
        "code_dir": str(CODE_DIR),
        "sample_rate": _state["sr"],
        "loaded": _state["runtime"] is not None,
        "device": _state["device"],
        "supports_speed": False,
        "supports_clone": True,
        "supports_voice_design": True,
        "modes": ["clone", "direction", "design", "plain"],
        "voices": sorted(_listed_voices().keys()),
        "idle_unload": IDLE_UNLOAD,
        # 进程侧显存读数。**这是「卸载是真是假」的唯一判据**：
        # loaded=false 而这里 allocated_mb 仍很大，就说明对象没析构（见 _release_vram）。
        "vram": _vram_snapshot(),
    }
    if deep:
        # 加载中：显式回 503「还没好」，别让探活把「正在预热」误判成「坏了」。
        if _state["runtime"] is None:
            body["status"] = "loading"
            body["deep_ok"] = False
            body["detail"] = (
                "模型加载/预热中（含 CUDA Graph 捕获，本机实测约 225s），"
                "此时请按未就绪轮询 /health"
            )
            return body
        try:
            t0 = time.time()
            wav, info = synth("好的", DEFAULT_VOICE, cfg_scale=DEFAULT_CFG)
            body.update(
                deep_ok=len(wav) > 1000,
                deep_wav_bytes=len(wav),
                deep_ms=int((time.time() - t0) * 1000),
                deep_mode=info["mode"],
            )
            if not body["deep_ok"]:
                body["status"] = "degraded"
                body["detail"] = "合成成功但音频过短"
        except Exception as exc:
            body["status"] = "error"
            body["deep_ok"] = False
            body["detail"] = f"{type(exc).__name__}: {exc}"
            traceback.print_exc()
    return body


# ---------------------------------------------------------------- HTTP


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # 降噪：只留我们自己的 print
        return

    def _json(self, code, obj):
        payload = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def _body(self):
        n = int(self.headers.get("Content-Length") or 0)
        if n <= 0:
            return {}
        raw = self.rfile.read(n)
        try:
            return json.loads(raw.decode("utf-8"))
        except Exception:
            raise ValueError("请求体不是合法 JSON")

    def do_GET(self):
        path, _, query = self.path.partition("?")
        if path == "/health":
            deep = "deep=1" in query
            body = health(deep=deep)
            code = 200 if body["status"] in ("ok", "degraded") else 503
            return self._json(code, body)
        if path == "/voices":
            return self._json(200, {"backend": "breeze-tts-2", "voices": _listed_voices()})
        return self._json(404, {"error": f"unknown path {path}"})

    def do_POST(self):
        path, _, _q = self.path.partition("?")
        try:
            if path == "/tts":
                return self._handle_tts()
            if path == "/voices/register":
                return self._handle_register()
            if path == "/unload":
                return self._json(200, {"ok": True, "unloaded": unload()})
            return self._json(404, {"error": f"unknown path {path}"})
        except ValueError as exc:
            return self._json(400, {"error": str(exc)})
        except BusyError as exc:
            return self._json(503, {"error": str(exc)})
        except FileNotFoundError as exc:
            return self._json(400, {"error": str(exc)})
        except Exception as exc:
            traceback.print_exc()
            return self._json(500, {"error": f"{type(exc).__name__}: {exc}"})

    def _handle_tts(self):
        body = self._body()
        text = (body.get("text") or "").strip()
        if not text:
            raise ValueError("text 不能为空")
        voice = body.get("voice") or None
        ref_audio = body.get("ref_audio") or None
        ref_text = body.get("ref_text") or None
        instruction = body.get("instruction") or None
        cfg = body.get("cfg_scale")
        cfg = float(cfg) if cfg not in (None, "") else None
        seed = int(body.get("seed") or 42)
        if body.get("speed") or body.get("rate"):
            print("[breeze] speed/rate 被忽略（Breeze TTS 2 不支持语速控制）", flush=True)

        if not _synth_lock.acquire(timeout=SRV_BUSY_TIMEOUT):
            raise BusyError(
                f"服务忙：上一个合成超过 {SRV_BUSY_TIMEOUT:.0f}s 还没结束（单并发）。"
                "要么等它、要么重启 TTS（POST /api/deps/tts/stop 再 start）。"
                "根因通常是显存被别的进程挤占导致解码极慢——先 nvidia-smi 看空闲显存。"
            )
        try:
            _state["last_used"] = time.time()
            wav, info = synth(
                text[:4000], voice=voice, ref_audio=ref_audio, ref_text=ref_text,
                instruction=instruction, cfg_scale=cfg, seed=seed,
            )
            _state["last_used"] = time.time()
        finally:
            _synth_lock.release()

        print(
            f"[breeze] {len(text)} chars mode={info['mode']} segs={info['segments']} "
            f"-> {info['seconds']}s audio rtf={info['rtf']} {len(wav)} bytes",
            flush=True,
        )
        self.send_response(200)
        self.send_header("Content-Type", "audio/wav")
        self.send_header("Content-Length", str(len(wav)))
        self.send_header("X-TTS-Backend", "breeze-tts-2")
        self.send_header("X-TTS-Mode", info["mode"])
        self.send_header("X-TTS-Sample-Rate", str(info["sample_rate"]))
        self.send_header("X-TTS-Seconds", str(info["seconds"]))
        self.send_header("X-TTS-Rtf", str(info["rtf"]))
        self.send_header("X-TTS-Supports-Speed", "0")
        self.end_headers()
        self.wfile.write(wav)

    def _handle_register(self):
        body = self._body()
        name = re.sub(r"[^A-Za-z0-9_\-]", "", (body.get("name") or "").strip())
        audio_path = (body.get("audio_path") or "").strip()
        text = (body.get("text") or "").strip()
        if not name:
            raise ValueError("name 只能包含字母/数字/下划线/短横线")
        if not Path(audio_path).is_file():
            raise FileNotFoundError(f"参考音频不存在：{audio_path}")
        if not text:
            raise ValueError("必须提供参考音频的逐字稿 text")
        old = VOICES_DIR / name
        new = VOICES_DIR / (name + f".tmp{int(time.time())}")
        new.mkdir(parents=True, exist_ok=True)
        import shutil

        shutil.copyfile(audio_path, new / "ref.wav")
        (new / "ref.txt").write_text(text, encoding="utf-8")
        (new / "meta.json").write_text(
            json.dumps({"name": name, "text": text, "audio_path": audio_path},
                       ensure_ascii=False, indent=2),
            encoding="utf-8",
        )
        if old.exists():
            shutil.rmtree(old, ignore_errors=True)
        new.rename(old)
        return self._json(200, {"ok": True, "name": name, "voices": sorted(_listed_voices().keys())})


def main():
    VOICES_DIR.mkdir(parents=True, exist_ok=True)
    if IDLE_UNLOAD > 0:
        threading.Thread(target=_idle_watcher, daemon=True).start()

    srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(
        f"[breeze] listening on :{PORT} model={MODEL_DIR} code={CODE_DIR} "
        f"idle_unload={IDLE_UNLOAD}",
        flush=True,
    )

    # 预热放后台线程：必须先把端口绑上、先能回答 /health，否则引擎探活会看到一个
    # 225 秒都不存在的端口，直接判「起不来」。加载期间 /health 回 200 + loaded=false，
    # /health?deep=1 回 503「正在预热」。
    if os.environ.get("BREEZE_PRELOAD", "1") == "1":
        def _warm():
            try:
                load()
            except Exception:
                traceback.print_exc()
                print("[breeze] 预加载失败，转为懒加载（首个 /tts 再试）", flush=True)

        threading.Thread(target=_warm, daemon=True).start()

    srv.serve_forever()


if __name__ == "__main__":
    main()
