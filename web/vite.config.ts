import { defineConfig, loadEnv } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig(({ mode }) => {
 const env = loadEnv(mode, process.cwd(), '');
 const target = env.DEV_API_TARGET || 'http://127.0.0.1:8080';
 return {
  plugins: [react()],
  server: {
   port: 5173, strictPort: true,
   proxy: Object.fromEntries(['/api', '/health', '/ready', '/media'].map(path => [path, { target, changeOrigin: false }]))
  }
 };
});
