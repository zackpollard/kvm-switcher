<script lang="ts">
	import { fetchServers, createSession, type ServerInfo, type KVMSession } from '$lib/api';
	import { goto } from '$app/navigation';

	let servers: ServerInfo[] = $state([]);
	let loading = $state(true);
	let error = $state('');
	let connecting = $state<string | null>(null);

	async function loadServers() {
		try {
			loading = true;
			error = '';
			const res = await fetch('/api/servers');
			if (res.status === 401) {
				window.location.href = '/auth/login';
				return;
			}
			if (!res.ok) throw new Error(`Failed to fetch servers: ${res.statusText}`);
			servers = await res.json();
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to load servers';
		} finally {
			loading = false;
		}
	}

	async function connect(serverName: string) {
		try {
			connecting = serverName;
			error = '';
			const session = await createSession(serverName);
			goto(`/kvm/${session.id}`);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to connect';
			connecting = null;
		}
	}

	$effect(() => {
		loadServers();
		const interval = setInterval(loadServers, 10000);
		return () => clearInterval(interval);
	});
</script>

<div class="mx-auto max-w-7xl px-4 py-8 sm:px-6 lg:px-8">
	<div class="mb-8 flex items-center justify-between">
		<div>
			<h1 class="text-2xl font-bold text-white">Servers</h1>
			<p class="mt-1 text-sm text-gray-400">Select a server to open a KVM console session.</p>
		</div>
		<button
			onclick={loadServers}
			class="rounded-md bg-gray-800 px-3 py-2 text-sm text-gray-300 hover:bg-gray-700 hover:text-white"
		>
			Refresh
		</button>
	</div>

	{#if error}
		<div class="mb-6 rounded-md border border-red-800 bg-red-900/50 px-4 py-3 text-sm text-red-200">
			{error}
		</div>
	{/if}

	{#if loading && servers.length === 0}
		<div class="flex items-center justify-center py-20">
			<div class="h-8 w-8 animate-spin rounded-full border-2 border-gray-600 border-t-blue-400"></div>
			<span class="ml-3 text-gray-400">Loading servers...</span>
		</div>
	{:else if servers.length === 0}
		<div class="rounded-lg border border-gray-800 bg-gray-900 py-16 text-center">
			<p class="text-gray-400">No servers configured.</p>
			<p class="mt-1 text-sm text-gray-500">Add servers to configs/servers.yaml</p>
		</div>
	{:else}
		<div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
			{#each servers as server}
				<div class="rounded-lg border border-gray-800 bg-gray-900 p-5 transition-colors hover:border-gray-700">
					<div class="mb-4 flex items-start justify-between">
						<div>
							<h2 class="text-lg font-semibold text-white">{server.name}</h2>
							<p class="mt-0.5 font-mono text-sm text-gray-400">{server.bmc_ip}:{server.bmc_port}</p>
						</div>
						<span class="rounded-full bg-gray-800 px-2.5 py-0.5 text-xs text-gray-400">
							{server.board_type}
						</span>
					</div>

					<div class="flex items-center justify-between">
						{#if server.has_active_session}
							<span class="flex items-center gap-1.5 text-sm text-green-400">
								<span class="h-2 w-2 rounded-full bg-green-400"></span>
								Active session
							</span>
						{:else}
							<span class="flex items-center gap-1.5 text-sm text-gray-500">
								<span class="h-2 w-2 rounded-full bg-gray-600"></span>
								No session
							</span>
						{/if}

						<div class="flex items-center gap-2">
							<a
								href="/ipmi/{server.name}/"
								target="_blank"
								rel="noopener noreferrer"
								class="rounded-md bg-gray-700 px-3 py-2 text-sm font-medium text-gray-200 hover:bg-gray-600 hover:text-white"
							>
								IPMI
							</a>
							<button
								onclick={() => connect(server.name)}
								disabled={connecting === server.name}
								class="rounded-md bg-blue-600 px-4 py-2 text-sm font-medium text-white hover:bg-blue-500 disabled:cursor-not-allowed disabled:opacity-50"
							>
								{#if connecting === server.name}
									Connecting...
								{:else}
									KVM
								{/if}
							</button>
						</div>
					</div>
				</div>
			{/each}
		</div>
	{/if}
</div>
