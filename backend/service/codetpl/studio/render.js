// FlowEdge · studio —— 「黑画布 MG 信息图 + 章节 HUD」
// 视觉签名（对标 anything2explainer / 瑞士派生排版）：
//   纯黑底、超大无衬线标题、单一强调色、细分隔线、底部分段章节进度条。
//   全部由代码绘制，无素材、无生成模型，逐帧可复现。
// 页面契约（lemo）：window.READY / window.DUR / window.render(t)

import { clamp, lerp, easeInOut, easeOut, wrapText, wrapBalanced, wrapByPunct, setFont, makeGrain, SANS, MONO } from './shared/kit.js';

const DW = 1920, DH = 1080;
const BG = '#050506';
const WHITE = '#F5F7FA';
const DIM = 'rgba(245,247,250,.55)';
const FAINT = 'rgba(245,247,250,.22)';
const ACC = '#4D9FFF';
const WARM = '#FF7A59';

const canvas = document.getElementById('c');
const ctx = canvas.getContext('2d', { alpha: false });
let SPEC = null, CARDS = [], DUR = 0, GRAIN = null, VW = DW, VH = DH;

function resize() {
  VW = canvas.width = Math.max(2, window.innerWidth | 0);
  VH = canvas.height = Math.max(2, window.innerHeight | 0);
  if (!GRAIN) GRAIN = makeGrain(DW, DH, 12, 23);
}

function beginFrame() {
  const s = Math.min(VW / DW, VH / DH);
  ctx.setTransform(1, 0, 0, 1, 0, 0);
  ctx.fillStyle = BG;
  ctx.fillRect(0, 0, VW, VH);
  ctx.setTransform(s, 0, 0, s, (VW - DW * s) / 2, (VH - DH * s) / 2);
}

function line(x1, y1, x2, y2, color = FAINT, w = 1) {
  ctx.strokeStyle = color;
  ctx.lineWidth = w;
  ctx.beginPath();
  ctx.moveTo(x1, y1);
  ctx.lineTo(x2, y2);
  ctx.stroke();
}

function bgLayer(t, i) {
  ctx.fillStyle = BG;
  ctx.fillRect(0, 0, DW, DH);

  // 一处极弱的冷光，避免纯黑发死
  const g = ctx.createRadialGradient(DW * 0.22, DH * 0.16, 40, DW * 0.22, DH * 0.16, DW * 0.85);
  g.addColorStop(0, 'rgba(77,159,255,.075)');
  g.addColorStop(0.45, 'rgba(40,70,120,.035)');
  g.addColorStop(1, 'rgba(0,0,0,0)');
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, DW, DH);

  // 技术网格（极淡）
  ctx.strokeStyle = 'rgba(255,255,255,.026)';
  ctx.lineWidth = 1;
  for (let x = 120; x <= 1800; x += 120) line(x, 96, x, DH - 72, 'rgba(255,255,255,.022)');
  for (let y = 96; y <= DH - 72; y += 120) line(120, y, 1800, y, 'rgba(255,255,255,.022)');

  if (GRAIN) {
    ctx.globalAlpha = 0.42;
    ctx.drawImage(GRAIN, 0, 0, DW, DH);
    ctx.globalAlpha = 1;
  }

  // 压角
  const v = ctx.createRadialGradient(DW / 2, DH / 2, DH * 0.34, DW / 2, DH / 2, DW * 0.74);
  v.addColorStop(0, 'rgba(0,0,0,0)');
  v.addColorStop(1, 'rgba(0,0,0,.62)');
  ctx.fillStyle = v;
  ctx.fillRect(0, 0, DW, DH);
}

function hud(card, i, total, local) {
  setFont(ctx, 19, 600, MONO);
  ctx.fillStyle = ACC;
  ctx.textAlign = 'left';
  ctx.textBaseline = 'alphabetic';
  const kick = (card.kicker || SPEC.kicker || 'FLOWEDGE · EXPLAINER').toUpperCase();
  ctx.fillText(kick, 120, 112);

  setFont(ctx, 19, 500, MONO);
  ctx.fillStyle = DIM;
  ctx.textAlign = 'right';
  ctx.fillText(`${String(i + 1).padStart(2, '0')} / ${String(total).padStart(2, '0')}`, 1800, 112);

  // HUD 底线，带一个随进度走的强调段
  line(120, 148, 1800, 148, FAINT);
  const segW = (1800 - 120) * clamp(local, 0, 1);
  ctx.fillStyle = ACC;
  ctx.fillRect(120, 147, segW, 2);
}

function heading(card, y, local) {
  const w = 1560;
  // 从大往小试，取第一档能塞进两行且断行均衡的字号 —— 宁可变小，也不要出现「多」单独一行
  let size = 104, lines = [];
  for (const s of [104, 94, 86, 78, 70, 64]) {
    setFont(ctx, s, 700, SANS);
    const cand = wrapByPunct(ctx, card.heading || '', w, 2);
    if (cand.length <= 2) {
      size = s;
      lines = cand;
      break;
    }
  }
  if (!lines.length) {
    setFont(ctx, 64, 700, SANS);
    lines = wrapText(ctx, card.heading || '', w).slice(0, 2);
    size = 64;
  }
  const lh = size * 1.18;
  const p = easeOut(clamp(local / 0.18, 0, 1));

  ctx.save();
  ctx.beginPath();
  ctx.rect(118, y - size, w * p + 8, lines.length * lh + size * 0.5);
  ctx.clip();
  lines.forEach((ln, k) => {
    setFont(ctx, size, 700, SANS);
    ctx.fillStyle = WHITE;
    ctx.textAlign = 'left';
    ctx.textBaseline = 'alphabetic';
    ctx.fillText(ln, 120, y + k * lh);
  });
  ctx.restore();
  return y + (lines.length - 1) * lh;
}

function points(card, y, local) {
  const pts = (card.points || []).slice(0, 4);
  pts.forEach((txt, j) => {
    const ap = clamp((local - 0.16 - j * 0.075) / 0.2, 0, 1);
    if (ap <= 0) return;
    const py = y + 78 + j * 74;
    ctx.globalAlpha = ap;

    setFont(ctx, 20, 600, MONO);
    ctx.fillStyle = j === 0 ? ACC : DIM;
    ctx.textAlign = 'left';
    ctx.textBaseline = 'alphabetic';
    ctx.fillText(String(j + 1).padStart(2, '0'), 120, py);

    ctx.fillStyle = j === 0 ? ACC : FAINT;
    ctx.fillRect(168, py - 9, 18, 2);

    setFont(ctx, 34, 500, SANS);
    ctx.fillStyle = 'rgba(245,247,250,.88)';
    const wrapped = wrapText(ctx, txt, 1180);
    ctx.fillText(wrapped[0] + (wrapped.length > 1 ? '…' : ''), 210, py);
    ctx.globalAlpha = 1;
  });
}

function barsFigure(card, y, local) {
  const labels = card.labels || [], vals = card.values || [];
  if (!labels.length || !vals.length) return;
  const max = Math.max(...vals, 1);
  const x0 = 120, x1 = 1800, labelW = 250;
  labels.forEach((lb, i) => {
    const bp = clamp((local - 0.22 - i * 0.08) / 0.35, 0, 1);
    if (bp <= 0) return;
    const by = y + i * 58;
    ctx.globalAlpha = bp;
    setFont(ctx, 22, 500, SANS);
    ctx.fillStyle = DIM;
    ctx.textAlign = 'left';
    ctx.textBaseline = 'alphabetic';
    ctx.fillText(lb, x0, by + 16);

    const trackW = x1 - x0 - labelW - 90;
    const bw = trackW * (vals[i] / max) * easeOut(bp);
    ctx.fillStyle = 'rgba(255,255,255,.06)';
    ctx.fillRect(x0 + labelW, by, trackW, 10);
    ctx.fillStyle = i % 2 ? WARM : ACC;
    ctx.fillRect(x0 + labelW, by, Math.max(2, bw), 10);

    setFont(ctx, 20, 600, MONO);
    ctx.fillStyle = 'rgba(245,247,250,.78)';
    ctx.fillText(String(vals[i]), x0 + labelW + Math.max(2, bw) + 16, by + 14);
    ctx.globalAlpha = 1;
  });
}

function footer(card, i, total, local) {
  // 分段章节进度条
  const y = 986, x0 = 120, x1 = 1800;
  const gap = 12;
  const segW = (x1 - x0 - gap * (total - 1)) / total;
  for (let k = 0; k < total; k++) {
    const sx = x0 + k * (segW + gap);
    ctx.fillStyle = 'rgba(255,255,255,.10)';
    ctx.fillRect(sx, y, segW, 5);
    let fill = 0;
    if (k < i) fill = 1;
    else if (k === i) fill = easeInOut(clamp(local, 0, 1));
    if (fill > 0) {
      ctx.fillStyle = ACC;
      ctx.fillRect(sx, y, segW * fill, 5);
    }
  }

  setFont(ctx, 17, 500, MONO);
  ctx.fillStyle = DIM;
  ctx.textAlign = 'left';
  ctx.fillText((card.kicker || 'CHAPTER').toUpperCase(), 120, 962);

  setFont(ctx, 16, 500, MONO);
  ctx.fillStyle = 'rgba(245,247,250,.34)';
  ctx.textAlign = 'right';
  ctx.fillText((card.note || SPEC.title_en || 'FLOWEDGE').toUpperCase(), 1800, 962);
}

function drawFrame(t) {
  resize();
  beginFrame();
  const n = Math.max(1, CARDS.length);
  const per = DUR / n;
  const i = clamp(Math.floor(t / per), 0, n - 1);
  const local = clamp((t - i * per) / per, 0, 1);
  const card = CARDS[i];

  bgLayer(t, i);
  hud(card, i, n, local);

  const yEnd = heading(card, 330, local);
  const hasBars = card.figure === 'bars' && (card.labels || []).length;
  if (hasBars) {
    points(card, yEnd + 40, local);
    barsFigure(card, 706, local);
  } else {
    points(card, yEnd + 56, local);
  }
  footer(card, i, n, local);
}

async function init() {
  resize();
  const r = await fetch('spec.json', { cache: 'no-store' });
  SPEC = await r.json();
  CARDS = (SPEC.cards && SPEC.cards.length) ? SPEC.cards : [{ heading: SPEC.title, points: [] }];
  const per = SPEC.card_seconds > 0 ? SPEC.card_seconds : 6;
  DUR = SPEC.duration > 0 ? SPEC.duration : CARDS.length * per;
  if (document.fonts && document.fonts.ready) await document.fonts.ready;
  drawFrame(0);
  window.DUR = DUR;
  window.render = (t) => drawFrame(clamp(Number(t) || 0, 0, DUR));
  window.TEXTS = (t) => {
    const per2 = DUR / Math.max(1, CARDS.length);
    const k = clamp(Math.floor((Number(t) || 0) / per2), 0, CARDS.length - 1);
    const c = CARDS[k];
    return [c.heading, ...(c.points || [])].filter(Boolean);
  };
  window.READY = true;
}

window.addEventListener('resize', () => drawFrame(0));
init().catch((e) => { console.error(e); window.READY = true; });
