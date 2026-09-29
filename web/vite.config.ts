import { defineConfig } from 'vite';

// Dev-сервер Vite проксирует /room/* на Go-сервер, включая WebSocket upgrade.
// Это избавляет от CORS/Origin-проблем и делает поведение dev == prod (один origin).
export default defineConfig({
  server: {
    port: 5173,
    proxy: {
      '/room': {
        target: 'http://localhost:8080',
        ws: true,
        changeOrigin: true,
      },
      '/health': 'http://localhost:8080',
      '/rooms': 'http://localhost:8080',
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
});
