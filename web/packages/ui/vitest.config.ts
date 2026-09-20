import { fileURLToPath } from 'node:url'

import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: { '@sendplane/api': fileURLToPath(new URL('../api/src/index.ts', import.meta.url)) },
  },
  test: {
    environment: 'happy-dom',
    include: ['test/**/*.spec.ts'],
  },
})
