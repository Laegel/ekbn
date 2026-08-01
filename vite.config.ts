import { defineConfig } from 'vite';
import tailwindcss from '@tailwindcss/vite';

export default defineConfig({
  root: '.',
  build: {
    outDir: 'dist',
    assetsDir: '.',
  },
  plugins: [
    tailwindcss(),
  ],
  server: {
    port: 3003,
    proxy: {
      '/api': {
        target: 'http://localhost:8091',
        changeOrigin: true,
        ws: true,
      },
    },
    watch: {
      ignored: ['**/columns/**', '**/node_modules/**', '**/.git/**'],
    },
  },
});
