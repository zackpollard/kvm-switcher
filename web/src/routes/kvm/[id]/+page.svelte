<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { getSession, deleteSession, createSession, getKVMWebSocketURL, type KVMSession } from '$lib/api';
	import KVMViewer from '$lib/components/KVMViewer.svelte';
	import SessionTimeoutWarning from '$lib/components/SessionTimeoutWarning.svelte';

	let session: KVMSession | null = $state(null);
	let activeSessionId = $state(page.params.id);
	let error = $state('');
	let isFullscreen = $state(false);
	let viewerContainer = $state<HTMLDivElement>(undefined!);
	let reconnecting = $state(false);
	let manualDisconnect = false;

	async function loadSession() {
		try {
			session = await getSession(activeSessionId);
			if (session.status === 'error') {
				error = session.error || 'Session encountered an error';
				reconnecting = false;
			} else if (session.status === 'connected') {
				reconnecting = false;
			}
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to load session';
			reconnecting = false;
		}
	}

	async function disconnect() {
		manualDisconnect = true;
		try {
			await deleteSession(activeSessionId);
			goto('/');
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to disconnect';
		}
	}

	async function handleViewerDisconnect() {
		if (manualDisconnect || reconnecting) return;

		reconnecting = true;
		error = '';
		const serverName = session?.server_name;
		if (!serverName) {
			error = 'Cannot reconnect: unknown server';
			reconnecting = false;
			return;
		}

		// Clean up old session
		try {
			await deleteSession(activeSessionId);
		} catch {
			// Old session may already be gone
		}

		// Create a new session for the same server.
		// Keep reconnecting=true until the new session is connected —
		// it gets cleared in loadSession when status changes from 'starting'.
		try {
			const newSession = await createSession(serverName);
			session = newSession;
			activeSessionId = newSession.id;
			// Update URL without re-triggering effects
			history.replaceState({}, '', `/kvm/${newSession.id}`);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Reconnection failed';
			reconnecting = false;
		}
	}

	function sendCtrlAltDel() {
		viewerContainer?.dispatchEvent(new CustomEvent('send-ctrl-alt-del'));
	}

	function toggleFullscreen() {
		if (!document.fullscreenElement) {
			viewerContainer?.requestFullscreen();
			isFullscreen = true;
		} else {
			document.exitFullscreen();
			isFullscreen = false;
		}
	}

	$effect(() => {
		// Read activeSessionId synchronously so the effect re-runs when it changes
		const _id = activeSessionId;
		loadSession();
		const interval = setInterval(async () => {
			if (session?.status === 'starting' || session?.status === 'connected') {
				await loadSession();
			}
		}, session?.status === 'starting' ? 2000 : 30000);
		return () => clearInterval(interval);
	});

	$effect(() => {
		const handler = () => {
			isFullscreen = !!document.fullscreenElement;
		};
		document.addEventListener('fullscreenchange', handler);
		return () => document.removeEventListener('fullscreenchange', handler);
	});
</script>

<div class="flex h-[calc(100vh-3.5rem)] flex-col">
	<!-- Toolbar -->
	<div class="flex items-center justify-between border-b border-gray-800 bg-gray-900 px-4 py-2">
		<div class="flex items-center gap-3">
			<a href="/" class="text-sm text-gray-400 hover:text-white">&larr; Back</a>
			{#if session}
				<span class="text-sm font-medium text-white">{session.server_name}</span>
				<span class="font-mono text-xs text-gray-500">{session.bmc_ip}</span>
				{#if reconnecting}
					<span class="flex items-center gap-1.5 text-xs text-yellow-400">
						<span class="h-2 w-2 animate-pulse rounded-full bg-yellow-400"></span>
						Reconnecting...
					</span>
				{:else if session.status === 'starting'}
					<span class="flex items-center gap-1.5 text-xs text-yellow-400">
						<span class="h-2 w-2 animate-pulse rounded-full bg-yellow-400"></span>
						Starting...
					</span>
				{:else if session.status === 'connected'}
					<span class="flex items-center gap-1.5 text-xs text-green-400">
						<span class="h-2 w-2 rounded-full bg-green-400"></span>
						Connected
					</span>
				{:else if session.status === 'error'}
					<span class="flex items-center gap-1.5 text-xs text-red-400">
						<span class="h-2 w-2 rounded-full bg-red-400"></span>
						Error
					</span>
				{/if}
			{/if}
		</div>

		<div class="flex items-center gap-2">
			<button
				onclick={sendCtrlAltDel}
				class="rounded bg-gray-800 px-3 py-1.5 text-xs text-gray-300 hover:bg-gray-700 hover:text-white"
			>
				Ctrl+Alt+Del
			</button>
			<button
				onclick={toggleFullscreen}
				class="rounded bg-gray-800 px-3 py-1.5 text-xs text-gray-300 hover:bg-gray-700 hover:text-white"
			>
				{isFullscreen ? 'Exit Fullscreen' : 'Fullscreen'}
			</button>
			<button
				onclick={disconnect}
				class="rounded bg-red-900 px-3 py-1.5 text-xs text-red-200 hover:bg-red-800"
			>
				Disconnect
			</button>
		</div>
	</div>

	<!-- Session Timeout Warning -->
	{#if session?.status === 'connected' && session.idle_timeout_remaining_seconds != null}
		<SessionTimeoutWarning
			sessionId={activeSessionId}
			remainingSeconds={session.idle_timeout_remaining_seconds}
		/>
	{/if}

	<!-- KVM Viewer Area -->
	<div bind:this={viewerContainer} class="relative flex-1 bg-black">
		{#if error}
			<div class="flex h-full items-center justify-center">
				<div class="text-center">
					<p class="text-red-400">{error}</p>
					<button
						onclick={() => goto('/')}
						class="mt-4 rounded bg-gray-800 px-4 py-2 text-sm text-gray-300 hover:bg-gray-700"
					>
						Back to Servers
					</button>
				</div>
			</div>
		{:else if reconnecting || session?.status === 'starting'}
			<div class="flex h-full items-center justify-center">
				<div class="text-center">
					<div class="mx-auto h-10 w-10 animate-spin rounded-full border-2 border-gray-600 border-t-blue-400"></div>
					{#if reconnecting}
						<p class="mt-4 text-gray-400">Reconnecting...</p>
						<p class="mt-1 text-sm text-gray-500">Session timed out, re-authenticating with BMC</p>
					{:else}
						<p class="mt-4 text-gray-400">Starting KVM session...</p>
						<p class="mt-1 text-sm text-gray-500">Authenticating with BMC and launching viewer</p>
					{/if}
				</div>
			</div>
		{:else if session?.status === 'connected'}
			<KVMViewer wsUrl={getKVMWebSocketURL(activeSessionId)} container={viewerContainer} ondisconnect={handleViewerDisconnect} password={session?.kvm_password} />
		{:else if session?.status === 'disconnected'}
			<div class="flex h-full items-center justify-center">
				<div class="text-center">
					<p class="text-gray-400">Session disconnected</p>
					<button
						onclick={() => goto('/')}
						class="mt-4 rounded bg-gray-800 px-4 py-2 text-sm text-gray-300 hover:bg-gray-700"
					>
						Back to Servers
					</button>
				</div>
			</div>
		{/if}
	</div>
</div>
