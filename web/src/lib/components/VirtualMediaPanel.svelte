<script lang="ts">
	import { Button, LoadingSpinner } from '@immich/ui';
	import { getVirtualMediaStatus, mountVirtualMedia, ejectVirtualMedia, fetchISOs, mountLocalISO, type VirtualMediaStatus, type ISOFile } from '$lib/api';

	interface Props {
		sessionId: string;
		onunsupported?: () => void;
	}

	let { sessionId, onunsupported }: Props = $props();

	let status: VirtualMediaStatus | null = $state(null);
	let supported = $state(true);
	let imageUrl = $state('');
	let loading = $state(false);
	let error = $state('');
	let initialLoading = $state(true);

	let libraryISOs: ISOFile[] = $state([]);
	let selectedISO = $state('');
	let libraryLoading = $state(false);

	async function fetchStatus() {
		try {
			const result = await getVirtualMediaStatus(sessionId);
			if (result === null) {
				supported = false;
				onunsupported?.();
				return;
			}
			status = result;
			error = '';
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to fetch status';
		} finally {
			initialLoading = false;
		}
	}

	async function loadLibraryISOs() {
		try {
			const result = await fetchISOs();
			libraryISOs = result.isos;
			if (libraryISOs.length > 0 && !selectedISO) {
				selectedISO = libraryISOs[0].filename;
			}
		} catch {
			// Silently fail — library is supplementary
		}
	}

	async function handleMountLibrary() {
		if (!selectedISO || loading) return;
		loading = true;
		libraryLoading = true;
		error = '';
		try {
			await mountLocalISO(sessionId, selectedISO);
			await fetchStatus();
			selectedISO = '';
		} catch (e) {
			error = e instanceof Error ? e.message : 'Mount from library failed';
		} finally {
			loading = false;
			libraryLoading = false;
		}
	}

	async function handleMount() {
		if (!imageUrl.trim() || loading) return;
		loading = true;
		error = '';
		try {
			await mountVirtualMedia(sessionId, imageUrl.trim());
			await fetchStatus();
			imageUrl = '';
		} catch (e) {
			error = e instanceof Error ? e.message : 'Mount failed';
		} finally {
			loading = false;
		}
	}

	async function handleEject() {
		if (loading) return;
		loading = true;
		error = '';
		try {
			await ejectVirtualMedia(sessionId);
			await fetchStatus();
		} catch (e) {
			error = e instanceof Error ? e.message : 'Eject failed';
		} finally {
			loading = false;
		}
	}

	// Fetch on mount and poll every 10 seconds
	$effect(() => {
		fetchStatus();
		loadLibraryISOs();
		const interval = setInterval(fetchStatus, 10000);
		return () => clearInterval(interval);
	});
</script>

<div class="absolute right-0 top-full z-50 mt-1 w-80 rounded border border-light-200 bg-light-50 p-3 shadow-lg">
	{#if initialLoading}
		<div class="flex items-center justify-center py-4">
			<LoadingSpinner size="small" />
		</div>
	{:else if !supported}
		<p class="text-xs text-muted">Virtual media is not supported by this board.</p>
	{:else}
		<div class="mb-2 text-xs font-medium text-dark">Virtual Media</div>

		<!-- Current status -->
		<div class="mb-3 rounded bg-light-100 px-2 py-1.5 text-xs text-muted">
			{#if status?.inserted}
				<div class="flex items-center gap-1.5">
					<span class="inline-block h-2 w-2 shrink-0 rounded-full bg-success-500" aria-hidden="true"></span>
					<span class="text-dark">Mounted: <span class="font-mono">{status.image}</span></span>
				</div>
				{#if status.media_type}
					<div class="mt-1 text-muted">Type: {status.media_type}</div>
				{/if}
				{#if status.write_protected}
					<div class="mt-1 text-muted">Write protected</div>
				{/if}
			{:else}
				<div class="flex items-center gap-1.5">
					<span class="inline-block h-2 w-2 shrink-0 rounded-full bg-light-300" aria-hidden="true"></span>
					<span>No media inserted</span>
				</div>
			{/if}
		</div>

		<!-- Mount controls -->
		{#if !status?.inserted}
			<div class="flex gap-2">
				<input
					type="text"
					bind:value={imageUrl}
					placeholder="https://example.com/os.iso"
					disabled={loading}
					class="flex-1 rounded border border-light-200 bg-light px-2 py-1 text-xs text-dark placeholder:text-light-400 focus:border-primary-400 focus:outline-none disabled:opacity-50"
				/>
				<Button
					onclick={handleMount}
					disabled={!imageUrl.trim() || loading}
					size="tiny"
					variant="filled"
					color="primary"
				>
					{#if loading}
						<LoadingSpinner size="tiny" />
					{:else}
						Mount
					{/if}
				</Button>
			</div>

			<!-- Mount from library -->
			<div class="mt-3 border-t border-light-200 pt-3">
				<div class="mb-1.5 text-xs text-muted">or mount from library</div>
				{#if libraryISOs.length > 0}
					<div class="flex gap-2">
						<select
							bind:value={selectedISO}
							disabled={loading}
							class="flex-1 rounded border border-light-200 bg-white px-2 py-1 text-xs text-dark focus:border-primary-400 focus:outline-none disabled:opacity-50"
						>
							{#each libraryISOs as iso}
								<option value={iso.filename}>{iso.filename}</option>
							{/each}
						</select>
						<Button
							onclick={handleMountLibrary}
							disabled={!selectedISO || loading}
							size="tiny"
							variant="filled"
							color="primary"
						>
							{#if libraryLoading}
								<LoadingSpinner size="tiny" />
							{:else}
								Mount
							{/if}
						</Button>
					</div>
				{:else}
					<p class="text-xs text-muted">No ISOs uploaded. <a href="/isos" class="text-primary hover:text-primary-600 transition-colors">Upload one</a></p>
				{/if}
			</div>
		{:else}
			<Button
				onclick={handleEject}
				disabled={loading}
				size="tiny"
				variant="outline"
				color="danger"
			>
				{#if loading}
					<LoadingSpinner size="tiny" />
				{:else}
					Eject
				{/if}
			</Button>
		{/if}

		<!-- Error display -->
		{#if error}
			<div class="mt-2 rounded bg-danger-100 px-2 py-1.5 text-xs text-danger" role="alert">
				{error}
			</div>
		{/if}
	{/if}
</div>
