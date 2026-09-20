import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import { contentCacheBust } from './cacheBust.js'

const cacheBust = encodeURIComponent(contentCacheBust(import.meta.dirname))

function cacheBustPlugin() {
  return {
    name: 'replicaro-cache-bust',
    transformIndexHtml: {
      order: 'post' as const,
      handler(html: string) {
        return html.replace(
          /((?:src|href)="\/(?:assets\/[^"]+|favicon(?:-teal-64\.png|\.ico)|icons\.svg))"/g,
          `$1?cb=${cacheBust}"`,
        )
      },
    },
    renderChunk(code: string) {
      return code.replace(
        /(["'])\/(replicaro-wordmark-(?:white|teal)\.png)(?:\?cb=[^"']*)?\1/g,
        `$1/$2?cb=${cacheBust}$1`,
      )
    },
  }
}

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), cacheBustPlugin(), {
	name: 'replicaro-development-installation',
	apply: 'serve',
	transformIndexHtml: async (html) => {
		// Development receives the same UUID as the packaged shell. No extra
		// unauthenticated API endpoint or independently configured identity.
		const response = await fetch('http://127.0.0.1:9460/', { signal: AbortSignal.timeout(5000), redirect: 'error' });
		if (!response.ok) throw new Error('Replicaro backend is unavailable');
		const shell = await response.text();
		const metadata = shell.match(/<meta name="replicaro-client-uuid" content="[0-9a-f-]{36}">/g);
		if (metadata?.length !== 1) throw new Error('Replicaro installation UUID is unavailable');
		return html.replace('</head>', metadata[0] + '</head>');
	},
  }],
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    testTimeout: 30_000,
    hookTimeout: 30_000,
    restoreMocks: true,
    clearMocks: true,
    exclude: ['e2e/**', 'node_modules/**', 'dist/**'],
	coverage: {
		thresholds: { statements: 50, branches: 33, functions: 38, lines: 53 },
	},
  },
})
