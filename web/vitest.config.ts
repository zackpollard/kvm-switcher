import { svelte } from '@sveltejs/vite-plugin-svelte';
import { defineConfig } from 'vitest/config';

export default defineConfig({
	plugins: [svelte({ hot: false })],
	test: {
		include: ['src/**/*.test.{js,ts}'],
		environment: 'jsdom',
		setupFiles: ['src/tests/setup.ts'],
		globals: true,
		alias: {
			'$lib': '/src/lib',
			'$app/state': '/src/tests/mocks/app-state.ts',
			'$app/navigation': '/src/tests/mocks/app-navigation.ts',
			'$env/dynamic/public': '/src/tests/mocks/env-dynamic-public.ts',
			'$app/environment': '/src/tests/mocks/app-environment.ts'
		}
	},
	resolve: {
		conditions: ['browser']
	}
});
