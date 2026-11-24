"use client";

import { Translation, useT } from "@/components/i18n-provider";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type ReactNode } from "react";
import { Lifecycle } from "./lifecycle";
import { Operations, operationName } from "./operations";
import { WorkspaceTerminal } from "./terminal";
import { RemoteAccess } from "./remote-access";
import { WorkspaceCodexSettings } from "./workspace-codex-settings";
import { WorkspaceRepositoryCredential } from "./workspace-repository-credential";
import { Empty, Facts, Notice, Status } from "./shared";
import { hostName, localizeSSHMessage, statusName } from "@/lib/i18n";
import { Button } from "@/components/ui/button";
import { active, api, byteSize, hostPath, hostQuery, hostReady, stamp, workspaceState, wsPath, type Accepted, type Container, type Generation,
  type Git, type Metrics, type Operation, type Workspace, type WorkspaceAccess } from "@/lib/api";

const views = [["","overview"],["access","access"],["git","git"],["logs","logs"],["settings","settings"],["terminal","webTerminal"]] as const;
function scoped<T>(id:string, suffix:string, interval?:number, enabled=true) {
  return {queryKey:[suffix,id], queryFn:()=>api<T>(wsPath(id)+"/"+suffix), refetchInterval:interval, enabled};
}

export function WorkspaceDetails({id,view}: {id:string;view:string}) {
  const { locale, t } = useT();
  const router = useRouter();
  const client=useQueryClient();
  const [deleting,setDeleting]=useState<string>();
  const q=useQuery({queryKey:["workspace",id],queryFn:()=>api<Workspace>(wsPath(id)),refetchInterval:2000});
  const hostID=q.data?.assigned_host_id??"";
  const host=useQuery({...hostQuery(hostID),enabled:!!hostID});
  const o=useQuery(scoped<Operation[]>(id,"operations",1500));
  const access=useQuery(scoped<WorkspaceAccess>(id,"access",3500,
    !!q.data && (view==="access" || workspaceState(q.data)==="stopped")));
  const deletion=o.data?.find(op=>op.id===deleting);
  useEffect(()=>{if(deletion?.status==="succeeded"){
    client.invalidateQueries({queryKey:["workspaces"]});router.replace("/workspaces");
  }},[deletion?.status,client,router]);
  if(!q.data) return <><div className="title"><div><Link className="back link" href="/workspaces">{t("backWorkspaces")}</Link><h1>{t("workspace")}</h1></div></div>
    <section className="panel"><h2>{t("workspaceState")}</h2>{deletion?.status==="succeeded" ? <Empty>{t("workspaceDeleted")}</Empty> :
      q.error ? <Notice error={q.error} retry={()=>q.refetch()}/> : <Empty>{t("loadingWorkspace")}</Empty>}</section></>;
  const w=q.data;
  const state=workspaceState(w);
  const busy=!!o.data?.some(op=>active(op.status))||["creating","rebuilding","deleting"].includes(w.observed_state);
  const reachable=!!host.data&&hostReady(host.data);
  const unreachable=!!host.data&&!reachable;
  const running=w.observed_state==="running"&&!busy&&reachable;
  const canStart=reachable && access.isSuccess && (access.data.status==="stopped" || access.data.status==="disabled");
  const queued=(o:Accepted,kind:string)=>{if(kind==="delete")setDeleting(o.operation_id);};
  return <>
    <div className="title"><div><Link className="back link" href="/workspaces">{t("backWorkspaces")}</Link>
      <h1>{w.name}<Status state={state}/></h1>
      <p><Link className="link" href={hostPath(w.assigned_host_id)}>{host.data ? hostName(t, host.data) : t("host")}</Link> · {w.generation ? t("generationNumber", { number: w.generation.toLocaleString(locale) }) : t("notProvisioned")} · {w.branch}</p></div>
      <div className="actions">
        {view!=="access"&&<Button asChild><Link href={wsPath(id)+"/access"}>{t("connectSsh")}</Link></Button>}
        <Lifecycle workspace={w} busy={busy} onQueued={queued}
          startBlocked={!canStart}/>
      </div>
    </div>
    <nav className="tabbar" aria-label={t("workspaceViews")}>{views.map(([value,label])=>
      <Link className="tab" key={value} href={wsPath(id)+(value?"/"+value:"")} aria-current={view===value?"page":undefined}>{t(label)}</Link>)}</nav>
    {q.error && <Notice error={q.error} retry={()=>q.refetch()}/>}
    {unreachable && <p className="muted" role="status">{t("hostLastReported", { host: hostName(t, host.data), state: statusName(t, host.data.status) })}
      {!host.data.reachable && host.data.last_seen && " " + t("lastSeenTime", { time: stamp(locale, host.data.last_seen) })}</p>}
    {host.error && <Notice error={host.error} retry={()=>host.refetch()}/>}
    {state==="paused" && reachable ? <p className="muted" role="status">{t("unpauseDocker")}</p> :
      w.error && <p className="notice" role="status"><b>{t("workspaceNeedsAttention")}</b> {w.error}</p>}
    {busy && o.data?.[0] && <p className="muted small" role="status">{t("lifecycleOperation")} {operationName(t, o.data[0].type)} · {statusName(t, o.data[0].status)}</p>}
    {reachable && view!=="access" && state==="stopped" && access.data?.status==="needs_rebuild" &&
      <p role="status" className="muted">{t("rebuildForSSHSettings")}</p>}
    {reachable && view!=="access" && state==="stopped" && (access.isError || access.data?.status==="unavailable") &&
      <Notice error={access.error ?? new Error(localizeSSHMessage(locale, access.data?.message || "") || t("sshVerificationFailed"))}
        retry={()=>access.refetch()}/>}
    {view==="" && <Inspector workspace={w} running={running} reachable={reachable}>
      <section className="panel"><div className="section-head"><h2>{t("lifecycleHistory")}</h2></div>
        {o.error ? <Notice error={o.error} retry={()=>o.refetch()}/> : o.data ? <Operations rows={o.data} canCancel/> : <Empty>{t("loadingActivity")}</Empty>}
      </section>
    </Inspector>}
    {view==="access" && <RemoteAccess id={id} busy={busy} query={access}/>}
    {view==="terminal" && <><p className="muted"><Translation id="terminalLoginHint" values={{ command: <code>codex login --device-auth</code> }}/></p>
      {running ? <WorkspaceTerminal key={w.generation} id={id}/> : <section className="panel"><h2>{t("developmentTerminal")}</h2>
        <Empty>{unreachable?t("terminalHostUnavailable"):state==="paused"?t("terminalContainerPaused"):t("waitForWorkspaceStart")}</Empty></section>}</>}
    {view==="git" && <GitView id={id} running={running} unreachable={unreachable}/>}
    {view==="logs" && <LogsView id={id} generation={w.generation} reachable={reachable}/>}
    {view==="settings" && <WorkspaceSettings workspace={w} busy={busy} onQueued={queued}/>}
  </>;
}

function Inspector({workspace:w,running,reachable,children}: {workspace:Workspace;running:boolean;reachable:boolean;children:ReactNode}) {
  const { locale, t } = useT();
  const c=useQuery(scoped<Container>(w.id,"inspect",5000,w.generation>0&&reachable));
  const m=useQuery(scoped<Metrics>(w.id,"metrics",5000,running));
  const h=useQuery(scoped<Generation[]>(w.id,"containers",4000));
  const d=reachable?c.data:undefined;
  return <div className="two-up">
    <section className="panel"><div className="section-head"><h2>{t("containerInspector")}</h2></div>
      <Facts items={[
        [t("image"),<code>{w.image}</code>],[t("repository"),w.repo_url || t("localRepository")],
        [t("state"),d ? t("observedAndDesired", { observed: statusName(t, d.state), desired: statusName(t, w.desired_state) })
          : t("desiredState", { state: statusName(t, w.desired_state) })],
        [t("limits"),t("resourceLimits", { cpu: (w.cpu_milli / 1000).toLocaleString(locale), memory: w.memory_mb.toLocaleString(locale) })],
        [t("diskReservation"),byteSize(w.disk_bytes, locale)],
        [t("liveCpu"),running&&m.data?.cpu_percent!=null ? (m.data.cpu_percent / 100).toLocaleString(locale, { style: "percent", minimumFractionDigits: 1, maximumFractionDigits: 1 }) : null],
        [t("liveMemory"),running&&m.data ? `${byteSize(m.data.memory_bytes, locale)} / ${byteSize(m.data.memory_limit_bytes, locale)}` : null],
      ]}/>
      {d && <details className="metadata">
        <summary>{t("containerMetadata")}</summary>
        <Facts items={[
        [t("containerId"),d&&<code>{d.id}</code>],[t("imageId"),d&&<code>{d.image_id}</code>],
        [t("repositoryDigest"),d.image_digests?.join(", ")||null],
        ["PID",d.pid.toLocaleString(locale, { useGrouping: false })],[t("exitCode"),d.state==="exited"?d.exit_code.toLocaleString(locale):null],[t("started"),stamp(locale, d.started_at)],[t("finished"),stamp(locale, d.finished_at)],
        [t("mounts"),d.mounts.map(v=><div key={v.name}><code>{v.destination}</code><span className="entry-secondary">{v.name} · {v.writable ? t("mountReadWrite") : t("mountReadOnly")}</span></div>)],
        [t("networks"),d.networks.join(", ")||null],[t("environmentNames"),d.env_names.join(", ")||null],
        [t("labels"),Object.entries(d.labels).sort().map(([k,v])=><div key={k}><code>{k}={v}</code></div>)],
        ]}/>
      </details>}
      {reachable&&c.error && <Notice error={c.error} retry={()=>c.refetch()}/>}
      {running&&m.error && <Notice error={m.error} retry={()=>m.refetch()}/>}
    </section>
    <div className="view-grid">{children}<section className="panel"><div className="section-head"><h2>{t("containerGenerations")}</h2></div>
      {h.error ? <Notice error={h.error} retry={()=>h.refetch()}/> : h.data?.length ?
        <table className="data-table"><thead><tr><th>{t("generation")}</th><th>{t("container")}</th><th>{t("recordedState")}</th></tr></thead>
          <tbody>{h.data.map(v=><tr key={v.generation}><td>{v.generation.toLocaleString(locale)}</td><td><code>{v.id.slice(0,12)}</code></td><td><Status state={v.status}/></td></tr>)}</tbody>
        </table> : <Empty>{h.isPending?t("loadingGenerations"):t("noContainer")}</Empty>}
    </section></div>
  </div>;
}

function GitSnapshot({g}: {g:Git}) {
  const { t } = useT();
  return <div className="view-grid">
    <Facts items={[["HEAD",<code>{g.head}</code>],[t("workingTree"),g.status?<pre className="log">{g.status}</pre>:t("clean")]]}/>
    {g.patch?<pre className="log" aria-label={t("gitPatch")}>{g.patch}</pre>:<Empty>{t("noTrackedChanges")}</Empty>}
    {g.truncated&&<p className="small muted">{t("truncatedSnapshot")}</p>}
  </div>;
}
function GitView({id,running,unreachable}:{id:string;running:boolean;unreachable:boolean}) {
  const { t } = useT();
  const q=useQuery(scoped<Git>(id,"git",undefined,running));
  return <section className="panel"><div className="section-head"><h2>{t("workingTree")}</h2><Button variant="outline" onClick={()=>q.refetch()} disabled={!running||q.isFetching}>{t("refresh")}</Button></div>
    {!running?<Empty>{unreachable?t("gitHostUnavailable"):t("gitNeedsStart")}</Empty>:q.error?<Notice error={q.error} retry={()=>q.refetch()}/>:q.data?<GitSnapshot g={q.data}/>:<Empty>{t("readingGitStatus")}</Empty>}
  </section>;
}
function LogsView({id,generation,reachable}:{id:string;generation:number;reachable:boolean}) {
  const { t } = useT();
  const q=useQuery({
    ...scoped<{text:string;truncated:boolean}>(id,"logs",undefined,generation>0&&reachable),
    queryKey:["logs",id,generation],
  });
  return <section className="panel"><div className="section-head"><h2>{t("containerLogs")}</h2><Button variant="outline" onClick={()=>q.refetch()} disabled={!generation||!reachable||q.isFetching}>{t("refresh")}</Button></div>
    {!generation?<Empty>{t("noContainer")}</Empty>:!reachable?<Empty>{t("logsHostUnavailable")}</Empty>:q.error?<Notice error={q.error} retry={()=>q.refetch()}/>:q.isPending?<Empty>{t("readingContainerLogs")}</Empty>:
      q.data?.text?<pre className="log">{q.data.text}{q.data.truncated?"\n"+t("outputTruncated"):""}</pre>:<Empty>{t("noContainerOutput")}</Empty>}
  </section>;
}
function WorkspaceSettings({workspace:w,busy,onQueued}:{workspace:Workspace;busy:boolean;onQueued:(o:Accepted,k:string)=>void}) {
  const { t } = useT();
  return <div className="two-up">
    <WorkspaceCodexSettings key={w.id} id={w.id} busy={busy}/>
    <div className="view-grid">
      {w.repo_url && <WorkspaceRepositoryCredential key={w.id} id={w.id} busy={busy}/>}
      <section className="panel"><div className="section-head"><h2>{t("dataAndRemoval")}</h2></div>
        <p className="small muted mb-3">{t("resetPurgeHint")}</p>
        <Lifecycle workspace={w} busy={busy} onQueued={onQueued} destructive/>
      </section>
    </div>
  </div>;
}
