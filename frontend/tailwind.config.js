/** @type {import('tailwindcss').Config} */
export default {
  content: [
    "./index.html",
    "./src/**/*.{js,ts,jsx,tsx}",
  ],
  theme: {
    extend: {
      colors: {
        canvas: '#0F1115',
        surface: '#1A1D24',
        'surface-hover': '#242832',
        border: '#2E3340',
        primary: '#3B82F6',
        'primary-glow': 'rgba(59, 130, 246, 0.3)',
        ai: '#8B5CF6',
        control: '#3B82F6',
        data: '#06B6D4',
        output: '#10B981',
        error: '#EF4444',
        warning: '#F59E0B',
        success: '#10B981',
      },
      fontFamily: {
        sans: ['Inter', 'system-ui', 'sans-serif'],
        mono: ['JetBrains Mono', 'monospace'],
      },
    },
  },
  plugins: [],
}
