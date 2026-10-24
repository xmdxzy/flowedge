import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 3000,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
        // 实时看板走 /api/ws：dev 下不转发 WS 就会一直显示"实时已断开"。
        // 打包后是同源部署，与这项无关。
        ws: true,
      },
    },
  },
})
