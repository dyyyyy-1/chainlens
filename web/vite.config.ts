import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// 开发期把 /api 代理到本机的查询服务，避免前端处理跨域。
// 生产构建由 cmd/api 直接托管 web/dist，同源，不需要代理。
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: true,
  },
})
