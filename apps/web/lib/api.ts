import type { QueryClient } from "@tanstack/react-query";
import type { Locale } from "./i18n";

export const noResponseMessage = "The control plane did not return a response.";

export class APIError extends Error {
  readonly status: number;
  readonly code?: string;
  readonly details?: unknown;

  constructor(status: number, message: string, code?: string, details?: unknown) {
    super(message);
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  if (init?.body) headers.set("Content-Type", "application/json");
  const response = await fetch("/api/v1" + path, { ...init, headers, cache: "no-store" });
  if (response.status === 204) return undefined as T;
  const json = await response.json().catch(() => null);
  const data = json?.data;
  if (!response.ok || data === null || typeof data !== "object") {
    throw new APIError(response.status, json?.error?.message ?? noResponseMessage, json?.error?.code, json?.error?.details);
  }
  return data as T;
}

export function change<T>(path: string, method: string, data?: unknown, key?: string) {
  return api<T>(path, { method, body: data === undefined ? undefined : JSON.stringify(data),
    headers: key ? { "Idempotency-Key": key } : undefined });
}

// crypto.randomUUID exists only in secure contexts; the console may be served over plain HTTP on a LAN address.
export function idempotencyKey() {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), b => b.toString(16).padStart(2, "0")).join("");
}

export function mutationOutcomeUnknown(error: Error | null) {
  // A control-plane rejection has a 4xx status and its application error code.
  return error !== null && (!(error instanceof APIError) || error.status < 400 || error.status >= 500 || !error.code);
}

export async function saveSetting(client: QueryClient, queryKey: readonly string[], write: () => Promise<unknown>) {
  let acknowledged = true;
  try {
    await write();
  } catch (error) {
    if (!(error instanceof Error) || !mutationOutcomeUnknown(error)) throw error;
    acknowledged = false;
  }
  // An acknowledgement can arrive after a read has already shown a newer value.
  await client.invalidateQueries({ queryKey, exact: true });
  return acknowledged;
}

export type Workspace = {
  id: string; name: string; image: string; repo_url: string; branch: string;
  cpu_milli: number; memory_mb: number;
  assigned_host_id: string; disk_bytes: number;
  desired_state: string; observed_state: string; paused: boolean; generation: number; error: string; updated_at: string;
};
export type GrantBinding = {
  workspace_id: string;
  grant_id?: string | null;
  binding_revision: number;
  readiness: "unbound" | "pending" | "ready" | "unavailable";
  reason?: string;
};

export type RepositoryCredentialStatus = { configured: boolean; username: string };
export type Operation = {
  id: string; workspace_id: string; type: string; status: string; target_generation: number;
  attempts: number; error: string; created_at: string; finished_at: string | null;
};
export type Accepted = { workspace_id: string; operation_id: string; status: string };
export type HostStatus = "enrolling" | "online" | "offline" | "degraded";
export type HostScheduling = "enabled" | "cordoned" | "disabled";
export type HostResources = { cpu_milli: number; memory_bytes: number; disk_bytes: number };
export type Host = {
  id: string; name: string; status: HostStatus; last_seen: string | null;
  scheduling_state: HostScheduling; scheduling_version: number;
  connection_type: "agent" | "ssh"; ssh_address?: string; ssh_user?: string; ssh_host_key?: string; connection_error?: string;
  agent_version: string; engine: string; os: string; arch: string; healthy: boolean;
  images: { image: string; ready: boolean }[]; max_operations: number; max_operations_target: number; reachable: boolean;
  running_containers: number; total_containers: number;
  allocatable: HostResources; reserved: HostResources; available: HostResources;
};
export type HostEnrollment = {host: Host; enrollment_token: string; expires_at: string; agent_address?: string; ca_certificate: string};
export type HostContainer = { id: string; name: string; image: string; state: string; workspace_id?: string };
export type Image = { id: string; tags: string[] | null; size_bytes: number };
export type Generation = { generation: number; id: string; image_id: string; status: string; created_at: string };
export type WorkspaceAccess = {
  has_public_keys: boolean; status: "applying" | "stopped" | "paused" | "unavailable" | "ready";
  message?: string; host: string; bind: string; port?: number; alias: string; user: string;
  project_path: string; fingerprint?: string; gateway_fingerprint?: string; known_hosts?: string; ssh_config?: string;
};
export const publicKeyLines = (text: string) => text.split(/\r?\n/).map(line=>line.trim()).filter(Boolean);
export type Container = {
  id: string; name: string; image: string; image_id: string; image_digests: string[] | null;
  state: string; pid: number; exit_code: number; started_at: string; finished_at: string;
  mounts: { name: string; destination: string; writable: boolean }[];
  networks: string[]; env_names: string[]; labels: Record<string,string>;
};
export type Git = { head: string; status: string; patch: string; truncated: boolean };
export type Settings = {
  workspace_image: string;
  ssh_host: string; ssh_bind: string; agent_public_address: string;
};

export async function readSettings(signal?: AbortSignal) {
  const settings = await api<Settings>("/settings", { signal });
  if (typeof settings?.workspace_image !== "string" || typeof settings.ssh_host !== "string" ||
    typeof settings.ssh_bind !== "string" || typeof settings.agent_public_address !== "string") {
    throw new APIError(200, noResponseMessage);
  }
  return settings;
}

export const settingsQuery = {
  queryKey: ["settings"],
  queryFn: ({ signal }: { signal: AbortSignal }) => readSettings(signal),
  refetchInterval: 5000,
};
export const workspaceImageDraft = (value: string | undefined, saved: string) => value?.trim() === saved ? undefined : value;
type Template = { id: string; name: string; image: string; cpu_milli: number; memory_mb: number; disk_bytes?: number };
export const templatesQuery = {
  queryKey: ["templates"],
  queryFn: ({ signal }: { signal: AbortSignal }) => api<Template[]>("/templates", { signal }),
  refetchInterval: 5000,
};
export type Session = { username: string };
export const sessionQuery = {
  queryKey: ["session"],
  queryFn: ({ signal }: { signal: AbortSignal }) => api<Session>("/auth/session", { signal }),
};

export async function replaceSession(client: QueryClient, session: Session) {
  // A poll can start after onMutate cancels queries but before the observer is disabled.
  await client.cancelQueries({ queryKey: sessionQuery.queryKey, exact: true });
  client.setQueryData(sessionQuery.queryKey, session);
}
export const accountMutationKey = ["auth", "account"];
export type Metrics = { cpu_percent: number | null; memory_bytes: number; memory_limit_bytes: number };
// HTML counts UTF-16 code units. The API allows up to 64 Unicode code points.
export const workspaceOrHostNameMaxLength = 64 * 2;
export const active = (state?: string) => state === "queued" || state === "running";
export const workspaceState = (w: Workspace) => w.paused && w.observed_state === "stopped" ? "paused" : w.observed_state;
export const wsPath = (id: string) => "/workspaces/" + encodeURIComponent(id);
export const grantBindingPath = (id: string) => wsPath(id) + "/grant-binding";
export function changeGrantBinding(id: string, grantID: string, revision: number) {
  return api<GrantBinding>(grantBindingPath(id), {
    method: grantID ? "PUT" : "DELETE",
    headers: { "If-Match": '"' + revision + '"' },
    body: grantID ? JSON.stringify({ grant_id: grantID }) : undefined,
  });
}
export const repositoryCredentialPath = (id: string) => wsPath(id) + "/repository-credential";
export const repositoryCredentialKey = (id: string) => ["repository-credential", id] as const;
export const repositoryCredentialQuery = (id: string) => ({
  queryKey: repositoryCredentialKey(id),
  queryFn: ({ signal }: { signal: AbortSignal }) => api<RepositoryCredentialStatus>(repositoryCredentialPath(id), { signal }),
});
export const hostPath = (id: string) => "/hosts/" + encodeURIComponent(id);
export const hostWorkspacePath = (path: "/workspaces" | "/workspaces/new", hostID: string) =>
  path + "?host_id=" + encodeURIComponent(hostID);
export const hostListQuery = { queryKey:["hosts"], queryFn:()=>api<Host[]>("/hosts"), refetchInterval:5000 };
export const hostQuery = (id: string) => ({
  queryKey: ["host", id],
  queryFn: ({ signal }: { signal: AbortSignal }) => api<Host>(hostPath(id), { signal }),
  refetchInterval: 5000,
});
export const hostImageReady = (h: Host, image: string) => h.images.some(status => status.image === image && status.ready);
export function hostPlacementIssue(h: Host, image: string) {
  if (h.scheduling_state !== "enabled") return h.scheduling_state;
  if (h.status !== "online") return h.status;
  if (!hostImageReady(h, image)) return "image_unavailable";
  return "";
}
export const canPlaceOnHost = (h: Host, image: string) => !hostPlacementIssue(h, image);
// A connected host whose Docker engine is unusable cannot serve live views or start work, like an offline one.
export const hostReady = (h: Host) => h.reachable && h.healthy;
export const GiB = 1024 ** 3;
export const MiB = 1024 ** 2;
export const stamp = (locale: Locale, time?: string | null) => time && !time.startsWith("0001") ?
  new Intl.DateTimeFormat(locale, { dateStyle: "medium", timeStyle: "short" }).format(new Date(time)) : "—";
export function byteSize(bytes: number, locale: Locale) {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (bytes >= 1024 && i < units.length - 1) { bytes /= 1024; i++; }
  return bytes.toLocaleString(locale,{ maximumFractionDigits: 2 }) + " " + units[i];
}
export const hostResourceFields = [
  { label: "cpuCores", key: "cpu_milli", format: (milli: number, locale: Locale) => (milli / 1000).toLocaleString(locale) },
  { label: "memory", key: "memory_bytes", format: byteSize },
  { label: "disk", key: "disk_bytes", format: byteSize },
] as const;
