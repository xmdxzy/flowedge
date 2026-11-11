// FlowEdge · blueprint knowledge card renderer
// Page contract (lemo): window.READY, window.DUR, window.render(t), window.TEXTS(t)
// Deterministic: every frame depends only on t (no Math.random / Date.now).

// 设计尺寸随画幅变：横屏 1920×1080，竖屏 1080×DH（9:16→1920，3:4→1440）
let W = 1920, H = 1080, PORTRAIT = false, VW = 1920, VH = 1080, SC = 1;
import { wrapByPunct } from './shared/kit.js';

const INK = 'rgba(226,238,252,0.92)';
const INK_DIM = 'rgba(178,200,228,0.62)';
const INK_FAINT = 'rgba(150,178,212,0.34)';
const ACCENT = 'rgba(120,214,255,0.95)';
const PAPER_1 = '#0d1f3d', PAPER_2 = '#071326';

const canvas = document.getElementById('c');
const ctx = canvas.getContext('2d', { alpha: false });
let SPEC = null, CARDS = [], DUR = 0, bg = null;

// ---------- deterministic helpers ----------
function hash(n) {
  const x = Math.sin(n * 127.1 + 311.7) * 43758.5453;
  return x - Math.floor(x);
}
function jit(seed, amp) { return (hash(seed) - 0.5) * amp; }
function clamp(v, a, b) { return v < a ? a : v > b ? b : v; }
function lerp(a, b, t) { return a + (b - a) * t; }
function prog(t, a, b) { return clamp((t - a) / (b - a || 1e-6), 0, 1); }
function ease(x) { return x < 0.5 ? 2 * x * x : 1 - Math.pow(-2 * x + 2, 2) / 2; }

// ---------- hand-drawn primitives ----------
function handLine(c, x1, y1, x2, y2, seed, amp = 1.5, segs = 18) {
  c.beginPath();
  c.moveTo(x1, y1);
  for (let i = 1; i <= segs; i++) {
    const p = i / segs;
    const x = lerp(x1, x2, p) + jit(seed + i * 7.3, amp);
    const y = lerp(y1, y2, p) + jit(seed + i * 11.9, amp);
    c.lineTo(x, y);
  }
  c.stroke();
}

function handRect(c, x, y, w, h, seed, amp = 1.4) {
  handLine(c, x, y, x + w, y, seed, amp);
  handLine(c, x + w, y, x + w, y + h, seed + 31, amp);
  handLine(c, x + w, y + h, x, y + h, seed + 67, amp);
  handLine(c, x, y + h, x, y, seed + 97, amp);
}

function crossMark(c, x, y, s) {
  c.beginPath();
  c.moveTo(x - s, y); c.lineTo(x + s, y);
  c.moveTo(x, y - s); c.lineTo(x, y + s);
  c.stroke();
}

function dimensionLine(c, x1, y1, x2, y2, label, seed) {
  c.save();
  c.strokeStyle = INK_FAINT; c.lineWidth = 1.2;
  handLine(c, x1, y1, x2, y2, seed, 0.8, 10);
  const dx = x2 - x1, dy = y2 - y1, len = Math.hypot(dx, dy) || 1;
  const nx = -dy / len, ny = dx / len;
  [[x1, y1], [x2, y2]].forEach(([x, y], i) => {
    c.beginPath();
    c.moveTo(x - nx * 6, y - ny * 6);
    c.lineTo(x + nx * 6, y + ny * 6);
    c.stroke();
    c.beginPath();
    c.arc(x, y, 2.2, 0, Math.PI * 2);
    c.fillStyle = INK_FAINT; c.fill();
  });
  if (label) {
    c.fillStyle = INK_DIM;
    c.font = '400 17px "Cascadia Mono", Consolas, monospace';
    c.textAlign = 'center'; c.textBaseline = 'bottom';
    c.fillText(label, (x1 + x2) / 2 + nx * 14, (y1 + y2) / 2 + ny * 14 - 4);
  }
  c.restore();
}

// ---------- text ----------
const CJK = /[\u3400-\u9fff\uf900-\ufaff]/;
function setText(c, size, weight = 400, mono = false) {
  c.font = `${weight} ${size}px ${mono
    ? '"Cascadia Mono", Consolas, "Courier New", monospace'
    : '"Microsoft YaHei", "PingFang SC", "Noto Sans CJK SC", sans-serif'}`;
}
function textW(c, s) { return c.measureText(s).width; }
function wrapText(c, text, maxW) {
  const out = [];
  let cur = '';
  for (const ch of text) {
    const next = cur + ch;
    if (textW(c, next) > maxW && cur) { out.push(cur); cur = ch; }
    else cur = next;
  }
  if (cur) out.push(cur);
  return out;
}
// letters visible for progress p (0..1)
function typed(s, p) {
  const n = Math.round(s.length * clamp(p, 0, 1));
  return s.slice(0, n);
}

// ---------- blueprint base (rendered once) ----------
// setupLayout 由视口决定设计尺寸与缩放：竖屏不是把横屏挤扁，图版会重排
function setupLayout() {
  const cv = document.getElementById('c');
  VW = cv.width = Math.max(2, window.innerWidth | 0);
  VH = cv.height = Math.max(2, window.innerHeight | 0);
  PORTRAIT = VH >= VW * 0.99;
  if (PORTRAIT) {
    W = 1080;
    H = Math.round((1080 * VH) / VW);
  } else {
    W = 1920;
    H = 1080;
  }
  SC = PORTRAIT ? VW / W : Math.min(VW / W, VH / H);
}

function buildBackground() {
  const off = document.createElement('canvas');
  off.width = W; off.height = H;
  const c = off.getContext('2d');

  const g = c.createRadialGradient(W * 0.42, H * 0.36, 80, W * 0.5, H * 0.5, W * 0.78);
  g.addColorStop(0, PAPER_1);
  g.addColorStop(1, PAPER_2);
  c.fillStyle = g;
  c.fillRect(0, 0, W, H);

  // fine grid
  c.strokeStyle = 'rgba(112,166,232,0.05)';
  c.lineWidth = 1;
  c.beginPath();
  for (let x = 0; x <= W; x += 24) { c.moveTo(x + 0.5, 0); c.lineTo(x + 0.5, H); }
  for (let y = 0; y <= H; y += 24) { c.moveTo(0, y + 0.5); c.lineTo(W, y + 0.5); }
  c.stroke();

  // major grid
  c.strokeStyle = 'rgba(112,166,232,0.11)';
  c.lineWidth = 1;
  c.beginPath();
  for (let x = 0; x <= W; x += 120) { c.moveTo(x + 0.5, 0); c.lineTo(x + 0.5, H); }
  for (let y = 0; y <= H; y += 120) { c.moveTo(0, y + 0.5); c.lineTo(W, y + 0.5); }
  c.stroke();

  // paper grain
  for (let i = 0; i < 900; i++) {
    const x = hash(i * 3.1) * W, y = hash(i * 7.7 + 5) * H;
    c.fillStyle = `rgba(160,200,240,${0.012 + hash(i) * 0.03})`;
    c.fillRect(x, y, 1.4, 1.4);
  }

  // outer frame (double line)
  c.strokeStyle = 'rgba(228,240,255,0.88)';
  c.lineWidth = 3;
  handRect(c, 40, 36, W - 80, H - 72, 11, 1.2);
  c.strokeStyle = 'rgba(228,240,255,0.42)';
  c.lineWidth = 1.2;
  handRect(c, 54, 50, W - 108, H - 100, 23, 1.0);

  // corner registration marks
  c.strokeStyle = 'rgba(228,240,255,0.55)';
  c.lineWidth = 1.6;
  [[70, 66], [W - 70, 66], [70, H - 66], [W - 70, H - 66]].forEach(([x, y]) => crossMark(c, x, y, 13));

  return off;
}

// ---------- title block (bottom-right) ----------
function drawTitleBlock(c, idx, total) {
  const bw = 620, bh = 122, x = W - 70 - bw, y = H - 66 - bh;
  const r1 = y + 42, r2 = y + 82;          // row separators
  const c1 = x + 200, c2 = x + 400;        // column separators (rows 2–3)
  c.save();
  c.fillStyle = 'rgba(9,24,48,0.78)';
  c.fillRect(x, y, bw, bh);
  c.strokeStyle = 'rgba(228,240,255,0.75)';
  c.lineWidth = 1.6;
  handRect(c, x, y, bw, bh, 41, 0.9);

  c.strokeStyle = 'rgba(228,240,255,0.42)';
  c.lineWidth = 1;
  c.beginPath();
  c.moveTo(x, r1); c.lineTo(x + bw, r1);
  c.moveTo(x, r2); c.lineTo(x + bw, r2);
  c.moveTo(c1, r1); c.lineTo(c1, y + bh);
  c.moveTo(c2, r1); c.lineTo(c2, y + bh);
  c.stroke();

  const cell = (kx, ky, kw, label, value, fs = 15) => {
    setText(c, 10, 400, true);
    c.fillStyle = INK_FAINT; c.textAlign = 'left'; c.textBaseline = 'top';
    c.fillText(label, kx, ky + 4);
    setText(c, fs, 600, true);
    c.fillStyle = INK; c.textBaseline = 'top';
    let v = String(value);
    while (v.length > 1 && textW(c, v) > kw - 10) v = v.slice(0, -2) + '…';
    c.fillText(v, kx, ky + 17);
  };
  cell(x + 12, y, bw - 24, 'TITLE', (SPEC.title || 'UNTITLED'), 17);
  cell(x + 12, r1, 188, 'DWG NO.', (SPEC.number || 'FE-' + String(idx + 1).padStart(2, '0')));
  cell(c1 + 12, r1, 188, 'SCALE', SPEC.scale || '1:1');
  cell(c2 + 12, r1, bw - 412, 'REV', SPEC.rev || 'A');
  cell(x + 12, r2, 188, 'PROJECT', (SPEC.kicker || 'FLOWEDGE'));
  cell(c1 + 12, r2, 188, 'DATE', SPEC.date || '2026.10.30');
  cell(c2 + 12, r2, bw - 412, 'SHEET', `${idx + 1} / ${total}`);
  c.restore();
}

// ---------- figures ----------
function drawBars(c, card, p, x, y, w) {
  const values = (card.values || []).slice(0, 5);
  const labels = (card.labels || []).slice(0, 5);
  if (!values.length) return;
  const max = Math.max(...values.map(v => Math.abs(v)), 1);
  const rowH = 74;
  values.forEach((v, i) => {
    const rp = prog(p, i * 0.16, i * 0.16 + 0.55);
    if (rp <= 0) return;
    const yy = y + i * rowH;
    const barMax = w - 260;
    const bw = barMax * (Math.abs(v) / max) * ease(rp);
    c.save();
    // label
    setText(c, 24, 500, true);
    c.fillStyle = INK; c.textAlign = 'left'; c.textBaseline = 'middle';
    c.fillText(labels[i] || `ITEM ${i + 1}`, x, yy + 14);
    // outline track
    c.strokeStyle = INK_FAINT; c.lineWidth = 1.2;
    handRect(c, x + 170, yy, barMax, 34, 300 + i * 13, 0.8);
    // fill (hatched)
    c.beginPath();
    c.rect(x + 170, yy, bw, 34);
    c.clip();
    c.fillStyle = 'rgba(120,214,255,0.16)';
    c.fillRect(x + 170, yy, bw, 34);
    c.strokeStyle = ACCENT; c.lineWidth = 1.1;
    for (let sx = x + 170 - 40; sx < x + 170 + bw + 40; sx += 9) {
      c.beginPath(); c.moveTo(sx, yy + 34); c.lineTo(sx + 34, yy); c.stroke();
    }
    c.restore();
    // value
    setText(c, 27, 700, true);
    c.fillStyle = ACCENT; c.textAlign = 'left'; c.textBaseline = 'middle';
    c.fillText(String(v), x + 170 + barMax + 24, yy + 14);
  });
}

function drawFlow(c, card, p, x, y, w) {
  const labels = (card.labels || []).slice(0, 4);
  if (!labels.length) return;
  const n = labels.length;
  const gap = (w - n * 220) / Math.max(n - 1, 1);
  labels.forEach((lb, i) => {
    const rp = prog(p, i * 0.22, i * 0.22 + 0.6);
    if (rp <= 0) return;
    const bx = x + i * (220 + gap), by = y;
    c.save();
    c.globalAlpha = ease(rp);
    c.strokeStyle = INK; c.lineWidth = 2;
    handRect(c, bx, by, 220, 108, 500 + i * 17, 1.1);
    setText(c, 26, 600, true);
    c.fillStyle = INK; c.textAlign = 'center'; c.textBaseline = 'middle';
    c.fillText(lb, bx + 110, by + 54);
    c.restore();
    if (i < n - 1) {
      const ap = prog(p, i * 0.22 + 0.45, i * 0.22 + 0.85);
      if (ap > 0) {
        c.save();
        c.strokeStyle = ACCENT; c.lineWidth = 2.2;
        const ax1 = bx + 228, ax2 = lerp(bx + 228, bx + 228 + gap, ap), ay = by + 54;
        handLine(c, ax1, ay, ax2, ay, 700 + i * 9, 1.0, 10);
        c.beginPath();
        c.moveTo(ax2, ay); c.lineTo(ax2 - 12, ay - 7); c.lineTo(ax2 - 12, ay + 7);
        c.closePath(); c.fillStyle = ACCENT; c.fill();
        c.restore();
      }
    }
  });
}

// ---------- card ----------
const CARD_X = 150, CARD_W = W - 300;
function drawCard(c, card, lt, cardDur, idx, total) {
  const fin = prog(lt, 0, 0.35) * (1 - prog(lt, cardDur - 0.3, cardDur));
  if (fin <= 0) return;
  c.save();
  c.globalAlpha = fin;

  // 版心与节奏随画幅变：竖屏图版更窄更高，字号按 1080 宽重排
  const CX = PORTRAIT ? 64 : 150;
  const CW = W - CX * 2;
  const kickY = PORTRAIT ? H * 0.135 : 150;
  const hy = PORTRAIT ? H * 0.175 : 206;
  const hSize = PORTRAIT ? 74 : 54;
  const pStep = PORTRAIT ? 104 : 60;
  const pSize = PORTRAIT ? 42 : 30;
  const figY = PORTRAIT ? H * 0.54 : 618;
  const dimY = PORTRAIT ? H * 0.815 : 972;

  // index number (顶部水印序号)
  setText(c, PORTRAIT ? 150 : 132, 700, true);
  c.fillStyle = 'rgba(150,196,250,0.085)';
  c.textAlign = 'right'; c.textBaseline = 'top';
  c.fillText(String(idx + 1).padStart(2, '0'), W - (PORTRAIT ? 64 : 96), kickY - 46);

  // kicker
  setText(c, PORTRAIT ? 22 : 20, 500, true);
  c.fillStyle = INK_FAINT; c.letterSpacing = '4px';
  c.textAlign = 'left'; c.textBaseline = 'top';
  c.fillText((card.kicker || SPEC.kicker || 'FLOWEDGE · BLUEPRINT NOTE').toUpperCase(), CX, kickY);
  c.letterSpacing = '0px';

  // heading：竖屏按标点断行，最多 3 行（横屏维持单行打字机）
  const hp = prog(lt, 0.15, 0.75);
  setText(c, hSize, 700);
  const lines = PORTRAIT ? wrapByPunct(c, card.heading || '', CW, 3) : [card.heading || ''];
  let hBottom = hy, hWidth = 0;
  lines.forEach((ln, k) => {
    setText(c, hSize, 700);
    c.fillStyle = INK; c.textAlign = 'left'; c.textBaseline = 'top';
    c.fillText(typed(ln, hp), CX, hy + k * hSize * 1.28);
    hBottom = hy + k * hSize * 1.28;
    hWidth = Math.max(hWidth, textW(c, ln));
  });

  // 双线（手绘抖动）在标题下方
  if (hp >= 1) {
    const up = prog(lt, 0.75, 1.25);
    c.strokeStyle = ACCENT; c.lineWidth = 2.6;
    handLine(c, CX, hBottom + hSize * 1.34, CX + hWidth * up, hBottom + hSize * 1.34, 900 + idx, 1.4, Math.max(4, Math.round(20 * up)));
    c.strokeStyle = 'rgba(120,214,255,0.35)'; c.lineWidth = 1.2;
    handLine(c, CX, hBottom + hSize * 1.34 + 8, CX + hWidth * up, hBottom + hSize * 1.34 + 8, 940 + idx, 1.0, Math.max(4, Math.round(16 * up)));
  }

  // points
  let py = hBottom + hSize * 1.34 + (PORTRAIT ? 64 : 62);
  const maxPts = PORTRAIT ? 4 : 6;
  (card.points || []).slice(0, maxPts).forEach((pt, i) => {
    const t0 = 1.15 + i * 0.62;
    const rp = prog(lt, t0, t0 + 0.55);
    if (rp <= 0) return;
    setText(c, pSize, 400);
    c.fillStyle = INK_DIM;
    c.textAlign = 'left'; c.textBaseline = 'top';
    c.fillText('▸', CX, py);
    c.fillStyle = INK;
    const pl = PORTRAIT ? wrapText(c, pt, CW - 44)[0] : pt;
    c.fillText(typed(pl, rp), CX + (PORTRAIT ? 40 : 34), py);
    py += pStep;
  });

  // figure
  if (card.figure === 'bars') drawBars(c, card, prog(lt, 1.6, 3.6), CX, figY, CW - 40);
  else if (card.figure === 'flow') drawFlow(c, card, prog(lt, 1.6, 3.4), CX, figY, CW - 80);

  // dimension decoration under content
  c.strokeStyle = INK_FAINT;
  dimensionLine(c, CX, dimY, CX + CW * 0.42, dimY, card.note || '', 1200 + idx);

  c.restore();
}
// ---------- page assembly ----------
function drawFrame(t) {
  const c = ctx;
  c.setTransform(1, 0, 0, 1, 0, 0);
  c.globalAlpha = 1;
  c.fillStyle = PAPER_2;
  c.fillRect(0, 0, VW, VH);
  c.setTransform(SC, 0, 0, SC, (VW - W * SC) / 2, (VH - H * SC) / 2);
  if (bg) c.drawImage(bg, 0, 0);

  // top strip
  setText(c, 19, 500, true);
  c.fillStyle = INK_FAINT; c.textAlign = 'left'; c.textBaseline = 'top';
  c.fillText(SPEC.kicker || 'FLOWEDGE', 70, 70);
  c.textAlign = 'right';
  c.fillText(SPEC.titleEn || 'FLOWEDGE · CODE VIDEO', W - 70, 70);

  // cards
  const n = CARDS.length;
  for (let i = 0; i < n; i++) {
    const a = i * (DUR / n), b = a + DUR / n;
    if (t >= a - 0.05 && t <= b + 0.05) {
      drawCard(c, CARDS[i], t - a, DUR / n, i, n);
    }
  }

  drawTitleBlock(c, Math.min(Math.floor(t / (DUR / n) + 1e-6), n - 1), n);

  // progress ticks (top-right, below the title strip)
  c.save();
  const px0 = W - 360, py0 = 108, pw = 290;
  c.strokeStyle = 'rgba(228,240,255,0.22)'; c.lineWidth = 1;
  c.beginPath(); c.moveTo(px0, py0); c.lineTo(px0 + pw, py0); c.stroke();
  c.strokeStyle = ACCENT; c.lineWidth = 2;
  c.beginPath(); c.moveTo(px0, py0); c.lineTo(px0 + pw * clamp(t / DUR, 0, 1), py0); c.stroke();
  for (let i = 0; i <= n; i++) {
    const x = px0 + (pw * i) / n;
    c.beginPath(); c.moveTo(x, py0 - 4); c.lineTo(x, py0 + 4); c.stroke();
  }
  c.restore();

  // frame counter
  setText(c, 16, 400, true);
  c.fillStyle = INK_FAINT; c.textAlign = 'left'; c.textBaseline = 'bottom';
  c.fillText(`TC ${String(Math.floor(t / 60)).padStart(2, '0')}:${(t % 60).toFixed(2).padStart(5, '0')}`, 70, H - 62);
  c.fillText(`F ${Math.round(t * (SPEC.fps || 24))}`, 340, H - 62);
}

function collectTexts(t) {
  const out = [];
  const n = CARDS.length, cardDur = DUR / n;
  const i = clamp(Math.floor(t / cardDur), 0, n - 1);
  const card = CARDS[i], lt = t - i * cardDur;
  if (lt < 0.35) return out;

  out.push({ id: 'title', text: SPEC.title || '', x0: 70, y0: 60, x1: 70 + 600, y1: 100 });
  out.push({ id: 'kicker', text: (card.kicker || SPEC.kicker || ''), x0: CARD_X, y0: 140, x1: CARD_X + 700, y1: 175 });
  if (card.heading) out.push({ id: 'h' + i, text: card.heading, x0: CARD_X, y0: 200, x1: CARD_X + 900, y1: 300 });
  (card.points || []).forEach((pt, j) => {
    if (lt >= 1.15 + j * 0.62) out.push({ id: 'p' + i + '_' + j, text: pt, x0: CARD_X + 34, y0: 340 + j * 56, x1: CARD_X + 1500, y1: 396 + j * 56 });
  });
  (card.labels || []).forEach((lb, j) => {
    if (lt >= 1.6) out.push({ id: 'l' + i + '_' + j, text: lb, x0: CARD_X, y0: 660, x1: CARD_X + 1200, y1: 760 });
  });
  return out;
}

// ---------- boot ----------
async function boot() {
  let spec = null;
  try {
    const r = await fetch('spec.json', { cache: 'no-store' });
    if (r.ok) spec = await r.json();
  } catch (e) { /* fallback below */ }
  if (!spec) spec = { title: 'UNTITLED', duration: 12, cards: [{ heading: 'NO SPEC', points: ['spec.json 缺失'] }] };

  SPEC = spec;
  CARDS = (spec.cards && spec.cards.length) ? spec.cards : [{ heading: spec.title || 'UNTITLED', points: [] }];
  const cardSec = Number(spec.card_seconds) > 0 ? Number(spec.card_seconds) : 6;
  DUR = Number(spec.duration) > 0 ? Number(spec.duration) : CARDS.length * cardSec;
  if (!(DUR > 0)) DUR = 6;
  SPEC.fps = Number(spec.fps) > 0 ? Number(spec.fps) : 24;

  setupLayout();
  bg = buildBackground();

  // 设计稿固定 1920×1080；--size 只决定截图视口，构图按视口等比缩放后居中，
  // 这样 960×540 / 720×1280 竖屏都不会裁掉右下角的标题栏。
  const vw = window.innerWidth || W, vh = window.innerHeight || H;
  const scale = Math.min(vw / W, vh / H);
  canvas.style.width = Math.round(W * scale) + 'px';
  canvas.style.height = Math.round(H * scale) + 'px';
  canvas.style.left = Math.round((vw - W * scale) / 2) + 'px';
  canvas.style.top = Math.round((vh - H * scale) / 2) + 'px';

  window.DUR = DUR;
  window.render = (t) => drawFrame(Math.max(0, Math.min(Number(t) || 0, DUR)));
  window.TEXTS = (t) => collectTexts(Math.max(0, Math.min(Number(t) || 0, DUR)));
  drawFrame(0);
  window.READY = true;
}

boot();
