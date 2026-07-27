import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 构建配置要点：
// - base 固定为 /admin/ui/：产物被 Go 嵌入并挂载在该路径下，资源引用须带前缀；
// - outDir 指向 internal/webui/dist：go:embed 直接嵌入该目录，无需拷贝步骤；
// - 单文件产物（不做 code split）：控制台体量小，减少请求数与嵌入文件数；
// - dev 模式代理 /admin 与 /v1 到本地网关，便于热更开发。
export default defineConfig({
  plugins: [react()],
  base: '/admin/ui/',
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
    rollupOptions: {
      output: {
        entryFileNames: 'assets/app.js',
        chunkFileNames: 'assets/[name].js',
        assetFileNames: 'assets/[name][extname]',
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      '/admin': 'http://127.0.0.1:8080',
      '/v1': 'http://127.0.0.1:8080',
      '/metrics': 'http://127.0.0.1:8080',
    },
  },
})
