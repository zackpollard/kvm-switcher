<script lang="ts">
	import type { Snippet } from 'svelte';

	let { header, children, footer, class: className = '' }: {
		header?: Snippet;
		children?: Snippet;
		footer?: Snippet;
		class?: string;
	} = $props();
</script>

<div class="rounded-xl border border-light-200 bg-light-50 shadow-sm transition-colors hover:border-light-400 {className}">
	{#if header}
		<div class="px-5 py-4">
			{@render header()}
		</div>
	{/if}

	{#if children}
		<div class="px-5 py-3">
			{@render children()}
		</div>
	{/if}

	{#if footer}
		<div class="border-t border-light-200 px-5 py-3">
			{@render footer()}
		</div>
	{/if}
</div>
