"""多线程 range 分片下载（urllib，无第三方依赖）。"""
import os
import sys
import threading
import time
import urllib.request

url, out = sys.argv[1], sys.argv[2]
n = int(sys.argv[3]) if len(sys.argv) > 3 else 8

req = urllib.request.Request(url, method="HEAD")
with urllib.request.urlopen(req, timeout=60) as r:
    size = int(r.headers["Content-Length"])
print(f"size={size/1024/1024:.1f}MB threads={n}", flush=True)

chunk = size // n
lock = threading.Lock()
done = [0]
t0 = time.time()


def dl(i, start, end, path):
    req = urllib.request.Request(url, headers={"Range": f"bytes={start}-{end}"})
    with urllib.request.urlopen(req, timeout=300) as r, open(path, "wb") as f:
        while True:
            b = r.read(1 << 20)
            if not b:
                break
            f.write(b)
            with lock:
                done[0] += len(b)


parts = []
threads = []
for i in range(n):
    start = i * chunk
    end = size - 1 if i == n - 1 else (i + 1) * chunk - 1
    p = f"{out}.part{i}"
    parts.append(p)
    t = threading.Thread(target=dl, args=(i, start, end, p), daemon=True)
    t.start()
    threads.append(t)

last = 0
while any(t.is_alive() for t in threads):
    time.sleep(5)
    pct = done[0] * 100 / size
    if int(pct) != last:
        last = int(pct)
        print(f"{pct:.1f}% {done[0]/1024/1024:.0f}MB {done[0]/1024/1024/max(time.time()-t0,1):.2f}MB/s", flush=True)

with open(out, "wb") as f:
    for p in parts:
        with open(p, "rb") as g:
            f.write(g.read())
        os.remove(p)
print(f"done {os.path.getsize(out)} bytes in {time.time()-t0:.0f}s", flush=True)
