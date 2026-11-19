import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'

const backend = 'http://localhost:8080'

export default defineConfig({
  plugins: [vue()],
  server: {
    proxy: {
      '/api': backend,
      '/auth': backend,
      '/setup': backend,
      '/webhooks': backend,
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
  },
})
