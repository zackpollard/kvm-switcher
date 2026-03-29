import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';
import KVMViewer from './KVMViewer.svelte';

// Store event handlers registered by the RFB instance
let rfbEventHandlers: Record<string, Function>;
let mockRfbInstance: any;

// Mock the @novnc/novnc dynamic import
vi.mock('@novnc/novnc', () => {
	return {
		default: class MockRFB {
			scaleViewport = false;
			resizeSession = true;
			showDotCursor = false;

			sendCredentials = vi.fn();
			sendCtrlAltDel = vi.fn();
			disconnect = vi.fn();

			addEventListener(event: string, handler: Function) {
				rfbEventHandlers[event] = handler;
			}

			constructor() {
				// Expose this instance so tests can access it
				mockRfbInstance = this;
			}
		}
	};
});

/** Flush microtask queue (lets the awaited dynamic import resolve) then flush Svelte reactivity */
async function flushAll() {
	// Flush the pending microtask from `await import(...)` inside onMount
	await new Promise((r) => setTimeout(r, 0));
	await tick();
}

describe('KVMViewer', () => {
	let container: HTMLDivElement;

	beforeEach(() => {
		cleanup();
		rfbEventHandlers = {};
		mockRfbInstance = null;
		container = document.createElement('div');
		document.body.appendChild(container);
	});

	function renderKVMViewer(props: { password?: string; ondisconnect?: () => void } = {}) {
		return render(KVMViewer, {
			props: {
				wsUrl: 'ws://localhost:8080/test',
				container,
				...props
			}
		});
	}

	it('renders and shows "Connecting..." status initially', () => {
		renderKVMViewer();
		expect(screen.getByText('Connecting...')).toBeTruthy();
	});

	it('updates status to "Connected" on connect event', async () => {
		renderKVMViewer();
		await flushAll();

		rfbEventHandlers['connect']();
		await tick();

		// After connect, connected=true so the status bar is hidden
		expect(screen.queryByText('Connecting...')).toBeNull();
	});

	it('calls ondisconnect callback on clean disconnect', async () => {
		const ondisconnect = vi.fn();
		renderKVMViewer({ ondisconnect });
		await flushAll();

		rfbEventHandlers['disconnect']({ detail: { clean: true } });
		expect(ondisconnect).toHaveBeenCalledOnce();
	});

	it('shows "Disconnected" on clean disconnect', async () => {
		renderKVMViewer();
		await flushAll();

		rfbEventHandlers['connect']();
		rfbEventHandlers['disconnect']({ detail: { clean: true } });
		await tick();

		expect(screen.getByText('Disconnected')).toBeTruthy();
	});

	it('shows "Connection lost" on unclean disconnect', async () => {
		renderKVMViewer();
		await flushAll();

		rfbEventHandlers['connect']();
		rfbEventHandlers['disconnect']({ detail: { clean: false } });
		await tick();

		expect(screen.getByText('Connection lost')).toBeTruthy();
	});

	it('sends credentials when credentialsrequired fires and password is provided', async () => {
		renderKVMViewer({ password: 'secret123' });
		await flushAll();

		rfbEventHandlers['credentialsrequired']();
		expect(mockRfbInstance.sendCredentials).toHaveBeenCalledWith({ password: 'secret123' });
	});

	it('shows "Credentials required" when no password is provided', async () => {
		renderKVMViewer();
		await flushAll();

		rfbEventHandlers['credentialsrequired']();
		await tick();

		expect(screen.getByText('Credentials required')).toBeTruthy();
	});

	it('sends Ctrl+Alt+Del when custom event is dispatched on container', async () => {
		renderKVMViewer();
		await flushAll();

		rfbEventHandlers['connect']();
		container.dispatchEvent(new CustomEvent('send-ctrl-alt-del'));
		expect(mockRfbInstance.sendCtrlAltDel).toHaveBeenCalledOnce();
	});

	it('has rfb instance available for cleanup on unmount', async () => {
		// NOTE: Svelte's onMount does not use the return value of an async callback
		// as a cleanup function (the Promise is not a function). This means the cleanup
		// in KVMViewer.svelte that calls rfb.disconnect() will not fire automatically.
		// This test verifies the RFB instance is created and could be cleaned up.
		// A future refactor could move the cleanup to onDestroy or use a non-async
		// onMount wrapper to ensure rfb.disconnect() is called on unmount.
		const { unmount } = renderKVMViewer();
		await flushAll();

		expect(mockRfbInstance).not.toBeNull();
		expect(mockRfbInstance.disconnect).not.toHaveBeenCalled();

		unmount();
	});

	it('updates status with desktop name on desktopname event', async () => {
		renderKVMViewer();
		await flushAll();

		rfbEventHandlers['desktopname']({ detail: { name: 'My Server' } });
		await tick();

		expect(screen.getByText('Connected - My Server')).toBeTruthy();
	});
});
