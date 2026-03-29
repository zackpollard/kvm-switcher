<script lang="ts">
	import '../app.css';
	import { fetchAuthStatus, type AuthStatus } from '$lib/api';
	import { Button, ThemeSwitcher, initializeTheme } from '@immich/ui';

	let { children } = $props();
	let auth: AuthStatus | null = $state(null);

	$effect(() => {
		initializeTheme();
	});

	$effect(() => {
		fetchAuthStatus().then((s) => (auth = s)).catch(() => (auth = null));
	});

	$effect(() => {
		if ('serviceWorker' in navigator) {
			navigator.serviceWorker
				.register('/sw.js', { scope: '/', updateViaCache: 'none' })
				.then((reg) => {
					reg.update();
				});

			// Reload when a new SW takes control (e.g., after server rebuild)
			let reloading = false;
			navigator.serviceWorker.addEventListener('controllerchange', () => {
				if (!reloading) {
					reloading = true;
					window.location.reload();
				}
			});
		}
	});
</script>

<div class="min-h-screen bg-light text-dark">
	<nav class="border-b border-light-300 bg-light-50">
		<div class="mx-auto max-w-7xl px-4 sm:px-6 lg:px-8">
			<div class="flex h-14 items-center justify-between">
				<a href="/" class="flex items-center gap-2 text-lg font-semibold text-dark">
					<svg class="h-6 w-6 text-primary" fill="none" viewBox="0 0 24 24" stroke="currentColor" aria-hidden="true">
						<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9.75 17L9 20l-1 1h8l-1-1-.75-3M3 13h18M5 17h14a2 2 0 002-2V5a2 2 0 00-2-2H5a2 2 0 00-2 2v10a2 2 0 002 2z" />
					</svg>
					KVM Switcher
				</a>

				<div class="flex items-center gap-3">
					<ThemeSwitcher size="small" />

					{#if auth?.authenticated}
						<span class="text-sm text-muted">
							{auth.name || auth.email}
						</span>
						<Button
							href="/auth/logout"
							size="small"
							variant="ghost"
							color="secondary"
						>
							Logout
						</Button>
					{/if}
				</div>
			</div>
		</div>
	</nav>

	<main>
		{@render children()}
	</main>
</div>
