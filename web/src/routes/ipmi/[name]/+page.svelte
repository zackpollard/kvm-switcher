<script lang="ts">
	import { page } from '$app/state';

	const name = $derived(page.params.name);
	let error = $state('');

	$effect(() => {
		if (!('serviceWorker' in navigator)) {
			error = 'Service Workers are not supported in this browser.';
			return;
		}

		if (navigator.serviceWorker.controller) {
			// SW is active but we still got SvelteKit — BMC proxy failed.
			error = 'Failed to load BMC interface. The BMC may be unreachable.';
			return;
		}

		// SW not yet controlling — wait for it to take control, then reload.
		navigator.serviceWorker.addEventListener('controllerchange', () => {
			window.location.reload();
		});
	});
</script>

<div class="flex min-h-screen items-center justify-center bg-gray-950">
	<div class="text-center">
		{#if error}
			<p class="text-red-400">{error}</p>
			<a
				href="/"
				class="mt-4 inline-block rounded-md bg-gray-800 px-4 py-2 text-sm text-gray-300 hover:bg-gray-700 hover:text-white"
			>
				Back to servers
			</a>
		{:else}
			<div class="mb-4 h-8 w-8 mx-auto animate-spin rounded-full border-2 border-gray-600 border-t-blue-400"></div>
			<p class="text-gray-400">Loading IPMI interface for {name}...</p>
		{/if}
	</div>
</div>
