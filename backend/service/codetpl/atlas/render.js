// FlowEdge · atlas —— 「DK 图鉴 / 3D 剖面图解」
// 视觉签名（对标小红书爆款「把北京地铁切开」）：
//   羊皮纸米色底 #F0EDE6、思源宋体大标题、细折线引线 + 实心圆点标注、
//   底部页码序列与「概念示意」免责标注。画幅自适应（16:9 / 3:4 都成立）。
// 页面契约（lemo）：window.READY / window.DUR / window.render(t)

import { clamp, lerp, prog, easeInOut, easeOut, wrapText, wrapBalanced, wrapByPunct, setFont, roundRect, makeGrain, SANS, SERIF, MONO } from './shared/kit.js';

const DW = 1920, DH = 1080;      // 设计画布
const PAPER = '#F0EDE6';
const INK = '#1F2429';
const INK_DIM = 'rgba(31,36,41,.62)';
const INK_FAINT = 'rgba(31,36,41,.26)';
const NAVY = '#1F3A5F';
const RUST = '#A8452E';
const SAND = '#DCD5C6';

const canvas = document.getElementById('c');
const ctx = canvas.getContext('2d', { alpha: false });
let SPEC = null, CARDS = [], DUR = 0, GRAIN = null, VW = DW, VH = DH;

function resize() {
  VW = canvas.width = Math.max(2, window.innerWidth | 0);
  VH = canvas.height = Math.max(2, window.innerHeight | 0);
  if (!GRAIN) GRAIN = makeGrain(DW, DH, 15, 11);
}

function beginFrame() {
  // 设计坐标 → 实际像素（等比，居中，超出部分由底色兜住）
  const s = Math.min(VW / DW, VH / DH);
  ctx.setTransform(1, 0, 0, 1, 0, 0);
  ctx.fillStyle = PAPER;
  ctx.fillRect(0, 0, VW, VH);
  ctx.setTransform(s, 0, 0, s, (VW - DW * s) / 2, (VH - DH * s) / 2);
}

// ---------- 纸张 ----------
function paper() {
  ctx.fillStyle = PAPER;
  ctx.fillRect(0, 0, DW, DH);
  // 暖色微渐变，避免纯色死板
  const g = ctx.createLinearGradient(0, 0, DW, DH);
  g.addColorStop(0, 'rgba(255,252,246,.55)');
  g.addColorStop(0.5, 'rgba(240,237,230,0)');
  g.addColorStop(1, 'rgba(222,214,199,.45)');
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, DW, DH);
  if (GRAIN) {
    ctx.globalAlpha = 0.5;
    ctx.drawImage(GRAIN, 0, 0, DW, DH);
    ctx.globalAlpha = 1;
  }
  // 四角压暗（印刷品的边缘）
  const v = ctx.createRadialGradient(DW / 2, DH / 2, DH * 0.42, DW / 2, DH / 2, DW * 0.66);
  v.addColorStop(0, 'rgba(120,105,80,0)');
  v.addColorStop(1, 'rgba(120,105,80,.13)');
  ctx.fillStyle = v;
  ctx.fillRect(0, 0, DW, DH);
}

function hairline(x1, y1, x2, y2, color = INK_FAINT, w = 1) {
  ctx.strokeStyle = color;
  ctx.lineWidth = w;
  ctx.beginPath();
  ctx.moveTo(x1, y1);
  ctx.lineTo(x2, y2);
  ctx.stroke();
}

// ---------- 剖面图 ----------
function hatch(x, y, w, h, alpha, seedPhase) {
  ctx.save();
  ctx.beginPath();
  ctx.rect(x, y, w, h);
  ctx.clip();
  ctx.strokeStyle = `rgba(31,58,95,${alpha})`;
  ctx.lineWidth = 1;
  const step = 11;
  const off = (seedPhase * step) % step;
  for (let i = -h; i < w + h; i += step) {
    ctx.beginPath();
    ctx.moveTo(x + i + off, y);
    ctx.lineTo(x + i + off - h, y + h);
    ctx.stroke();
  }
  ctx.restore();
}

// 图层剖面：把 content 的抽象结构画成「地层」
function drawSection(card, x, y, w, h, p) {
  const bands = Math.min(5, Math.max(3, (card.points || []).length + 1));
  const gap = 10;
  const bandH = (h - gap * (bands - 1)) / bands;
  const tones = [0.17, 0.23, 0.13, 0.20, 0.16];
  const fills = ['#E4DCC9', '#D4CBB6', '#EFE9DC', '#C9BFA8', '#DED5C2'];

  for (let i = 0; i < bands; i++) {
    const bp = clamp((p - i * 0.1) / 0.55, 0, 1);
    if (bp <= 0) continue;
    const by = y + i * (bandH + gap) + (1 - easeOut(bp)) * 26;
    const bw = w * (0.55 + 0.45 * easeOut(bp));

    ctx.globalAlpha = easeOut(bp);
    ctx.fillStyle = fills[i % fills.length];
    ctx.fillRect(x, by, bw, bandH);
    hatch(x, by, bw, bandH, tones[i % tones.length], i * 0.37 + p);
    ctx.strokeStyle = 'rgba(31,36,41,.34)';
    ctx.lineWidth = 1.2;
    ctx.strokeRect(x + 0.5, by + 0.5, bw - 1, bandH - 1);
    ctx.globalAlpha = 1;

    // 层内纹理：一条深浅分界线
    ctx.strokeStyle = 'rgba(31,36,41,.16)';
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(x, by + bandH * 0.62);
    ctx.lineTo(x + bw, by + bandH * 0.62);
    ctx.stroke();
  }
  return { bands, bandH, gap };
}

// 标注系统：细折线 + 实心小圆点（DK 图鉴的招牌）
// 没给 labels 时自动从 points 里取短标签 —— 剖面图不能是空图版，必须有人读得懂的标注
function autoLabels(card) {
  if ((card.labels || []).length) return card.labels;
  return (card.points || []).slice(0, 3).map((t) => {
    const s = String(t).trim();
    return s.length > 9 ? s.slice(0, 9) + '…' : s;
  });
}

function drawCallouts(card, box, p) {
  const labels = autoLabels(card);
  if (!labels.length) return;
  const { x, y, w, h } = box;
  const n = labels.length;
  labels.forEach((lb, i) => {
    const ap = clamp((p - 0.35 - i * 0.12) / 0.5, 0, 1);
    if (ap <= 0) return;
    const ay = y + h * ((i + 0.5) / n);
    const ax = x + w * (0.32 + 0.24 * ((i * 7) % 3) / 2);
    const bx = x + w + 54;
    const by = ay - 26 + i * 4;
    const elbowX = lerp(ax, bx, 0.45);

    ctx.globalAlpha = ap;
    ctx.strokeStyle = 'rgba(31,36,41,.55)';
    ctx.lineWidth = 1.3;
    ctx.beginPath();
    ctx.moveTo(ax, ay);
    const t1 = clamp(ap / 0.55, 0, 1), t2 = clamp((ap - 0.45) / 0.55, 0, 1);
    ctx.lineTo(lerp(ax, elbowX, t1), lerp(ay, ay, t1));
    ctx.lineTo(lerp(elbowX, bx, t2), lerp(ay, by, t2));
    ctx.stroke();

    // 圆点
    ctx.beginPath();
    ctx.arc(ax, ay, 4.6, 0, Math.PI * 2);
    ctx.fillStyle = i % 3 === 1 ? RUST : NAVY;
    ctx.fill();
    ctx.strokeStyle = PAPER;
    ctx.lineWidth = 2;
    ctx.stroke();

    // 标签
    if (ap > 0.6) {
      ctx.globalAlpha = clamp((ap - 0.6) / 0.4, 0, 1);
      setFont(ctx, 20, 600, SANS);
      ctx.fillStyle = INK;
      ctx.textAlign = 'left';
      ctx.textBaseline = 'middle';
      ctx.fillText(lb, bx + 12, by);
      ctx.globalAlpha = ap;
    }
    ctx.globalAlpha = 1;
  });
}

function drawBars(card, x, y, w, h, p) {
  const labels = card.labels || [], vals = card.values || [];
  if (!labels.length || !vals.length) return;
  const max = Math.max(...vals, 1);
  const rowH = h / labels.length;
  labels.forEach((lb, i) => {
    const bp = clamp((p - 0.2 - i * 0.1) / 0.5, 0, 1);
    if (bp <= 0) return;
    const by = y + i * rowH;
    setFont(ctx, 21, 600, SANS);
    ctx.fillStyle = INK_DIM;
    ctx.textAlign = 'left';
    ctx.textBaseline = 'alphabetic';
    ctx.fillText(lb, x, by + 22);
    const bw = (w - 210) * (vals[i] / max) * easeOut(bp);
    ctx.fillStyle = i % 2 ? 'rgba(31,58,95,.82)' : RUST;
    ctx.fillRect(x + 200, by + 4, Math.max(2, bw), 12);
    setFont(ctx, 20, 500, MONO);
    ctx.fillStyle = INK_DIM;
    ctx.fillText(String(vals[i]), x + 210 + Math.max(2, bw), by + 16);
  });
}

// ---------- 单张图鉴页 ----------
function drawPage(card, i, total, local, alphaOut) {
  paper();
  ctx.globalAlpha = alphaOut;

  // 版心线
  hairline(110, 84, 1810, 84, 'rgba(31,36,41,.20)');

  // 眉标
  setFont(ctx, 19, 600, MONO);
  ctx.fillStyle = NAVY;
  ctx.textAlign = 'left';
  ctx.textBaseline = 'alphabetic';
  const kick = (card.kicker || 'KNOWLEDGE ATLAS').toUpperCase();
  ctx.save();
  ctx.letterSpacing = '3px';
  ctx.fillText(kick, 110, 128);
  ctx.restore();

  // 页码序列右上
  setFont(ctx, 19, 500, MONO);
  ctx.fillStyle = INK_DIM;
  ctx.textAlign = 'right';
  ctx.fillText(`PLATE ${String(i + 1).padStart(2, '0')} / ${String(total).padStart(2, '0')}`, 1810, 128);

  // 标题（宋体，遮罩展开）：从大往小试，取第一档能两行内断完的字号
  const pIn = clamp(local / 0.16, 0, 1);
  let size = 76, lines = [];
  for (const s of [80, 72, 64, 58, 52]) {
    setFont(ctx, s, 700, SERIF);
    const cand = wrapByPunct(ctx, card.heading || '', 880, 2);
    if (cand.length <= 2) {
      size = s;
      lines = cand;
      break;
    }
  }
  if (!lines.length) {
    setFont(ctx, 52, 700, SERIF);
    lines = wrapText(ctx, card.heading || '', 880).slice(0, 2);
    size = 52;
  }
  const shown = lines.length;
  const lh = size * 1.3;
  let ty = 214 + size;
  ctx.save();
  ctx.beginPath();
  ctx.rect(110, ty - size, 900 * easeOut(pIn), shown * lh + 10);
  ctx.clip();
  for (let k = 0; k < shown; k++) {
    setFont(ctx, size, 700, SERIF);
    ctx.fillStyle = INK;
    ctx.textAlign = 'left';
    ctx.textBaseline = 'alphabetic';
    ctx.fillText(lines[k], 110, ty + k * lh);
  }
  ctx.restore();
  ty += (shown - 1) * lh;

  // 标题下的 navy 短线
  ctx.fillStyle = NAVY;
  ctx.fillRect(110, ty + 12, 132 * easeOut(clamp((local - 0.05) / 0.2, 0, 1)), 3);
  ty += 56;

  // 正文要点：编号 + 一条细分隔
  (card.points || []).slice(0, 4).forEach((txt, j) => {
    const ap = clamp((local - 0.12 - j * 0.07) / 0.18, 0, 1);
    const y = ty + j * 112;
    setFont(ctx, 18, 600, MONO);
    ctx.fillStyle = 'rgba(168,69,46,.85)';
    ctx.textAlign = 'left';
    ctx.textBaseline = 'alphabetic';
    ctx.globalAlpha = alphaOut * ap;
    ctx.fillText(String(j + 1).padStart(2, '0'), 110, y + 26);

    setFont(ctx, 30, 500, SANS);
    ctx.fillStyle = 'rgba(31,36,41,.86)';
    const wrapped = wrapText(ctx, txt, 700);
    ctx.fillText(wrapped[0] + (wrapped.length > 1 ? '…' : ''), 168, y + 26);

    hairline(110, y + 62, 110 + 760 * ap, y + 62, 'rgba(31,36,41,.14)');
    ctx.globalAlpha = alphaOut;
  });

  // 右侧图版
  const box = { x: 986, y: 214, w: 540, h: 520 };
  const fp = clamp((local - 0.1) / 0.3, 0, 1);
  ctx.strokeStyle = 'rgba(31,36,41,.30)';
  ctx.lineWidth = 1.4;
  ctx.strokeRect(box.x - 26, box.y - 34, box.w + 148, box.h + 96);

  if (card.figure === 'bars') drawBars(card, box.x, box.y, box.w, box.h, fp);
  else {
    drawSection(card, box.x, box.y, box.w, box.h * 0.9, fp);
    drawCallouts(card, { ...box, h: box.h * 0.9 }, fp);
  }

  // 图版说明
  setFont(ctx, 17, 500, MONO);
  ctx.fillStyle = INK_DIM;
  ctx.textAlign = 'left';
  ctx.fillText((card.note || 'CONCEPTUAL SECTION').toUpperCase(), box.x - 26, box.y + box.h + 48);

  // 页脚
  hairline(110, 946, 1810, 946, 'rgba(31,36,41,.20)');
  setFont(ctx, 17, 500, SANS);
  ctx.fillStyle = 'rgba(31,36,41,.52)';
  ctx.textAlign = 'left';
  ctx.fillText('本图为原理概念示意，非等比例还原', 110, 988);

  setFont(ctx, 17, 500, MONO);
  ctx.fillStyle = INK_DIM;
  ctx.textAlign = 'center';
  ctx.fillText(`— ${(SPEC.title || 'KNOWLEDGE ATLAS')} · 第 ${i + 1} 页 / 共 ${total} 页 —`, 960, 988);

  setFont(ctx, 15, 600, MONO);
  ctx.fillStyle = 'rgba(31,36,41,.40)';
  ctx.textAlign = 'right';
  ctx.fillText('FLOWEDGE · ATLAS', 1810, 988);

  ctx.globalAlpha = 1;
}

function drawFrame(t) {
  resize();
  beginFrame();
  const n = Math.max(1, CARDS.length);
  const per = DUR / n;
  const i = clamp(Math.floor(t / per), 0, n - 1);
  const local = clamp((t - i * per) / per, 0, 1);
  // 结尾 12% 淡出，给换页留呼吸
  const alphaOut = local > 0.88 ? 1 - (local - 0.88) / 0.12 : 1;
  drawPage(CARDS[i], i, n, local, alphaOut);
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
