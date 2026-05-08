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
    port: 3000,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
      '/events': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
    watch: {
      ignored: ['**/columns/**', '**/node_modules/**', '**/.git/**'],
    },
  },
});
