<script lang="ts">
	import { onMount } from 'svelte';

	let { wsUrl, container, ondisconnect, password }: { wsUrl: string; container: HTMLDivElement; ondisconnect?: () => void; password?: string } = $props();

	let canvasContainer: HTMLDivElement;
	let rfb: any = null;
	let status = $state('Connecting...');
	let connected = $state(false);

	const t0 = performance.now();
	const log = (msg: string) => console.log(`[KVM +${(performance.now() - t0).toFixed(0)}ms] ${msg}`);

	log('KVMViewer component created');

	onMount(async () => {
		log('onMount start');

		log('importing noVNC...');
		const { default: RFB } = await import('@novnc/novnc');
		log('noVNC imported');

		try {
			log(`connecting WebSocket to ${wsUrl}`);
			rfb = new RFB(canvasContainer, wsUrl, {
				wsProtocols: ['binary']
			});

			rfb.scaleViewport = true;
			rfb.resizeSession = false;
			rfb.showDotCursor = true;

			rfb.addEventListener('connect', () => {
				log('VNC connected');
				status = 'Connected';
				connected = true;
			});

			rfb.addEventListener('disconnect', (e: any) => {
				log(`VNC disconnected (clean=${e.detail.clean})`);
				status = e.detail.clean ? 'Disconnected' : 'Connection lost';
				connected = false;
				rfb = null;
				ondisconnect?.();
			});

			rfb.addEventListener('credentialsrequired', () => {
				log('credentials required');
				if (password && rfb) {
					rfb.sendCredentials({ password });
				} else {
					status = 'Credentials required';
				}
			});

			rfb.addEventListener('desktopname', (e: any) => {
				log(`desktop name: ${e.detail.name}`);
				status = `Connected - ${e.detail.name}`;
			});
		} catch (e) {
			log(`connection failed: ${e}`);
			status = `Connection failed: ${e instanceof Error ? e.message : 'Unknown error'}`;
		}

		const ctrlAltDelHandler = () => {
			if (rfb) {
				rfb.sendCtrlAltDel();
			}
		};
		container?.addEventListener('send-ctrl-alt-del', ctrlAltDelHandler);

		return () => {
			container?.removeEventListener('send-ctrl-alt-del', ctrlAltDelHandler);
			if (rfb) {
				rfb.disconnect();
				rfb = null;
			}
		};
	});
</script>

<div class="flex h-full flex-col overflow-hidden">
	{#if !connected}
		<div class="bg-gray-900/80 px-3 py-1.5 text-center text-xs text-gray-400">
			{status}
		</div>
	{/if}
	<div bind:this={canvasContainer} class="relative min-h-0 flex-1"></div>
</div>
