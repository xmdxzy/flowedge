# -*- coding: utf-8 -*-
"""FlowEdge 流式 E2E：WS 订阅 node_token，触发 LLM 工作流，验证打字机数据流"""
import json, threading, time, urllib.request
import websocket

tokens = []          # (ts, delta)
final_output = None
done = threading.Event()

def on_message(ws, msg):
    global final_output
    try:
        ev = json.loads(msg)
    except Exception:
        return
    if ev.get('type') == 'node_token':
        tokens.append((time.time(), ev['data'].get('delta', '')))
    elif ev.get('type') == 'node_finished' and ev.get('node_id') == 'n2':
        final_output = (ev.get('data') or {}).get('output')
        done.set()
        threading.Timer(0.2, lambda: ws.close()).start()

def on_open(ws):
    def trigger():
        time.sleep(0.5)
        req = urllib.request.Request('http://127.0.0.1:8080/api/workflows/6/run', method='POST')
        try:
            urllib.request.urlopen(req, timeout=10)
            print('[e2e] workflow triggered')
        except Exception as e:
            print('[e2e] trigger failed:', e)
    threading.Thread(target=trigger, daemon=True).start()

ws = websocket.WebSocketApp('ws://127.0.0.1:8080/api/ws',
                            on_message=on_message, on_open=on_open)
t0 = time.time()
ws.run_forever(ping_interval=20)

if not done.is_set():
    print('[e2e] FAIL: node_finished(n2) not received in 120s')
    raise SystemExit(1)

deltas = [d for _, d in tokens]
full = ''.join(deltas)
t_first = tokens[0][0] - t0
t_last = tokens[-1][0] - t0
span = t_last - t_first
out_text = (final_output or {}).get('text', '')

print(f'[e2e] token events : {len(tokens)}')
print(f'[e2e] first delta  @ {t_first:.2f}s, last @ {t_last:.2f}s, span={span:.2f}s')
print(f'[e2e] streamed text: {full[:60]}{"..." if len(full)>60 else ""}')
print(f'[e2e] final output : {out_text[:60]}{"..." if len(out_text)>60 else ""}')
assert len(tokens) >= 3, 'too few token events — streaming not incremental'
assert full == out_text, 'streamed text != final output'
assert len(set(t for t, _ in tokens)) > 1 or span > 0.2, 'all deltas arrived at once — not streaming'
print('[e2e] STREAMING PASS ✓')
