/// <reference types="vite/client" />

interface ImportMetaEnv {
  /**
   * Origin the sendplane handler is mounted on. Empty means same-origin, which
   * is the case when the Go binary serves this build itself.
   */
  readonly VITE_SENDPLANE_API?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
