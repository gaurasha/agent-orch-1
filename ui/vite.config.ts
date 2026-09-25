import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The dev server proxies /v1 to the control plane so the UI runs against a
// real backend with no CORS juggling. The production build is static and is
// served by the control plane itself (see serve.go uiHandler).
export default defineConfig({
  plugins: [react()],
  build: { outDir: 'dist', sourcemap: true },
  server: {
    port: 5173,
    proxy: {
      '/v1': { target: process.env.AGENTORCH_API ?? 'http://localhost:8080', changeOrigin: true },
    },
  },
})
