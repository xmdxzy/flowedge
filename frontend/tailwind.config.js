/** @type {import('tailwindcss').Config} */
// 颜色一律走 CSS 变量（定义在 src/index.css 的「主题令牌 · 唯一色源」）。
// 这样组件里写 bg-canvas / border-border / text-primary 就会自动跟随深浅主题，
// 透明度简写（bg-surface/60）也照常可用，不需要再写 [data-theme] 补丁。
const token = (name) => `rgb(var(${name}) / <alpha-value>)`;

export default {
  content: [
    "./index.html",
    "./src/**/*.{js,ts,jsx,tsx}",
  ],
  theme: {
    extend: {
      colors: {
        // 结构与表面
        canvas: token('--fe-rgb-bg'),
        surface: token('--fe-rgb-panel'),
        'surface-hover': token('--fe-rgb-hover'),
        border: token('--fe-rgb-line'),
        // 主色与强调色
        primary: token('--fe-rgb-brand'),
        'primary-glow': token('--fe-rgb-brand'),
        accent: token('--fe-rgb-cyan'),
        // 节点类型色
        ai: token('--fe-rgb-violet'),
        control: token('--fe-rgb-brand'),
        data: token('--fe-rgb-cyan'),
        output: token('--fe-rgb-ok'),
        purple: token('--fe-rgb-violet'),
        // 状态色（浅色主题下会自动压暗，保证对比度）
        error: token('--fe-rgb-danger'),
        warning: token('--fe-rgb-warn'),
        success: token('--fe-rgb-ok'),
      },
      fontFamily: {
        sans: ['Inter', 'system-ui', 'Microsoft YaHei UI', 'sans-serif'],
        mono: ['JetBrains Mono', 'Consolas', 'monospace'],
      },
      boxShadow: {
        node: '0 8px 24px rgb(var(--fe-shadow) / 0.18), inset 0 1px 0 rgb(var(--fe-sheen) / 0.05)',
        'node-hover': '0 12px 32px rgb(var(--fe-shadow) / 0.26), inset 0 1px 0 rgb(var(--fe-sheen) / 0.07)',
        'btn-primary': '0 4px 14px rgb(var(--fe-rgb-brand) / 0.32)',
        'glow-brand': '0 0 0 1px rgb(var(--fe-rgb-brand) / 0.4), 0 6px 22px rgb(var(--fe-rgb-brand) / 0.22)',
      },
    },
  },
  plugins: [],
}
