<script lang="ts">
	import { fetchISOs, uploadISO, deleteISO, type ISOFile, type ISOListResponse } from '$lib/api';
	import { Button, LoadingSpinner } from '@immich/ui';

	let data: ISOListResponse | null = $state(null);
	let loading = $state(true);
	let error = $state('');

	let dragging = $state(false);
	let uploading = $state(false);
	let uploadProgress = $state(0);
	let uploadFilename = $state('');
	let uploadError = $state('');
	let uploadSuccess = $state('');

	let deleteConfirm = $state<string | null>(null);
	let deleting = $state<string | null>(null);

	let fileInput: HTMLInputElement;

	function formatBytes(bytes: number): string {
		if (bytes === 0) return '0 B';
		const k = 1024;
		const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
		const i = Math.floor(Math.log(bytes) / Math.log(k));
		return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
	}

	function formatTime(ts: string): string {
		const date = new Date(ts);
		const now = new Date();
		const diffMs = now.getTime() - date.getTime();
		const diffSec = Math.floor(diffMs / 1000);

		if (diffSec < 60) return `${diffSec}s ago`;
		if (diffSec < 3600) return `${Math.floor(diffSec / 60)}m ago`;
		if (diffSec < 86400) return `${Math.floor(diffSec / 3600)}h ago`;
		if (diffSec < 604800) return `${Math.floor(diffSec / 86400)}d ago`;

		return date.toLocaleString();
	}

	function formatTimeFull(ts: string): string {
		return new Date(ts).toLocaleString();
	}

	function storagePercent(): number {
		if (!data || data.max_size_bytes === 0) return 0;
		return Math.round((data.total_size_bytes / data.max_size_bytes) * 100);
	}

	async function loadISOs() {
		try {
			error = '';
			const result = await fetchISOs();
			data = result;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to load ISOs';
		} finally {
			loading = false;
		}
	}

	async function handleFile(file: File) {
		if (!file.name.toLowerCase().endsWith('.iso')) {
			uploadError = 'Only .iso files are accepted.';
			return;
		}

		uploading = true;
		uploadProgress = 0;
		uploadFilename = file.name;
		uploadError = '';
		uploadSuccess = '';

		try {
			await uploadISO(file, (pct) => {
				uploadProgress = pct;
			});
			uploadSuccess = `${file.name} uploaded successfully.`;
			await loadISOs();
		} catch (e) {
			uploadError = e instanceof Error ? e.message : 'Upload failed';
		} finally {
			uploading = false;
		}
	}

	function handleDrop(e: DragEvent) {
		const files = e.dataTransfer?.files;
		if (files && files.length > 0) {
			handleFile(files[0]);
		}
	}

	function handleFileSelect(e: Event) {
		const input = e.target as HTMLInputElement;
		if (input.files && input.files.length > 0) {
			handleFile(input.files[0]);
			input.value = '';
		}
	}

	async function handleDelete(filename: string) {
		if (deleteConfirm !== filename) {
			deleteConfirm = filename;
			return;
		}

		deleting = filename;
		deleteConfirm = null;
		error = '';

		try {
			await deleteISO(filename);
			await loadISOs();
		} catch (e) {
			error = e instanceof Error ? e.message : 'Delete failed';
		} finally {
			deleting = null;
		}
	}

	function cancelDelete() {
		deleteConfirm = null;
	}

	$effect(() => {
		loadISOs();
	});
</script>

<div class="mx-auto max-w-7xl px-4 py-8 sm:px-6 lg:px-8">
	<div class="mb-6">
		<a href="/" class="text-sm text-muted hover:text-dark transition-colors">&larr; Back to Infrastructure</a>
	</div>

	<div class="mb-8 flex items-center justify-between">
		<div>
			<h1 class="text-2xl font-bold text-dark">ISO Library</h1>
			<p class="mt-1 text-sm text-muted">Manage ISO images for virtual media mounting.</p>
		</div>
		<div class="flex items-center gap-3">
			<Button
				onclick={() => loadISOs()}
				size="small"
				variant="outline"
				color="secondary"
			>
				Refresh
			</Button>
			<Button
				onclick={() => fileInput.click()}
				disabled={uploading}
				size="small"
				variant="filled"
				color="primary"
			>
				Upload ISO
			</Button>
			<input
				bind:this={fileInput}
				type="file"
				accept=".iso"
				onchange={handleFileSelect}
				class="hidden"
			/>
		</div>
	</div>

	<!-- Storage usage -->
	{#if data && data.max_size_bytes > 0}
		<div class="mb-6 rounded-lg border border-light-200 bg-light-50 p-4">
			<div class="mb-2 flex items-center justify-between text-sm">
				<span class="text-muted">Storage Usage</span>
				<span class="text-dark font-medium">{formatBytes(data.total_size_bytes)} / {formatBytes(data.max_size_bytes)} ({storagePercent()}%)</span>
			</div>
			<div class="h-2 w-full overflow-hidden rounded-full bg-light-200">
				<div
					class="h-full rounded-full transition-all {storagePercent() > 90 ? 'bg-danger-500' : storagePercent() > 70 ? 'bg-warning-500' : 'bg-primary'}"
					style="width: {storagePercent()}%"
				></div>
			</div>
		</div>
	{/if}

	<!-- Drag-and-drop zone / Upload progress -->
	<div class="mb-6">
		{#if uploading}
			<div class="rounded-lg border-2 border-dashed border-primary bg-primary-50/10 p-6">
				<div class="text-center">
					<p class="text-sm font-medium text-dark">{uploadFilename}</p>
					<p class="mt-1 text-xs text-muted">{formatBytes(0)} &mdash; {uploadProgress}% uploaded</p>
					<div class="mx-auto mt-3 h-2 w-64 max-w-full overflow-hidden rounded-full bg-light-200">
						<div
							class="h-full rounded-full bg-primary transition-all"
							style="width: {uploadProgress}%"
						></div>
					</div>
				</div>
			</div>
		{:else}
			<!-- svelte-ignore a11y_no_static_element_interactions -->
			<div
				ondragover={(e) => { e.preventDefault(); dragging = true; }}
				ondragleave={() => { dragging = false; }}
				ondrop={(e) => { e.preventDefault(); dragging = false; handleDrop(e); }}
				onclick={() => fileInput.click()}
				class="cursor-pointer rounded-lg border-2 border-dashed p-8 text-center transition-colors {dragging ? 'border-primary bg-primary-50/10' : 'border-light-300 hover:border-light-400'}"
				role="button"
				tabindex="0"
				onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); fileInput.click(); } }}
			>
				<svg class="mx-auto h-10 w-10 text-light-400" fill="none" viewBox="0 0 24 24" stroke="currentColor" aria-hidden="true">
					<path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5" d="M7 16a4 4 0 01-.88-7.903A5 5 0 1115.9 6L16 6a5 5 0 011 9.9M15 13l-3-3m0 0l-3 3m3-3v12" />
				</svg>
				<p class="mt-2 text-sm text-muted">Drag and drop an ISO file here, or click to browse</p>
				<p class="mt-1 text-xs text-light-400">.iso files only</p>
			</div>
		{/if}
	</div>

	<!-- Upload messages -->
	{#if uploadSuccess}
		<div class="mb-6 rounded-lg border border-success-200 bg-success-50 px-4 py-3 text-sm text-success" role="status">
			{uploadSuccess}
		</div>
	{/if}
	{#if uploadError}
		<div class="mb-6 rounded-lg border border-danger-200 bg-danger-50 px-4 py-3 text-sm text-danger" role="alert">
			{uploadError}
		</div>
	{/if}

	<!-- Error -->
	{#if error}
		<div class="mb-6 rounded-lg border border-danger-200 bg-danger-50 px-4 py-3 text-sm text-danger" role="alert">
			{error}
		</div>
	{/if}

	<!-- Loading -->
	{#if loading}
		<div class="flex items-center justify-center py-20" aria-live="polite">
			<LoadingSpinner size="large" />
			<span class="ml-3 text-muted">Loading ISOs...</span>
		</div>
	{:else if !data || data.isos.length === 0}
		<!-- Empty state -->
		<div class="rounded-lg border border-light-200 bg-light-50 py-16 text-center">
			<p class="text-muted">No ISOs uploaded.</p>
			<p class="mt-1 text-sm text-muted">Drag and drop an ISO file or click Upload.</p>
		</div>
	{:else}
		<!-- Table -->
		<div class="overflow-x-auto rounded-lg border border-light-200">
			<table class="w-full text-left text-sm">
				<thead>
					<tr class="border-b border-light-200 bg-light-50">
						<th class="whitespace-nowrap px-4 py-3 font-medium text-muted">Filename</th>
						<th class="whitespace-nowrap px-4 py-3 font-medium text-muted">Size</th>
						<th class="whitespace-nowrap px-4 py-3 font-medium text-muted">Uploaded</th>
						<th class="whitespace-nowrap px-4 py-3 font-medium text-muted">Last Used</th>
						<th class="whitespace-nowrap px-4 py-3 font-medium text-muted">Actions</th>
					</tr>
				</thead>
				<tbody>
					{#each data.isos as iso (iso.id)}
						<tr class="border-b border-light-200 last:border-b-0 hover:bg-light-100 transition-colors">
							<td class="px-4 py-3 text-dark font-medium">
								<div class="flex items-center gap-2">
									<svg class="h-4 w-4 shrink-0 text-muted" fill="none" viewBox="0 0 24 24" stroke="currentColor" aria-hidden="true">
										<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 17v-2m3 2v-4m3 4v-6m2 10H7a2 2 0 01-2-2V5a2 2 0 012-2h5.586a1 1 0 01.707.293l5.414 5.414a1 1 0 01.293.707V19a2 2 0 01-2 2z" />
									</svg>
									<span class="truncate" title={iso.filename}>{iso.filename}</span>
								</div>
							</td>
							<td class="whitespace-nowrap px-4 py-3 text-dark">{formatBytes(iso.size_bytes)}</td>
							<td class="whitespace-nowrap px-4 py-3 text-muted" title={formatTimeFull(iso.uploaded_at)}>
								{formatTime(iso.uploaded_at)}
								{#if iso.uploaded_by}
									<span class="text-light-400"> by {iso.uploaded_by}</span>
								{/if}
							</td>
							<td class="whitespace-nowrap px-4 py-3 text-muted">
								{#if iso.last_used}
									<span title={formatTimeFull(iso.last_used)}>{formatTime(iso.last_used)}</span>
								{:else}
									<span class="text-light-400">Never</span>
								{/if}
							</td>
							<td class="whitespace-nowrap px-4 py-3">
								<div class="flex items-center gap-2">
									<a
										href="/api/isos/{encodeURIComponent(iso.filename)}/download"
										class="text-xs text-primary hover:text-primary-600 transition-colors"
										download
									>
										Download
									</a>
									{#if deleteConfirm === iso.filename}
										<Button
											onclick={() => handleDelete(iso.filename)}
											disabled={deleting === iso.filename}
											size="tiny"
											variant="filled"
											color="danger"
											loading={deleting === iso.filename}
										>
											Confirm
										</Button>
										<Button
											onclick={cancelDelete}
											size="tiny"
											variant="ghost"
											color="secondary"
										>
											Cancel
										</Button>
									{:else}
										<Button
											onclick={() => handleDelete(iso.filename)}
											disabled={deleting === iso.filename}
											size="tiny"
											variant="outline"
											color="danger"
											loading={deleting === iso.filename}
										>
											Delete
										</Button>
									{/if}
								</div>
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>

		<p class="mt-3 text-center text-xs text-muted">
			{data.isos.length} {data.isos.length === 1 ? 'ISO' : 'ISOs'} &mdash; {formatBytes(data.total_size_bytes)} total
		</p>
	{/if}
</div>
