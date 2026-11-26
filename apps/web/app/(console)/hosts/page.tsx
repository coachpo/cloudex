"use client";

import { useT } from "@/components/i18n-provider";
import { hostName } from "@/lib/i18n";
import Link from "next/link";
import { useQuery } from "@tanstack/react-query";
import { ArrowRight, KeyRound, Plus, RadioTower, Server } from "lucide-react";
import { useRef, useState } from "react";
import { byteSize, canPlaceOnHost, hostImageReady, hostListQuery, hostPath, settingsQuery, stamp } from "@/lib/api";
import { HostEnrollmentPanel } from "@/components/host-enrollment";
import { Empty, Loading, Notice, PageHeader, Status } from "@/components/shared";
import { Button } from "@/components/ui/button";

export default function HostsPage() {
  const { locale, t } = useT();
  const [adding, setAdding] = useState(false);
  const addHost = useRef<HTMLButtonElement>(null);
  const hosts = useQuery(hostListQuery);
  const settings = useQuery(settingsQuery);
  const image = settings.data?.workspace_image;
  return <>
    <PageHeader title={t("hosts")} actions={<Button ref={addHost} type="button" aria-expanded={adding} aria-disabled={adding} className="aria-disabled:opacity-50"
      onClick={() => { if (!adding) setAdding(true); }}><Plus data-icon="inline-start"/>{t("addHost")}</Button>}/>
    {adding && <HostEnrollmentPanel onClose={() => {
      setAdding(false);
      addHost.current?.focus();
    }}/>}
    <section className="panel" aria-labelledby="host-fleet">
      <div className="section-head"><div><h2 id="host-fleet">{t("dockerHosts")}</h2><p>{t("newReservationCapacity")}</p></div>
        {hosts.data && <span className="count">{hosts.data.length.toLocaleString(locale)}</span>}</div>
      {hosts.error && <Notice error={hosts.error} retry={() => hosts.refetch()}/>}
      {settings.error && <Notice error={settings.error} retry={() => settings.refetch()}/>}
      {hosts.isPending ? <Loading label={t("loadingHosts")} rows={4}/> : hosts.data?.length === 0 ? <Empty large icon={Server} title={t("noRegisteredHosts")}/> :
        hosts.data && <div className="table-wrap"><table className="data-table interactive host-table">
          <thead><tr>
            <th scope="col">{t("host")}</th><th scope="col">{t("connection")}</th><th scope="col" className="on-wide">{t("scheduling")}</th><th scope="col" className="numeric on-wide">{t("cpuCores")}</th>
            <th scope="col" className="numeric on-wide">{t("memory")}</th><th scope="col" className="numeric on-wide">{t("disk")}</th>
            <th scope="col" className="numeric"><span className="sr-only">{t("open")}</span></th>
          </tr></thead>
          <tbody>{hosts.data.map(h => {
            const capacity = image && canPlaceOnHost(h, image) ? h.available : null;
            const Kind = h.connection_type === "ssh" ? KeyRound : h.id === "local" ? Server : RadioTower;
            return <tr key={h.id}>
              <td>
                <Link className="entry-link" href={hostPath(h.id)}><Kind aria-hidden="true"/>{hostName(t, h)}</Link>
                <span className="entry-secondary">{h.id === "local" ? t("controlPlaneHost") : h.connection_type === "ssh" ? t("sshDirect") : h.engine ? `Docker ${h.engine}` : t("remoteHost")}</span>
                {capacity && <span className="entry-secondary mobile-meta">
                  {t("hostResourceSummary", { cpu: (capacity.cpu_milli / 1000).toLocaleString(locale),
                    memory: byteSize(capacity.memory_bytes, locale), disk: byteSize(capacity.disk_bytes, locale) })}
                </span>}
              </td>
              <td><Status state={h.status}/>
                <span className="entry-secondary mobile-meta mt-1.5"><span className="sr-only">{t("scheduling")} </span><Status state={h.scheduling_state}/></span>
                {h.status === "offline" && h.last_seen && <span className="entry-secondary">{t("lastSeenTime", { time: stamp(locale, h.last_seen) })}</span>}
                {h.status === "online" && image && !hostImageReady(h, image) && <span className="entry-secondary">{t("workspaceImageMissing")}</span>}
              </td>
              <td className="on-wide"><Status state={h.scheduling_state}/></td>
              <td className="numeric on-wide">{capacity ? (capacity.cpu_milli / 1000).toLocaleString(locale) : "—"}</td>
              <td className="numeric on-wide">{capacity ? byteSize(capacity.memory_bytes, locale) : "—"}</td>
              <td className="numeric on-wide">{capacity ? byteSize(capacity.disk_bytes, locale) : "—"}</td>
              <td className="numeric"><Link className="rowlink" href={hostPath(h.id)} aria-label={t("openNamed", { name: hostName(t, h) })}><ArrowRight size={16}/></Link></td>
            </tr>;
          })}</tbody>
        </table></div>}
    </section>
  </>;
}
