export interface ServerInfo {
	name: string;
	bmc_ip: string;
	bmc_port: number;
	board_type: string;
	has_active_session: boolean;
	active_session_id?: string;
}

export interface Viewer {
	id: string;
	display_name: string;
	ip: string;
	has_control: boolean;
	connected_at: string;
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
	idle_timeout_remaining_seconds?: number;
	viewers?: Viewer[];
	viewer_count?: number;
	pending_control_request?: PendingControlRequest;
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

export async function keepAliveSession(id: string): Promise<void> {
	const res = await fetch(`${API_BASE}/sessions/${id}/keepalive`, { method: 'PATCH' });
	if (!res.ok) throw new Error(`Failed to keep alive session: ${res.statusText}`);
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

export function getKVMWebSocketURL(sessionId: string, viewerId?: string): string {
	const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
	const base = `${protocol}//${window.location.host}/ws/kvm/${sessionId}`;
	return viewerId ? `${base}?viewer_id=${viewerId}` : base;
}

export function generateViewerId(): string {
	return crypto.randomUUID();
}

// --- iKVM control commands ---

export async function kvmPowerControl(sessionId: string, action: 'on' | 'off' | 'cycle' | 'reset' | 'soft_reset' | 'bmc_reset'): Promise<void> {
	const res = await fetch(`${API_BASE}/sessions/${sessionId}/power`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ action })
	});
	if (!res.ok) {
		const err = await res.json().catch(() => ({ error: res.statusText }));
		throw new Error(err.error || res.statusText);
	}
}

export async function kvmDisplayLock(sessionId: string, lock: boolean): Promise<void> {
	const res = await fetch(`${API_BASE}/sessions/${sessionId}/display-lock`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ lock })
	});
	if (!res.ok) {
		const err = await res.json().catch(() => ({ error: res.statusText }));
		throw new Error(err.error || res.statusText);
	}
}

export async function kvmResetVideo(sessionId: string): Promise<void> {
	const res = await fetch(`${API_BASE}/sessions/${sessionId}/reset-video`, {
		method: 'POST'
	});
	if (!res.ok) {
		const err = await res.json().catch(() => ({ error: res.statusText }));
		throw new Error(err.error || res.statusText);
	}
}

export async function kvmMouseMode(sessionId: string, mode: 'relative' | 'absolute'): Promise<void> {
	const res = await fetch(`${API_BASE}/sessions/${sessionId}/mouse-mode`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ mode })
	});
	if (!res.ok) {
		const err = await res.json().catch(() => ({ error: res.statusText }));
		throw new Error(err.error || res.statusText);
	}
}

export async function kvmKeyboardLayout(sessionId: string, layout: string): Promise<void> {
	const res = await fetch(`${API_BASE}/sessions/${sessionId}/keyboard-layout`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ layout })
	});
	if (!res.ok) {
		const err = await res.json().catch(() => ({ error: res.statusText }));
		throw new Error(err.error || res.statusText);
	}
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
	circuit_breaker_state?: string;
	last_updated?: string;
	error?: string;
}

export async function fetchServerStatuses(): Promise<Record<string, DeviceStatus>> {
	const res = await fetch(`${API_BASE}/server-status`);
	if (!res.ok) throw new Error(`Failed to fetch statuses: ${res.statusText}`);
	return res.json();
}

export interface AuditEntry {
	id: number;
	timestamp: string;
	event_type: string;
	user_email?: string;
	server_name?: string;
	session_id?: string;
	remote_addr?: string;
	details?: any;
}

export async function fetchAuditLog(params?: {
	event_type?: string;
	server_name?: string;
	user_email?: string;
	limit?: number;
	offset?: number;
}): Promise<AuditEntry[]> {
	const query = new URLSearchParams();
	if (params?.event_type) query.set('event_type', params.event_type);
	if (params?.server_name) query.set('server_name', params.server_name);
	if (params?.user_email) query.set('user_email', params.user_email);
	if (params?.limit) query.set('limit', String(params.limit));
	if (params?.offset) query.set('offset', String(params.offset));
	const qs = query.toString();
	const res = await fetch(`${API_BASE}/audit-log${qs ? '?' + qs : ''}`);
	if (!res.ok) throw new Error('Failed to fetch audit log');
	return res.json();
}

export interface VirtualMediaStatus {
	inserted: boolean;
	image?: string;
	media_type?: string;
	write_protected: boolean;
}

export async function getVirtualMediaStatus(sessionId: string): Promise<VirtualMediaStatus | null> {
	const res = await fetch(`${API_BASE}/sessions/${sessionId}/virtual-media`);
	if (res.status === 501) return null; // not supported
	if (!res.ok) throw new Error('Failed to get virtual media status');
	return res.json();
}

export async function mountVirtualMedia(sessionId: string, imageUrl: string): Promise<void> {
	const res = await fetch(`${API_BASE}/sessions/${sessionId}/virtual-media/mount`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ image_url: imageUrl })
	});
	if (!res.ok) {
		const data = await res.json().catch(() => ({ error: res.statusText }));
		throw new Error(data.error || 'Mount failed');
	}
}

export async function ejectVirtualMedia(sessionId: string): Promise<void> {
	const res = await fetch(`${API_BASE}/sessions/${sessionId}/virtual-media/eject`, {
		method: 'POST'
	});
	if (!res.ok) {
		const data = await res.json().catch(() => ({ error: res.statusText }));
		throw new Error(data.error || 'Eject failed');
	}
}

export interface ISOFile {
	id: number;
	filename: string;
	size_bytes: number;
	sha256?: string;
	uploaded_by?: string;
	uploaded_at: string;
	last_used?: string;
}

export interface ISOListResponse {
	isos: ISOFile[];
	total_size_bytes: number;
	max_size_bytes: number;
}

export async function fetchISOs(): Promise<ISOListResponse> {
	const res = await fetch(`${API_BASE}/isos`);
	if (!res.ok) throw new Error('Failed to fetch ISOs');
	return res.json();
}

export function uploadISO(file: File, onProgress?: (pct: number) => void): Promise<ISOFile> {
	return new Promise((resolve, reject) => {
		const xhr = new XMLHttpRequest();
		xhr.open('POST', `${API_BASE}/isos`);

		if (onProgress) {
			xhr.upload.onprogress = (e) => {
				if (e.lengthComputable) {
					onProgress(Math.round((e.loaded / e.total) * 100));
				}
			};
		}

		xhr.onload = () => {
			if (xhr.status >= 200 && xhr.status < 300) {
				resolve(JSON.parse(xhr.responseText));
			} else {
				try {
					const data = JSON.parse(xhr.responseText);
					reject(new Error(data.error || `Upload failed: ${xhr.statusText}`));
				} catch {
					reject(new Error(`Upload failed: ${xhr.statusText}`));
				}
			}
		};
		xhr.onerror = () => reject(new Error('Upload failed: network error'));

		const formData = new FormData();
		formData.append('file', file);
		xhr.send(formData);
	});
}

export async function deleteISO(name: string): Promise<void> {
	const res = await fetch(`${API_BASE}/isos/${encodeURIComponent(name)}`, { method: 'DELETE' });
	if (!res.ok) {
		const data = await res.json().catch(() => ({ error: res.statusText }));
		throw new Error(data.error || 'Delete failed');
	}
}

export interface ISODownloadStatus {
	id: string;
	filename: string;
	url: string;
	total_bytes: number;
	downloaded: number;
	status: string; // "downloading" | "complete" | "error"
	error?: string;
	started_at: string;
}

export async function downloadISOFromURL(url: string, filename?: string): Promise<ISODownloadStatus> {
	const res = await fetch(`${API_BASE}/iso-downloads`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ url, filename: filename || undefined }),
	});
	if (!res.ok) {
		const data = await res.json().catch(() => ({ error: res.statusText }));
		throw new Error(data.error || 'Download failed');
	}
	return res.json();
}

export async function fetchISODownloads(): Promise<ISODownloadStatus[]> {
	const res = await fetch(`${API_BASE}/iso-downloads`);
	if (!res.ok) throw new Error('Failed to fetch downloads');
	return res.json();
}

export async function mountLocalISO(sessionId: string, filename: string): Promise<void> {
	const res = await fetch(`${API_BASE}/sessions/${sessionId}/virtual-media/mount-local`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ filename })
	});
	if (!res.ok) {
		const data = await res.json().catch(() => ({ error: res.statusText }));
		throw new Error(data.error || 'Mount failed');
	}
}

export interface PendingControlRequest {
	requester_id: string;
	requester_name: string;
	requested_at: string;
	timeout_sec: number;
}

export async function requestViewerControl(sessionId: string, viewerId?: string): Promise<void> {
	const res = await fetch(`${API_BASE}/sessions/${sessionId}/viewers/request-control`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ viewer_id: viewerId }),
	});
	if (!res.ok) throw new Error('Failed to request control');
}

export async function releaseViewerControl(sessionId: string, viewerId?: string): Promise<void> {
	const res = await fetch(`${API_BASE}/sessions/${sessionId}/viewers/release-control`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ viewer_id: viewerId }),
	});
	if (!res.ok) throw new Error('Failed to release control');
}

export async function acceptControlRequest(sessionId: string): Promise<void> {
	const res = await fetch(`${API_BASE}/sessions/${sessionId}/viewers/accept-control`, { method: 'POST' });
	if (!res.ok) throw new Error('Failed to accept control request');
}

export async function denyControlRequest(sessionId: string): Promise<void> {
	const res = await fetch(`${API_BASE}/sessions/${sessionId}/viewers/deny-control`, { method: 'POST' });
	if (!res.ok) throw new Error('Failed to deny control request');
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
