// tsc does not copy hand-written/generated .d.ts files out of rootDir, so the
// generated OpenAPI types are placed next to the emitted declarations here.
import { copyFile, mkdir } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
await mkdir(resolve(root, 'dist'), { recursive: true })
await copyFile(resolve(root, 'src/schema.d.ts'), resolve(root, 'dist/schema.d.ts'))
