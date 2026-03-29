import { describe, it, expect, vi, beforeEach } from 'vitest';
import { fetchServers, createSession, getSession, deleteSession, keepAliveSession, listSessions, getKVMWebSocketURL, fetchServerStatuses, kvmPowerControl, kvmResetVideo, kvmDisplayLock, kvmMouseMode, kvmKeyboardLayout, createIPMISession } from './api';

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

describe('session management', () => {
	it('getSession returns full session object with all fields', async () => {
		const session = {
			id: 'sess-full',
			server_name: 'server-2',
			bmc_ip: '10.0.0.5',
			status: 'connected',
			conn_mode: 'ikvm_native',
			kvm_password: 'secret123',
			idle_timeout_remaining_seconds: 900,
			error: undefined,
			created_at: '2026-03-29T10:00:00Z',
			last_activity: '2026-03-29T10:05:00Z'
		};
		mockFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve(session) });

		const result = await getSession('sess-full');
		expect(result).toEqual(session);
		expect(result.id).toBe('sess-full');
		expect(result.server_name).toBe('server-2');
		expect(result.bmc_ip).toBe('10.0.0.5');
		expect(result.status).toBe('connected');
		expect(result.conn_mode).toBe('ikvm_native');
		expect(result.kvm_password).toBe('secret123');
		expect(result.idle_timeout_remaining_seconds).toBe(900);
		expect(result.error).toBeUndefined();
		expect(mockFetch).toHaveBeenCalledWith('/api/sessions/sess-full');
	});

	it('getSession throws on 404', async () => {
		mockFetch.mockResolvedValue({ ok: false, statusText: 'Not Found' });
		await expect(getSession('nonexistent')).rejects.toThrow('Failed to get session: Not Found');
	});

	it('listSessions returns array of sessions', async () => {
		const sessions = [
			{ id: 'sess-1', server_name: 'server-1', status: 'connected', bmc_ip: '10.0.0.1', created_at: '2026-03-29T10:00:00Z', last_activity: '2026-03-29T10:05:00Z' },
			{ id: 'sess-2', server_name: 'server-2', status: 'starting', bmc_ip: '10.0.0.2', created_at: '2026-03-29T10:01:00Z', last_activity: '2026-03-29T10:01:00Z' }
		];
		mockFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve(sessions) });

		const result = await listSessions();
		expect(result).toEqual(sessions);
		expect(result).toHaveLength(2);
		expect(mockFetch).toHaveBeenCalledWith('/api/sessions');
	});

	it('keepAliveSession calls PATCH endpoint', async () => {
		mockFetch.mockResolvedValue({ ok: true });
		await keepAliveSession('sess-ka');
		expect(mockFetch).toHaveBeenCalledWith('/api/sessions/sess-ka/keepalive', { method: 'PATCH' });
	});

	it('deleteSession calls DELETE endpoint and returns', async () => {
		mockFetch.mockResolvedValue({ ok: true });
		const result = await deleteSession('sess-del');
		expect(result).toBeUndefined();
		expect(mockFetch).toHaveBeenCalledWith('/api/sessions/sess-del', { method: 'DELETE' });
	});

	it('createSession returns session with id and status starting', async () => {
		const session = { id: 'new-sess', server_name: 'server-3', status: 'starting', bmc_ip: '10.0.0.3', created_at: '2026-03-29T11:00:00Z', last_activity: '2026-03-29T11:00:00Z' };
		mockFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve(session) });

		const result = await createSession('server-3');
		expect(result.id).toBe('new-sess');
		expect(result.status).toBe('starting');
		expect(mockFetch).toHaveBeenCalledWith('/api/sessions', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ server_name: 'server-3' })
		});
	});

	it('getKVMWebSocketURL returns ws:// URL for http: protocol', () => {
		Object.defineProperty(window, 'location', {
			value: { protocol: 'http:', host: 'localhost:8080' },
			writable: true,
			configurable: true
		});
		const url = getKVMWebSocketURL('sess-ws');
		expect(url).toBe('ws://localhost:8080/ws/kvm/sess-ws');
	});

	it('getKVMWebSocketURL returns wss:// URL for https: protocol', () => {
		Object.defineProperty(window, 'location', {
			value: { protocol: 'https:', host: 'kvm.example.com' },
			writable: true,
			configurable: true
		});
		const url = getKVMWebSocketURL('sess-wss');
		expect(url).toBe('wss://kvm.example.com/ws/kvm/sess-wss');
	});

	it('fetchServers returns array of server info objects', async () => {
		const servers = [
			{ name: 'srv-a', bmc_ip: '10.0.0.10', bmc_port: 443, board_type: 'ami_megarac', has_active_session: true, active_session_id: 'sess-a' },
			{ name: 'srv-b', bmc_ip: '10.0.0.11', bmc_port: 80, board_type: 'ami_megarac', has_active_session: false }
		];
		mockFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve(servers) });

		const result = await fetchServers();
		expect(result).toEqual(servers);
		expect(result).toHaveLength(2);
		expect(result[0].has_active_session).toBe(true);
		expect(result[0].active_session_id).toBe('sess-a');
		expect(result[1].has_active_session).toBe(false);
		expect(mockFetch).toHaveBeenCalledWith('/api/servers');
	});

	it('fetchServers 401 triggers an error', async () => {
		mockFetch.mockResolvedValue({ ok: false, statusText: 'Unauthorized' });
		await expect(fetchServers()).rejects.toThrow('Failed to fetch servers: Unauthorized');
	});
});
