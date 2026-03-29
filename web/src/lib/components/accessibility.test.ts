import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import { tick } from 'svelte';
import { readFileSync } from 'fs';
import { resolve } from 'path';
import SessionTimeoutWarning from './SessionTimeoutWarning.svelte';
import KVMViewer from './KVMViewer.svelte';

// Mock the api module
vi.mock('$lib/api', () => ({
	keepAliveSession: vi.fn().mockResolvedValue(undefined)
}));

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
				mockRfbInstance = this;
			}
		}
	};
});

/** Flush microtask queue then flush Svelte reactivity */
async function flushAll() {
	await new Promise((r) => setTimeout(r, 0));
	await tick();
}

describe('SessionTimeoutWarning accessibility', () => {
	it('warning container has role="alert" for screen reader announcements', () => {
		const { container } = render(SessionTimeoutWarning, {
			props: { sessionId: 'test-a11y', remainingSeconds: 250 }
		});
		const alertEl = container.querySelector('[role="alert"]');
		expect(alertEl).not.toBeNull();
	});

	it('warning container has aria-live attribute for dynamic content updates', () => {
		const { container } = render(SessionTimeoutWarning, {
			props: { sessionId: 'test-a11y', remainingSeconds: 250 }
		});
		const liveEl = container.querySelector('[aria-live]');
		expect(liveEl).not.toBeNull();
	});

	it('critical warning also has role="alert"', () => {
		const { container } = render(SessionTimeoutWarning, {
			props: { sessionId: 'test-a11y', remainingSeconds: 45 }
		});
		const alertEl = container.querySelector('[role="alert"]');
		expect(alertEl).not.toBeNull();
	});

	it('Stay connected button is keyboard accessible (is a <button> element)', () => {
		render(SessionTimeoutWarning, {
			props: { sessionId: 'test-a11y', remainingSeconds: 200 }
		});
		const button = screen.getByText('Stay connected');
		expect(button.tagName).toBe('BUTTON');
	});
});

describe('KVMViewer accessibility', () => {
	let container: HTMLDivElement;

	beforeEach(() => {
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

	it('status text is visible for screen readers when not connected', () => {
		renderKVMViewer();
		const statusEl = screen.getByText('Connecting...');
		expect(statusEl).toBeTruthy();
		// The status text must be in a visible element (not display:none or visibility:hidden)
		expect(statusEl).toBeVisible();
	});

	it('status region has aria-live attribute for dynamic status updates', () => {
		const { container: rendered } = renderKVMViewer();
		const liveEl = rendered.querySelector('[aria-live]');
		expect(liveEl).not.toBeNull();
	});

	it('canvas container div exists for noVNC to attach to', () => {
		const { container: rendered } = renderKVMViewer();
		// The component renders a flex-col div with a child div for the canvas
		const canvasDiv = rendered.querySelector('.flex-1');
		expect(canvasDiv).not.toBeNull();
	});

	it('disconnected status is announced to screen readers', async () => {
		const { container: rendered } = renderKVMViewer();
		await flushAll();

		rfbEventHandlers['connect']();
		rfbEventHandlers['disconnect']({ detail: { clean: true } });
		await tick();

		const statusEl = screen.getByText('Disconnected');
		expect(statusEl).toBeVisible();
	});
});

describe('app.html structure', () => {
	it('has lang="en" on the html element', () => {
		const appHtmlPath = resolve(__dirname, '../../app.html');
		const html = readFileSync(appHtmlPath, 'utf-8');
		expect(html).toMatch(/<html[^>]+lang="en"/);
	});

	it('has meta charset defined', () => {
		const appHtmlPath = resolve(__dirname, '../../app.html');
		const html = readFileSync(appHtmlPath, 'utf-8');
		expect(html).toMatch(/<meta[^>]+charset="utf-8"/i);
	});

	it('has viewport meta tag for responsive design', () => {
		const appHtmlPath = resolve(__dirname, '../../app.html');
		const html = readFileSync(appHtmlPath, 'utf-8');
		expect(html).toMatch(/<meta[^>]+name="viewport"/);
	});
});
