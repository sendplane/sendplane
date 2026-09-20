import { fileURLToPath } from 'node:url'

import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'

export default defineConfig(({ mode }) => ({
  plugins: [vue()],
  // The Go binary serves the build from the root of its console mount; set
  // VITE_BASE when mounting it somewhere else.
  base: process.env.VITE_BASE ?? '/',
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    target: 'es2022',
    sourcemap: mode !== 'production',
  },
  server: {
    port: 5173,
    proxy: process.env.VITE_SENDPLANE_API
      ? undefined
      : // Without an explicit API origin the dev server forwards to a local
        // sendplane binary, which keeps the console same-origin in development.
        {
          '/api': { target: 'http://localhost:8080', changeOrigin: true },
          '/t': { target: 'http://localhost:8080', changeOrigin: true },
        },
  },
}))
