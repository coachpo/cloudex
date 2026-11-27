"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowUpRight, UserRound } from "lucide-react";
import Link from "next/link";
import { useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";
import { grantProblem } from "@/lib/i18n";
import { useT } from "@/components/i18n-provider";
import { GrantActionDialog } from "@/components/grant-action-dialog";
import { Empty, Facts, Loading, Notice, Status } from "@/components/shared";
import { Button } from "@/components/ui/button";
import { APIError, api, change, wsPath } from "@/lib/api";
import {
  grantAttemptPath, grantAuthorizationPath, grantBindingsKey, grantChannel, grantName, grantPath,
  grantsQuery, grantStatusLabels, type GrantAttempt, type GrantAuthorization, type GrantChannel,
  type GrantIdentity, type ManagedGrant,
} from "@/lib/grants";

function GrantIdentityDetails({ identity, showName }: { identity: GrantIdentity; showName?: boolean }) {
  const { t } = useT();
  const subject = identity.upstream_user_id || identity.subject;
  const items: [string, ReactNode][] = [
    [t("grantChannel"), grantChannel(identity.channel)],
    [t("grantIdentity"), <span className="wide-code">{showName ? grantName(identity) + " · " : ""}<code>{subject}</code></span>],
    [t("grantUpstreamWorkspace"), identity.upstream_workspace_id
      ? <code className="wide-code">{identity.upstream_workspace_id}</code> : t("grantWorkspaceUnknown")],
  ];
  if (identity.email) items.push([t("grantEmail"), identity.email]);
  if (identity.channel === "chatgpt") items.push([t("grantRegistration"), <code className="wide-code">{identity.client_id}</code>]);
  return <Facts items={items}/>;
}

function WorkspaceLinks({ ids, names }: { ids: string[]; names: Map<string, string> }) {
  return <ul className="m-0 list-none space-y-1 p-0">
    {ids.map(id => <li key={id}><Link className="link break-all" href={wsPath(id) + "/settings#workspace-grant"}>
      {names.get(id) ?? id}
    </Link></li>)}
  </ul>;
}

type GrantAction = { grant: ManagedGrant; kind: "disconnect" | "delete" };
type StartAuthorization = { channel: GrantChannel; grant_id?: string; method?: "device_code" };

export function GrantSettings() {
  const { t } = useT();
  const client = useQueryClient();
  const grants = useQuery(grantsQuery);
  const grantList = grants.data?.grants ?? [];
  const workspaceNames = new Map(grantList.flatMap(grant =>
    (grant.workspaces ?? []).map(workspace => [workspace.id, workspace.name] as const)));
  const refreshViews = () => Promise.all([
    client.invalidateQueries({ queryKey: grantsQuery.queryKey }),
    client.invalidateQueries({ queryKey: grantBindingsKey }),
  ]);
  const start = useMutation({
    gcTime: 0,
    mutationFn: (input: StartAuthorization) => change<GrantAuthorization>(grantAuthorizationPath, "POST", input),
  });
  const started = start.data;
  const device = started?.device;
  const replacing = grantList.find(grant => grant.id === start.variables?.grant_id);
  const attemptID = started?.id ?? "";
  const attemptHeading = useRef<HTMLHeadingElement>(null);
  const callbackInput = useRef<HTMLInputElement>(null);
  const [callbackSubmitting, setCallbackSubmitting] = useState(false);
  const [callbackError, setCallbackError] = useState<{ message: string; invalid: boolean }>();
  const attempt = useQuery({
    queryKey: ["grant-authorization", attemptID],
    queryFn: ({ signal }) => api<GrantAttempt>(grantAttemptPath(attemptID), { signal }),
    enabled: !!attemptID,
    gcTime: 0,
    retry: (retries, error) => !(error instanceof APIError && error.status === 404) && retries < 2,
    refetchInterval: ({ state: { data, error } }) =>
      (!data || data.status === "pending") && !(error instanceof APIError && error.status === 404) ? 1500 : false,
    refetchIntervalInBackground: true,
  });
  const confirm = useMutation({
    gcTime: 0,
    mutationFn: (id: string) => change<ManagedGrant>(grantAttemptPath(id) + "/confirm", "POST"),
    onSuccess: () => {
      start.reset();
      return refreshViews();
    },
  });
  const cancelDevice = useMutation({
    mutationFn: async (id: string) => {
      try {
        await change<void>(grantAttemptPath(id), "DELETE");
      } catch (error) {
        if (!(error instanceof APIError && error.status === 404)) throw error;
      }
    },
    onSuccess: closeAttempt,
  });
  const action = useMutation<ManagedGrant | void, Error, GrantAction>({
    mutationFn: ({ grant, kind }: GrantAction) => kind === "disconnect"
      ? change<ManagedGrant>(grantPath(grant.id) + "/disconnect", "POST")
      : change<void>(grantPath(grant.id), "DELETE"),
    onSettled: refreshViews,
  });
  const busy = start.isPending || confirm.isPending || cancelDevice.isPending || action.isPending || callbackSubmitting;
  const conflict = action.error instanceof APIError && action.error.status === 409
    ? (action.error.details as { workspace_ids?: string[] } | undefined)?.workspace_ids : undefined;
  const attemptMissing = [attempt.error, confirm.error].some(error => error instanceof APIError && error.status === 404);
  const attemptResult = attemptMissing ? undefined : attempt.data;
  const attemptState = attemptResult?.status;
  const showProvider = !attemptMissing && attemptState !== "failed" && attemptState !== "ready";
  useEffect(() => {
    if (attemptID && attemptState !== "pending") attemptHeading.current?.focus();
  }, [attemptID, attemptState]);

  function begin(input: StartAuthorization) {
    confirm.reset();
    cancelDevice.reset();
    action.reset();
    setCallbackError(undefined);
    start.mutate(input);
  }
  async function submitCallback(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const input = callbackInput.current;
    if (!input || callbackSubmitting) return;
    const callbackURL = input.value.trim();
    input.value = "";
    if (!callbackURL) {
      setCallbackError({ message: t("grantCallbackURLInvalid"), invalid: true });
      input.focus();
      return;
    }
    setCallbackError(undefined);
    setCallbackSubmitting(true);
    try {
      await change<void>(grantAttemptPath(attemptID) + "/callback", "POST", { callback_url: callbackURL });
      await attempt.refetch();
    } catch (error) {
      const code = error instanceof APIError ? error.code ?? "" : "";
      const fallback = error instanceof APIError && error.status === 404 ? "grantCallbackAttemptUnavailable" : "grantCallbackSubmitFailed";
      const invalid = code === "invalid_oauth_callback_url" || code === "oauth_callback_address_mismatch" || code === "invalid_oauth_state";
      setCallbackError({ message: grantProblem(t, code, t(fallback)), invalid });
      if (invalid) input.focus();
    } finally {
      setCallbackSubmitting(false);
    }
  }
  function closeAttempt() {
    confirm.reset();
    start.reset();
  }
  function runAction(input: GrantAction) {
    start.reset();
    confirm.reset();
    action.mutate(input);
  }

  return <section className="panel view-grid" id="grants" aria-labelledby="grants-heading">
    <div className="section-head mb-0"><h2 id="grants-heading">{t("grantList")}</h2></div>
    {grants.error && <Notice error={grants.error} retry={() => grants.refetch()}/>}
    {action.error && <Notice error={action.error}/>}
    {!!conflict?.length && <WorkspaceLinks ids={conflict} names={workspaceNames}/>}
    {start.error && <Notice error={start.error}/>}
    {confirm.error && !attemptMissing && <Notice error={confirm.error}/>}
    {action.isSuccess && <p className="confirm" role="status">{t(action.variables.kind === "disconnect" ? "grantDisconnectDone" : "grantDeleteDone")}</p>}
    {confirm.isSuccess && <p className="confirm" role="status">{t("grantConfirmDone")}</p>}
    {grants.isPending && <Loading label={t("loading")}/>}
    {grants.data && !grantList.length && <Empty icon={UserRound}>{t("grantListEmpty")}</Empty>}
    {!!grantList.length && <div className="grant-list">
      {grantList.map(grant => {
        const ids = (grant.workspaces ?? []).map(workspace => workspace.id);
        const name = grantName(grant);
        return <article className="grant-card" key={grant.id} aria-labelledby={"grant-" + grant.id}>
          <div className="grant-card-head">
            <h3 id={"grant-" + grant.id}>{name}</h3>
            <Status state={grant.status === "active" ? "succeeded" : "degraded"} label={t(grantStatusLabels[grant.status])}/>
          </div>
          <GrantIdentityDetails identity={grant}/>
          {grant.reason && grant.reason !== "missing_inference_scope" && <p className="form-note">{grantProblem(t, grant.reason)}</p>}
          {!!ids.length && <div className="grid gap-1">
            <h4 className="small font-medium">{t("grantAffectedWorkspaces")}</h4>
            <WorkspaceLinks ids={ids} names={workspaceNames}/>
          </div>}
          <div className="actions border-t border-border pt-3">
            <Button type="button" variant="outline" size="sm" disabled={busy || !!started}
              onClick={() => begin({ channel: grant.channel, grant_id: grant.id })}>{t("grantReauthorize")}</Button>
            {grant.channel === "codex" && <Button type="button" variant="outline" size="sm" disabled={busy || !!started}
              onClick={() => begin({ channel: "codex", grant_id: grant.id, method: "device_code" })}>{t("grantReauthorizeDevice")}</Button>}
            {grant.status !== "disconnected" && <GrantActionDialog label={t("grantDisconnect")}
              title={t("grantDisconnectTitle", { name })} disabled={busy || !!started}
              action={() => runAction({ grant, kind: "disconnect" })}>
              <p>{t("grantDisconnectEffect")}</p>
              {!!ids.length && <WorkspaceLinks ids={ids} names={workspaceNames}/>}
            </GrantActionDialog>}
            <GrantActionDialog label={t("grantDelete")} title={t("grantDeleteTitle", { name })}
              disabled={busy || !!started || !!ids.length} destructive action={() => runAction({ grant, kind: "delete" })}>
              <p>{t("grantDeleteEffect")}</p>
            </GrantActionDialog>
          </div>
        </article>;
      })}
    </div>}
    {started ? <div className="form subtle-panel">
      <h3 ref={attemptHeading} tabIndex={-1}>{t(attemptState === "ready" ? "grantConfirmTitle" : device ? "grantDeviceCodeTitle" : "grantAuthorizationTitle")}</h3>
      {attemptMissing && <p role="alert" className="form-note">{t(device ? "grantDeviceCodeExpired" : "grantCallbackAttemptUnavailable")}</p>}
      {attempt.error && !attemptMissing && <Notice error={attempt.error} retry={() => attempt.refetch()}/>}
      {cancelDevice.error && <Notice error={cancelDevice.error}/>}
      {attemptResult?.status === "failed" && <p role="alert" className="form-note">{grantProblem(t, attemptResult.code ?? "", attemptResult.error || t("grantAuthorizationFailed"))}</p>}
      {attemptResult?.status === "ready" ? <>
        <GrantIdentityDetails identity={attemptResult.identity} showName/>
        {!!replacing?.workspaces?.length && <p className="form-note">{t("grantReauthorizeEffect")}</p>}
      </> :
        showProvider && <>
          {device ? <>
            <p id="grant-device-code-instructions" className="form-note">{t("grantDeviceCodeInstructions")}</p>
            <Facts items={[[t("grantDeviceCode"), <code className="wide-code font-semibold">{device.user_code}</code>]]}/>
          </> : <p className="form-note">{t("grantCallbackTunnel")}</p>}
          <p role={device ? undefined : "status"} className="form-note">{t("grantAuthorizationPending")}</p>
        </>}
      <div className="actions">
        {attemptState === "ready" ? <Button type="button" onClick={() => confirm.mutate(started.id)}
          disabled={busy || attempt.isError}>{t("grantConfirm")}</Button> :
          showProvider && <Button asChild><a href={device ? device.verification_url : started.authorization_url}
            aria-describedby={device ? "grant-device-code-instructions" : undefined} target="_blank" rel="noopener noreferrer">
            {t(device ? "grantOpenDeviceVerification" : "grantOpenSignIn")} <ArrowUpRight data-icon="inline-end"/>
          </a></Button>}
        <Button type="button" variant="outline" disabled={busy}
          onClick={() => device && !attemptMissing ? cancelDevice.mutate(started.id) : closeAttempt()}>{t("cancel")}</Button>
      </div>
      {showProvider && !device && <details className="border-t border-border pt-3">
        <summary id="grant-callback-summary" className="font-medium">{t("grantPasteCallback")}</summary>
        <form className="form mt-3" aria-labelledby="grant-callback-summary" method="post" autoComplete="off" noValidate onSubmit={submitCallback}>
          <div className="fields">
            <label htmlFor="grant-callback-url">{t("grantCallbackURL")}</label>
            <p id="grant-callback-hint" className="form-note">{t("grantCallbackHint")}</p>
            {/* Keep the input unnamed so that native submission cannot send its value. */}
            <input ref={callbackInput} id="grant-callback-url" className="field" type="url" required
              autoComplete="off" autoCapitalize="none" autoCorrect="off" spellCheck={false} readOnly={callbackSubmitting}
              aria-invalid={callbackError?.invalid ?? false} onChange={() => setCallbackError(undefined)}
              aria-describedby={callbackError ? "grant-callback-hint grant-callback-error" : "grant-callback-hint"}/>
            {callbackError && <p id="grant-callback-error" className="form-note" role="alert">{callbackError.message}</p>}
          </div>
          <div className="actions"><Button type="submit" variant="outline" disabled={busy}>{t("grantSubmitCallback")}</Button></div>
        </form>
      </details>}
    </div> : <div className="actions">
      <Button type="button" disabled={busy} onClick={() => begin({ channel: "chatgpt" })}>{t("grantSignInChatGPT")}</Button>
      <Button type="button" variant="outline" disabled={busy} onClick={() => begin({ channel: "codex" })}>{t("grantSignInCodex")}</Button>
      <Button type="button" variant="outline" disabled={busy}
        onClick={() => begin({ channel: "codex", method: "device_code" })}>{t("grantSignInCodexDevice")}</Button>
    </div>}
  </section>;
}
