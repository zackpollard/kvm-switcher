import { describe, it, expect, vi, beforeEach } from 'vitest';
import { fetchServers, createSession, getSession, deleteSession, keepAliveSession, fetchServerStatuses, kvmPowerControl, kvmResetVideo, kvmDisplayLock, kvmMouseMode, kvmKeyboardLayout, createIPMISession } from './api';

const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

beforeEach(() => {
	mockFetch.mockReset();
});

describe('fetchServers', () => {
	it('returns servers on success', async () => {
		const servers = [{ name: 'server-1', bmc_ip: '10.0.0.1', bmc_port: 80, board_type: 'ami_megarac', has_active_session: false }];
		mockFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve(servers) });

		const result = await fetchServers();
		expect(result).toEqual(servers);
		expect(mockFetch).toHaveBeenCalledWith('/api/servers');
	});

	it('throws on error', async () => {
		mockFetch.mockResolvedValue({ ok: false, statusText: 'Internal Server Error' });
		await expect(fetchServers()).rejects.toThrow('Failed to fetch servers');
	});
});

describe('createSession', () => {
	it('returns session on success', async () => {
		const session = { id: 'abc123', server_name: 'server-1', status: 'starting' };
		mockFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve(session) });

		const result = await createSession('server-1');
		expect(result).toEqual(session);
		expect(mockFetch).toHaveBeenCalledWith('/api/sessions', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ server_name: 'server-1' })
		});
	});

	it('throws with error message from response', async () => {
		mockFetch.mockResolvedValue({
			ok: false,
			statusText: 'Too Many Requests',
			json: () => Promise.resolve({ error: 'maximum concurrent sessions reached' })
		});
		await expect(createSession('server-1')).rejects.toThrow('maximum concurrent sessions reached');
	});

	it('falls back to statusText when json parse fails', async () => {
		mockFetch.mockResolvedValue({
			ok: false,
			statusText: 'Bad Gateway',
			json: () => Promise.reject(new Error('parse error'))
		});
		await expect(createSession('server-1')).rejects.toThrow('Bad Gateway');
	});
});

describe('getSession', () => {
	it('returns session with timeout remaining', async () => {
		const session = {
			id: 'abc123',
			server_name: 'server-1',
			status: 'connected',
			idle_timeout_remaining_seconds: 1500
		};
		mockFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve(session) });

		const result = await getSession('abc123');
		expect(result.idle_timeout_remaining_seconds).toBe(1500);
	});
});

describe('deleteSession', () => {
	it('calls DELETE endpoint', async () => {
		mockFetch.mockResolvedValue({ ok: true });
		await deleteSession('abc123');
		expect(mockFetch).toHaveBeenCalledWith('/api/sessions/abc123', { method: 'DELETE' });
	});
});

describe('keepAliveSession', () => {
	it('calls PATCH keepalive endpoint', async () => {
		mockFetch.mockResolvedValue({ ok: true });
		await keepAliveSession('abc123');
		expect(mockFetch).toHaveBeenCalledWith('/api/sessions/abc123/keepalive', { method: 'PATCH' });
	});

	it('throws on error', async () => {
		mockFetch.mockResolvedValue({ ok: false, statusText: 'Not Found' });
		await expect(keepAliveSession('abc123')).rejects.toThrow('Failed to keep alive session');
	});
});

describe('fetchServerStatuses', () => {
	it('returns statuses with circuit breaker state', async () => {
		const statuses = {
			'server-1': { online: true, power_state: 'on', circuit_breaker_state: 'closed' },
			'server-2': { online: false, circuit_breaker_state: 'open' }
		};
		mockFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve(statuses) });

		const result = await fetchServerStatuses();
		expect(result['server-1'].circuit_breaker_state).toBe('closed');
		expect(result['server-2'].circuit_breaker_state).toBe('open');
	});
});

describe('kvmPowerControl', () => {
	it('sends power action on success', async () => {
		mockFetch.mockResolvedValue({ ok: true });
		await kvmPowerControl('sess-1', 'reset');
		expect(mockFetch).toHaveBeenCalledWith('/api/sessions/sess-1/power', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ action: 'reset' })
		});
	});

	it('throws with error message from response', async () => {
		mockFetch.mockResolvedValue({
			ok: false,
			statusText: 'Internal Server Error',
			json: () => Promise.resolve({ error: 'BMC unreachable' })
		});
		await expect(kvmPowerControl('sess-1', 'on')).rejects.toThrow('BMC unreachable');
	});

	it('falls back to statusText when json parse fails', async () => {
		mockFetch.mockResolvedValue({
			ok: false,
			statusText: 'Bad Gateway',
			json: () => Promise.reject(new Error('parse error'))
		});
		await expect(kvmPowerControl('sess-1', 'off')).rejects.toThrow('Bad Gateway');
	});
});

describe('kvmResetVideo', () => {
	it('calls reset-video endpoint', async () => {
		mockFetch.mockResolvedValue({ ok: true });
		await kvmResetVideo('sess-1');
		expect(mockFetch).toHaveBeenCalledWith('/api/sessions/sess-1/reset-video', {
			method: 'POST'
		});
	});
});

describe('kvmDisplayLock', () => {
	it('sends lock=true', async () => {
		mockFetch.mockResolvedValue({ ok: true });
		await kvmDisplayLock('sess-1', true);
		expect(mockFetch).toHaveBeenCalledWith('/api/sessions/sess-1/display-lock', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ lock: true })
		});
	});

	it('sends lock=false', async () => {
		mockFetch.mockResolvedValue({ ok: true });
		await kvmDisplayLock('sess-1', false);
		expect(mockFetch).toHaveBeenCalledWith('/api/sessions/sess-1/display-lock', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ lock: false })
		});
	});
});

describe('kvmMouseMode', () => {
	it('sends mouse mode', async () => {
		mockFetch.mockResolvedValue({ ok: true });
		await kvmMouseMode('sess-1', 'absolute');
		expect(mockFetch).toHaveBeenCalledWith('/api/sessions/sess-1/mouse-mode', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ mode: 'absolute' })
		});
	});
});

describe('kvmKeyboardLayout', () => {
	it('sends keyboard layout', async () => {
		mockFetch.mockResolvedValue({ ok: true });
		await kvmKeyboardLayout('sess-1', 'de');
		expect(mockFetch).toHaveBeenCalledWith('/api/sessions/sess-1/keyboard-layout', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ layout: 'de' })
		});
	});
});

describe('deleteSession (error path)', () => {
	it('throws on error', async () => {
		mockFetch.mockResolvedValue({ ok: false, statusText: 'Not Found' });
		await expect(deleteSession('bad-id')).rejects.toThrow('Failed to delete session');
	});
});

describe('createSession (409 conflict)', () => {
	it('throws with server error on 409', async () => {
		mockFetch.mockResolvedValue({
			ok: false,
			statusText: 'Conflict',
			json: () => Promise.resolve({ error: 'session already exists for this server' })
		});
		await expect(createSession('server-1')).rejects.toThrow('session already exists for this server');
	});
});

describe('createIPMISession', () => {
	it('returns IPMI session data on success', async () => {
		const ipmiSession = {
			board_type: 'ami_megarac',
			session_cookie: 'cookie123',
			csrf_token: 'csrf456',
			username: 'admin',
			privilege: 4,
			extended_priv: 256
		};
		mockFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve(ipmiSession) });

		const result = await createIPMISession('server-1');
		expect(result).toEqual(ipmiSession);
		expect(mockFetch).toHaveBeenCalledWith('/api/ipmi-session/server-1', { method: 'POST' });
	});

	it('throws with error message on failure', async () => {
		mockFetch.mockResolvedValue({
			ok: false,
			statusText: 'Unauthorized',
			json: () => Promise.resolve({ error: 'BMC login failed' })
		});
		await expect(createIPMISession('server-1')).rejects.toThrow('BMC login failed');
	});
});
