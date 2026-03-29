<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { getSession, deleteSession, createSession, getKVMWebSocketURL, kvmPowerControl, kvmDisplayLock, kvmResetVideo, kvmMouseMode, kvmKeyboardLayout, type KVMSession } from '$lib/api';
	import KVMViewer from '$lib/components/KVMViewer.svelte';
	import SessionTimeoutWarning from '$lib/components/SessionTimeoutWarning.svelte';
	import { Button, LoadingSpinner, Alert } from '@immich/ui';

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
	<div class="flex items-center justify-between border-b border-light-300 bg-light-50 px-4 py-2" role="toolbar" aria-label="KVM controls">
		<div class="flex items-center gap-3">
			<Button href="/" size="tiny" variant="ghost" color="secondary">&larr; Back</Button>
			{#if session}
				<span class="text-sm font-medium text-dark">{session.server_name}</span>
				<span class="font-mono text-xs text-muted">{session.bmc_ip}</span>
				{#if reconnecting}
					<span class="flex items-center gap-1.5 text-xs text-warning" aria-live="polite">
						<span class="h-2 w-2 animate-pulse rounded-full bg-warning" aria-hidden="true"></span>
						Reconnecting...
					</span>
				{:else if session.status === 'starting'}
					<span class="flex items-center gap-1.5 text-xs text-warning" aria-live="polite">
						<span class="h-2 w-2 animate-pulse rounded-full bg-warning" aria-hidden="true"></span>
						Starting...
					</span>
				{:else if session.status === 'connected'}
					<span class="flex items-center gap-1.5 text-xs text-success">
						<span class="h-2 w-2 rounded-full bg-success" aria-hidden="true"></span>
						Connected
					</span>
				{:else if session.status === 'error'}
					<span class="flex items-center gap-1.5 text-xs text-danger">
						<span class="h-2 w-2 rounded-full bg-danger" aria-hidden="true"></span>
						Error
					</span>
				{/if}
			{/if}
		</div>

		<div class="flex items-center gap-2">
			<Button onclick={sendCtrlAltDel} size="tiny" variant="outline" color="secondary">
				Ctrl+Alt+Del
			</Button>
			{#if isIKVM}
				<div class="relative">
					<Button
						onclick={() => { showPowerMenu = !showPowerMenu; showMouseMenu = false; showKbdMenu = false; }}
						size="tiny"
						variant="outline"
						color="secondary"
						aria-haspopup="true"
						aria-expanded={showPowerMenu}
					>
						Power
					</Button>
					{#if showPowerMenu}
						<div class="absolute right-0 top-full z-50 mt-1 w-40 rounded border border-light-300 bg-light-50 py-1 shadow-lg" role="menu">
							<button onclick={() => handlePower('on')} class="block w-full px-3 py-1.5 text-left text-xs text-success hover:bg-light-200" role="menuitem">Power On</button>
							<button onclick={() => handlePower('off')} class="block w-full px-3 py-1.5 text-left text-xs text-danger hover:bg-light-200" role="menuitem">Power Off</button>
							<button onclick={() => handlePower('cycle')} class="block w-full px-3 py-1.5 text-left text-xs text-warning hover:bg-light-200" role="menuitem">Power Cycle</button>
							<button onclick={() => handlePower('reset')} class="block w-full px-3 py-1.5 text-left text-xs text-warning hover:bg-light-200" role="menuitem">Hard Reset</button>
							<button onclick={() => handlePower('soft_reset')} class="block w-full px-3 py-1.5 text-left text-xs text-warning hover:bg-light-200" role="menuitem">Soft Reset</button>
							<hr class="my-1 border-light-300" />
							<button onclick={() => handlePower('bmc_reset')} class="block w-full px-3 py-1.5 text-left text-xs text-primary hover:bg-light-200" role="menuitem">BMC Cold Reset</button>
						</div>
					{/if}
				</div>
				<Button
					onclick={toggleDisplayLock}
					size="tiny"
					variant="outline"
					color="secondary"
					title="Lock host display"
				>
					Lock Display
				</Button>
				<Button
					onclick={resetVideo}
					size="tiny"
					variant="outline"
					color="secondary"
					title="Reset video capture engine (fixes stuck/corrupted display)"
				>
					Reset Video
				</Button>
				<div class="relative">
					<Button
						onclick={() => { showMouseMenu = !showMouseMenu; showKbdMenu = false; showPowerMenu = false; }}
						size="tiny"
						variant="outline"
						color="secondary"
						aria-haspopup="true"
						aria-expanded={showMouseMenu}
					>
						Mouse
					</Button>
					{#if showMouseMenu}
						<div class="absolute right-0 top-full z-50 mt-1 w-32 rounded border border-light-300 bg-light-50 py-1 shadow-lg" role="menu">
							<button onclick={() => { setMouseMode('absolute'); showMouseMenu = false; }} class="block w-full px-3 py-1.5 text-left text-xs text-dark hover:bg-light-200" role="menuitem">Absolute</button>
							<button onclick={() => { setMouseMode('relative'); showMouseMenu = false; }} class="block w-full px-3 py-1.5 text-left text-xs text-dark hover:bg-light-200" role="menuitem">Relative</button>
						</div>
					{/if}
				</div>
				<div class="relative">
					<Button
						onclick={() => { showKbdMenu = !showKbdMenu; showMouseMenu = false; showPowerMenu = false; }}
						size="tiny"
						variant="outline"
						color="secondary"
						aria-haspopup="true"
						aria-expanded={showKbdMenu}
					>
						Keyboard
					</Button>
					{#if showKbdMenu}
						<div class="absolute right-0 top-full z-50 mt-1 w-32 rounded border border-light-300 bg-light-50 py-1 shadow-lg" role="menu">
							<button onclick={() => { setKeyboard('en'); showKbdMenu = false; }} class="block w-full px-3 py-1.5 text-left text-xs text-dark hover:bg-light-200" role="menuitem">English</button>
							<button onclick={() => { setKeyboard('fr'); showKbdMenu = false; }} class="block w-full px-3 py-1.5 text-left text-xs text-dark hover:bg-light-200" role="menuitem">French</button>
							<button onclick={() => { setKeyboard('de'); showKbdMenu = false; }} class="block w-full px-3 py-1.5 text-left text-xs text-dark hover:bg-light-200" role="menuitem">German</button>
							<button onclick={() => { setKeyboard('es'); showKbdMenu = false; }} class="block w-full px-3 py-1.5 text-left text-xs text-dark hover:bg-light-200" role="menuitem">Spanish</button>
							<button onclick={() => { setKeyboard('jp'); showKbdMenu = false; }} class="block w-full px-3 py-1.5 text-left text-xs text-dark hover:bg-light-200" role="menuitem">Japanese</button>
						</div>
					{/if}
				</div>
			{/if}
			<Button onclick={toggleFullscreen} size="tiny" variant="outline" color="secondary">
				{isFullscreen ? 'Exit Fullscreen' : 'Fullscreen'}
			</Button>
			<Button onclick={disconnect} size="tiny" variant="filled" color="danger">
				Disconnect
			</Button>
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
					<Alert color="danger" title={error} />
					<Button
						onclick={() => goto('/')}
						size="small"
						variant="outline"
						color="secondary"
						class="mt-4"
					>
						Back to Servers
					</Button>
				</div>
			</div>
		{:else if reconnecting}
			<div class="flex h-full items-center justify-center">
				<div class="text-center">
					<p class="text-muted">Connection lost</p>
					<Button onclick={reconnect} size="small" variant="filled" color="primary" class="mt-4">
						Reconnect
					</Button>
				</div>
			</div>
		{:else if session?.status === 'starting'}
			<div class="flex h-full items-center justify-center" aria-live="polite">
				<div class="text-center">
					<LoadingSpinner size="large" />
					<p class="mt-4 text-muted">Starting KVM session...</p>
				</div>
			</div>
		{:else if session?.status === 'connected'}
			<KVMViewer wsUrl={getKVMWebSocketURL(activeSessionId)} container={viewerContainer} ondisconnect={handleViewerDisconnect} password={session?.kvm_password} />
		{:else if session?.status === 'disconnected'}
			<div class="flex h-full items-center justify-center">
				<div class="text-center">
					<p class="text-muted">Session disconnected</p>
					<Button
						onclick={() => goto('/')}
						size="small"
						variant="outline"
						color="secondary"
						class="mt-4"
					>
						Back to Servers
					</Button>
				</div>
			</div>
		{/if}
	</div>
</div>
