#!/usr/bin/env python
"""WASAPI loopback recorder: capture what the machine is playing.

Why: this box exposes no "Stereo Mix" capture endpoint, so the only way to grab
system output is WASAPI loopback (via `soundcard`). Every render endpoint is
recorded in parallel-of-one loop, so it does not matter which device the user
actually plays through.

Usage:
  python scripts/record-system-audio.py --seconds 330 [--out DIR] [--devices a,b]

Writes, under DIR:
  <device>.wav       16-bit / 48 kHz / stereo, one per render endpoint
  levels.jsonl       one line per ~0.25 s per device: rms/peak in dBFS
  meta.json          device list + settings (written at start and at the end)
Exit codes: 0 ok, 2 no device could be opened, 3 every device stayed silent.
"""
import argparse
import json
import os
import re
import sys
import time
import wave
from datetime import datetime

import numpy as np
import soundcard as sc

BLOCK = 4800          # 0.1 s @ 48 kHz
RATE = 48000
CH = 2


def safe(name):
    return re.sub(r'[^0-9A-Za-z\u4e00-\u9fff]+', '_', name).strip('_')[:60] or 'dev'


def progress(task, pct, msg, state='running'):
    d = os.environ.get('DSH_PROGRESS_DIR')
    if not d:
        return
    try:
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, task + '.jsonl'), 'a', encoding='utf-8') as f:
            f.write(json.dumps({'v': 1, 'task': task, 'state': state,
                                'pct': round(pct, 1), 'msg': msg}, ensure_ascii=False) + '\n')
    except Exception:
        pass


def db(x):
    return float(20 * np.log10(max(x, 1e-9)))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--seconds', type=float, default=300)
    ap.add_argument('--out', default='')
    ap.add_argument('--devices', default='')
    a = ap.parse_args()

    stamp = datetime.now().strftime('%Y%m%d-%H%M%S')
    out = a.out or os.path.join('.tmp', 'audio-capture', stamp)
    os.makedirs(out, exist_ok=True)

    speakers = sc.all_speakers()
    loops = sc.all_microphones(include_loopback=True)
    spk_names = {s.name for s in speakers}
    wanted = [w.strip() for w in a.devices.split(',') if w.strip()]
    targets = [m for m in loops if (not wanted and m.name in spk_names) or m.name in wanted]

    recs, handled = [], []
    for m in targets:
        try:
            r = m.recorder(samplerate=RATE, channels=CH, blocksize=BLOCK)
            r.__enter__()   # soundcard opens the WASAPI client here, not in __init__
            p = os.path.join(out, safe(m.name) + '.wav')
            w = wave.open(p, 'wb')
            w.setnchannels(CH)
            w.setsampwidth(2)
            w.setframerate(RATE)
            recs.append((m.name, r, w, p, 0.0))
            handled.append(m.name)
        except Exception as e:  # keep the other devices alive
            print(f'[skip] {m.name}: {e}', file=sys.stderr)

    if not recs:
        print('no loopback device could be opened', file=sys.stderr)
        return 2

    meta = {'started': datetime.now().isoformat(timespec='seconds'), 'dir': os.path.abspath(out),
            'seconds': a.seconds, 'rate': RATE, 'channels': CH, 'devices': handled,
            'all_render_endpoints': [s.name for s in speakers]}
    with open(os.path.join(out, 'meta.json'), 'w', encoding='utf-8') as f:
        json.dump(meta, f, ensure_ascii=False, indent=1)
    print(f'recording {len(recs)} endpoint(s) for {a.seconds:.0f}s -> {out}', flush=True)
    for n in handled:
        print('  *', n, flush=True)

    lv = open(os.path.join(out, 'levels.jsonl'), 'w', encoding='utf-8')
    t0 = time.time()
    elapsed = 0.0
    last_lv = 0.0
    last_pg = 0.0
    last_buf = {n: np.zeros((BLOCK, CH), dtype=np.float32) for n in handled}
    peak_seen = {n: 0.0 for n in handled}
    try:
        while True:
            elapsed = time.time() - t0
            if elapsed >= a.seconds:
                break
            for name, r, w, p, _ in recs:
                try:
                    buf = r.record(numframes=BLOCK)
                except Exception as e:
                    print(f'[err] {name}: {e}', file=sys.stderr, flush=True)
                    continue
                if buf.ndim == 1:
                    buf = np.stack([buf] * CH, axis=1)
                last_buf[name] = buf
                w.writeframes((np.clip(buf, -1, 1) * 32767).astype('<i2').tobytes())
            now = time.time()
            if now - last_lv >= 0.25 or last_lv == 0.0:
                last_lv = now
                row = {'t': round(now - t0, 2)}
                for n in handled:
                    b = last_buf[n]
                    peak_seen[n] = max(peak_seen[n], float(np.max(np.abs(b))))
                    row[n] = {'rms_dbfs': round(db(float(np.sqrt(np.mean(b ** 2)))), 1),
                              'peak_dbfs': round(db(float(np.max(np.abs(b)))), 1)}
                lv.write(json.dumps(row, ensure_ascii=False) + '\n')
                lv.flush()
                loud = max(peak_seen.values()) if peak_seen else 0.0
                if loud < 0.0015 and now - t0 > 8:
                    print('[warn] 8s 以上仍无输入电平（可能没开始播放 / 音量太低）', flush=True)
            if now - last_pg >= 5:
                last_pg = now
                progress('record', now_marker(now - t0, a.seconds),
                         f'{now - t0:.0f}s / {a.seconds:.0f}s')
                print(f'[t] {now - t0:.0f}s', flush=True)
    finally:
        for name, r, w, p, _ in recs:
            try:
                w.close()
            except Exception:
                pass
            try:
                r.__exit__(None, None, None)
            except Exception:
                pass
        lv.close()

    meta['ended'] = datetime.now().isoformat(timespec='seconds')
    meta['wavs'] = {n: p for n, r, w, p, _ in recs}
    with open(os.path.join(out, 'meta.json'), 'w', encoding='utf-8') as f:
        json.dump(meta, f, ensure_ascii=False, indent=2)
    progress('record', 100, 'done', 'done')
    print('done:', out, flush=True)
    return 0


def now_marker(elapsed, total):
    return max(0.0, min(100.0, 100.0 * elapsed / max(total, 1e-6)))


if __name__ == '__main__':
    sys.exit(main())
