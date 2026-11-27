import { svelte } from '@sveltejs/vite-plugin-svelte'
import { defineConfig } from 'vite'

// The build goes into the Go module, which embeds it in the couchlog binary.
// `npm run dev` proxies the API to a running `couchlog server`.
export default defineConfig({
  plugins: [svelte()],
  build: { outDir: '../internal/web/dist', emptyOutDir: true },
  server: { proxy: { '/api': 'http://127.0.0.1:7791' } },
})
