<script lang="ts">
	import { requestViewerControl, releaseViewerControl, type Viewer } from '$lib/api';
	import { Button } from '@immich/ui';

	interface Props {
		viewers: Viewer[];
		currentUserName: string;
		sessionId: string;
		myViewerId: string;
	}

	let { viewers, currentUserName, sessionId, myViewerId }: Props = $props();

	let requesting = $state(false);
	let error = $state('');

	let iHaveControl = $derived(viewers.some((v) => v.id === myViewerId && v.has_control));

	function getInitial(name: string): string {
		if (name.includes('.')) return name.split('.')[0].charAt(0).toUpperCase(); // IP: use first octet
		return (name.charAt(0) || '?').toUpperCase();
	}

	async function handleRequestControl(viewerId: string) {
		requesting = true;
		error = '';
		try {
			await requestViewerControl(sessionId, viewerId);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to request control';
		} finally {
			requesting = false;
		}
	}

	async function handleReleaseControl(viewerId: string) {
		requesting = true;
		error = '';
		try {
			await releaseViewerControl(sessionId, viewerId);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to release control';
		} finally {
			requesting = false;
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
					: 'bg-light-200 text-muted'}"
				title="{viewer.display_name} — {viewer.has_control ? 'Controlling' : 'Viewing'}"
				aria-label="{viewer.display_name}, {viewer.has_control ? 'controlling' : 'viewing'}"
			>
				{getInitial(viewer.display_name)}
			</div>
		{/each}
	</div>

	{#if viewers.length > 1}
		{#if iHaveControl}
			<Button
				style="border-radius: 0.75rem"
				onclick={() => handleReleaseControl(myViewerId)}
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
				onclick={() => handleRequestControl(myViewerId)}
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
