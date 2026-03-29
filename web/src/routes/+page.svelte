<script lang="ts">
	import { fetchServers, createSession, createIPMISession, fetchServerStatuses, type ServerInfo, type DeviceStatus } from '$lib/api';
	import { goto } from '$app/navigation';
	import { Alert, Badge, Button, LoadingSpinner, Card, CardHeader, CardBody, CardFooter } from '@immich/ui';

	let servers: ServerInfo[] = $state([]);
	let statuses: Record<string, DeviceStatus> = $state({});
	let loading = $state(true);
	let error = $state('');
	let connecting = $state<string | null>(null);
	let openingIPMI = $state<string | null>(null);

	const TYPE_LABELS: Record<string, string> = {
		ami_megarac: 'Servers',
		dell_idrac8: 'Servers',
		dell_idrac9: 'Servers',
		nanokvm: 'Servers',
		apc_ups: 'Power',
	};

	const TYPE_ORDER = ['Servers', 'Power'];

	function groupedServers() {
		const groups: Record<string, ServerInfo[]> = {};
		for (const s of servers) {
			const group = TYPE_LABELS[s.board_type] || 'Other';
			(groups[group] ??= []).push(s);
		}
		return TYPE_ORDER.filter((g) => groups[g]).map((g) => ({ label: g, servers: groups[g] }));
	}

	function boardLabel(bt: string): string {
		const labels: Record<string, string> = {
			ami_megarac: 'MegaRAC',
			dell_idrac8: 'iDRAC8',
			dell_idrac9: 'iDRAC9',
			nanokvm: 'NanoKVM',
			apc_ups: 'APC NMC',
		};
		return labels[bt] || bt;
	}

	function healthColor(h?: string): string {
		if (h === 'ok' || h === 'OK') return 'text-success';
		if (h === 'warning' || h === 'Warning') return 'text-warning';
		if (h === 'critical' || h === 'Critical') return 'text-danger';
		return 'text-muted';
	}

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

	async function loadStatuses() {
		try {
			statuses = await fetchServerStatuses();
		} catch {
			// Silently fail — statuses are supplementary
		}
	}

	async function openIPMI(serverName: string) {
		try {
			openingIPMI = serverName;
			error = '';
			const session = await createIPMISession(serverName);

			if (session.board_type === 'ami_megarac') {
				document.cookie = `SessionCookie=${session.session_cookie};path=/`;
				document.cookie = `CSRFTOKEN=${session.csrf_token};path=/`;
				document.cookie = `Username=${session.username};path=/`;
				document.cookie = `PNO=${session.privilege};path=/`;
				document.cookie = `Extendedpriv=${session.extended_priv};path=/`;
				document.cookie = `settings={};path=/`;
				document.cookie = 'SessionExpired=;expires=Thu, 01 Jan 1970 00:00:00 GMT;path=/';
			}

			window.open(`/ipmi/${serverName}/`, '_blank');
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to open IPMI';
		} finally {
			openingIPMI = null;
		}
	}

	async function connect(serverName: string, existingSessionId?: string) {
		try {
			connecting = serverName;
			error = '';
			if (existingSessionId) {
				// Reuse existing session — skip BMC auth, connect instantly
				goto(`/kvm/${existingSessionId}`);
				return;
			}
			const session = await createSession(serverName);
			goto(`/kvm/${session.id}`);
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to connect';
			connecting = null;
		}
	}

	$effect(() => {
		loadServers();
		loadStatuses();
		const serverInterval = setInterval(loadServers, 10000);
		const statusInterval = setInterval(loadStatuses, 30000);
		return () => {
			clearInterval(serverInterval);
			clearInterval(statusInterval);
		};
	});
</script>

<div class="mx-auto max-w-7xl px-4 py-8 sm:px-6 lg:px-8">
	<div class="mb-8 flex items-center justify-between">
		<div>
			<h1 class="text-2xl font-bold text-dark">Infrastructure</h1>
			<p class="mt-1 text-sm text-muted">Manage servers, KVM consoles, and power devices.</p>
		</div>
		<Button
			onclick={() => { loadServers(); loadStatuses(); }}
			size="small"
			variant="outline"
			color="secondary"
		>
			Refresh
		</Button>
	</div>

	{#if error}
		<div class="mb-6" role="alert">
			<Alert color="danger" title={error} />
		</div>
	{/if}

	{#if loading && servers.length === 0}
		<div class="flex items-center justify-center py-20" aria-live="polite">
			<LoadingSpinner size="large" />
			<span class="ml-3 text-muted">Loading...</span>
		</div>
	{:else if servers.length === 0}
		<Card color="secondary">
			<CardBody>
				<div class="py-10 text-center">
					<p class="text-muted">No devices configured.</p>
					<p class="mt-1 text-sm text-muted">Add servers to configs/servers.yaml</p>
				</div>
			</CardBody>
		</Card>
	{:else}
		{#each groupedServers() as group}
			<div class="mb-8">
				<h2 class="mb-4 text-lg font-semibold text-dark">{group.label}</h2>
				<div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
					{#each group.servers as server}
						{@const st = statuses[server.name]}
						<Card color="secondary" class="transition-colors hover:border-light-400">
							<CardHeader>
								<div class="flex items-start justify-between">
									<div class="flex items-center gap-2">
										{#if st}
											<span class="h-2.5 w-2.5 rounded-full {st.online ? 'bg-success' : 'bg-danger'}" title="{st.online ? 'Online' : 'Offline'}" aria-label="{st.online ? 'Online' : 'Offline'}" role="img"></span>
										{:else}
											<span class="h-2.5 w-2.5 rounded-full bg-light-400" title="Unknown" aria-label="Status unknown" role="img"></span>
										{/if}
										<h3 class="text-lg font-semibold text-dark">{server.name} <span class="text-sm font-normal text-muted">{server.bmc_ip}</span></h3>
									</div>
									<Badge size="tiny" color="secondary" shape="round">
										{boardLabel(server.board_type)}
									</Badge>
								</div>
							</CardHeader>

							<CardBody>
								<!-- Stats -->
								<div class="space-y-1 text-sm">
									{#if st?.model}
										<p class="text-dark">{st.model}</p>
									{/if}
									{#if server.board_type !== 'apc_ups'}
										<!-- Server stats -->
										{#if st?.power_state}
											<div class="flex items-center gap-2">
												<span class="text-muted">Power:</span>
												<span class={st.power_state === 'on' ? 'text-success' : 'text-danger'}>
													{st.power_state === 'on' ? 'On' : 'Off'}
												</span>
												{#if st.load_watts}
													<span class="text-muted">({st.load_watts}W)</span>
												{/if}
											</div>
										{/if}
										{#if st?.health}
											<div class="flex items-center gap-2">
												<span class="text-muted">Health:</span>
												<span class={healthColor(st.health)}>{st.health}</span>
											</div>
										{/if}
										{#if st?.temperature_c}
											<div class="flex items-center gap-2">
												<span class="text-muted">Temp:</span>
												<span class="text-dark">{st.temperature_c}&deg;C</span>
											</div>
										{/if}
										{#if st?.app_version || st?.image_version}
											<div class="flex items-center gap-2">
												<span class="text-muted">Version:</span>
												<span class="text-dark">{st.app_version || ''}</span>
												{#if st.image_version}
													<span class="text-muted">({st.image_version})</span>
												{/if}
												{#if st.update_available}
													<Badge size="tiny" color="warning" shape="round">Update</Badge>
												{/if}
											</div>
										{/if}
									{:else}
										<!-- APC UPS/PDU stats -->
										{#if st?.load_watts || st?.load_pct || st?.load_amps}
											<div class="flex items-center gap-2">
												<span class="text-muted">Load:</span>
												{#if st.load_watts}<span class="text-dark">{st.load_watts}W</span>{/if}
												{#if st.load_pct}<span class="text-dark">{st.load_pct}%</span>{/if}
												{#if st.load_amps}<span class="text-muted">({st.load_amps}A)</span>{/if}
											</div>
										{/if}
										{#if st?.voltage}
											<div class="flex items-center gap-2">
												<span class="text-muted">Voltage:</span>
												<span class="text-dark">{st.voltage}V</span>
											</div>
										{/if}
										{#if st?.battery_pct != null && st.battery_pct > 0}
											<div class="flex items-center gap-2">
												<span class="text-muted">Battery:</span>
												<span class={st.battery_pct > 50 ? 'text-success' : st.battery_pct > 20 ? 'text-warning' : 'text-danger'}>
													{st.battery_pct}%
												</span>
												{#if st.runtime_min}
													<span class="text-muted">({st.runtime_min} min)</span>
												{/if}
											</div>
										{/if}
										{#if st?.temperature_c}
											<div class="flex items-center gap-2">
												<span class="text-muted">Temp:</span>
												<span class="text-dark">{st.temperature_c}&deg;C</span>
											</div>
										{/if}
									{/if}
									{#if !st}
										<p class="font-mono text-xs text-muted">{server.bmc_ip}</p>
									{/if}
								</div>
							</CardBody>

							<CardFooter>
								<div class="flex w-full items-center justify-between">
									{#if server.has_active_session}
										<span class="text-xs text-success">Session active</span>
									{:else}
										<span></span>
									{/if}

									<div class="flex items-center gap-2">
										<Button
											onclick={() => openIPMI(server.name)}
											disabled={openingIPMI === server.name}
											size="small"
											variant="outline"
											color="secondary"
											loading={openingIPMI === server.name}
										>
											{#if openingIPMI === server.name}
												Opening...
											{:else}
												{server.board_type === 'apc_ups' ? 'Panel' : server.board_type === 'nanokvm' ? 'KVM' : 'IPMI'}
											{/if}
										</Button>
										{#if server.board_type !== 'apc_ups' && server.board_type !== 'nanokvm'}
											<Button
												onclick={() => connect(server.name, server.active_session_id)}
												disabled={connecting === server.name}
												size="small"
												variant="filled"
												color="primary"
												loading={connecting === server.name}
											>
												{#if connecting === server.name}
													Connecting...
												{:else}
													KVM
												{/if}
											</Button>
										{/if}
									</div>
								</div>
							</CardFooter>
						</Card>
					{/each}
				</div>
			</div>
		{/each}
	{/if}
</div>
