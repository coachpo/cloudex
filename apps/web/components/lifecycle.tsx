"use client";

import { useT } from "@/components/i18n-provider";
import { useState } from "react";
import { useIsMutating, useMutation, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription,
  AlertDialogFooter, AlertDialogHeader, AlertDialogTitle, AlertDialogTrigger } from "@/components/ui/alert-dialog";
import { Notice } from "./shared";
import { change, idempotencyKey, mutationOutcomeUnknown, repositoryCredentialKey, workspaceState, wsPath,
  type Accepted, type Workspace } from "@/lib/api";
import { grantBindingQuery, grantsQuery } from "@/lib/grants";

type Kind = "start" | "stop" | "rebuild" | "reset" | "delete";
type LifecycleRequest = Readonly<{
  kind: Kind;
  key: string;
  body: Readonly<{confirm: string; purge: boolean}>;
}>;
const labels = { start: "start", stop: "stop", rebuild: "rebuild", reset: "resetProject", delete: "deleteWorkspace" } as const;
const effects = {
  stop: "stopEffects",
  rebuild: "rebuildEffects",
  reset: "resetEffects",
  delete: "deleteEffects",
} as const;

export function Lifecycle({workspace, busy, onQueued, destructive = false, startBlocked = false}: {
  workspace: Workspace; busy: boolean; onQueued: (result: Accepted, kind: Kind) => void; destructive?:boolean; startBlocked?:boolean;
}) {
  const { t } = useT();
  const client = useQueryClient();
  const savingCredential = useIsMutating({ mutationKey: repositoryCredentialKey(workspace.id) }) > 0;
  const bindingKey = grantBindingQuery(workspace.id).queryKey;
  const savingGrant = useIsMutating({ mutationKey: bindingKey }) > 0;
  const [purge,setPurge] = useState(false);
  const mutation = useMutation({
    mutationFn: ({kind, body, key}: LifecycleRequest) =>
      change<Accepted>(wsPath(body.confirm) + (kind === "delete" ? "" : "/" + kind), kind === "delete" ? "DELETE" : "POST", body, key),
    onSuccess: (response, {kind}) => {
      client.invalidateQueries({queryKey:["workspaces"]});
      client.invalidateQueries({queryKey:["operations"]});
      client.invalidateQueries({queryKey:["workspace",workspace.id]});
      client.invalidateQueries({queryKey:["access",workspace.id]});
      client.invalidateQueries({queryKey: bindingKey});
      if (kind === "delete") client.invalidateQueries({queryKey: grantsQuery.queryKey});
      onQueued(response,kind);
    },
  });
  const state = workspaceState(workspace);
  const uncertainRequest = mutationOutcomeUnknown(mutation.error) ? mutation.variables : undefined;
  const disabled = busy || mutation.isPending || !!uncertainRequest;
  const awaitingCredential = (kind: Kind) => (savingGrant || savingCredential) && (kind === "rebuild" || kind === "reset");
  function queue(kind: Kind) {
    if (disabled || awaitingCredential(kind)) return;
    mutation.mutate({
      kind,
      key: idempotencyKey(),
      body: {confirm: workspace.id, purge: kind === "delete" && purge},
    });
  }
  const confirm = (kind: Exclude<Kind,"start">) => <AlertDialog key={kind} onOpenChange={open=>{if(open)setPurge(false);}}>
    <AlertDialogTrigger asChild><Button type="button" variant={destructive ? "destructive" : "outline"} disabled={disabled || awaitingCredential(kind)}>{t(labels[kind])}</Button></AlertDialogTrigger>
    <AlertDialogContent>
      <AlertDialogHeader><AlertDialogTitle>{t("lifecycleConfirm", { action: t(labels[kind]), name: workspace.name })}</AlertDialogTitle>
        <AlertDialogDescription>{t(effects[kind])}</AlertDialogDescription></AlertDialogHeader>
      {kind==="delete" && <label className="label-inline rounded-md border border-border bg-[var(--sunken)] px-3 py-2"><input type="checkbox" checked={purge} onChange={e=>setPurge(e.target.checked)}/>
        {t("purgeWorkspace")}</label>}
      <AlertDialogFooter><AlertDialogCancel>{t("keepWorkspace")}</AlertDialogCancel>
        <AlertDialogAction variant={destructive ? "danger" : "default"} disabled={disabled || awaitingCredential(kind)} onClick={()=>queue(kind)}>{t(labels[kind])}</AlertDialogAction>
      </AlertDialogFooter>
    </AlertDialogContent>
  </AlertDialog>;
  return <div className="grid gap-3">
    <div className="actions">
      {destructive ? <>{confirm("reset")}{confirm("delete")}</> : <>
        {(state==="running" || state==="paused") && confirm("stop")}
        {state==="stopped" && !!workspace.generation &&
          <Button type="button" variant="outline" disabled={disabled || startBlocked} onClick={()=>queue("start")}>{t("start")}</Button>}
        {confirm("rebuild")}
      </>}
    </div>
    {mutation.error && <Notice error={mutation.error}
      retry={uncertainRequest && !awaitingCredential(uncertainRequest.kind) ? ()=>mutation.mutate(uncertainRequest) : undefined}/>}
  </div>;
}
