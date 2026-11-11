// FlowEdge · code_video design tokens
//
// 三层结构的第 1 层（第 2 层是各风格里的卡型 blocks，第 3 层是风格目录 worlds）。
// 为什么要单独一层：调研头部科普号（抖音「基地」）得到的结论是——
//   「好看」不是审美，是**约束**：三色纪律、字号阶、间距阶全部写死，
//   15 分钟片子不出现第四种强调色，靠的就是这份 token 表。
// 所以新风格要从这里取色，不要在 render.js 里再手写 #RRGGBB。
//
// 改色只改这里；改完必须重渲样片核对（颜色是确定性输出，diff 一眼能看出来）。

export const TOKENS = {
  // 暗色 3D 卡片世界：黑底 + 三色纪律（白 / 薄荷绿 / 紫）
  darkcard: {
    bg: '#07080A',
    ink: '#F2F5F7',
    dim: 'rgba(242,245,247,.60)',
    faint: 'rgba(242,245,247,.34)',
    accent: '#5BE9B9', // 薄荷绿：主强调、进度、标签
    accent2: '#A78BFA', // 紫：次级强调
    radius: 28,
    type: { display: 76, heading: 68, body: 29, mono: 17 },
  },

  // DK 图鉴 / 剖面图解：博物馆羊皮纸米色 + 海军蓝 + 赭红
  atlas: {
    paper: '#F0EDE6',
    ink: '#1F2429',
    dim: 'rgba(31,36,41,.62)',
    faint: 'rgba(31,36,41,.26)',
    accent: '#1F3A5F', // 海军蓝：结构线与主标注
    accent2: '#A8452E', // 赭红：次级标注、数据
    sand: '#DCD5C6',
    type: { display: 80, heading: 72, body: 30, mono: 19 },
  },

  // 黑画布 MG 信息图：纯黑 + 单一电光蓝
  studio: {
    bg: '#050506',
    ink: '#F5F7FA',
    dim: 'rgba(245,247,250,.55)',
    faint: 'rgba(245,247,250,.22)',
    accent: '#4D9FFF',
    accent2: '#FF7A59',
    type: { display: 104, heading: 94, body: 34, mono: 19 },
  },

  // 工程蓝图（首版）：深海军蓝 + 冷青
  blueprint: {
    paper: '#071326',
    paper2: '#0d1f3d',
    ink: 'rgba(226,238,252,0.92)',
    dim: 'rgba(178,200,228,0.62)',
    faint: 'rgba(150,178,212,0.34)',
    accent: 'rgba(120,214,255,0.95)',
    type: { body: 22, mono: 17 },
  },
};

// 把 token 写进 CSS 变量，供 DOM 型风格（darkcard）在 CSS 里引用。
// CSS 里保留同名兜底值：JS 没跑起来也不会变成无色。
export function applyCssTokens(root, t) {
  const set = (k, v) => root.style.setProperty(k, v);
  if (t.bg) set('--bg', t.bg);
  if (t.ink) set('--ink', t.ink);
  if (t.dim) set('--dim', t.dim);
  if (t.faint) set('--faint', t.faint);
  if (t.accent) set('--mint', t.accent);
  if (t.accent2) set('--purple', t.accent2);
}
