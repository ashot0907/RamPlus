import { writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import type { Plugin } from 'vite'
import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

const outDir = fileURLToPath(new URL('../internal/webui/dist', import.meta.url))

// .keep — чтобы go:embed в internal/webui собирался и без фронтенда
// (vite очищает каталог сборки).
const keepFile = (): Plugin => ({
  name: 'ramplus-keep',
  closeBundle() {
    writeFileSync(resolve(outDir, '.keep'), '')
  },
})

// Сервер ramplus-web отдаёт строгий CSP: никаких inline-скриптов, data:
// и внешних источников — шрифты и стили только файлами из бандла.
export default defineConfig({
  plugins: [react(), keepFile()],
  build: {
    outDir,
    emptyOutDir: true,
    assetsInlineLimit: 0,
    modulePreload: { polyfill: false },
    sourcemap: false,
    target: 'es2022',
  },
  server: {
    // npm run dev: API — через запущенный ramplus-web.
    proxy: { '/api': 'http://127.0.0.1:7781' },
  },
  test: {
    environment: 'node',
    include: ['src/**/*.test.{ts,tsx}'],
  },
})
