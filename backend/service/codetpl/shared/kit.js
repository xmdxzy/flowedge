// FlowEdge · code_video shared kit
// 所有风格模板共用的确定性数学 / 排版 / 纹理工具。
// 铁律：任何绘制只依赖 t，禁止 Math.random() / Date.now()（否则逐帧渲染不可复现）。

export const clamp = (v, a, b) => (v < a ? a : v > b ? b : v);
export const lerp = (a, b, t) => a + (b - a) * t;
export const prog = (t, a, b) => clamp((t - a) / (b - a || 1e-6), 0, 1);
export const easeInOut = (x) => (x < 0.5 ? 2 * x * x : 1 - Math.pow(-2 * x + 2, 2) / 2);
export const easeOut = (x) => 1 - Math.pow(1 - x, 3);
export const easeOutQuint = (x) => 1 - Math.pow(1 - x, 5);

// 确定性伪随机：同一个 seed 永远同一个值
export function hash(n) {
  const x = Math.sin(n * 127.1 + 311.7) * 43758.5453;
  return x - Math.floor(x);
}
export function jit(seed, amp) {
  return (hash(seed) - 0.5) * amp;
}
export function rng(seed) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

// ---------- 纹理 ----------
// 颗粒/纸张噪点：只生成一次，之后当位图贴
export function makeGrain(W, H, alpha = 18, seed = 7) {
  const cv = document.createElement('canvas');
  cv.width = W;
  cv.height = H;
  const c = cv.getContext('2d');
  const img = c.createImageData(W, H);
  const r = rng(seed);
  for (let i = 0; i < img.data.length; i += 4) {
    const v = 128 + (r() - 0.5) * 255;
    img.data[i] = img.data[i + 1] = img.data[i + 2] = v;
    img.data[i + 3] = alpha;
  }
  c.putImageData(img, 0, 0);
  return cv;
}

export function drawVignette(c, W, H, strength = 0.55, color = '0,0,0') {
  const g = c.createRadialGradient(W / 2, H * 0.46, Math.min(W, H) * 0.18, W / 2, H * 0.5, Math.max(W, H) * 0.78);
  g.addColorStop(0, `rgba(${color},0)`);
  g.addColorStop(1, `rgba(${color},${strength})`);
  c.fillStyle = g;
  c.fillRect(0, 0, W, H);
}

// ---------- 几何 ----------
export function roundRect(c, x, y, w, h, r) {
  const rr = Math.min(r, Math.abs(w) / 2, Math.abs(h) / 2);
  c.beginPath();
  c.moveTo(x + rr, y);
  c.arcTo(x + w, y, x + w, y + h, rr);
  c.arcTo(x + w, y + h, x, y + h, rr);
  c.arcTo(x, y + h, x, y, rr);
  c.arcTo(x, y, x + w, y, rr);
  c.closePath();
}

// ---------- 排版 ----------
export const SANS = '"Microsoft YaHei", "PingFang SC", "Noto Sans SC", "Source Han Sans SC", sans-serif';
export const SERIF = '"Noto Serif SC", "Source Han Serif SC", "STSong", "SimSun", serif';
export const MONO = '"Cascadia Mono", Consolas, "Courier New", monospace';

export function setFont(c, size, weight = 400, family = SANS) {
  c.font = `${weight} ${size}px ${family}`;
  return c;
}

export function textW(c, s) {
  return c.measureText(s).width;
}

// 按可用宽度自动缩字号（中文按字切，英文按词切）
export function wrapText(c, text, maxW) {
  const out = [];
  let cur = '';
  for (const ch of String(text || '')) {
    const next = cur + ch;
    if (c.measureText(next).width > maxW && cur) {
      out.push(cur);
      cur = ch;
    } else cur = next;
  }
  if (cur) out.push(cur);
  return out;
}

// 均衡断行：不让末行只剩一两个字（「…差这么多」被甩到第二行那种）。
// 在 [0.62,1.0]×maxW 里挑「行数不超限 + 末行最满」的宽度。
export function wrapBalanced(c, text, maxW, maxLines = 2) {
  const s = String(text || '');
  if (!s) return [];
  const plain = wrapText(c, s, maxW);
  if (plain.length <= 1) return plain;

  let best = plain;
  let bestScore = Infinity;
  for (let ratio = 1.0; ratio >= 0.6; ratio -= 0.04) {
    const w = maxW * ratio;
    const lines = wrapText(c, s, w);
    if (lines.length > maxLines) continue;
    const lastFullness = c.measureText(lines[lines.length - 1]).width / w;
    const score = lines.length * 10 + (1 - lastFullness) * 4 + (1 - ratio) * 0.8;
    if (score < bestScore) {
      bestScore = score;
      best = lines;
    }
  }
  return best;
}

// 中文按标点断行：优先在「，。、；：」后断，避免把「为什么」拆成「为什/么」。
// 单个分句超宽时退回逐字断，保证不溢出。
export function wrapByPunct(c, text, maxW, maxLines = 2) {
  const s = String(text || '');
  if (!s) return [];
  const chunks = s.match(/[^，。、；：！？,.;:!?]+[，。、；：！？,.;:!?]?/g) || [s];

  const lines = [];
  let cur = '';
  for (const ch of chunks) {
    const next = cur + ch;
    if (cur && c.measureText(next).width > maxW) {
      lines.push(cur);
      cur = ch;
    } else cur = next;
  }
  if (cur) lines.push(cur);

  // 分句自己就超宽 → 那段逐字断
  const fixed = [];
  for (const ln of lines) {
    if (c.measureText(ln).width > maxW) fixed.push(...wrapText(c, ln, maxW));
    else fixed.push(ln);
  }
  if (fixed.length <= maxLines) return fixed;
  return wrapBalanced(c, s, maxW, maxLines);
}

// 从 baseSize 往下降，直到整段能塞进 maxW 或到 minSize
export function fitText(c, text, maxW, baseSize, weight = 700, family = SANS, minSize = 24) {  let size = baseSize;
  while (size > minSize && c.measureText(String(text || '')).width * (size / baseSize) > maxW) size -= 2;
  setFont(c, size, weight, family);
  return size;
}

// 逐字排（字间距），align: left | center | right
export function letterSpaced(c, text, x, y, spacing, align = 'left') {  const chars = [...String(text || '')];
  const widths = chars.map((ch) => c.measureText(ch).width + spacing);
  const total = widths.reduce((a, b) => a + b, 0) - spacing;
  let cx = align === 'center' ? x - total / 2 : align === 'right' ? x - total : x;
  c.textAlign = 'left';
  chars.forEach((ch, i) => {
    c.fillText(ch, cx, y);
    cx += widths[i];
  });
  return total;
}

// 描点引线：从 (ax,ay) 折一次到 (bx,by)，DK 图鉴的典型标注走线
export function leader(c, ax, ay, bx, by, elbow = 0.55) {
  const mx = lerp(ax, bx, elbow);
  c.beginPath();
  c.moveTo(ax, ay);
  c.lineTo(mx, ay);
  c.lineTo(bx, by);
  c.stroke();
}

export async function loadSpec() {
  const r = await fetch('spec.json', { cache: 'no-store' });
  return r.json();
}

// 卡片时间轴：把总时长切成 n 张卡，返回 {i, local, n}
export function cardAt(t, dur, n) {
  const per = dur / Math.max(1, n);
  const i = clamp(Math.floor(t / per), 0, n - 1);
  return { i, local: clamp((t - i * per) / per, 0, 1), per, n };
}
