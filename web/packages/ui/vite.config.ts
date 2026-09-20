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
      // The host owns Vue and the client; the code editor and the block editor
      // are dynamically imported so each lands in its own chunk in the host's
      // bundle. GrapesJS is listed by its exact id so that the stylesheet this
      // package inlines (`grapesjs/dist/css/…?inline`) is still bundled here.
      external: [
        'vue',
        'vue-i18n',
        '@sendplane/api',
        'grapesjs',
        'grapesjs-mjml',
        /^@codemirror\//,
        /^@lezer\//,
      ],
      output: { chunkFileNames: '[name]-[hash].js' },
    },
  },
})
