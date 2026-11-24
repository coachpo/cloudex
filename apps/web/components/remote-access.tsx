"use client";

import { Translation, useT } from "@/components/i18n-provider";
import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { ArrowUpRight, Copy } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription,
  AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Empty, Facts, Notice, Status } from "./shared";
import { change, idempotencyKey, mutationOutcomeUnknown, publicKeyLines, wsPath, type Accepted, type WorkspaceAccess } from "@/lib/api";
import { localizeSSHMessage } from "@/lib/i18n";

const labels = {ready:"sshReady", applying:"sshApplying", disabled:"sshDisabled", stopped:"workspaceStopped",
  paused:"workspacePaused", needs_rebuild:"rebuildRequired", unavailable:"unavailable"} as const;
type SSHRequest = Readonly<{path: string; keys: readonly string[]; key: string}>;

function CopyText({label,value}:{label:string;value:string}) {
  const { t } = useT();
  const [result,setResult]=useState("");
  return <div className="grid gap-2">
    <div className="actions">
      <Button type="button" variant="outline" size="sm" onClick={async()=>{
        try {await navigator.clipboard.writeText(value);setResult(t("copied"));}
        catch {setResult(t("copyManually"));}
      }}><Copy data-icon="inline-start"/>{label}</Button>
      <span className="small muted" role="status">{result}</span>
    </div>
    <pre className="log" tabIndex={0}>{value}</pre>
  </div>;
}

export function RemoteAccess({id,busy,query:q}:{id:string;busy:boolean;query:UseQueryResult<WorkspaceAccess,Error>}) {
  const { locale, t } = useT();
  const path=wsPath(id)+"/access";
  const client=useQueryClient();
  const [draft,setDraft]=useState<string|null>(null);
  const [confirmedKeys,setConfirmedKeys]=useState<string[]|null>(null);
  const mutation=useMutation({
    mutationFn:({path, keys, key}: SSHRequest)=>change<Accepted>(path,"PUT",{public_keys:keys},key),
    onSuccess:()=> {
      setDraft(null);
      return Promise.all([
        client.invalidateQueries({queryKey:["access",id]}),
        client.invalidateQueries({queryKey:["workspace",id]}),
        client.invalidateQueries({queryKey:["operations"]}),
      ]);
    },
  });
  const access=q.data;
  if(!access) return <section className="panel"><h2>{t("remoteSsh")}</h2>
    {q.error?<Notice error={q.error} retry={()=>q.refetch()}/>:<Empty>{t("checkingAccess")}</Empty>}</section>;
  const saved=access.public_keys.join("\n");
  const checkFailed=q.isError || access.status==="unavailable";
  const input=draft??saved;
  const keys=publicKeyLines(input);
  const uncertainRequest = mutationOutcomeUnknown(mutation.error) ? mutation.variables : undefined;
  const disabled=busy||mutation.isPending||access.status==="applying"||!!uncertainRequest;
  const action=keys.length ? (access.enabled ? t("applySshKeys") : t("enableSsh")) : t("disableSsh");
  const submit=(e:FormEvent<HTMLFormElement>)=>{e.preventDefault();setConfirmedKeys(keys);};
  return <>
    {q.error&&<Notice error={q.error} retry={()=>q.refetch()}/>}
    <div className="two-up">
      <section className="panel view-grid" aria-labelledby="connect-desktop">
        <div className="section-head mb-0"><div><h2 id="connect-desktop">{t("connectCodexDesktop")}</h2>
          <p>{t("sharedSshGateway")}</p></div>
          <Status state={access.status==="ready"?"connected":access.status==="unavailable"?"offline":access.status} label={t(labels[access.status])}/>
        </div>
        <Facts items={[
          [t("sshAlias"),<code>{access.alias}</code>],
          [t("gatewayHost"),access.host||null],[t("gatewayPort"),access.port?.toLocaleString(locale, { useGrouping: false })],[t("workspaceUser"),access.user],
          [t("projectFolder"),<code>{access.project_path}</code>],
          [t("workspaceFingerprint"),access.fingerprint&&<code>{access.fingerprint}</code>],
          [t("gatewayFingerprint"),access.gateway_fingerprint&&<code>{access.gateway_fingerprint}</code>],
        ]}/>
        {checkFailed ? <Notice error={q.error ?? new Error(localizeSSHMessage(locale, access.message || "") || t("sshVerificationFailed"))}
          retry={()=>q.refetch()}/> : access.message && <p className="muted" role="status">{localizeSSHMessage(locale, access.message)}</p>}
        {access.status==="disabled"&&<Empty>{t("addKeyHint")}</Empty>}
        {access.ssh_config&&<div className="grid gap-2">
          <p className="muted"><Translation id="testSshHint" values={{ command: <code className="wide-code">ssh {access.alias}</code> }}/></p>
          <div className="actions">
            <Button asChild><a href={`codex://settings/connections/ssh/add?name=${encodeURIComponent(access.alias)}`}>{t("addCodexHost")} <ArrowUpRight data-icon="inline-end"/></a></Button>
            <span className="small muted">{t("codexConnections")}</span>
          </div>
        </div>}
      </section>
      <div className="view-grid">
        <section className="panel">
          <div className="section-head"><div><h2>{t("authorizedKeys")}</h2>
            <p>{t(access.public_keys.length === 1 ? "authorizedKeyCountOne" : "authorizedKeyCount", { count: access.public_keys.length.toLocaleString(locale) })}</p></div></div>
          <form className="form" onSubmit={submit}>
            <FieldGroup className="form"><Field className="fields"><FieldLabel htmlFor="access-keys">{t("sshPublicKeys")}</FieldLabel>
              <textarea className="field" id="access-keys" spellCheck={false} value={input} maxLength={61440}
                disabled={mutation.isPending || !!uncertainRequest} onChange={e=>setDraft(e.target.value)} aria-describedby="access-key-hint"/>
              <p id="access-key-hint" className="form-note">{t("sshPublicKeysHint")}</p>
            </Field></FieldGroup>
            {mutation.error&&<Notice error={mutation.error}
              retry={uncertainRequest ? ()=>mutation.mutate(uncertainRequest) : undefined}/>}
            {mutation.isSuccess&&<p className="small muted" role="status">{t("sshConfigSaved")}</p>}
            <div className="actions"><Button disabled={disabled||keys.join("\n")===saved}>{action}</Button></div>
          </form>
        </section>
        <section className="panel">
          <div className="section-head"><h2>{t("networkAndSignIn")}</h2></div>
          <p className="muted">{t("sshHopsPublicKey")} {access.bind.startsWith("127.")?
            t("sshLoopback"):
            t("sshPrivateNetwork")}</p>
          <p className="muted mt-3"><Translation id="headlessLoginHint" values={{ command: <code>codex login --device-auth</code> }}/></p>
        </section>
      </div>
    </div>
    {access.ssh_config&&access.known_hosts&&<section className="panel">
      <div className="section-head"><div><h2>{t("localSshSetup")}</h2><p>{t("installSshEntries")}</p></div></div>
      <div className="two-up mt-3">
        <div className="grid min-w-0 gap-2">
          <h3>{t("trustBothHosts")}</h3>
          <p className="muted"><Translation id="knownHostsHint" values={{ file: <code>~/.ssh/known_hosts</code> }}/></p>
          <CopyText label={t("copyKnownHosts")} value={access.known_hosts}/>
        </div>
        <div className="grid min-w-0 gap-2">
          <h3>{t("addSshHost")}</h3>
          <p className="muted"><Translation id="sshConfigHint" values={{ file: <code>~/.ssh/config</code> }}/></p>
          <CopyText label={t("copySshConfig")} value={access.ssh_config}/>
        </div>
      </div>
    </section>}
    <AlertDialog open={confirmedKeys!==null} onOpenChange={open=>{if(!open)setConfirmedKeys(null);}}>
      <AlertDialogContent>
        <AlertDialogHeader><AlertDialogTitle>{t("actionQuestion", { action })}</AlertDialogTitle>
          <AlertDialogDescription>{t("sshRebuildEffects")}</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter><AlertDialogCancel>{t("cancel")}</AlertDialogCancel>
          <AlertDialogAction disabled={disabled} onClick={()=>{
            if(confirmedKeys) mutation.mutate({path, keys: confirmedKeys, key: idempotencyKey()});
          }}>{t("confirmAndRebuild")}</AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  </>;
}
