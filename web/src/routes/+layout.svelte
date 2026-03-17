<script lang="ts">
	import '../app.css';
	import { fetchAuthStatus, type AuthStatus } from '$lib/api';

	let { children } = $props();
	let auth: AuthStatus | null = $state(null);

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

<div class="min-h-screen bg-gray-950 text-gray-100">
	<nav class="border-b border-gray-800 bg-gray-900">
		<div class="mx-auto max-w-7xl px-4 sm:px-6 lg:px-8">
			<div class="flex h-14 items-center justify-between">
				<a href="/" class="flex items-center gap-2 text-lg font-semibold text-white">
					<svg class="h-6 w-6 text-blue-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
						<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9.75 17L9 20l-1 1h8l-1-1-.75-3M3 13h18M5 17h14a2 2 0 002-2V5a2 2 0 00-2-2H5a2 2 0 00-2 2v10a2 2 0 002 2z" />
					</svg>
					KVM Switcher
				</a>

				{#if auth?.authenticated}
					<div class="flex items-center gap-3">
						<span class="text-sm text-gray-400">
							{auth.name || auth.email}
						</span>
						<a
							href="/auth/logout"
							class="rounded-md bg-gray-800 px-3 py-1.5 text-sm text-gray-300 hover:bg-gray-700 hover:text-white"
						>
							Logout
						</a>
					</div>
				{/if}
			</div>
		</div>
	</nav>

	<main>
		{@render children()}
	</main>
</div>
