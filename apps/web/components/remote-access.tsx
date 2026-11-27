"use client";

import { Translation, useT } from "@/components/i18n-provider";
import { useState } from "react";
import type { UseQueryResult } from "@tanstack/react-query";
import Link from "next/link";
import { ArrowUpRight, Copy, KeyRound } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Facts, Loading, Notice, Status } from "./shared";
import type { WorkspaceAccess } from "@/lib/api";
import { localizeSSHMessage } from "@/lib/i18n";

const labels = {ready:"sshReady", applying:"sshApplying", stopped:"workspaceStopped",
  paused:"workspacePaused", unavailable:"unavailable"} as const;

function CopyText({label,value}:{label:string;value:string}) {
  const { t } = useT();
  const [result, setResult] = useState<{ value: string; message: "copied" | "copyManually" }>();
  return <div className="grid gap-2">
    <div className="actions">
      <Button type="button" variant="outline" size="sm" onClick={async()=>{
        try {await navigator.clipboard.writeText(value);setResult({ value, message: "copied" });}
        catch {setResult({ value, message: "copyManually" });}
      }}><Copy data-icon="inline-start"/>{label}</Button>
      <span className={result?.value === value && result.message === "copied" ? "confirm" : "small muted"} role="status">{result?.value === value ? t(result.message) : ""}</span>
    </div>
    <pre className="log" tabIndex={0}>{value}</pre>
  </div>;
}

export function RemoteAccess({query:q}:{query:UseQueryResult<WorkspaceAccess,Error>}) {
  const { locale, t } = useT();
  const access=q.data;
  if(!access) return <section className="panel"><div className="section-head"><h2>{t("remoteSsh")}</h2></div>
    {q.error?<Notice error={q.error} retry={()=>q.refetch()}/>:<Loading label={t("checkingAccess")}/>}</section>;
  const checkFailed=q.isError || access.status==="unavailable";
  return <>
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
        {!access.has_public_keys && <p className="notice warn"><KeyRound aria-hidden="true"/><span className="notice-body"><Translation id="workspaceAddSSHKey" values={{ settings: <Link className="link" href="/settings#ssh-keys">{t("sshPublicKeys")}</Link> }}/></span></p>}
        {access.has_public_keys && access.ssh_config && <div className="grid gap-2">
          <p className="muted"><Translation id="testSshHint" values={{ command: <code className="wide-code">ssh {access.alias}</code> }}/></p>
          <div className="actions">
            <Button asChild><a href={`codex://settings/connections/ssh/add?name=${encodeURIComponent(access.alias)}`}>{t("addCodexHost")} <ArrowUpRight data-icon="inline-end"/></a></Button>
            <span className="small muted">{t("codexConnections")}</span>
          </div>
        </div>}
      </section>
      <div className="view-grid">
        <section className="panel">
          <div className="section-head"><h2>{t("sshNetwork")}</h2></div>
          <p className="muted leading-relaxed">{t("sshHopsPublicKey")} {access.bind.startsWith("127.")?
            t("sshLoopback"):
            t("sshPrivateNetwork")}</p>
        </section>
      </div>
    </div>
    {access.ssh_config&&access.known_hosts&&<section className="panel">
      <div className="section-head"><div><h2>{t("localSshSetup")}</h2><p>{t("installSshEntries")}</p></div></div>
      <div className="halves">
        <div className="grid min-w-0 content-start gap-3">
          <h3>{t("trustBothHosts")}</h3>
          <p className="muted"><Translation id="knownHostsHint" values={{ file: <code>~/.ssh/known_hosts</code> }}/></p>
          <CopyText label={t("copyKnownHosts")} value={access.known_hosts}/>
        </div>
        <div className="grid min-w-0 content-start gap-3">
          <h3>{t("addSshHost")}</h3>
          <p className="muted"><Translation id="sshConfigHint" values={{ file: <code>~/.ssh/config</code> }}/></p>
          <CopyText label={t("copySshConfig")} value={access.ssh_config}/>
        </div>
      </div>
    </section>}
  </>;
}
