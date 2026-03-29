<script lang="ts">
	import { onMount, onDestroy } from 'svelte';

	let { wsUrl, container, ondisconnect, password }: { wsUrl: string; container: HTMLDivElement; ondisconnect?: () => void; password?: string } = $props();

	let canvasContainer: HTMLDivElement;
	let rfb: any = null;
	let status = $state('Connecting...');
	let connected = $state(false);
	let ctrlAltDelHandler: (() => void) | null = null;

	onMount(async () => {
		const { default: RFB } = await import('@novnc/novnc');

		try {
			rfb = new RFB(canvasContainer, wsUrl, {
				wsProtocols: ['binary']
			});

			rfb.scaleViewport = true;
			rfb.resizeSession = false;
			rfb.showDotCursor = true;

			rfb.addEventListener('connect', () => {
				status = 'Connected';
				connected = true;
			});

			rfb.addEventListener('disconnect', (e: any) => {
				status = e.detail.clean ? 'Disconnected' : 'Connection lost';
				connected = false;
				rfb = null;
				ondisconnect?.();
			});

			rfb.addEventListener('credentialsrequired', () => {
				if (password && rfb) {
					rfb.sendCredentials({ password });
				} else {
					status = 'Credentials required';
				}
			});

			rfb.addEventListener('desktopname', (e: any) => {
				status = `Connected - ${e.detail.name}`;
			});
		} catch (e) {
			status = `Connection failed: ${e instanceof Error ? e.message : 'Unknown error'}`;
		}

		ctrlAltDelHandler = () => {
			if (rfb) {
				rfb.sendCtrlAltDel();
			}
		};
		container?.addEventListener('send-ctrl-alt-del', ctrlAltDelHandler);
	});

	onDestroy(() => {
		if (ctrlAltDelHandler) {
			container?.removeEventListener('send-ctrl-alt-del', ctrlAltDelHandler);
		}
		if (rfb) {
			rfb.disconnect();
			rfb = null;
		}
	});
</script>

<div class="flex h-full flex-col overflow-hidden">
	{#if !connected}
		<div class="bg-gray-900/80 px-3 py-1.5 text-center text-xs text-gray-400" role="status" aria-live="polite">
			{status}
		</div>
	{/if}
	<div bind:this={canvasContainer} class="relative min-h-0 flex-1"></div>
</div>
