"""agent_task 的 DSH 适配器（替代 dsh_agent.py 的薄包装）。

为什么要有这一层
--------------
dsh_agent.py 只做了三件事：读密钥、`cmd /c dsh headless`、把 stdout 透传。够跑通，
但在编排里跑多了会暴露六个问题，这个适配器逐个补上：

  1. **没有硬超时** —— 900s 只是引擎侧的超时，dsh 自己挂住时子进程不会退。这里用
     Popen + communicate(timeout)，超时后 `taskkill /T /F` 干掉**整棵进程树**
     （dsh 是 .cmd，只杀父进程会留下 node 子进程继续写会话文件）。
  2. **没有重试** —— 免费模型池会 429 / 首包超时。这里对**可重试错误**做指数退避
     （429 / rate / timeout / 5xx / 空产出），业务错误（语法错、权限拒绝）不重试。
  3. **输出没有结构** —— 引擎只能拿 chars 计数或 contains 判断。这里让 agent 在末尾
     给一行 `RESULT {"ok":true,"summary":"...","files":[...]}`，适配器解析成结构化
     meta；没给就退化成"exit==0 即 ok"，不会因为格式没遵守就判失败。
  4. **prompt 走 argv 有长度上限** —— Windows 命令行 32767 字符，长规格会爆。超过阈值
     就落临时文件，改成"读这个文件并执行"。
  5. **并发写同一个会话目录** —— 每个任务默认给唯一 `--session-id`，互不覆盖；
     需要复用上下文时才用 `--session-id` 显式指定。
  6. **没有成本/耗时留痕** —— 每次调用在 workdir 的 `.adapter/` 下写一份 meta
     （attempts、每次耗时、exit、模型、session、错误原因），事后可对账。

用法
----
    python dsh_adapter.py "<prompt>"                    # 兼容旧契约
    python dsh_adapter.py --prompt-file p.txt --timeout 1200 --retries 3
    python dsh_adapter.py "<prompt>" --workdir W --result W/out.json

退出码：0 成功；3 密钥缺失；4 全部重试后仍失败；5 超时；2 prompt 为空。
stdout 只放 agent 产出（引擎当结果用），其余全部进 stderr。
"""

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import time
import uuid

HOME = os.path.expanduser("~")
CRED = os.path.join(HOME, ".dsh", ".credentials.yaml")
HERE = os.path.dirname(os.path.abspath(__file__))
PATCH = os.path.join(HERE, "dsh-model.yml")

# argv 里放 prompt 的安全上限（Windows 命令行总长 32767，留足余量给路径与参数）
ARGV_PROMPT_LIMIT = 6000

# 这些特征出现时值得重试；业务错误不重试，避免把一次真实的失败重试成一小时
RETRYABLE = re.compile(
    r"(429|rate.?limit|too many requests|timeout|timed out|ETIMEDOUT|ECONNRESET|"
    r"502|503|504|temporarily unavailable|overloaded|NO_ADAPTER|empty (output|response))",
    re.IGNORECASE,
)

# 配置类错误重试没有意义（例如 --session-id 指到一个不存在的会话）
NOT_RETRYABLE = re.compile(
    r"(session .*does not exist|unknown option|invalid (value|argument)|"
    r"not found|no such file|cannot find module|permission denied|EACCES)",
    re.IGNORECASE,
)

# 宽容匹配：`RESULT {...}` / `RESULT: {...}` / 大小写差异都认；有多个时取最后一个
RESULT_LINE = re.compile(r"RESULT\s*:?\s*(\{[^{}]*\})", re.IGNORECASE)

RESULT_SPEC = (
    "\n\n---\n"
    "收尾格式（务必遵守，编排器要靠它判定成败）：最后单独一行给出\n"
    'RESULT {"ok": true, "summary": "一句话说明你做了什么", "files": ["改动过的文件相对路径"]}\n'
    "如果任务没做完或你认为不合格，把 ok 写成 false 并在 summary 里说明卡在哪。\n"
)


def read_key(name="OPENCODE_API_KEY"):
    if os.environ.get(name):
        return os.environ[name]
    try:
        with open(CRED, encoding="utf-8", errors="replace") as f:
            for line in f:
                s = line.strip()
                if s.startswith(name + ":"):
                    val = s.split(":", 1)[1].strip().strip("'\"")
                    if val:
                        return val
    except OSError:
        pass
    return ""


def resolve_dsh():
    path = shutil.which("dsh") or os.path.join(HOME, "AppData", "Roaming", "npm", "dsh.cmd")
    return path


def kill_tree(pid):
    """杀整棵进程树：dsh 是 .cmd 包着 node，只杀父进程会留下 node 继续写文件。"""
    try:
        subprocess.run(["taskkill", "/T", "/F", "/PID", str(pid)],
                       capture_output=True, timeout=20)
    except Exception:
        pass


def run_once(prompt, env, dsh, timeout, session_id, model=None, patch=None):
    """跑一次 dsh headless。返回 (ok, stdout, stderr, exit_code, timed_out)。"""
    use_patch = patch or PATCH
    dsh_js = os.path.join(HOME, "AppData", "Roaming", "npm", "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js")
    node = shutil.which("node")
    if dsh.lower().endswith((".cmd", ".bat")) and node and os.path.exists(dsh_js):
        # 直接用 node 跑 bin.js：绕开 cmd /c 包装，避免子进程组在下一轮收到 Ctrl+C
        cmd = [node, dsh_js, "headless", "--patch", use_patch]
    elif dsh.lower().endswith((".cmd", ".bat")):
        cmd = ["cmd", "/c", dsh, "headless", "--patch", use_patch]
    else:
        cmd = [dsh, "headless", "--patch", use_patch]
    if session_id:
        cmd += ["--session-id", session_id]
    cmd.append(prompt)

    proc = subprocess.Popen(cmd, env=env, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, cwd=os.getcwd(),
                            creationflags=getattr(subprocess, "CREATE_NEW_PROCESS_GROUP", 0))
    timed_out = False
    try:
        out_b, err_b = proc.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        timed_out = True
        kill_tree(proc.pid)
        try:
            out_b, err_b = proc.communicate(timeout=20)
        except subprocess.TimeoutExpired:
            out_b, err_b = b"", b""
    out = (out_b or b"").decode("utf-8", errors="replace")
    err = (err_b or b"").decode("utf-8", errors="replace")
    return (not timed_out and proc.returncode == 0), out, err, proc.returncode, timed_out


def parse_result(text, exit_ok):
    """从产出里抽 RESULT 行；抽不到就按 exit code 退化，不因格式问题误判失败。"""
    matches = RESULT_LINE.findall(text or "")
    m = matches[-1] if matches else None
    if m:
        try:
            payload = json.loads(m)
            if isinstance(payload, dict):
                return payload, True
        except json.JSONDecodeError:
            pass
    return {"ok": bool(exit_ok), "summary": "(agent 未给 RESULT 行，按退出码判定)"}, False


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("prompt", nargs="?", default=None, help="任务描述（也可用 --prompt-file）")
    ap.add_argument("--prompt-file", default=None)
    ap.add_argument("--workdir", default=None, help="dsh 的工作目录（默认继承当前目录）")
    ap.add_argument("--timeout", type=int, default=900, help="单次硬超时（秒）")
    ap.add_argument("--retries", type=int, default=2, help="可重试错误的最大重试次数")
    ap.add_argument("--session-id", default="auto",
                    help="auto=每次唯一（并发安全）；none=不指定（复用最近会话）；其它=指定")
    ap.add_argument("--model", default=None, help="覆盖 dsh-model.yml 里的模型")
    ap.add_argument("--patch", default=None, help="覆盖 patch 文件路径")
    ap.add_argument("--no-result-spec", action="store_true", help="不追加 RESULT 格式要求")
    ap.add_argument("--result", default=None, help="meta 输出路径（默认 workdir/.adapter/<id>.json）")
    args = ap.parse_args()

    # ---- prompt ----
    text = args.prompt
    if args.prompt_file:
        with open(args.prompt_file, encoding="utf-8") as f:
            text = f.read()
    if not text and not sys.stdin.isatty():
        text = sys.stdin.read()
    text = (text or "").strip()
    if not text:
        sys.stderr.write("dsh_adapter: empty prompt\n")
        return 2

    workdir = args.workdir or os.getcwd()
    os.makedirs(workdir, exist_ok=True)

    # ---- 长 prompt 落文件，避免命令行超长 ----
    long_prompt_file = None
    if len(text) > ARGV_PROMPT_LIMIT or "\n" in text or "\r" in text:
        long_prompt_file = os.path.join(workdir, ".adapter_prompt.md")
        with open(long_prompt_file, "w", encoding="utf-8") as f:
            f.write(text)
        text = (f"你的完整任务说明在 {long_prompt_file}，请先读这个文件，然后按里面的要求执行。")

    if not args.no_result_spec:
        text = text + RESULT_SPEC

    # ---- 密钥 ----
    env = dict(os.environ)
    key = read_key()
    if not key:
        sys.stderr.write("dsh_adapter: OPENCODE_API_KEY not found\n")
        return 3
    env["OPENCODE_API_KEY"] = key
    # 本机代理会把 127.0.0.1 劫持成假 502（AGENTS §0.6）
    env.setdefault("NO_PROXY", "127.0.0.1,localhost")
    env.setdefault("no_proxy", "127.0.0.1,localhost")

    # 换模型这件事没有命令行入口，两个坑都实测过：
    #   1) `dsh headless --model X` → `error: unknown option '--model'`
    #   2) 自己生成临时 patch 再传给 --patch → dsh 在 profile-boot 阶段直接崩
    #      （--patch 与 profile 有耦合，不是随便一个同格式文件就能塞进去）
    # 所以要换模型，只能改 dsh-model.yml 里的 model 字段本身。
    if args.model:
        sys.stderr.write(
            "dsh_adapter: dsh headless 不支持 --model（实测 unknown option），"
            "临时 patch 也会让 profile 启动失败。\n"
            f"            要换模型，请直接编辑 {PATCH} 的 model 字段。\n")
        return 3
    effective_patch = args.patch or PATCH

    dsh = resolve_dsh()
    if not os.path.exists(dsh):
        sys.stderr.write(f"dsh_adapter: dsh not found at {dsh}\n")
        return 3

    # ---- session ----
    # dsh 的 --session-id 语义是「恢复这个会话」：id 不存在会直接报
    # `session "..." does not exist; omit --session-id to start a new Session`。
    # 所以默认不传（让 dsh 自己新建一个），并发时各进程天然各用各的会话目录。
    if args.session_id in ("auto", "none", "", "new"):
        session_id = None
    else:
        session_id = args.session_id

    # ---- 重试循环 ----
    attempts = []
    final = None
    total_started = time.time()
    for attempt in range(args.retries + 1):
        started = time.time()
        ok, out, err, code, timed_out = run_once(
            text, env, dsh, args.timeout, session_id, None, effective_patch)
        elapsed = round(time.time() - started, 1)
        attempts.append({"attempt": attempt + 1, "ok": ok, "exit": code,
                         "timed_out": timed_out, "elapsed_s": elapsed,
                         "stdout_chars": len(out), "stderr_tail": err[-300:]})
        # dsh 自己的告警压到 stderr，别污染引擎的产出
        if err.strip():
            sys.stderr.write(err[-2000:])
        final = (ok, out, err, code, timed_out)
        if ok and out.strip():
            break
        # 判断是否值得重试：配置类错误直接放弃；其余看超时/可重试特征/空产出
        reason = err + out
        if NOT_RETRYABLE.search(reason):
            retryable = False
        else:
            retryable = timed_out or (not out.strip()) or bool(RETRYABLE.search(reason))
        if attempt >= args.retries or not retryable:
            break
        backoff = min(30, 5 * (2 ** attempt))
        sys.stderr.write(f"dsh_adapter: 第 {attempt + 1} 次失败（可重试），{backoff}s 后重试\n")
        time.sleep(backoff)

    ok, out, err, code, timed_out = final
    payload, had_result_line = parse_result(out, ok)

    # ---- meta ----
    meta = {
        "ok": bool(ok and out.strip()) and payload.get("ok", True),
        "exit": code,
        "timed_out": timed_out,
        "attempts": len(attempts),
        "elapsed_s": round(time.time() - total_started, 1),
        "attempt_history": attempts,
        "session_id": session_id,
        "model": args.model or "(from patch)",
        "patch": effective_patch,
        "workdir": workdir,
        "result_line": payload,
        "had_result_line": had_result_line,
        "long_prompt_file": long_prompt_file,
    }
    result_path = args.result
    if not result_path:
        adir = os.path.join(workdir, ".adapter")
        os.makedirs(adir, exist_ok=True)
        # 并发时多个进程会在同一秒内落盘，文件名必须带 pid，否则互相覆盖
    stem = session_id or ("auto-%d-%d" % (os.getpid(), int(time.time() * 1000) % 100000))
    result_path = os.path.join(adir, stem + ".json")
    try:
        with open(result_path, "w", encoding="utf-8") as f:
            json.dump(meta, f, ensure_ascii=False, indent=2)
    except OSError:
        pass

    sys.stdout.write(out)
    if timed_out:
        return 5
    if not meta["ok"]:
        return 4
    return 0


if __name__ == "__main__":
    sys.exit(main())
