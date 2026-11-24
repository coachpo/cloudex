"use client";

import { useT } from "@/components/i18n-provider";
import Link from "next/link";
import { useQuery } from "@tanstack/react-query";
import { ArrowRight, Box, CircuitBoard, GitBranch, RefreshCw, Search } from "lucide-react";
import { useState } from "react";
import { Operations } from "@/components/operations";
import { Empty, NewWorkspace, Notice, Status, VolumeLegend } from "@/components/shared";
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
  return <>
    <div className="title"><h1>{t("workspaces")}</h1><NewWorkspace hostID={preferredHostID}/></div>
    <section className="panel topology" aria-label={t("fleetSummary")}>
      <div><h2 className="host-link"><CircuitBoard size={16} aria-hidden="true"/><Link href="/hosts" className="link">{t("hosts")}</Link></h2>
        <span className="small muted">{hosts.data ? t("hostOnlineSummary", { online: number(hosts.data.filter(h => h.status === "online").length), total: number(hosts.data.length) }) : hosts.error ? t("hostStatusUnavailable") : t("loadingHosts")}</span></div>
      <div><span className="small muted">{t("lastReportedRunning")}</span><strong>{number(ws.data ? counts.running ?? 0 : undefined)}</strong></div>
      <div><span className="small muted">{t("totalWorkspaces")}</span><strong>{number(ws.data?.length)}</strong></div>
      <div><span className="small muted">{t("operationsInFlight")}</span><strong>{number(ops.data?.filter(o => active(o.status)).length)}</strong></div>
    </section>
    <div className="two-up">
      <section className="panel" aria-labelledby="fleet">
        <div className="section-head"><h2 id="fleet">{t("workspaceFleet")}</h2>
          <Button type="button" variant="ghost" size="icon-sm" aria-label={t("refreshWorkspaces")}
            disabled={ws.isFetching} onClick={() => {ws.refetch();ops.refetch();hosts.refetch();}}><RefreshCw/></Button>
        </div>
        <div className="tabbar" aria-label={t("filterByState")}>
          {[["all",t("all")],["running",t("state.running")],["paused",t("state.paused")],["stopped",t("state.stopped")],["error",t("needsAttention")]].map(([key,label]) =>
            <button key={key} className="tab" aria-pressed={key===filter} onClick={() => setFilter(key)}>{label}</button>)}
        </div>
        <div className="toolbar py-3"><label className="field-wrap"><Search size={15} aria-hidden="true"/>
          <input type="search" value={term} aria-label={t("searchWorkspaces")} placeholder={t("searchWorkspaces")} onChange={e=>setTerm(e.target.value)}/>
        </label>
          <select className="field filter-select" aria-label={t("filterByHost")} value={hostID} onChange={e => setHost(e.target.value)}>
            <option value="">{t("allHosts")}</option>
            {hostID && !byHostID.has(hostID) && <option value={hostID}>{t("selectedHost")}</option>}
            {hosts.data?.map(h => <option value={h.id} key={h.id}>{hostName(t, h)}</option>)}
          </select>
          <span className="push small muted">{ws.data ? t(shown.length === 1 ? "workspaceCountOne" : "workspaceCount", { count: number(shown.length) }) : ""}</span></div>
        {ws.error && <Notice error={ws.error} retry={() => ws.refetch()}/>}
        <table className="data-table">
          <thead><tr><th>{t("workspace")}</th><th>{t("state")}</th><th className="on-wide">{t("host")}</th><th className="on-wide">{t("branch")}</th>
            <th className="numeric on-wide">{t("generation")}</th><th className="numeric"><span className="sr-only">{t("open")}</span></th></tr></thead>
          <tbody>{shown.map(w=> {
            const h = byHostID.get(w.assigned_host_id);
            const location = <><Link className="link" href={hostPath(w.assigned_host_id)}>{h ? hostName(t, h) : t("host")}</Link>
              {h && !hostReady(h) && <span className="entry-secondary">{statusName(t, h.status)}</span>}</>;
            return <tr key={w.id}><td>
            <Link className="entry-link link" href={wsPath(w.id)}><Box size={15} aria-hidden="true"/>{w.name}</Link>
            <span className="entry-secondary on-wide">{repositoryLabel(w.repo_url, t)}</span>
            <span className="entry-secondary mobile-meta">{location}</span>
          </td><td><Status state={workspaceState(w)}/></td><td className="on-wide host-title">{location}</td>
          <td className="on-wide"><span className="actions"><GitBranch size={14} aria-hidden="true"/>{w.branch}</span></td>
          <td className="numeric on-wide">{number(w.generation || undefined)}</td>
          <td className="numeric"><Link className="rowlink" href={wsPath(w.id)} aria-label={t("openNamed", { name: w.name })}><ArrowRight size={16}/></Link></td></tr>;
          })}</tbody>
        </table>
        {ws.isPending ? <Empty>{t("loadingWorkspaces")}</Empty> : ws.data?.length === 0 ?
          <Empty>{t("noWorkspaces")} <Link className="link" href="/workspaces/new">{t("createFirstWorkspace")} <span aria-hidden="true">→</span></Link></Empty> :
          ws.data && !shown.length ? <Empty>{t("noFilteredWorkspaces")}</Empty> : null}
        {ws.data && <p className="small muted mt-3">{t("updated")} {stamp(locale, new Date(ws.dataUpdatedAt).toISOString())}</p>}
      </section>
      <div className="view-grid">
        <section className="panel" aria-labelledby="recent-ops"><div className="section-head"><h2 id="recent-ops">{t("recentOperations")}</h2></div>
          {ops.error ? <Notice error={ops.error} retry={()=>ops.refetch()}/> : ops.isPending ? <Empty>{t("loadingActivity")}</Empty> :
            <Operations rows={recent} names={new Map(all.map(w=>[w.id,w.name]))}/>}
        </section>
        <section className="panel"><div className="section-head"><h2>{t("persistentStorage")}</h2></div>
          <VolumeLegend state={t("projectAndSSHStorage")}/>
          <p className="small muted mt-2">{t("rebuildStorageHint")}</p>
        </section>
      </div>
    </div>
  </>;
}
