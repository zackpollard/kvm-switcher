export interface ServerInfo {
	name: string;
	bmc_ip: string;
	bmc_port: number;
	board_type: string;
	has_active_session: boolean;
}

export interface KVMSession {
	id: string;
	server_name: string;
	bmc_ip: string;
	status: 'starting' | 'connected' | 'disconnected' | 'error';
	container_id?: string;
	websocket_port?: number;
	conn_mode?: string;
	kvm_password?: string;
	created_at: string;
	last_activity: string;
	error?: string;
}

const API_BASE = '/api';

export async function fetchServers(): Promise<ServerInfo[]> {
	const res = await fetch(`${API_BASE}/servers`);
	if (!res.ok) throw new Error(`Failed to fetch servers: ${res.statusText}`);
	return res.json();
}

export async function createSession(serverName: string): Promise<KVMSession> {
	const res = await fetch(`${API_BASE}/sessions`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ server_name: serverName })
	});
	if (!res.ok) {
		const err = await res.json().catch(() => ({ error: res.statusText }));
		throw new Error(err.error || res.statusText);
	}
	return res.json();
}

export async function getSession(id: string): Promise<KVMSession> {
	const res = await fetch(`${API_BASE}/sessions/${id}`);
	if (!res.ok) throw new Error(`Failed to get session: ${res.statusText}`);
	return res.json();
}

export async function deleteSession(id: string): Promise<void> {
	const res = await fetch(`${API_BASE}/sessions/${id}`, { method: 'DELETE' });
	if (!res.ok) throw new Error(`Failed to delete session: ${res.statusText}`);
}

export async function listSessions(): Promise<KVMSession[]> {
	const res = await fetch(`${API_BASE}/sessions`);
	if (!res.ok) throw new Error(`Failed to list sessions: ${res.statusText}`);
	return res.json();
}

export function getKVMWebSocketURL(sessionId: string): string {
	const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
	return `${protocol}//${window.location.host}/ws/kvm/${sessionId}`;
}

export interface IPMISession {
	board_type: string;
	session_cookie: string;
	csrf_token: string;
	username: string;
	privilege: number;
	extended_priv: number;
}

export async function createIPMISession(name: string): Promise<IPMISession> {
	const res = await fetch(`${API_BASE}/ipmi-session/${name}`, { method: 'POST' });
	if (!res.ok) {
		const err = await res.json().catch(() => ({ error: res.statusText }));
		throw new Error(err.error || res.statusText);
	}
	return res.json();
}

export interface DeviceStatus {
	online: boolean;
	power_state?: string;
	model?: string;
	health?: string;
	load_watts?: number;
	load_pct?: number;
	load_amps?: number;
	voltage?: number;
	battery_pct?: number;
	runtime_min?: number;
	temperature_c?: number;
	app_version?: string;
	image_version?: string;
	update_available?: boolean;
}

export async function fetchServerStatuses(): Promise<Record<string, DeviceStatus>> {
	const res = await fetch(`${API_BASE}/server-status`);
	if (!res.ok) throw new Error(`Failed to fetch statuses: ${res.statusText}`);
	return res.json();
}

export interface AuthStatus {
	authenticated: boolean;
	oidc_enabled?: boolean;
	email?: string;
	name?: string;
	roles?: string[];
}

export async function fetchAuthStatus(): Promise<AuthStatus> {
	const res = await fetch('/auth/me');
	if (!res.ok) throw new Error('Failed to fetch auth status');
	return res.json();
}
