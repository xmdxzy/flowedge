// FlowEdge · darkcard —— 「暗色 3D 卡片世界 + 准一镜到底」
// 视觉签名（对标抖音「基地」）：主题化暗色空间、卡面组件库、相机飞行代替剪辑。
// 页面契约（lemo）：window.READY / window.DUR / window.render(t) / window.TEXTS(t)
// 确定性：每一帧只依赖 t。

import { clamp, lerp, easeInOut, hash, SANS, MONO } from './shared/kit.js';

const STAGE_W = 1920, STAGE_H = 1080;
const SEG = 1500;            // 卡片间距（世界坐标）
const HOLD = 0.56;           // 每张卡停留比例，剩下的时间飞向下一张

const stage = document.getElementById('stage');
const world = document.getElementById('world');
const bg = document.getElementById('bg');
const bctx = bg.getContext('2d');

let SPEC = null, CARDS = [], DUR = 0, nodes = [];

function scaleStage() {
  const s = Math.min(window.innerWidth / STAGE_W, window.innerHeight / STAGE_H);
  stage.style.width = STAGE_W + 'px';
  stage.style.height = STAGE_H + 'px';
  stage.style.left = '50%';
  stage.style.top = '50%';
  stage.style.transform = `translate(-50%,-50%) scale(${s})`;
  bg.width = STAGE_W;
  bg.height = STAGE_H;
  bg.style.width = STAGE_W + 'px';
  bg.style.height = STAGE_H + 'px';
  bg.style.left = '50%';
  bg.style.top = '50%';
  bg.style.marginLeft = -STAGE_W / 2 + 'px';
  bg.style.marginTop = -STAGE_H / 2 + 'px';
  bg.style.transform = `scale(${s})`;
  bg.style.transformOrigin = '50% 50%';
}

function el(cls, parent) {
  const d = document.createElement('div');
  if (cls) d.className = cls;
  parent.appendChild(d);
  return d;
}

function buildCard(card, i) {
  const d = el('card', world);
  // 每张卡在世界里轻微左右交错，让走廊不呆板；
  // 交错量由相机侧补偿（见 drawFrame 的 compX），否则飞行时卡片会偏出画面。
  const xo = i % 2 === 0 ? -46 : 46;
  const yo = i % 2 === 0 ? 8 : -8;
  const yaw = i % 2 === 0 ? 4.2 : -4.2;
  d.style.transform = `translate3d(${xo}px, ${yo}px, ${-i * SEG}px) rotateY(${yaw}deg)`;
  d.style.zIndex = String(1000 - i);

  const idx = el('div', d);
  idx.className = 'idx';
  idx.textContent = String(i + 1).padStart(2, '0') + ' / ' + String(CARDS.length).padStart(2, '0');

  if (card.kicker) {
    const k = el('div', d);
    k.className = 'kick';
    k.innerHTML = '<i></i><span>' + escapeHtml(card.kicker) + '</span>';
  }

  const h = document.createElement('h1');
  h.textContent = card.heading || '';
  // 中文标题按字数定字号：宁可小一点，也不要断成「…差这么 / 多」
  const len = [...(card.heading || '')].length;
  h.style.setProperty('--h1', (len <= 12 ? 76 : len <= 16 ? 68 : len <= 20 ? 58 : 50) + 'px');
  d.appendChild(h);

  const rule = el('div', d);
  rule.className = 'rule';

  const ul = document.createElement('ul');
  const hasBars = card.figure === 'bars' && (card.labels || []).length && (card.values || []).length;
  // 有柱状图时只留两条要点，否则要点和图表打架
  (card.points || []).slice(0, hasBars ? 2 : 4).forEach((p) => {
    const li = document.createElement('li');
    li.textContent = p;
    ul.appendChild(li);
  });
  d.appendChild(ul);

  let bars = null;
  if (card.figure === 'bars' && (card.labels || []).length && (card.values || []).length) {
    bars = el('div', d);
    const max = Math.max(...card.values, 1);
    card.labels.forEach((lb, k) => {
      const b = el('div', bars);
      b.className = 'bar';
      const em = document.createElement('em');
      em.textContent = String(card.values[k] || 0);
      b.appendChild(em);
      const bar = document.createElement('b');
      bar.dataset.h = String((card.values[k] || 0) / max);
      b.appendChild(bar);
      const sp = document.createElement('span');
      sp.textContent = lb;
      b.appendChild(sp);
    });
  }

  if (card.note) {
    const n = el('div', d);
    n.className = 'note';
    n.textContent = card.note;
  }

  nodes.push({ root: d, h1: h, rule, ul, lis: [...ul.children], bars, baseZ: -i * SEG, xo });
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"]/g, (ch) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[ch]));
}

// 场景道具：低多边形感的方块，散落在走廊里提供视差
function buildProps() {
  const list = [
    [-620, 300, -420, 96], [660, -280, -260, 64], [-760, -340, -980, 120],
    [720, 330, -1180, 150], [-300, 420, -1700, 90], [420, -420, -2000, 130],
    [-900, 120, -2400, 180], [880, -100, -2900, 210],
  ];
  list.forEach(([x, y, z, s], i) => {
    const p = el('div', world);
    p.className = 'prop';
    p.style.width = s + 'px';
    p.style.height = s + 'px';
    p.style.marginLeft = -s / 2 + 'px';
    p.style.marginTop = -s / 2 + 'px';
    p.style.transform = `translate3d(${x}px, ${y}px, ${z}px) rotate(${(hash(i * 13.7) * 40 - 20).toFixed(1)}deg)`;
  });
  // 两团色雾：薄荷绿 + 紫，跟相机一起走以制造纵深
  [['91,233,185', -540, -180, -1500, 520], ['167,139,250', 620, 220, -2300, 620]].forEach(([rgb, x, y, z, s]) => {
    const g = el('div', world);
    g.className = 'glow';
    g.style.width = s + 'px';
    g.style.height = s + 'px';
    g.style.marginLeft = -s / 2 + 'px';
    g.style.marginTop = -s / 2 + 'px';
    g.style.background = `radial-gradient(circle, rgba(${rgb},.30), rgba(${rgb},0) 70%)`;
    g.style.transform = `translate3d(${x}px, ${y}px, ${z}px)`;
  });
  const floor = el('div', world);
  floor.id = 'floor';
}

function drawBG(t, p) {
  const W = STAGE_W, H = STAGE_H;
  bctx.fillStyle = '#07080A';
  bctx.fillRect(0, 0, W, H);

  // 冷色顶光
  const g = bctx.createRadialGradient(W * 0.5, H * 0.34, 60, W * 0.5, H * 0.5, W * 0.72);
  g.addColorStop(0, 'rgba(38,52,66,.55)');
  g.addColorStop(0.55, 'rgba(14,18,24,.35)');
  g.addColorStop(1, 'rgba(4,5,7,0)');
  bctx.fillStyle = g;
  bctx.fillRect(0, 0, W, H);

  // 薄荷绿/紫的两团远景色雾，随相机进度缓慢漂移（视差）
  const blob = (cx, cy, r, rgb, a) => {
    const rg = bctx.createRadialGradient(cx, cy, 0, cx, cy, r);
    rg.addColorStop(0, `rgba(${rgb},${a})`);
    rg.addColorStop(1, `rgba(${rgb},0)`);
    bctx.fillStyle = rg;
    bctx.fillRect(cx - r, cy - r, r * 2, r * 2);
  };
  blob(W * 0.24 + Math.sin(p * 0.7) * 50, H * 0.30, 420, '91,233,185', 0.085);
  blob(W * 0.78 - Math.sin(p * 0.55) * 60, H * 0.66, 500, '167,139,250', 0.075);

  // 细扫描线：给暗部一点工业质感
  bctx.globalAlpha = 0.05;
  bctx.strokeStyle = '#ffffff';
  bctx.lineWidth = 1;
  const off = (p * 34) % 6;
  for (let y = off; y < H; y += 6) {
    bctx.beginPath();
    bctx.moveTo(0, y);
    bctx.lineTo(W, y);
    bctx.stroke();
  }
  bctx.globalAlpha = 1;

  // 压角
  const vg = bctx.createRadialGradient(W / 2, H / 2, H * 0.28, W / 2, H / 2, W * 0.72);
  vg.addColorStop(0, 'rgba(0,0,0,0)');
  vg.addColorStop(1, 'rgba(0,0,0,.78)');
  bctx.fillStyle = vg;
  bctx.fillRect(0, 0, W, H);
}

function drawFrame(t) {
  const per = DUR / Math.max(1, CARDS.length);
  const k = clamp(Math.floor(t / per), 0, CARDS.length - 1);
  const local = clamp((t - k * per) / per, 0, 1);

  // 相机进度：停留 HOLD 后飞向下一张，首尾都平滑
  const fly = easeInOut(clamp((local - HOLD) / (1 - HOLD), 0, 1));
  const p = clamp(k + fly, 0, CARDS.length - 1);

  // 相机横向对准当前关注的卡：卡片本身有左右交错，不做补偿就会偏出画面
  const i0 = clamp(Math.floor(p), 0, Math.max(0, nodes.length - 1));
  const i1 = Math.min(i0 + 1, Math.max(0, nodes.length - 1));
  const f = clamp(p - i0, 0, 1);
  const compX = lerp(nodes[i0] ? nodes[i0].xo || 0 : 0, nodes[i1] ? nodes[i1].xo || 0 : 0, f);

  const dx = Math.sin(t * 0.42) * 12 - compX;
  const dy = Math.cos(t * 0.33) * 9;
  const ry = Math.sin(t * 0.31) * 1.0 - 0.8;
  const rx = Math.cos(t * 0.24) * 0.7;

  world.style.transform = `translate3d(${dx.toFixed(2)}px, ${dy.toFixed(2)}px, ${(p * SEG).toFixed(2)}px) rotateX(${rx.toFixed(2)}deg) rotateY(${ry.toFixed(2)}deg)`;

  nodes.forEach((nd, i) => {
    const d = p - i;                       // 0 = 正对镜头
    let op = 1;
    if (d > 0.55) op = 1 - clamp((d - 0.55) / 0.45, 0, 1);        // 飞过镜头
    else if (d < -1.05) op = 1 - clamp((-d - 1.05) / 0.35, 0, 1); // 还在远处
    nd.root.style.opacity = op.toFixed(3);

    // 卡内节奏：接近时标题展开、要点逐条上浮
    const near = clamp((1.05 - Math.abs(d)) / 0.55, 0, 1);
    nd.h1.style.clipPath = `inset(0 ${(100 - near * 100).toFixed(1)}% 0 0)`;
    nd.rule.style.width = (near * 168).toFixed(1) + 'px';
    nd.lis.forEach((li, j) => {
      const a = clamp((near - j * 0.14) / 0.42, 0, 1);
      li.style.opacity = a.toFixed(3);
      li.style.transform = `translateY(${((1 - a) * 16).toFixed(1)}px)`;
    });
    if (nd.bars) {
      nd.bars.style.opacity = clamp((near - 0.25) / 0.5, 0, 1).toFixed(3);
      [...nd.bars.children].forEach((b, j) => {
        const bar = b.querySelector('b');
        const a = clamp((near - 0.3 - j * 0.09) / 0.45, 0, 1);
        bar.style.height = (Number(bar.dataset.h) * 92 * a).toFixed(1) + 'px';
      });
    }
  });

  drawBG(t, p);
}

async function init() {
  scaleStage();
  const r = await fetch('spec.json', { cache: 'no-store' });
  SPEC = await r.json();
  CARDS = (SPEC.cards && SPEC.cards.length) ? SPEC.cards : [{ heading: SPEC.title }];
  const per = SPEC.card_seconds > 0 ? SPEC.card_seconds : 6;
  DUR = SPEC.duration > 0 ? SPEC.duration : CARDS.length * per;

  buildProps();
  CARDS.forEach(buildCard);

  // 等字体就绪再开门，否则首帧字宽测不准
  if (document.fonts && document.fonts.ready) await document.fonts.ready;
  drawFrame(0);

  window.DUR = DUR;
  window.render = (t) => drawFrame(clamp(Number(t) || 0, 0, DUR));
  window.TEXTS = (t) => {
    const per2 = DUR / Math.max(1, CARDS.length);
    const i = clamp(Math.floor((Number(t) || 0) / per2), 0, CARDS.length - 1);
    const c = CARDS[i];
    return [c.heading, ...(c.points || [])].filter(Boolean);
  };
  window.READY = true;
}

window.addEventListener('resize', () => { scaleStage(); drawFrame(0); });
init().catch((e) => { console.error(e); window.READY = true; });
