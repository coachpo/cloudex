"use client";

import { useT } from "@/components/i18n-provider";
import Link from "next/link";
import { useQuery } from "@tanstack/react-query";
import { Activity, ArrowRight, Box, Boxes, Database, GitBranch, Play, RefreshCw, Search, Server } from "lucide-react";
import { useState } from "react";
import { Operations } from "@/components/operations";
import { Empty, Loading, Meter, NewWorkspace, Notice, PageHeader, Status } from "@/components/shared";
import { hostName, statusName, type Translate } from "@/lib/i18n";
import { Button } from "@/components/ui/button";
import { api, active, canPlaceOnHost, hostListQuery, hostPath, hostReady, settingsQuery, stamp, workspaceState, wsPath,
  type Workspace, type Operation } from "@/lib/api";

function repositoryLabel(url: string, t: Translate) {
  if (!url) return t("localRepository");
  try {
    const repository = new URL(url);
    return repository.pathname.replace(/^\//, "") || repository.host;
  } catch {
    return t("invalidRepoUrl");
  }
}

export function Workspaces({ initialHostID }: { initialHostID: string }) {
  const { locale, t } = useT();
  const [filter, setFilter] = useState("all");
  const [hostID, setHost] = useState(initialHostID);
  const [term, setTerm] = useState("");
  const ws = useQuery({ queryKey:["workspaces"], queryFn: () => api<Workspace[]>("/workspaces"), refetchInterval: 2500 });
  const ops = useQuery({ queryKey:["operations"], queryFn: () => api<Operation[]>("/operations"), refetchInterval: 2500 });
  const hosts = useQuery(hostListQuery);
  const settings = useQuery(settingsQuery);
  const byHostID = new Map(hosts.data?.map(h => [h.id, h]));
  const selectedHost = byHostID.get(hostID);
  const image = settings.data?.workspace_image;
  const preferredHostID = selectedHost && image && canPlaceOnHost(selectedHost, image) ? selectedHost.id : undefined;
  const all = ws.data ?? [];
  const recent = ops.data?.slice(0,5) ?? [];
  const shown = all.filter(w => (!hostID || w.assigned_host_id === hostID) && (filter === "all" || workspaceState(w) === filter) &&
    [w.name,w.repo_url,w.branch].some(v => v.toLowerCase().includes(term.trim().toLowerCase())));
  const counts = all.reduce<Record<string,number>>((total,w) => ({...total,[w.observed_state]:(total[w.observed_state] ?? 0) + 1}), {});
  const number = (value?: number) => value?.toLocaleString(locale) ?? "—";
  const online = hosts.data?.filter(h => h.status === "online").length;
  return <>
    <PageHeader title={t("workspaces")} actions={<NewWorkspace hostID={preferredHostID}/>}/>
    <section className="stats" aria-label={t("fleetSummary")}>
      <Link className="stat" href="/hosts">
        <span className="stat-label"><Server aria-hidden="true"/>{t("hosts")}</span>
        {hosts.data ? <>
          <span className="stat-value" aria-hidden="true">{number(online)}<small>/ {number(hosts.data.length)}</small></span>
          <span className="sr-only">{t("hostOnlineSummary", { online: number(online), total: number(hosts.data.length) })}</span>
          <Meter value={online ?? 0} total={hosts.data.length} label={t("hostOnlineSummary", { online: number(online), total: number(hosts.data.length) })}/>
        </> : <span className="small muted">{hosts.error ? t("hostStatusUnavailable") : t("loadingHosts")}</span>}
      </Link>
      <div className="stat"><span className="stat-label"><Play aria-hidden="true"/>{t("lastReportedRunning")}</span>
        <span className="stat-value">{number(ws.data ? counts.running ?? 0 : undefined)}</span></div>
      <div className="stat"><span className="stat-label"><Boxes aria-hidden="true"/>{t("totalWorkspaces")}</span>
        <span className="stat-value">{number(ws.data?.length)}</span></div>
      <div className="stat"><span className="stat-label"><Activity aria-hidden="true"/>{t("operationsInFlight")}</span>
        <span className="stat-value">{number(ops.data?.filter(o => active(o.status)).length)}</span></div>
    </section>
    <div className="two-up">
      <section className="panel" aria-labelledby="fleet">
        <div className="section-head">
          <h2 id="fleet">{t("workspaceFleet")}</h2>
          <Button type="button" variant="ghost" size="icon-sm" aria-label={t("refreshWorkspaces")} title={t("refreshWorkspaces")}
            disabled={ws.isFetching} onClick={() => {ws.refetch();ops.refetch();hosts.refetch();}}><RefreshCw/></Button>
        </div>
        <div className="toolbar">
          <div className="segmented" role="group" aria-label={t("filterByState")}>
            {[["all",t("all")],["running",t("state.running")],["paused",t("state.paused")],["stopped",t("state.stopped")],["error",t("needsAttention")]].map(([key,label]) =>
              <button key={key} type="button" className="segment" aria-pressed={key===filter} onClick={() => setFilter(key)}>{label}</button>)}
          </div>
          <label className="field-wrap push max-lg:ml-0"><Search size={15} aria-hidden="true"/>
            <input type="search" value={term} aria-label={t("searchWorkspaces")} placeholder={t("searchWorkspaces")} onChange={e=>setTerm(e.target.value)}/>
          </label>
          <select className="field filter-select" aria-label={t("filterByHost")} value={hostID} onChange={e => setHost(e.target.value)}>
            <option value="">{t("allHosts")}</option>
            {hostID && !byHostID.has(hostID) && <option value={hostID}>{t("selectedHost")}</option>}
            {hosts.data?.map(h => <option value={h.id} key={h.id}>{hostName(t, h)}</option>)}
          </select>
        </div>
        {ws.error && <Notice error={ws.error} retry={() => ws.refetch()}/>}
        {ws.isPending ? <Loading label={t("loadingWorkspaces")} rows={5}/> : ws.data?.length === 0 ?
          <Empty large icon={Box} title={t("noWorkspaces")}>
            <Link className="link" href="/workspaces/new">{t("createFirstWorkspace")}</Link>
          </Empty> :
          ws.data && !shown.length ? <Empty icon={Search}>{t("noFilteredWorkspaces")}</Empty> :
          ws.data && <div className="table-wrap"><table className="data-table interactive">
            <thead><tr><th scope="col">{t("workspace")}</th><th scope="col">{t("state")}</th><th scope="col" className="on-wide">{t("host")}</th>
              <th scope="col" className="on-wide">{t("branch")}</th><th scope="col" className="numeric on-wide">{t("generation")}</th>
              <th scope="col" className="numeric"><span className="sr-only">{t("open")}</span></th></tr></thead>
            <tbody>{shown.map(w => {
              const h = byHostID.get(w.assigned_host_id);
              const location = <><Link className="link" href={hostPath(w.assigned_host_id)}>{h ? hostName(t, h) : t("host")}</Link>
                {h && !hostReady(h) && <span className="entry-secondary">{statusName(t, h.status)}</span>}</>;
              return <tr key={w.id}>
                <td>
                  <Link className="entry-link" href={wsPath(w.id)}><Box aria-hidden="true"/>{w.name}</Link>
                  <span className="entry-secondary on-wide">{repositoryLabel(w.repo_url, t)}</span>
                  <span className="entry-secondary mobile-meta">{location}</span>
                </td>
                <td><Status state={workspaceState(w)}/></td>
                <td className="on-wide">{location}</td>
                <td className="on-wide"><span className="meta-item"><GitBranch size={14} aria-hidden="true"/><code>{w.branch}</code></span></td>
                <td className="numeric on-wide">{number(w.generation || undefined)}</td>
                <td className="numeric"><Link className="rowlink" href={wsPath(w.id)} aria-label={t("openNamed", { name: w.name })}><ArrowRight size={16}/></Link></td>
              </tr>;
            })}</tbody>
          </table></div>}
        {ws.data && <p className="panel-foot">
          {t(shown.length === 1 ? "workspaceCountOne" : "workspaceCount", { count: number(shown.length) })}
          {" · "}{t("updated")} {stamp(locale, new Date(ws.dataUpdatedAt).toISOString())}
        </p>}
      </section>
      <div className="view-grid">
        <section className="panel" aria-labelledby="recent-ops">
          <div className="section-head"><h2 id="recent-ops">{t("recentOperations")}</h2></div>
          {ops.error ? <Notice error={ops.error} retry={()=>ops.refetch()}/> : ops.isPending ? <Loading label={t("loadingActivity")}/> :
            <Operations rows={recent} names={new Map(all.map(w=>[w.id,w.name]))}/>}
        </section>
        <section className="subtle-panel view-grid" aria-labelledby="persistent-storage">
          <h3 id="persistent-storage" className="meta-item"><Database aria-hidden="true" size={14}/>{t("persistentStorage")}</h3>
          <p className="small muted">{t("projectAndSSHStorage")}</p>
        </section>
      </div>
    </div>
  </>;
}
