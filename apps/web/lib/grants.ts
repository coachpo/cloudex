import { api, grantBindingPath, wsPath, type GrantBinding } from "./api";
import type { MessageKey } from "./i18n";

export type GrantChannel = "codex" | "chatgpt";
export type GrantIdentity = {
  channel: GrantChannel;
  client_id: string;
  subject: string;
  upstream_user_id?: string;
  upstream_workspace_id?: string;
  display_name?: string;
  email?: string;
};
export type ManagedGrant = GrantIdentity & {
  id: string;
  status: "active" | "missing_scope" | "reauth_required" | "disconnected" | "unavailable";
  reason?: string;
  workspaces: { id: string; name: string }[];
  grant_epoch: number;
};
export type GrantAuthorization = { id: string; authorization_url: string };
export type GrantAttempt =
  | { id: string; status: "pending" }
  | { id: string; status: "ready"; identity: GrantIdentity }
  | { id: string; status: "failed"; code?: string; error?: string };
export const grantPath = (id: string) => "/grants/" + encodeURIComponent(id);
export const grantAuthorizationPath = "/grant-authorizations";
export const grantAttemptPath = (id: string) => grantAuthorizationPath + "/" + encodeURIComponent(id);
export const gatewayCredentialPath = (id: string) => wsPath(id) + "/gateway-credential/rotate";
export const grantBindingsKey = ["grant-binding"] as const;
export const grantsQuery = {
  queryKey: ["grants"] as const,
  queryFn: ({ signal }: { signal: AbortSignal }) => api<{ grants: ManagedGrant[] }>("/grants", { signal }),
  refetchInterval: 5000,
};
export const grantBindingQuery = (id: string) => ({
  queryKey: [...grantBindingsKey, id] as const,
  queryFn: ({ signal }: { signal: AbortSignal }) => api<GrantBinding>(grantBindingPath(id), { signal }),
  refetchInterval: ({ state }: { state: { data?: GrantBinding } }) => state.data?.readiness === "pending" ? 2500 : false,
});

export function grantName(identity: GrantIdentity) {
  return identity.display_name || identity.email || identity.upstream_user_id || identity.subject;
}

export function grantChannel(channel: GrantChannel) {
  return channel === "chatgpt" ? "ChatGPT" : "Codex";
}

export const grantStatusLabels = {
  active: "grantActive",
  missing_scope: "grantMissingScope",
  reauth_required: "grantReauthRequired",
  disconnected: "grantDisconnected",
  unavailable: "grantUnavailable",
} satisfies Record<ManagedGrant["status"], MessageKey>;
