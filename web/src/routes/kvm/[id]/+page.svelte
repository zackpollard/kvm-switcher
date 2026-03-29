<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { getSession, deleteSession, createSession, getKVMWebSocketURL, kvmPowerControl, kvmDisplayLock, kvmResetVideo, kvmMouseMode, kvmKeyboardLayout, type KVMSession } from '$lib/api';
	import KVMViewer from '$lib/components/KVMViewer.svelte';
	import SessionTimeoutWarning from '$lib/components/SessionTimeoutWarning.svelte';

	let session: KVMSession | null = $state(null);
	let activeSessionId = $state(page.params.id);
	let error = $state('');
	let isFullscreen = $state(false);
	let viewerContainer = $state<HTMLDivElement>(undefined!);
	let reconnecting = $state(false);
	let manualDisconnect = false;
	let showPowerMenu = $state(false);
	let showMouseMenu = $state(false);
	let showKbdMenu = $state(false);
	let isIKVM = $derived(session?.conn_mode === 'ikvm');

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
		error = 'Connection lost. Click to reconnect.';
	}

	async function reconnect() {
		error = '';
		const serverName = session?.server_name;
		if (!serverName) {
			error = 'Cannot reconnect: unknown server';
			reconnecting = false;
			return;
		}

		try {
			await deleteSession(activeSessionId);
		} catch {
			// Old session may already be gone
		}

		try {
			const newSession = await createSession(serverName);
			session = newSession;
			activeSessionId = newSession.id;
			reconnecting = false;
			history.replaceState({}, '', `/kvm/${newSession.id}`);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Reconnection failed';
			reconnecting = false;
		}
	}

	function sendCtrlAltDel() {
		viewerContainer?.dispatchEvent(new CustomEvent('send-ctrl-alt-del'));
	}

	async function handlePower(action: 'on' | 'off' | 'cycle' | 'reset' | 'soft_reset' | 'bmc_reset') {
		showPowerMenu = false;
		const label = action === 'bmc_reset' ? 'cold reset the BMC (video/management controller)' : action.replace('_', ' ') + ' this server';
		if (!confirm(`Are you sure you want to ${label}?`)) return;
		try {
			await kvmPowerControl(activeSessionId, action);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Power command failed';
		}
	}

	async function toggleDisplayLock() {
		try {
			await kvmDisplayLock(activeSessionId, true);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Display lock failed';
		}
	}

	async function setMouseMode(mode: 'relative' | 'absolute') {
		try {
			await kvmMouseMode(activeSessionId, mode);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Mouse mode change failed';
		}
	}

	async function setKeyboard(layout: string) {
		try {
			await kvmKeyboardLayout(activeSessionId, layout);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Keyboard layout change failed';
		}
	}

	async function resetVideo() {
		try {
			await kvmResetVideo(activeSessionId);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Video reset failed';
		}
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
		// Only depend on activeSessionId, not session state
		const _id = activeSessionId;
		loadSession();
		const interval = setInterval(loadSession, 1000);
		return () => clearInterval(interval);
	});

	$effect(() => {
		const handler = () => {
			isFullscreen = !!document.fullscreenElement;
		};
		document.addEventListener('fullscreenchange', handler);
		return () => document.removeEventListener('fullscreenchange', handler);
	});

	$effect(() => {
		const handleKeydown = (e: KeyboardEvent) => {
			if (e.key === 'Escape') {
				showPowerMenu = false;
				showMouseMenu = false;
				showKbdMenu = false;
			}
		};
		document.addEventListener('keydown', handleKeydown);
		return () => document.removeEventListener('keydown', handleKeydown);
	});
</script>

<div class="flex h-[calc(100vh-3.5rem)] flex-col">
	<!-- Toolbar -->
	<div class="flex items-center justify-between border-b border-gray-800 bg-gray-900 px-4 py-2" role="toolbar" aria-label="KVM controls">
		<div class="flex items-center gap-3">
			<a href="/" class="text-sm text-gray-400 hover:text-white">&larr; Back</a>
			{#if session}
				<span class="text-sm font-medium text-white">{session.server_name}</span>
				<span class="font-mono text-xs text-gray-500">{session.bmc_ip}</span>
				{#if reconnecting}
					<span class="flex items-center gap-1.5 text-xs text-yellow-400" aria-live="polite">
						<span class="h-2 w-2 animate-pulse rounded-full bg-yellow-400" aria-hidden="true"></span>
						Reconnecting...
					</span>
				{:else if session.status === 'starting'}
					<span class="flex items-center gap-1.5 text-xs text-yellow-400" aria-live="polite">
						<span class="h-2 w-2 animate-pulse rounded-full bg-yellow-400" aria-hidden="true"></span>
						Starting...
					</span>
				{:else if session.status === 'connected'}
					<span class="flex items-center gap-1.5 text-xs text-green-400">
						<span class="h-2 w-2 rounded-full bg-green-400" aria-hidden="true"></span>
						Connected
					</span>
				{:else if session.status === 'error'}
					<span class="flex items-center gap-1.5 text-xs text-red-400">
						<span class="h-2 w-2 rounded-full bg-red-400" aria-hidden="true"></span>
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
			{#if isIKVM}
				<div class="relative">
					<button
						onclick={() => { showPowerMenu = !showPowerMenu; showMouseMenu = false; showKbdMenu = false; }}
						class="rounded bg-gray-800 px-3 py-1.5 text-xs text-gray-300 hover:bg-gray-700 hover:text-white"
						aria-haspopup="true"
						aria-expanded={showPowerMenu}
					>
						Power
					</button>
					{#if showPowerMenu}
						<div class="absolute right-0 top-full z-50 mt-1 w-40 rounded border border-gray-700 bg-gray-800 py-1 shadow-lg" role="menu">
							<button onclick={() => handlePower('on')} class="block w-full px-3 py-1.5 text-left text-xs text-green-400 hover:bg-gray-700" role="menuitem">Power On</button>
							<button onclick={() => handlePower('off')} class="block w-full px-3 py-1.5 text-left text-xs text-red-400 hover:bg-gray-700" role="menuitem">Power Off</button>
							<button onclick={() => handlePower('cycle')} class="block w-full px-3 py-1.5 text-left text-xs text-yellow-400 hover:bg-gray-700" role="menuitem">Power Cycle</button>
							<button onclick={() => handlePower('reset')} class="block w-full px-3 py-1.5 text-left text-xs text-yellow-400 hover:bg-gray-700" role="menuitem">Hard Reset</button>
							<button onclick={() => handlePower('soft_reset')} class="block w-full px-3 py-1.5 text-left text-xs text-orange-400 hover:bg-gray-700" role="menuitem">Soft Reset</button>
							<hr class="my-1 border-gray-700" />
							<button onclick={() => handlePower('bmc_reset')} class="block w-full px-3 py-1.5 text-left text-xs text-purple-400 hover:bg-gray-700" role="menuitem">BMC Cold Reset</button>
						</div>
					{/if}
				</div>
				<button
					onclick={toggleDisplayLock}
					class="rounded bg-gray-800 px-3 py-1.5 text-xs text-gray-300 hover:bg-gray-700 hover:text-white"
					title="Lock host display"
				>
					Lock Display
				</button>
				<button
					onclick={resetVideo}
					class="rounded bg-gray-800 px-3 py-1.5 text-xs text-gray-300 hover:bg-gray-700 hover:text-white"
					title="Reset video capture engine (fixes stuck/corrupted display)"
				>
					Reset Video
				</button>
				<div class="relative">
					<button
						onclick={() => { showMouseMenu = !showMouseMenu; showKbdMenu = false; showPowerMenu = false; }}
						class="rounded bg-gray-800 px-3 py-1.5 text-xs text-gray-300 hover:bg-gray-700 hover:text-white"
						aria-haspopup="true"
						aria-expanded={showMouseMenu}
					>
						Mouse
					</button>
					{#if showMouseMenu}
						<div class="absolute right-0 top-full z-50 mt-1 w-32 rounded border border-gray-700 bg-gray-800 py-1 shadow-lg" role="menu">
							<button onclick={() => { setMouseMode('absolute'); showMouseMenu = false; }} class="block w-full px-3 py-1.5 text-left text-xs text-gray-300 hover:bg-gray-700" role="menuitem">Absolute</button>
							<button onclick={() => { setMouseMode('relative'); showMouseMenu = false; }} class="block w-full px-3 py-1.5 text-left text-xs text-gray-300 hover:bg-gray-700" role="menuitem">Relative</button>
						</div>
					{/if}
				</div>
				<div class="relative">
					<button
						onclick={() => { showKbdMenu = !showKbdMenu; showMouseMenu = false; showPowerMenu = false; }}
						class="rounded bg-gray-800 px-3 py-1.5 text-xs text-gray-300 hover:bg-gray-700 hover:text-white"
						aria-haspopup="true"
						aria-expanded={showKbdMenu}
					>
						Keyboard
					</button>
					{#if showKbdMenu}
						<div class="absolute right-0 top-full z-50 mt-1 w-32 rounded border border-gray-700 bg-gray-800 py-1 shadow-lg" role="menu">
							<button onclick={() => { setKeyboard('en'); showKbdMenu = false; }} class="block w-full px-3 py-1.5 text-left text-xs text-gray-300 hover:bg-gray-700" role="menuitem">English</button>
							<button onclick={() => { setKeyboard('fr'); showKbdMenu = false; }} class="block w-full px-3 py-1.5 text-left text-xs text-gray-300 hover:bg-gray-700" role="menuitem">French</button>
							<button onclick={() => { setKeyboard('de'); showKbdMenu = false; }} class="block w-full px-3 py-1.5 text-left text-xs text-gray-300 hover:bg-gray-700" role="menuitem">German</button>
							<button onclick={() => { setKeyboard('es'); showKbdMenu = false; }} class="block w-full px-3 py-1.5 text-left text-xs text-gray-300 hover:bg-gray-700" role="menuitem">Spanish</button>
							<button onclick={() => { setKeyboard('jp'); showKbdMenu = false; }} class="block w-full px-3 py-1.5 text-left text-xs text-gray-300 hover:bg-gray-700" role="menuitem">Japanese</button>
						</div>
					{/if}
				</div>
			{/if}
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
			<div class="flex h-full items-center justify-center" role="alert">
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
		{:else if reconnecting}
			<div class="flex h-full items-center justify-center">
				<div class="text-center">
					<p class="text-gray-400">Connection lost</p>
					<button
						onclick={reconnect}
						class="mt-4 rounded bg-blue-700 px-4 py-2 text-sm text-white hover:bg-blue-600"
					>
						Reconnect
					</button>
				</div>
			</div>
		{:else if session?.status === 'starting'}
			<div class="flex h-full items-center justify-center" aria-live="polite">
				<div class="text-center">
					<div class="mx-auto h-10 w-10 animate-spin rounded-full border-2 border-gray-600 border-t-blue-400" role="status" aria-label="Loading"></div>
					<p class="mt-4 text-gray-400">Starting KVM session...</p>
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
