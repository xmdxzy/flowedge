#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""WorkBuddy harness 静态注入审计 —— 一条命令替代一堆临时探测。

为什么有这个脚本：每轮 token 优化我都要现写 5~8 段 python 去量同一批文件，
每段命令+输出留在上下文里约 2~3KB，八段 ≈ 20KB。固化后每次审计 ≈ 1KB。

用法：
    python scripts/token-audit.py            # 静态注入总量 + 红黑榜
    python scripts/token-audit.py --session  # 会话内实际开销（压缩摘要/工具输出），真正的成本大头
    python scripts/token-audit.py --dup      # 追加跨文件语义重合检测（14 字窗口）
    python scripts/token-audit.py --json     # 机器可读

为什么要 --session：静态注入只是首付。2026-10-23 实测 —— 静态 24.6k 字符，
但同一会话的自动压缩摘要 94.6k + 47.6k = 142k 字符（≈102k tokens）。
摘要体量由 transcript 决定，而 transcript 主要由**工具输出**决定 ⇒ 控制单次输出比继续砍文件更值钱。

判据口径（红线，超 5% 才报——多会话共写，字符级抖动无意义）：
    AGENTS.md ≤ 10,000 字符 · 项目 MEMORY ≤ 6,000 · 全局 MEMORY ≤ 2,500 · 用户 MEMORY ≤ 4,000
"""
import os, sys, json, re, glob

W = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
U = os.path.expanduser('~/.workbuddy')
def _active_profile():
    """多账号时按 MEMORY.md 的 mtime 取当前活跃 profile，别按字典序拿错账号。"""
    cands = glob.glob(os.path.join(U, 'user-*-personal'))
    if not cands:
        return U
    def score(d):
        best = 0
        for name in ('MEMORY.md', 'SOUL.md', 'USER.md'):
            p = os.path.join(d, name)
            if os.path.exists(p):
                best = max(best, os.path.getmtime(p))
        return best
    return max(cands, key=score)


P = _active_profile()

# (路径, 简称, 红线, 说明)
FILES = [
    (os.path.join(W, 'AGENTS.md'),                       '项目AGENTS',   10000, '项目规则·每会话注入'),
    (os.path.join(W, '.workbuddy/memory/MEMORY.md'),     '项目MEMORY',    6000, '项目长期记忆·每会话注入'),
    (os.path.join(U, 'AGENTS.md'),                       '全局AGENTS',    3000, '全局指令'),
    (os.path.join(U, 'MEMORY.md'),                       '全局MEMORY',    2500, '铁律唯一副本'),
    (os.path.join(P, 'SOUL.md'),                         'SOUL',          1500, '人格'),
    (os.path.join(P, 'USER.md'),                         'USER',          1000, '用户画像'),
    (os.path.join(P, 'IDENTITY.md'),                     'IDENTITY',       600, '身份'),
    (os.path.join(P, 'MEMORY.md'),                       '用户MEMORY',    4000, '规则摘要'),
]
SKILL_CACHE = os.path.join(U, '.skill-list-cache.json')
MCP_CACHE = os.path.join(U, 'mcp-tool-list.json')


def chars(p):
    try:
        with open(p, encoding='utf-8') as f:
            return len(f.read())
    except OSError:
        return None


def load_json(p):
    try:
        with open(p, encoding='utf-8') as f:
            return json.load(f)
    except (OSError, ValueError):
        return None


def skills_report():
    """返回 (可见技能 name+desc 字符, 缓存总数, 缓存总字符)"""
    d = load_json(SKILL_CACHE)
    if not d:
        return 0, 0, 0
    rows = d.get('results') or []
    on = [s for s in rows if not s.get('disable')]
    total = sum(len(s.get('name', '')) + len(s.get('description') or '') + 3 for s in on)
    return total, len(on), total


def mcp_report():
    """返回 [{server_key, chars, tools}] 降序；无法反查服务名，只给规模"""
    d = load_json(MCP_CACHE)
    if not d:
        return []
    out = []
    for k, tools in (d.get('entries') or {}).items():
        c = 0
        for t in tools:
            if isinstance(t, dict):
                c += len(t.get('name', '')) + len(t.get('description') or '') + 3
            else:
                c += len(str(t)) + 3
        out.append({'key': k[:8], 'chars': c, 'tools': len(tools),
                    'sample': [(t.get('name') if isinstance(t, dict) else str(t)) for t in tools[:2]]})
    return sorted(out, key=lambda r: -r['chars'])


def dup_pairs(paths, k=14, top=6):
    def grams(s):
        s = ''.join(ch for ch in s if not ch.isspace())
        return set(s[i:i + k] for i in range(len(s) - k + 1))
    txt = {}
    for p, n, _, _ in paths:
        if os.path.exists(p):
            txt[n] = grams(open(p, encoding='utf-8').read())
    res = []
    names = list(txt)
    for i in range(len(names)):
        for j in range(i + 1, len(names)):
            a, b = names[i], names[j]
            inter = txt[a] & txt[b]
            if not inter:
                continue
            base = min(len(txt[a]), len(txt[b])) or 1
            res.append((len(inter) / base, a, b, len(inter)))
    return sorted(res, reverse=True)[:top]


def session_report(top=6, want=None):
    """本会话 transcript 的实际开销：压缩摘要 + 单条大输出。
    want 可指定会话 id 前缀（并行会话时默认取最近写入的那个）。"""
    # 目录名规则：盘符小写 + 去冒号 + 反斜杠变连字符，其余大小写保留
    slug = W.replace(':', '').replace('\\', '-').replace('/', '-')
    slug = slug[0].lower() + slug[1:]
    proj = os.path.join(os.path.expanduser('~/.workbuddy/projects'), slug)
    files = sorted(glob.glob(os.path.join(proj, '*.jsonl')), key=os.path.getmtime, reverse=True)
    if want:
        files = [x for x in files if os.path.basename(x).startswith(want)] or files
    if not files:
        print('找不到会话日志：' + proj)
        return
    f = files[0]
    print('会话日志 ' + os.path.basename(f)[:8] + f'  {os.path.getsize(f)//1024} KB')

    def strs(x, acc):
        if isinstance(x, str):
            acc.append(x)
        elif isinstance(x, dict):
            for v in x.values():
                strs(v, acc)
        elif isinstance(x, list):
            for v in x: strs(v, acc)

    summaries, big = {}, []
    for line in open(f, encoding='utf-8', errors='replace'):
        try:
            o = json.loads(line)
        except ValueError:
            continue
        acc = []
        strs(o, acc)
        for s in acc:
            if s.lstrip().startswith('<cb_summary>'):
                summaries[hash(s) % 10**8] = len(s)
            elif len(s) > 4000:
                big.append((len(s), s[:56].replace('\n', ' ')))
    if summaries:
        tot = sum(summaries.values())
        print(f'压缩摘要 {len(summaries)} 篇，合计 {tot} 字符 ≈{int(tot*0.72):,} tokens')
        for n in sorted(summaries.values(), reverse=True)[:4]:
            print(f'   {n:7d}')
        print(f'   → 摘要/静态注入 = {tot/24400:.1f}×')
    print(f'单条 >4KB 输出 {len(big)} 条（去重后代表性条目）：')
    seen = set()
    for n, s in sorted(big, reverse=True):
        if s in seen:
            continue
        seen.add(s)
        print(f'   {n:7d}  {s}')
        if len(seen) >= top:
            break


def main():
    if '--session' in sys.argv or any(a.startswith('--session=') for a in sys.argv):
        want = None
        for a in sys.argv:
            if a.startswith('--session='):
                want = a.split('=', 1)[1]
        session_report(want=want)
        return 0
    if '--json' in sys.argv:
        data = {'files': [], 'skills': {}, 'mcp': mcp_report()}
        for p, n, red, _ in FILES:
            c = chars(p)
            data['files'].append({'name': n, 'chars': c, 'redline': red})
        s_total, s_n, _ = skills_report()
        data['skills'] = {'chars': s_total, 'count': s_n}
        print(json.dumps(data, ensure_ascii=False, indent=1))
        return 0

    print('WorkBuddy 静态注入审计  ' + os.path.basename(W))
    print('-' * 58)
    tot = 0
    over = []
    for p, n, red, _ in FILES:
        c = chars(p)
        if c is None:
            print(f'{n:12s} {"缺失":>8s}')
            continue
        tot += c
        flag = ''
        if c > red * 1.05:          # 5% 容差：多会话共写，±几十字符的抖动不构成信号
            flag = f'  ↑超线{red}(+{c - red})'
            over.append(n)
        print(f'{n:12s} {c:8d}{flag}')
    s_total, s_n, _ = skills_report()
    mcp = mcp_report()
    mcp_total = sum(r['chars'] for r in mcp)
    print('-' * 58)
    # 可控静态 = 我拥有的 8 个文件；技能/MCP 目录是 harness 侧上界，二者不能相加
    print(f'{"可控静态合计":10s} {tot:8d}  ≈{int(tot * 0.72):,} tokens（×0.72 粗估）')
    print(f'{"harness 上界":10s} {s_total + mcp_total:8d}  技能 {s_total}({s_n}个) + MCP {mcp_total}({sum(r["tools"] for r in mcp)}工具) —— 缓存全量，会话内只注入子集'
          )
    if over:
        print('超红线：' + '、'.join(over))

    print('\nMCP 目录 Top5（缓存里最大者，未必都在当前会话注入）')
    for r in mcp[:5]:
        print(f'  {r["chars"]:6d}  {r["tools"]:3d} 工具  {", ".join(str(x) for x in r["sample"])}')

    if '--dup' in sys.argv:
        print('\n跨文件语义重合（14 字窗口，占较小文件比例）')
        pairs = dup_pairs(FILES)
        if not pairs:
            print('  无重合 ≥1%')
        for r, a, b, n in pairs:
            print(f'  {r*100:5.1f}%  {n:4d}窗口  {a} ↔ {b}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
