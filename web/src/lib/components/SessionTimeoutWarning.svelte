<script lang="ts">
	import { keepAliveSession } from '$lib/api';
	import { Button } from '@immich/ui';

	interface Props {
		sessionId: string;
		remainingSeconds: number;
	}

	let { sessionId, remainingSeconds }: Props = $props();

	let localCountdown = $state(0);
	let keepingAlive = $state(false);
	let lastPropValue = $state(0);

	const WARNING_THRESHOLD = 300; // 5 minutes
	const CRITICAL_THRESHOLD = 60; // 1 minute

	// When the prop changes (from a fresh fetch), reset the local countdown
	$effect(() => {
		if (remainingSeconds !== lastPropValue) {
			localCountdown = remainingSeconds;
			lastPropValue = remainingSeconds;
		}
	});

	// Count down every second
	$effect(() => {
		if (localCountdown <= 0) return;
		const interval = setInterval(() => {
			localCountdown = Math.max(0, localCountdown - 1);
		}, 1000);
		return () => clearInterval(interval);
	});

	async function handleKeepAlive() {
		keepingAlive = true;
		try {
			await keepAliveSession(sessionId);
		} catch (e) {
			console.error('Keep alive failed:', e);
		} finally {
			keepingAlive = false;
		}
	}

	function formatTime(seconds: number): string {
		const m = Math.floor(seconds / 60);
		const s = Math.floor(seconds % 60);
		return m > 0 ? `${m}m ${s}s` : `${s}s`;
	}

	let level = $derived(
		localCountdown <= CRITICAL_THRESHOLD ? 'critical' :
		localCountdown <= WARNING_THRESHOLD ? 'warning' :
		'ok'
	);
</script>

{#if level !== 'ok'}
	<div
		role="alert"
		aria-live="assertive"
		class="flex items-center gap-3 px-4 py-2 text-sm {level === 'critical'
			? 'bg-danger-100 text-danger'
			: 'bg-warning-100 text-warning'}"
	>
		<span>
			{#if level === 'critical'}
				Session expires in <strong>{formatTime(localCountdown)}</strong>
			{:else}
				Session idle timeout in <strong>{formatTime(localCountdown)}</strong>
			{/if}
		</span>
		<Button
			onclick={handleKeepAlive}
			disabled={keepingAlive}
			size="tiny"
			variant="outline"
			color={level === 'critical' ? 'danger' : 'warning'}
			class="!rounded-xl"
		>
			{keepingAlive ? 'Extending...' : 'Stay connected'}
		</Button>
	</div>
{/if}
