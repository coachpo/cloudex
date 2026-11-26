"use client";

import { Translation, useT } from "@/components/i18n-provider";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Fragment, useEffect, useState, type ReactNode } from "react";
import { CircleAlert, GitBranch, GitCompare, Info, KeyRound, Layers, LayoutDashboard, Pause, RefreshCw, ScrollText, Server, Settings2, SquareTerminal, Trash2, TriangleAlert } from "lucide-react";
import { Lifecycle } from "./lifecycle";
import { Operations, operationName } from "./operations";
import { WorkspaceTerminal } from "./terminal";
import { RemoteAccess } from "./remote-access";
import { WorkspaceRepositoryCredential } from "./workspace-repository-credential";
import { Empty, Facts, Loading, MetaItem, Meter, Notice, PageHeader, Status } from "./shared";
import { hostName, statusName } from "@/lib/i18n";
import { Button } from "@/components/ui/button";
import { active, api, byteSize, hostPath, hostQuery, hostReady, stamp, workspaceState, wsPath, type Accepted, type Container, type Generation,
  type Git, type Metrics, type Operation, type Workspace, type WorkspaceAccess } from "@/lib/api";

const views = [["","overview",LayoutDashboard],["access","access",KeyRound],["git","git",GitCompare],["logs","logs",ScrollText],["settings","settings",Settings2],["terminal","webTerminal",SquareTerminal]] as const;
function scoped<T>(id:string, suffix:string, interval?:number, enabled=true) {
  return {queryKey:[suffix,id], queryFn:({signal}:{signal:AbortSignal})=>api<T>(wsPath(id)+"/"+suffix, {signal}), refetchInterval:interval, enabled};
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
    !!q.data && view==="access"));
  const deletion=o.data?.find(op=>op.id===deleting);
  useEffect(()=>{if(deletion?.status==="succeeded"){
    client.invalidateQueries({queryKey:["workspaces"]});router.replace("/workspaces");
  }},[deletion?.status,client,router]);
  const crumbs=[{href:"/workspaces",label:t("workspaces")}];
  if(!q.data) return <>
    <PageHeader crumbs={crumbs} title={t("workspace")}/>
    <section className="panel"><div className="section-head"><h2>{t("workspaceState")}</h2></div>
      {deletion?.status==="succeeded" ? <Empty icon={Trash2}>{t("workspaceDeleted")}</Empty> :
        q.error ? <Notice error={q.error} retry={()=>q.refetch()}/> : <Loading label={t("loadingWorkspace")}/>}</section></>;
  const w=q.data;
  const state=workspaceState(w);
  const busy=!!o.data?.some(op=>active(op.status))||["creating","rebuilding","deleting"].includes(w.observed_state);
  const reachable=!!host.data&&hostReady(host.data);
  const unreachable=!!host.data&&!reachable;
  const running=w.observed_state==="running"&&!busy&&reachable;
  const queued=(o:Accepted,kind:string)=>{if(kind==="delete")setDeleting(o.operation_id);};
  return <>
    <PageHeader crumbs={crumbs} current={w.name} title={w.name} status={<Status state={state}/>}
      meta={<>
        <MetaItem icon={Server}><Link className="link" href={hostPath(w.assigned_host_id)}>{host.data ? hostName(t, host.data) : t("host")}</Link></MetaItem>
        <MetaItem icon={Layers}>{w.generation ? t("generationNumber", { number: w.generation.toLocaleString(locale) }) : t("notProvisioned")}</MetaItem>
        <MetaItem icon={GitBranch}><code>{w.branch}</code></MetaItem>
      </>}
      actions={<>
        {view!=="access"&&<Button asChild><Link href={wsPath(id)+"/access"}><KeyRound data-icon="inline-start"/>{t("connectSsh")}</Link></Button>}
        <Lifecycle workspace={w} busy={busy} onQueued={queued} startBlocked={!reachable}/>
      </>}>
      <nav className="tabbar mt-3" aria-label={t("workspaceViews")}>{views.map(([value,label,Icon])=>
        <Link className="tab" key={value} href={wsPath(id)+(value?"/"+value:"")} aria-current={view===value?"page":undefined}>
          <Icon size={15} strokeWidth={1.75} aria-hidden="true"/>{t(label)}</Link>)}</nav>
    </PageHeader>
    {q.error && <Notice error={q.error} retry={()=>q.refetch()}/>}
    {unreachable && <div className="notice warn" role="status"><TriangleAlert aria-hidden="true"/><p className="notice-body">
      {t("hostLastReported", { host: hostName(t, host.data), state: statusName(t, host.data.status) })}
      {!host.data.reachable && host.data.last_seen && " " + t("lastSeenTime", { time: stamp(locale, host.data.last_seen) })}</p></div>}
    {host.error && <Notice error={host.error} retry={()=>host.refetch()}/>}
    {state==="paused" && reachable ? <div className="notice info" role="status"><Pause aria-hidden="true"/><p className="notice-body">{t("unpauseDocker")}</p></div> :
      w.error && <div className="notice" role="status"><CircleAlert aria-hidden="true"/><p className="notice-body"><b>{t("workspaceNeedsAttention")}</b>{" "}{w.error}</p></div>}
    {busy && o.data?.[0] && <div className="notice info" role="status"><span className="spinner mt-0.5" aria-hidden="true"/>
      <p className="notice-body">{t("lifecycleOperation")} {operationName(t, o.data[0].type)} · {statusName(t, o.data[0].status)}</p></div>}
    {view==="" && <Inspector workspace={w} running={running} reachable={reachable}>
      <section className="panel"><div className="section-head"><h2>{t("lifecycleHistory")}</h2></div>
        {o.error ? <Notice error={o.error} retry={()=>o.refetch()}/> : o.data ? <Operations rows={o.data} canCancel/> : <Loading label={t("loadingActivity")}/>}
      </section>
    </Inspector>}
    {view==="access" && <RemoteAccess query={access}/>}
    {view==="terminal" && <>
      {running ? <WorkspaceTerminal key={w.generation} id={id}/> : <section className="panel"><div className="section-head"><h2>{t("developmentTerminal")}</h2></div>
        <Empty large icon={SquareTerminal}>{unreachable?t("terminalHostUnavailable"):state==="paused"?t("terminalContainerPaused"):t("waitForWorkspaceStart")}</Empty></section>}
      <p className="hint"><Info aria-hidden="true"/><span><Translation id="terminalLoginHint" values={{ command: <code className="code-inline">codex login --device-auth</code> }}/></span></p></>}
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
  const cpuLimit=w.cpu_milli/10;
  return <div className="two-up">
    <section className="panel"><div className="section-head"><h2>{t("containerInspector")}</h2></div>
      {running&&m.data && <div className="meters mb-4">
        <div className="meter-row">
          <div className="meter-figures"><span>{t("liveCpu")}</span><strong>{m.data.cpu_percent!=null ? (m.data.cpu_percent / 100).toLocaleString(locale, { style: "percent", minimumFractionDigits: 1, maximumFractionDigits: 1 }) : "—"}</strong></div>
          {m.data.cpu_percent != null && <Meter value={m.data.cpu_percent} total={cpuLimit} warnWhenFull label={t("liveCpu")}/>}
        </div>
        <div className="meter-row">
          <div className="meter-figures"><span>{t("liveMemory")}</span><span><strong>{byteSize(m.data.memory_bytes, locale)}</strong> / {byteSize(m.data.memory_limit_bytes, locale)}</span></div>
          <Meter value={m.data.memory_bytes} total={m.data.memory_limit_bytes} warnWhenFull label={t("liveMemory")}/>
        </div>
      </div>}
      <Facts items={[
        [t("image"),<code>{w.image}</code>],[t("repository"),w.repo_url || t("localRepository")],
        [t("state"),d ? t("observedAndDesired", { observed: statusName(t, d.state), desired: statusName(t, w.desired_state) })
          : t("desiredState", { state: statusName(t, w.desired_state) })],
        [t("limits"),t("resourceLimits", { cpu: (w.cpu_milli / 1000).toLocaleString(locale), memory: w.memory_mb.toLocaleString(locale) })],
        [t("diskReservation"),byteSize(w.disk_bytes, locale)],
        ...(running&&m.data ? [] : [[t("liveCpu"),null],[t("liveMemory"),null]] as [string, null][]),
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
        <div className="table-wrap"><table className="data-table"><thead><tr><th scope="col">{t("generation")}</th><th scope="col">{t("container")}</th><th scope="col">{t("recordedState")}</th></tr></thead>
          <tbody>{h.data.map(v=><tr key={v.generation}><td>{v.generation.toLocaleString(locale)}</td><td><code>{v.id.slice(0,12)}</code></td><td><Status state={v.status}/></td></tr>)}</tbody>
        </table></div> : h.isPending ? <Loading label={t("loadingGenerations")}/> : <Empty icon={Layers}>{t("noContainer")}</Empty>}
    </section></div>
  </div>;
}

function Patch({text}: {text:string}) {
  return <>{text.split("\n").map((line,index)=>{
    const kind=line.startsWith("+")&&!line.startsWith("+++")?"diff-add":line.startsWith("-")&&!line.startsWith("---")?"diff-del":undefined;
    return <Fragment key={index}>{index > 0 && "\n"}{kind?<span className={kind}>{line}</span>:line}</Fragment>;
  })}</>;
}
function GitSnapshot({g}: {g:Git}) {
  const { t } = useT();
  return <div className="view-grid">
    <Facts items={[["HEAD",<code>{g.head}</code>],[t("workingTree"),g.status?
      <pre className="log" tabIndex={0} role="region" aria-label={t("workingTree")}><code>{g.status}</code></pre>:t("clean")]]}/>
    {g.patch?<pre className="log" tabIndex={0} role="region" aria-label={t("gitPatch")}><code><Patch text={g.patch}/></code></pre>:<Empty icon={GitCompare}>{t("noTrackedChanges")}</Empty>}
    {g.truncated&&<p className="hint"><Info aria-hidden="true"/>{t("truncatedSnapshot")}</p>}
  </div>;
}
function GitView({id,running,unreachable}:{id:string;running:boolean;unreachable:boolean}) {
  const { t } = useT();
  const q=useQuery(scoped<Git>(id,"git",undefined,running));
  return <section className="panel"><div className="section-head"><h2>{t("workingTree")}</h2><Button variant="outline" size="sm" onClick={()=>q.refetch()} disabled={!running||q.isFetching}><RefreshCw data-icon="inline-start"/>{t("refresh")}</Button></div>
    {!running?<Empty large icon={GitCompare}>{unreachable?t("gitHostUnavailable"):t("gitNeedsStart")}</Empty>:q.error?<Notice error={q.error} retry={()=>q.refetch()}/>:q.data?<GitSnapshot g={q.data}/>:<Loading label={t("readingGitStatus")}/>}
  </section>;
}
function LogsView({id,generation,reachable}:{id:string;generation:number;reachable:boolean}) {
  const { t } = useT();
  const q=useQuery({
    ...scoped<{text:string;truncated:boolean}>(id,"logs",undefined,generation>0&&reachable),
    queryKey:["logs",id,generation],
  });
  return <section className="panel"><div className="section-head"><h2>{t("containerLogs")}</h2><Button variant="outline" size="sm" onClick={()=>q.refetch()} disabled={!generation||!reachable||q.isFetching}><RefreshCw data-icon="inline-start"/>{t("refresh")}</Button></div>
    {!generation?<Empty large icon={ScrollText}>{t("noContainer")}</Empty>:!reachable?<Empty large icon={ScrollText}>{t("logsHostUnavailable")}</Empty>:q.error?<Notice error={q.error} retry={()=>q.refetch()}/>:q.isPending?<Loading label={t("readingContainerLogs")} rows={6}/>:
      q.data?.text?<pre className="log" tabIndex={0} role="region" aria-label={t("containerLogs")}>{q.data.text}{q.data.truncated?"\n"+t("outputTruncated"):""}</pre>:<Empty icon={ScrollText}>{t("noContainerOutput")}</Empty>}
  </section>;
}
function WorkspaceSettings({workspace:w,busy,onQueued}:{workspace:Workspace;busy:boolean;onQueued:(o:Accepted,k:string)=>void}) {
  const { t } = useT();
  return <div className="view-grid">
      {w.repo_url && <WorkspaceRepositoryCredential key={"credential-" + w.id} id={w.id} busy={busy}/>}
      <section className="panel danger-zone"><div className="section-head"><h2>{t("dataAndRemoval")}</h2></div>
        <p className="muted mb-4">{t("resetPurgeHint")}</p>
        <Lifecycle workspace={w} busy={busy} onQueued={onQueued} destructive/>
      </section>
  </div>;
}
