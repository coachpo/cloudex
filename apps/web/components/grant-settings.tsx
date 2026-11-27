"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowUpRight, UserRound } from "lucide-react";
import Link from "next/link";
import { useEffect, useRef, type ReactNode } from "react";
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
type StartAuthorization = { channel: GrantChannel; grant_id?: string };

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
  const replacing = grantList.find(grant => grant.id === start.variables?.grant_id);
  const attemptID = started?.id ?? "";
  const attemptHeading = useRef<HTMLHeadingElement>(null);
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
  const action = useMutation<ManagedGrant | void, Error, GrantAction>({
    mutationFn: ({ grant, kind }: GrantAction) => kind === "disconnect"
      ? change<ManagedGrant>(grantPath(grant.id) + "/disconnect", "POST")
      : change<void>(grantPath(grant.id), "DELETE"),
    onSettled: refreshViews,
  });
  const busy = start.isPending || confirm.isPending || action.isPending;
  const conflict = action.error instanceof APIError && action.error.status === 409
    ? (action.error.details as { workspace_ids?: string[] } | undefined)?.workspace_ids : undefined;
  const attemptResult = attempt.data;
  const attemptState = attemptResult?.status;
  useEffect(() => {
    if (attemptID && attemptState !== "pending") attemptHeading.current?.focus();
  }, [attemptID, attemptState]);

  function begin(input: StartAuthorization) {
    confirm.reset();
    action.reset();
    start.mutate(input);
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
    {confirm.error && <Notice error={confirm.error}/>}
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
      <h3 ref={attemptHeading} tabIndex={-1}>{t(attemptState === "ready" ? "grantConfirmTitle" : "grantAuthorizationTitle")}</h3>
      {attempt.error && <Notice error={attempt.error} retry={() => attempt.refetch()}/>}
      {attemptResult?.status === "failed" && <p role="alert" className="form-note">{grantProblem(t, attemptResult.code ?? "", attemptResult.error || t("grantAuthorizationFailed"))}</p>}
      {attemptResult?.status === "ready" ? <>
        <GrantIdentityDetails identity={attemptResult.identity} showName/>
        {!!replacing?.workspaces?.length && <p className="form-note">{t("grantReauthorizeEffect")}</p>}
      </> :
        attemptState !== "failed" && <>
          <p className="form-note">{t("grantCallbackTunnel")}</p>
          <p role="status" className="form-note">{t("grantAuthorizationPending")}</p>
        </>}
      <div className="actions">
        {attemptState === "ready" ? <Button type="button" onClick={() => confirm.mutate(started.id)}
          disabled={busy || attempt.isError}>{t("grantConfirm")}</Button> :
          attemptState !== "failed" && !(attempt.error instanceof APIError && attempt.error.status === 404) &&
          <Button asChild><a href={started.authorization_url} target="_blank" rel="noopener noreferrer">
            {t("grantOpenSignIn")} <ArrowUpRight data-icon="inline-end"/>
          </a></Button>}
        <Button type="button" variant="outline" disabled={busy} onClick={() => { confirm.reset(); start.reset(); }}>{t("cancel")}</Button>
      </div>
    </div> : <div className="actions">
      <Button type="button" disabled={busy} onClick={() => begin({ channel: "chatgpt" })}>{t("grantSignInChatGPT")}</Button>
      <Button type="button" variant="outline" disabled={busy} onClick={() => begin({ channel: "codex" })}>{t("grantSignInCodex")}</Button>
    </div>}
  </section>;
}
