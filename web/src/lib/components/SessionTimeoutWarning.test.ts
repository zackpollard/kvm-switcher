import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import SessionTimeoutWarning from './SessionTimeoutWarning.svelte';

// Mock the api module
vi.mock('$lib/api', () => ({
	keepAliveSession: vi.fn().mockResolvedValue(undefined)
}));

describe('SessionTimeoutWarning', () => {
	it('does not render when remaining time is above warning threshold', () => {
		render(SessionTimeoutWarning, {
			props: { sessionId: 'test-1', remainingSeconds: 600 }
		});
		expect(screen.queryByText(/Session/)).toBeNull();
	});

	it('renders warning when remaining time is below 5 minutes', () => {
		render(SessionTimeoutWarning, {
			props: { sessionId: 'test-1', remainingSeconds: 250 }
		});
		expect(screen.getByText(/Session idle timeout/)).toBeTruthy();
		expect(screen.getByText('Stay connected')).toBeTruthy();
	});

	it('renders critical when remaining time is below 1 minute', () => {
		render(SessionTimeoutWarning, {
			props: { sessionId: 'test-1', remainingSeconds: 45 }
		});
		expect(screen.getByText(/Session expires in/)).toBeTruthy();
	});

	it('shows Stay connected button', () => {
		render(SessionTimeoutWarning, {
			props: { sessionId: 'test-1', remainingSeconds: 200 }
		});
		const button = screen.getByText('Stay connected');
		expect(button).toBeTruthy();
	});
});
