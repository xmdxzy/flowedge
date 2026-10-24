#!/usr/bin/env python
"""沙箱隔离机制诊断：查清当前进程被什么约束、哪些突破手段可用。

用法：
    python scripts/probe-sandbox.py           # 只读诊断
    python scripts/probe-sandbox.py --spawn   # 额外做真实脱离实验（会起几个短命进程）

输出的是事实（DWORD 位标志、Win32 错误码），不是猜测。
"""
import argparse
import ctypes
import ctypes.wintypes as wt
import json
import os
import subprocess
import sys
import time

k32 = ctypes.WinDLL("kernel32", use_last_error=True)

# --- Job object 常量 ---
JobObjectExtendedLimitInformation = 9
JOB_OBJECT_LIMIT_WORKINGSET = 0x00000001
JOB_OBJECT_LIMIT_PROCESS_TIME = 0x00000002
JOB_OBJECT_LIMIT_JOB_TIME = 0x00000004
JOB_OBJECT_LIMIT_ACTIVE_PROCESS = 0x00000008
JOB_OBJECT_LIMIT_AFFINITY = 0x00000010
JOB_OBJECT_LIMIT_PRIORITY_CLASS = 0x00000020
JOB_OBJECT_LIMIT_PRESERVE_JOB_TIME = 0x00000040
JOB_OBJECT_LIMIT_SCHEDULING_CLASS = 0x00000080
JOB_OBJECT_LIMIT_PROCESS_MEMORY = 0x00000100
JOB_OBJECT_LIMIT_JOB_MEMORY = 0x00000200
JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION = 0x00000400
JOB_OBJECT_LIMIT_BREAKAWAY_OK = 0x00000800
JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK = 0x00001000
JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE = 0x00002000
JOB_OBJECT_LIMIT_SUBSET_AFFINITY = 0x00004000
JOB_OBJECT_LIMIT_JOB_MEMORY_HIGH = 0x00000200
JOB_OBJECT_LIMIT_CPU_RATE = 0x00010000  # 见 JOBOBJECT_CPU_RATE_CONTROL_INFORMATION

LIMIT_NAMES = {
    "WORKINGSET": JOB_OBJECT_LIMIT_WORKINGSET,
    "PROCESS_TIME": JOB_OBJECT_LIMIT_PROCESS_TIME,
    "JOB_TIME": JOB_OBJECT_LIMIT_JOB_TIME,
    "ACTIVE_PROCESS": JOB_OBJECT_LIMIT_ACTIVE_PROCESS,
    "AFFINITY": JOB_OBJECT_LIMIT_AFFINITY,
    "PRIORITY_CLASS": JOB_OBJECT_LIMIT_PRIORITY_CLASS,
    "PRESERVE_JOB_TIME": JOB_OBJECT_LIMIT_PRESERVE_JOB_TIME,
    "SCHEDULING_CLASS": JOB_OBJECT_LIMIT_SCHEDULING_CLASS,
    "PROCESS_MEMORY": JOB_OBJECT_LIMIT_PROCESS_MEMORY,
    "JOB_MEMORY": JOB_OBJECT_LIMIT_JOB_MEMORY,
    "DIE_ON_UNHANDLED_EXCEPTION": JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION,
    "BREAKAWAY_OK": JOB_OBJECT_LIMIT_BREAKAWAY_OK,
    "SILENT_BREAKAWAY_OK": JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK,
    "KILL_ON_JOB_CLOSE": JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
    "SUBSET_AFFINITY": JOB_OBJECT_LIMIT_SUBSET_AFFINITY,
    "CPU_RATE": 0x00010000,
}


class IO_COUNTERS(ctypes.Structure):
    _fields_ = [
        ("ReadOperationCount", ctypes.c_ulonglong),
        ("WriteOperationCount", ctypes.c_ulonglong),
        ("OtherOperationCount", ctypes.c_ulonglong),
        ("ReadTransferCount", ctypes.c_ulonglong),
        ("WriteTransferCount", ctypes.c_ulonglong),
        ("OtherTransferCount", ctypes.c_ulonglong),
    ]


class JOBOBJECT_BASIC_LIMIT_INFORMATION(ctypes.Structure):
    _fields_ = [
        ("PerProcessUserTimeLimit", ctypes.c_int64),
        ("PerJobUserTimeLimit", ctypes.c_int64),
        ("LimitFlags", wt.DWORD),
        ("MinimumWorkingSetSize", ctypes.c_size_t),
        ("MaximumWorkingSetSize", ctypes.c_size_t),
        ("ActiveProcessLimit", wt.DWORD),
        ("Affinity", ctypes.c_size_t),
        ("PriorityClass", wt.DWORD),
        ("SchedulingClass", wt.DWORD),
    ]


class JOBOBJECT_EXTENDED_LIMIT_INFORMATION(ctypes.Structure):
    _fields_ = [
        ("BasicLimitInformation", JOBOBJECT_BASIC_LIMIT_INFORMATION),
        ("IoInfo", IO_COUNTERS),
        ("ProcessMemoryLimit", ctypes.c_size_t),
        ("JobMemoryLimit", ctypes.c_size_t),
        ("PeakProcessMemoryUsed", ctypes.c_size_t),
        ("PeakJobMemoryUsed", ctypes.c_size_t),
    ]


class JOBOBJECT_BASIC_ACCOUNTING_INFORMATION(ctypes.Structure):
    _fields_ = [
        ("TotalUserTime", ctypes.c_int64),
        ("TotalKernelTime", ctypes.c_int64),
        ("ThisPeriodTotalUserTime", ctypes.c_int64),
        ("ThisPeriodTotalKernelTime", ctypes.c_int64),
        ("TotalPageFaultCount", wt.DWORD),
        ("TotalProcesses", wt.DWORD),
        ("ActiveProcesses", wt.DWORD),
        ("TotalTerminatedProcesses", wt.DWORD),
    ]


k32.IsProcessInJob.argtypes = [wt.HANDLE, wt.HANDLE, ctypes.POINTER(wt.BOOL)]
k32.IsProcessInJob.restype = wt.BOOL
k32.OpenProcess.argtypes = [wt.DWORD, wt.BOOL, wt.DWORD]
k32.OpenProcess.restype = wt.HANDLE
k32.QueryInformationJobObject.argtypes = [wt.HANDLE, ctypes.c_int, ctypes.c_void_p, wt.DWORD, ctypes.POINTER(wt.DWORD)]
k32.QueryInformationJobObject.restype = wt.BOOL
k32.CreateJobObjectW.argtypes = [ctypes.c_void_p, wt.LPCWSTR]
k32.CreateJobObjectW.restype = wt.HANDLE
k32.AssignProcessToJobObject.argtypes = [wt.HANDLE, wt.HANDLE]
k32.AssignProcessToJobObject.restype = wt.BOOL
k32.GetCurrentProcess.restype = wt.HANDLE


PROCESS_QUERY_LIMITED_INFORMATION = 0x1000


def process_in_my_job(pid):
    """目标进程是否在 job 里。

    ⚠️ 实测警告（2026-10-23）：`IsProcessInJob(h, NULL, ...)` 的 hJob=NULL 语义
    不能严格理解为"是否在**调用者所属**那个 job"。实测对一个明显不属于本次命令
    job 的进程也返回 True（本次 job 的 accounting.activeProcesses=2，但对方仍报
    IN MY JOB）。因此**不要用它判定"A 和 B 是否同一个 job"**。

    可靠的替代：用 QueryInformationJobObject 的 TotalProcesses/ActiveProcesses
    交叉核对；或者干脆不做 job 归属推断，只问"这个 PID 还活着吗"（alive 检查）。

    返回 None 表示查不到（进程不存在 / 无权限）。
    """
    h = k32.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, False, pid)
    if not h:
        return None
    try:
        in_job = wt.BOOL()
        if not k32.IsProcessInJob(h, None, ctypes.byref(in_job)):
            # 目标在我方 job 外时 IsProcessInJob 可能报错，仍以 False 处理
            return False
        return bool(in_job)
    finally:
        k32.CloseHandle(h)


k32.CloseHandle.argtypes = [wt.HANDLE]
k32.CloseHandle.restype = wt.BOOL


def _parent_name():
    """父进程映像名（判断是被 shell / svchost / explorer 拉起的硬证据）。"""
    try:
        out = subprocess.run(
            ["tasklist", "/FI", "PID eq %d" % os.getppid(), "/FO", "CSV", "/NH"],
            capture_output=True, text=True, timeout=10,
        ).stdout.strip().strip('"')
        return out.split('","')[0] if out else None
    except Exception:
        return None


def current_job_handle():
    """返回 (是否在 job 中, 拿到 job 句柄或 None)。"""
    in_job = wt.BOOL()
    if not k32.IsProcessInJob(k32.GetCurrentProcess(), None, ctypes.byref(in_job)):
        return None, None
    if not in_job:
        return False, None
    # 用 QueryInformationJobObject(NULL,...) 查"当前进程所属 job"
    buf = JOBOBJECT_EXTENDED_LIMIT_INFORMATION()
    ret = wt.DWORD()
    ok = k32.QueryInformationJobObject(None, JobObjectExtendedLimitInformation,
                                       ctypes.byref(buf), ctypes.sizeof(buf), ctypes.byref(ret))
    if not ok:
        return True, None
    return True, buf


def describe_job(buf):
    flags = buf.BasicLimitInformation.LimitFlags
    names = [n for n, v in LIMIT_NAMES.items() if flags & v]
    return {
        "limitFlags": "0x%08X" % flags,
        "active": names,
        "activeProcessLimit": buf.BasicLimitInformation.ActiveProcessLimit,
        "affinity": "0x%X" % buf.BasicLimitInformation.Affinity,
        "priorityClass": buf.BasicLimitInformation.PriorityClass,
        "processMemoryLimitMB": round(buf.ProcessMemoryLimit / 1048576, 1),
        "jobMemoryLimitMB": round(buf.JobMemoryLimit / 1048576, 1),
    }


def accounting():
    buf = JOBOBJECT_BASIC_ACCOUNTING_INFORMATION()
    ret = wt.DWORD()
    ok = k32.QueryInformationJobObject(None, 1, ctypes.byref(buf), ctypes.sizeof(buf), ctypes.byref(ret))
    if not ok:
        return {"error": ctypes.get_last_error()}
    return {
        "totalProcesses": buf.TotalProcesses,
        "activeProcesses": buf.ActiveProcesses,
        "totalTerminatedProcesses": buf.TotalTerminatedProcesses,
        "userTimeSec": round(buf.TotalUserTime / 1e7, 2),
        "kernelTimeSec": round(buf.TotalKernelTime / 1e7, 2),
    }


CREATE_BREAKAWAY_FROM_JOB = 0x01000000
CREATE_NEW_PROCESS_GROUP = 0x00000200
CREATE_NEW_CONSOLE = 0x00000010
DETACHED_PROCESS = 0x00000008
CREATE_NO_WINDOW = 0x08000000
CREATE_BREAKAWAY_AND_DETACHED = 0  # 占位


def try_spawn(cmd, flags, label, wait=1.0):
    """用 CreateProcess 直接起进程，返回 (ok, pid_or_err, errcode, alive_after_wait)。"""
    si = subprocess.STARTUPINFO()
    si.dwFlags |= subprocess.STARTF_USESHOWWINDOW
    si.wShowWindow = 0
    try:
        p = subprocess.Popen(cmd, creationflags=flags, startupinfo=si,
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                             stdin=subprocess.DEVNULL)
    except OSError as e:
        return {"label": label, "flags": "0x%08X" % flags, "ok": False,
                "winerror": getattr(e, "winerror", None), "errno": e.errno,
                "msg": str(e)}
    time.sleep(wait)
    alive = p.poll() is None
    res = {"label": label, "flags": "0x%08X" % flags, "ok": True, "pid": p.pid,
           "aliveAfter1s": alive}
    if not alive:
        res["exitCode"] = p.returncode
    else:
        # 不杀它，让它自己退（cmd /c exit 天生短命）
        try:
            p.wait(timeout=5)
        except subprocess.TimeoutExpired:
            pass
    return res


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--spawn", action="store_true")
    ap.add_argument("--json", action="store_true")
    ap.add_argument("--out", help="把结果写到这个文件（被远程通道拉起时用）")
    ap.add_argument("--tag", default="", help="标记，便于识别是谁拉起的")
    ap.add_argument("--pid", type=int, action="append", default=[],
                    help="额外查询这些 PID 是否与我方同 job（可多次）")
    a = ap.parse_args()

    out = {"pid": os.getpid(), "ppid": os.getppid(), "python": sys.version.split()[0],
           "tag": a.tag, "cwd": os.getcwd(), "argv": sys.argv}
    if a.tag:
        out["parentName"] = _parent_name()

    in_job, jbuf = current_job_handle()
    out["inJob"] = bool(in_job)
    if jbuf is not None:
        out["job"] = describe_job(jbuf)
        out["accounting"] = accounting()

    # 沙箱常见的另一层：无 Administrators 提权
    try:
        admin = ctypes.windll.shell32.IsUserAnAdmin()
    except Exception:
        admin = None
    out["isAdmin"] = bool(admin)

    # 环境证据：sandbox 包装进程
    out["envKeys"] = sorted(k for k in os.environ
                            if any(s in k.lower() for s in ("sandbox", "job", "cage", "seatbelt")))
    out["envSample"] = {k: os.environ[k][:120] for k in out["envKeys"]}

    if a.spawn:
        shell = os.environ.get("COMSPEC", r"C:\Windows\System32\cmd.exe")
        cases = [
            ("plain", 0, [shell, "/c", "exit 0"]),
            ("detached", DETACHED_PROCESS, [shell, "/c", "exit 0"]),
            ("newgroup", CREATE_NEW_PROCESS_GROUP, [shell, "/c", "exit 0"]),
            ("breakaway", CREATE_BREAKAWAY_FROM_JOB, [shell, "/c", "exit 0"]),
            ("breakaway+newconsole", CREATE_BREAKAWAY_FROM_JOB | CREATE_NEW_CONSOLE, [shell, "/c", "exit 0"]),
            ("breakaway+detached", CREATE_BREAKAWAY_FROM_JOB | DETACHED_PROCESS, [shell, "/c", "exit 0"]),
        ]
        out["spawn"] = [try_spawn(c, f, l) for l, f, c in cases]

    if a.pid:
        out["peers"] = {str(p): process_in_my_job(p) for p in a.pid}

    if a.out:
        tmp = a.out + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(out, f, ensure_ascii=False, indent=2)
        os.replace(tmp, a.out)

    if a.json:
        print(json.dumps(out, ensure_ascii=False, indent=2))
    else:
        for k in ("pid", "ppid", "inJob", "isAdmin"):
            print("%-10s %s" % (k, out.get(k)))
        if "job" in out:
            j = out["job"]
            print("\njob.limitFlags  %s" % j["limitFlags"])
            print("job.active      %s" % ", ".join(j["active"]))
            print("job.memLimit    proc=%sMB job=%sMB" % (j["processMemoryLimitMB"], j["jobMemoryLimitMB"]))
            print("job.activeProc  %s" % j["activeProcessLimit"])
            if "accounting" in out:
                print("job.accounting  %s" % json.dumps(out["accounting"]))
        if "spawn" in out:
            print("\n-- spawn probes --")
            for r in out["spawn"]:
                if r["ok"]:
                    print("%-22s flags=%s pid=%s alive=%s" % (r["label"], r["flags"], r["pid"], r["aliveAfter1s"]))
                else:
                    print("%-22s flags=%s BLOCKED winerror=%s %s" % (r["label"], r["flags"], r.get("winerror"), r["msg"]))
        if "peers" in out:
            print("\n-- peer job membership (same job as me?) --")
            for pid, v in out["peers"].items():
                print("%-8s %s" % (pid, {True: "IN MY JOB", False: "OUTSIDE (free)",
                                         None: "gone/no-access"}[v]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
