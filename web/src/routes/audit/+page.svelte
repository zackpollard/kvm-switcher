<script lang="ts">
	import { fetchAuditLog, type AuditEntry } from '$lib/api';
	import { Button, LoadingSpinner } from '@immich/ui';

	const PAGE_SIZE = 50;

	const EVENT_LABELS: Record<string, string> = {
		session_create: 'Session Created',
		session_delete: 'Session Deleted',
		session_timeout: 'Session Timed Out',
		session_connect: 'Session Connected',
		session_disconnect: 'Session Disconnected',
		session_keepalive: 'Session Keep-Alive',
		power_control: 'Power Control',
		ipmi_session: 'IPMI Session',
		login: 'Login',
		logout: 'Logout',
	};

	let entries: AuditEntry[] = $state([]);
	let loading = $state(true);
	let loadingMore = $state(false);
	let error = $state('');
	let hasMore = $state(true);

	let filterEventType = $state('');
	let filterServerName = $state('');
	let filterUserEmail = $state('');

	function eventLabel(eventType: string): string {
		return EVENT_LABELS[eventType] || eventType.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
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

	function buildParams(offset: number) {
		const params: Parameters<typeof fetchAuditLog>[0] = {
			limit: PAGE_SIZE,
			offset,
		};
		if (filterEventType.trim()) params.event_type = filterEventType.trim();
		if (filterServerName.trim()) params.server_name = filterServerName.trim();
		if (filterUserEmail.trim()) params.user_email = filterUserEmail.trim();
		return params;
	}

	async function loadEntries() {
		try {
			loading = true;
			error = '';
			const result = await fetchAuditLog(buildParams(0));
			entries = result;
			hasMore = result.length === PAGE_SIZE;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to load audit log';
		} finally {
			loading = false;
		}
	}

	async function loadMore() {
		try {
			loadingMore = true;
			error = '';
			const result = await fetchAuditLog(buildParams(entries.length));
			entries = [...entries, ...result];
			hasMore = result.length === PAGE_SIZE;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to load more entries';
		} finally {
			loadingMore = false;
		}
	}

	function applyFilters() {
		loadEntries();
	}

	function clearFilters() {
		filterEventType = '';
		filterServerName = '';
		filterUserEmail = '';
		loadEntries();
	}

	$effect(() => {
		loadEntries();
	});
</script>

<div class="mx-auto max-w-7xl px-4 py-8 sm:px-6 lg:px-8">
	<div class="mb-6">
		<a href="/" class="text-sm text-muted hover:text-dark transition-colors">&larr; Back to Infrastructure</a>
	</div>

	<div class="mb-8">
		<h1 class="text-2xl font-bold text-dark">Audit Log</h1>
		<p class="mt-1 text-sm text-muted">View system activity and session history.</p>
	</div>

	<!-- Filters -->
	<div class="mb-6 rounded-lg border border-light-200 bg-light-50 p-4">
		<div class="flex flex-wrap items-end gap-3">
			<div class="flex flex-col gap-1">
				<label for="filter-event" class="text-xs font-medium text-muted">Event Type</label>
				<input
					id="filter-event"
					type="text"
					bind:value={filterEventType}
					placeholder="e.g. session_create"
					class="h-9 rounded-md border border-light-200 bg-light px-3 text-sm text-dark placeholder:text-light-400 focus:border-primary focus:outline-none focus:ring-1 focus:ring-primary"
				/>
			</div>
			<div class="flex flex-col gap-1">
				<label for="filter-server" class="text-xs font-medium text-muted">Server Name</label>
				<input
					id="filter-server"
					type="text"
					bind:value={filterServerName}
					placeholder="e.g. brock"
					class="h-9 rounded-md border border-light-200 bg-light px-3 text-sm text-dark placeholder:text-light-400 focus:border-primary focus:outline-none focus:ring-1 focus:ring-primary"
				/>
			</div>
			<div class="flex flex-col gap-1">
				<label for="filter-user" class="text-xs font-medium text-muted">User Email</label>
				<input
					id="filter-user"
					type="text"
					bind:value={filterUserEmail}
					placeholder="e.g. admin@example.com"
					class="h-9 rounded-md border border-light-200 bg-light px-3 text-sm text-dark placeholder:text-light-400 focus:border-primary focus:outline-none focus:ring-1 focus:ring-primary"
				/>
			</div>
			<div class="flex gap-2">
				<Button onclick={applyFilters} size="small" variant="filled" color="primary">
					Filter
				</Button>
				<Button onclick={clearFilters} size="small" variant="outline" color="secondary">
					Clear
				</Button>
			</div>
		</div>
	</div>

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
			<span class="ml-3 text-muted">Loading audit log...</span>
		</div>
	{:else if entries.length === 0}
		<!-- Empty state -->
		<div class="rounded-lg border border-light-200 bg-light-50 py-16 text-center">
			<p class="text-muted">No audit entries found.</p>
			{#if filterEventType || filterServerName || filterUserEmail}
				<p class="mt-1 text-sm text-muted">Try adjusting your filters.</p>
			{/if}
		</div>
	{:else}
		<!-- Table -->
		<div class="overflow-x-auto rounded-lg border border-light-200">
			<table class="w-full text-left text-sm">
				<thead>
					<tr class="border-b border-light-200 bg-light-50">
						<th class="whitespace-nowrap px-4 py-3 font-medium text-muted">Time</th>
						<th class="whitespace-nowrap px-4 py-3 font-medium text-muted">Event</th>
						<th class="whitespace-nowrap px-4 py-3 font-medium text-muted">Server</th>
						<th class="whitespace-nowrap px-4 py-3 font-medium text-muted">User</th>
						<th class="whitespace-nowrap px-4 py-3 font-medium text-muted">IP</th>
						<th class="whitespace-nowrap px-4 py-3 font-medium text-muted">Session ID</th>
					</tr>
				</thead>
				<tbody>
					{#each entries as entry (entry.id)}
						<tr class="border-b border-light-200 last:border-b-0 hover:bg-light-50 transition-colors">
							<td class="whitespace-nowrap px-4 py-3 text-muted" title={formatTimeFull(entry.timestamp)}>
								{formatTime(entry.timestamp)}
							</td>
							<td class="whitespace-nowrap px-4 py-3 text-dark font-medium">
								{eventLabel(entry.event_type)}
							</td>
							<td class="whitespace-nowrap px-4 py-3 text-dark">
								{entry.server_name || '--'}
							</td>
							<td class="whitespace-nowrap px-4 py-3 text-dark">
								{entry.user_email || '--'}
							</td>
							<td class="whitespace-nowrap px-4 py-3 font-mono text-xs text-muted">
								{entry.remote_addr || '--'}
							</td>
							<td class="whitespace-nowrap px-4 py-3 font-mono text-xs text-muted">
								{entry.session_id ? entry.session_id.slice(0, 8) : '--'}
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>

		<!-- Load more -->
		{#if hasMore}
			<div class="mt-4 flex justify-center">
				<Button
					onclick={loadMore}
					disabled={loadingMore}
					size="small"
					variant="outline"
					color="secondary"
					loading={loadingMore}
				>
					{#if loadingMore}
						Loading...
					{:else}
						Load More
					{/if}
				</Button>
			</div>
		{/if}

		<p class="mt-3 text-center text-xs text-muted">
			Showing {entries.length} {entries.length === 1 ? 'entry' : 'entries'}
		</p>
	{/if}
</div>
