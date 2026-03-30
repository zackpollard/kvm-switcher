<script lang="ts">
	import { requestViewerControl, releaseViewerControl, acceptControlRequest, denyControlRequest, type Viewer, type PendingControlRequest } from '$lib/api';
	import { Button } from '@immich/ui';

	interface Props {
		viewers: Viewer[];
		currentUserName: string;
		sessionId: string;
		myViewerId: string;
		pendingRequest?: PendingControlRequest | null;
	}

	let { viewers, currentUserName, sessionId, myViewerId, pendingRequest = null }: Props = $props();

	let requesting = $state(false);
	let error = $state('');

	let iHaveControl = $derived(viewers.some((v) => v.id === myViewerId && v.has_control));
	let pendingFromMe = $derived(pendingRequest?.requester_id === myViewerId);
	let pendingForMe = $derived(iHaveControl && pendingRequest != null && !pendingFromMe);

	// Compute remaining seconds for the pending request timeout
	let pendingTimeRemaining = $derived(() => {
		if (!pendingRequest) return 0;
		const elapsed = (Date.now() - new Date(pendingRequest.requested_at).getTime()) / 1000;
		return Math.max(0, Math.ceil(pendingRequest.timeout_sec - elapsed));
	});

	function getInitial(name: string): string {
		if (name.includes('.')) return name.split('.')[0].charAt(0).toUpperCase();
		return (name.charAt(0) || '?').toUpperCase();
	}

	async function handleRequestControl() {
		requesting = true;
		error = '';
		try {
			await requestViewerControl(sessionId, myViewerId);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed';
		} finally {
			requesting = false;
		}
	}

	async function handleReleaseControl() {
		requesting = true;
		error = '';
		try {
			await releaseViewerControl(sessionId, myViewerId);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed';
		} finally {
			requesting = false;
		}
	}

	async function handleAccept() {
		try {
			await acceptControlRequest(sessionId);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed';
		}
	}

	async function handleDeny() {
		try {
			await denyControlRequest(sessionId);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed';
		}
	}
</script>

<div class="flex items-center gap-2" role="status" aria-label="Connected viewers">
	<span class="text-xs text-muted">{viewers.length} {viewers.length === 1 ? 'viewer' : 'viewers'}</span>

	<div class="flex items-center -space-x-1">
		{#each viewers as viewer (viewer.id)}
			<div
				class="relative flex h-6 w-6 items-center justify-center rounded-full text-xs font-medium {viewer.has_control
					? 'bg-success-200 text-success ring-2 ring-success-500'
					: viewer.id === myViewerId ? 'bg-primary-100 text-primary ring-1 ring-primary-300' : 'bg-light-200 text-muted'}"
				title="{viewer.display_name}{viewer.id === myViewerId ? ' (you)' : ''} — {viewer.has_control ? 'Controlling' : 'Viewing'}"
			>
				{viewer.id === myViewerId ? '⬤' : getInitial(viewer.display_name)}
			</div>
		{/each}
	</div>

	{#if viewers.length > 1}
		{#if pendingForMe}
			<!-- Someone is requesting control from me -->
			<div class="flex items-center gap-1 rounded-lg border border-warning-200 bg-warning-50 px-2 py-1">
				<span class="text-xs text-dark">{pendingRequest?.requester_name} wants control ({pendingTimeRemaining()}s)</span>
				<Button style="border-radius: 0.75rem" onclick={handleAccept} size="tiny" variant="filled" color="success">
					Accept
				</Button>
				<Button style="border-radius: 0.75rem" onclick={handleDeny} size="tiny" variant="ghost" color="danger">
					Deny
				</Button>
			</div>
		{:else if pendingFromMe}
			<span class="text-xs text-muted">Waiting for approval...</span>
		{:else if iHaveControl}
			<Button
				style="border-radius: 0.75rem"
				onclick={handleReleaseControl}
				disabled={requesting}
				size="tiny"
				variant="ghost"
				color="secondary"
			>
				{requesting ? 'Releasing...' : 'Release Control'}
			</Button>
		{:else}
			<Button
				style="border-radius: 0.75rem"
				onclick={handleRequestControl}
				disabled={requesting}
				size="tiny"
				variant="ghost"
				color="primary"
			>
				{requesting ? 'Requesting...' : 'Request Control'}
			</Button>
		{/if}
	{/if}

	{#if error}
		<span class="text-xs text-danger" role="alert">{error}</span>
	{/if}
</div>
