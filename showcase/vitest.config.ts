import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import { fileURLToPath, URL } from 'node:url'

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@slacksim': fileURLToPath(new URL('./demos/slacksim/src', import.meta.url)),
    },
  },
  test: {
    globals: true,
    environment: 'jsdom',
    setupFiles: ['./vitest.setup.ts'],
    include: ['demos/**/*.test.{ts,tsx}', 'engine/**/*.test.{ts,tsx}', 'engine/**/*.test.mjs'],
  },
})
