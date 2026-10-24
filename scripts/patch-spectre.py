#!/usr/bin/env python
"""把 VS Code 原生模块 binding.gyp 里的 Spectre 强制要求改成 false。

为什么需要
----------
`npm ci` 编译原生模块时报：
    error MSB8040: 此项目需要缓解了 Spectre 漏洞的库
原因：这些模块的 binding.gyp 里写了 `'SpectreMitigation': 'Spectre'`，
而本机 VS Build Tools 没装 "MSVC v143 - VS 2022 C++ x64/x86 Spectre 缓解库" 组件。

两条路：
  A. 装那个 VS 组件（官方、安全语义不变，但安装器在本机反复失败）
  B. 关掉这个编译选项（只影响编译产物是否插入 Spectre 屏障指令；
     本地开发工具的安全收益有限，换取可构建）

本脚本走 B，只动 node_modules 里的 binding.gyp —— 不改系统、不改源码仓库。
副作用可逆：重新 `npm ci` 即恢复原状。

用法：
    python patch-spectre.py <QCoder 目录>
"""
import os
import re
import sys

PATTERN = re.compile(rb"(SpectreMitigation['\"]\s*:\s*['\"])Spectre(['\"])")


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    root = os.path.join(sys.argv[1], "node_modules")
    if not os.path.isdir(root):
        print("没有 node_modules：%s" % root, file=sys.stderr)
        return 2

    changed, skipped = [], []
    for dirpath, dirnames, filenames in os.walk(root):
        for fn in filenames:
            # 不只 binding.gyp：@vscode/sqlite3 在 deps/sqlite3.gyp 里另有一处
            if not fn.endswith(".gyp") and not fn.endswith(".gypi"):
                continue
            p = os.path.join(dirpath, fn)
            try:
                with open(p, "rb") as f:
                    src = f.read()
            except OSError:
                continue
            if not PATTERN.search(src):
                continue
            new = PATTERN.sub(rb"\1false\2", src)
            if new == src:
                skipped.append(p)
                continue
            try:
                with open(p, "wb") as f:
                    f.write(new)
                changed.append(os.path.relpath(p, root))
            except OSError as e:
                print("写入失败 %s: %s" % (p, e), file=sys.stderr)

    for c in changed:
        print("patched  %s" % c)
    for s in skipped:
        print("nochange %s" % s)
    print("\n共修改 %d 个 binding.gyp" % len(changed))
    return 0 if changed else 1


if __name__ == "__main__":
    sys.exit(main())
