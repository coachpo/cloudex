"use client";

import { useT } from "@/components/i18n-provider";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { hostName, statusName } from "@/lib/i18n";
import { Empty, Loading, Meter, Notice, PageHeader } from "@/components/shared";
import { Dices, Server, TriangleAlert } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field";
import { APIError, canPlaceOnHost, change, GiB, hostListQuery, idempotencyKey, hostPlacementIssue, hostResourceFields, MiB, templatesQuery,
  workspaceOrHostNameMaxLength, type Accepted, type Host, type HostResources } from "@/lib/api";
import { randomWorkspaceName } from "@/lib/workspace-name";

export function NewWorkspaceForm({ initialHostID }: { initialHostID: string }) {
  const { t } = useT();
  const router = useRouter();
  const client = useQueryClient();
  const templates = useQuery(templatesQuery);
  const hosts = useQuery(hostListQuery);
  const template = templates.data?.[0];
  const [selectedHost, setHost] = useState(initialHostID);
  const [name, setName] = useState<string>();
  const [cores, setCores] = useState<string>();
  const [memory, setMemory] = useState<string>();
  const [disk, setDisk] = useState<string>();
  const request = useRef<{ payload: string; key: string } | null>(null);
  useEffect(() => {
    const generated = randomWorkspaceName();
    setName(current => current ?? generated);
  }, []);
  const host = hosts.data?.find(h => h.id === selectedHost);
  const eligible = (h: Host) => !!template && canPlaceOnHost(h, template.image);
  const resources = {
    cpu_milli: Math.round(Number(cores ?? (template?.cpu_milli ?? 0) / 1000) * 1000),
    memory_mb: Number(memory ?? template?.memory_mb ?? 0),
    disk_bytes: Math.round(Number(disk ?? (template?.disk_bytes ?? 0) / GiB) * GiB),
  };
  const mutation = useMutation({
    gcTime: 0,
    mutationFn: ({body,key}: {body: unknown; key: string}) => change<Accepted>("/workspaces","POST",body,key),
    onSettled: (_data, error) => {
      client.invalidateQueries({queryKey:hostListQuery.queryKey});
      if (error instanceof APIError && error.code === "HOST_INCOMPATIBLE") {
        client.invalidateQueries({queryKey:templatesQuery.queryKey});
      }
    },
    onSuccess: ({workspace_id}) => {
      client.invalidateQueries({queryKey:["workspaces"]});
      client.invalidateQueries({queryKey:["operations"]});
      router.push("/workspaces/" + workspace_id + "/access");
    },
  });
  const maySubmit = !!host && (eligible(host) || mutation.isError);
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (!host || !maySubmit || mutation.isPending) return;
    const f = new FormData(e.currentTarget);
    // Match the API's Unicode whitespace trimming before deriving the retry identity.
    const name = String(f.get("name")).replace(/^\p{White_Space}+|\p{White_Space}+$/gu, "");
    const repository = { url: f.get("url"), branch: f.get("branch") };
    const username = String(f.get("repo_username") ?? "");
    const token = String(f.get("repo_token") ?? "");
    const credential = username || token ? { username, token } : undefined;
    const body = {
      name,template_id:template?.id,auto_start:f.has("auto"),
      repository:{...repository, credential},
      placement:{type:"manual",host_id:host.id}, resources,
    };
    const payload = JSON.stringify({ ...body, repository: { ...repository, username: credential?.username } });
    if (request.current?.payload !== payload) request.current = { payload, key: idempotencyKey() };
    mutation.mutate({key:request.current.key,body});
  };
  return <>
    <PageHeader crumbs={[{ href: "/workspaces", label: t("workspaces") }]} current={t("newWorkspace")} title={t("newWorkspace")}
      description={template && <>{template.name} · <code className="wide-code">{template.image}</code></>}/>
    {!template ? (templates.error ? <Notice error={templates.error} retry={()=>templates.refetch()}/> : <Loading label={t("loadingTemplate")}/>) :
      <form className="create-layout" method="post" aria-label={t("workspaceConfig")} onSubmit={submit}>
        <div className="stack-lg">
          <section className="panel form" aria-labelledby="new-workspace-placement">
            <div className="section-head mb-0"><h2 id="new-workspace-placement">{t("placement")}</h2></div>
            <Field className="fields">
              <FieldLabel htmlFor="target-host">{t("targetHost")}</FieldLabel>
              <select className="field" id="target-host" name="host_id" value={host?.id ?? ""} required onChange={e => setHost(e.target.value)}>
                <option value="" disabled>{hosts.isPending ? t("loadingHosts") : t("selectHost")}</option>
                {hosts.data?.map(h => <option key={h.id} value={h.id} disabled={!eligible(h) && h.id !== selectedHost}>
                  {hostName(t, h)}{!eligible(h) ? ` · ${statusName(t, hostPlacementIssue(h, template.image))}` : ""}
                </option>)}
              </select>
              {!hosts.error && hosts.data && !hosts.data.some(eligible) &&
                <p className="form-note">{t("noEligibleHosts")} <Link className="link" href="/hosts">{t("viewHosts")}</Link></p>}
            </Field>
            {hosts.error && <Notice error={hosts.error} retry={() => hosts.refetch()}/>}
            <FieldGroup className="form-resources">
              <Field className="fields"><FieldLabel htmlFor="cores">{t("cpuLimit")}</FieldLabel>
                <input className="field" type="number" step="0.1" min="0.1" max="1000" id="cores" name="cores"
                  value={cores ?? template.cpu_milli / 1000} onChange={e => setCores(e.target.value)} required/></Field>
              <Field className="fields"><FieldLabel htmlFor="memory">{t("memoryLimit")}</FieldLabel>
                <input className="field" type="number" min="256" max="1048576" id="memory" name="memory"
                  value={memory ?? template.memory_mb} onChange={e => setMemory(e.target.value)} required/></Field>
              <Field className="fields"><FieldLabel htmlFor="disk">{t("diskReservationGiB")}</FieldLabel>
                <input className="field" type="number" min="0" step="0.25" id="disk" name="disk" aria-describedby="disk-hint"
                  value={disk ?? (template.disk_bytes ?? 0) / GiB} onChange={e => setDisk(e.target.value)} required/></Field>
            </FieldGroup>
            <p id="disk-hint" className="form-note">{t("diskReservationHint")}</p>
          </section>
          <section className="panel form" aria-labelledby="new-workspace-identity">
            <div className="section-head mb-0"><h2 id="new-workspace-identity">{t("workspace")}</h2></div>
            <Field className="fields">
              <FieldLabel htmlFor="name">{t("workspaceName")}</FieldLabel>
              <div className="flex min-w-0 gap-2">
                <input className="field min-w-0 flex-1" id="name" name="name" value={name ?? ""} onChange={e => setName(e.target.value)}
                  maxLength={workspaceOrHostNameMaxLength} required placeholder={t("workspaceNameExample")}/>
                <Button type="button" variant="outline" size="icon" aria-label={t("generateWorkspaceName")}
                  onClick={() => setName(randomWorkspaceName(name))}><Dices aria-hidden="true"/></Button>
              </div>
            </Field>
            <label className="label-inline"><input type="checkbox" name="auto" defaultChecked/> {t("keepWorkspaceRunning")}</label>
          </section>
          <section className="panel form" aria-labelledby="new-workspace-repository">
            <div className="section-head mb-0"><h2 id="new-workspace-repository">{t("repository")}</h2></div>
            <Field className="fields"><FieldLabel htmlFor="url">{t("repoUrl")} <span className="optional">{t("optional")}</span></FieldLabel>
              <input className="field" id="url" name="url" type="url" maxLength={1024} placeholder="https://…" aria-describedby="git-hint"/>
              <p id="git-hint" className="form-note">{t("repoUrlHint")}</p></Field>
            <FieldGroup className="form-split">
              <Field className="fields">
                <FieldLabel htmlFor="repo-username">{t("repositoryUsername")} <span className="optional">{t("optional")}</span></FieldLabel>
                <input className="field" id="repo-username" name="repo_username" autoCapitalize="none" autoComplete="off" spellCheck={false} maxLength={256}/>
              </Field>
              <Field className="fields">
                <FieldLabel htmlFor="repo-token">{t("repositoryToken")} <span className="optional">{t("optional")}</span></FieldLabel>
                <input className="field" id="repo-token" name="repo_token" type="password" autoComplete="new-password" spellCheck={false} maxLength={4096}/>
              </Field>
            </FieldGroup>
            <Field className="fields"><FieldLabel htmlFor="branch">{t("gitBranch")}</FieldLabel><input className="field font-mono" name="branch" id="branch" defaultValue="main" required maxLength={128}/></Field>
          </section>
          {mutation.error && <Notice error={mutation.error}/>}
          <div className="form-actions"><Button disabled={!maySubmit || mutation.isPending}>{mutation.isPending ? t("queueingWorkspace") : t("createWorkspace")}</Button>
            <Button asChild variant="ghost"><Link href="/workspaces">{t("cancel")}</Link></Button></div>
        </div>
        <aside className="panel" aria-labelledby="capacity-preview-heading">
          <div className="section-head"><h2 id="capacity-preview-heading">{t("hostCapacityPreview")}</h2></div>
          {host ? <CapacityPreview host={host} eligible={eligible(host)} requested={{
            cpu_milli: resources.cpu_milli, memory_bytes: resources.memory_mb * MiB, disk_bytes: resources.disk_bytes,
          }}/> : <Empty icon={Server}>{t("selectHost")}</Empty>}
        </aside>
      </form>}
  </>;
}

function CapacityPreview({ host, eligible, requested }: { host: Host; eligible: boolean; requested: HostResources }) {
  const { locale, t } = useT();
  if (!eligible) return <p className="notice warn" role="status"><TriangleAlert aria-hidden="true"/>{t("hostIneligible", { name: hostName(t, host) })}</p>;
  const exceeded = hostResourceFields.filter(r => requested[r.key] > host.available[r.key]).map(r => t(r.label));
  return <div className="view-grid">
    <p className="meta-item font-medium"><Server aria-hidden="true" size={14}/>{hostName(t, host)}</p>
    {hostResourceFields.map(({ label, key, format }) => <div className="meter-row" key={key}>
      <div className="meter-figures"><span>{t(label)}</span>
        <span><strong>{format(requested[key], locale)}</strong> / {format(host.available[key], locale)}</span></div>
      <Meter value={requested[key]} total={host.available[key]} warnWhenFull label={`${t(label)}: ${t("requested")} ${format(requested[key], locale)}, ${t("available")} ${format(host.available[key], locale)}`}/>
    </div>)}
    <p className="form-note">{t("requested")} / {t("available")}</p>
    {exceeded.length > 0 && <p className="notice warn" role="status"><TriangleAlert aria-hidden="true"/>{t("capacityExceeded", { resources: new Intl.ListFormat(locale).format(exceeded) })}</p>}
  </div>;
}
