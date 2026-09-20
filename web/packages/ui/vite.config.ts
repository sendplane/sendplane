import { fileURLToPath } from 'node:url'

import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'

const apiSrc = fileURLToPath(new URL('../api/src/index.ts', import.meta.url))

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: { '@sendplane/api': apiSrc },
  },
  build: {
    target: 'es2022',
    sourcemap: true,
    lib: {
      entry: fileURLToPath(new URL('src/index.ts', import.meta.url)),
      formats: ['es'],
      fileName: () => 'sendplane-ui.js',
      cssFileName: 'sendplane-ui',
    },
    rollupOptions: {
      // The host owns Vue and the client; the code editor is dynamically
      // imported so it lands in its own chunk in the host's bundle.
      external: ['vue', 'vue-i18n', '@sendplane/api', /^@codemirror\//, /^@lezer\//],
      output: { chunkFileNames: '[name]-[hash].js' },
    },
  },
})
