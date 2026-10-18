/** @type {import('tailwindcss').Config} */
export default {
  content: [
    "./index.html",
    "./src/**/*.{js,ts,jsx,tsx}",
  ],
  theme: {
    extend: {
      colors: {
        // 深海军蓝分层
        canvas: '#0A0D14',
        surface: '#10141F',
        'surface-hover': '#1A2233',
        border: '#232B3D',
        // 主色：靛蓝 → 青色渐变体系
        primary: '#6366F1',
        'primary-glow': 'rgba(99, 102, 241, 0.35)',
        accent: '#22D3EE',
        // 节点类型色
        ai: '#A78BFA',
        control: '#6366F1',
        data: '#22D3EE',
        output: '#34D399',
        error: '#F87171',
        warning: '#FBBF24',
        success: '#34D399',
        purple: '#C084FC',
      },
      fontFamily: {
        sans: ['Inter', 'system-ui', 'Microsoft YaHei UI', 'sans-serif'],
        mono: ['JetBrains Mono', 'Consolas', 'monospace'],
      },
      boxShadow: {
        'node': '0 8px 24px rgba(0,0,0,0.35), inset 0 1px 0 rgba(255,255,255,0.04)',
        'node-hover': '0 12px 32px rgba(0,0,0,0.45), inset 0 1px 0 rgba(255,255,255,0.06)',
        'btn-primary': '0 4px 14px rgba(99,102,241,0.4)',
      },
    },
  },
  plugins: [],
}
