"use client";

import { Translation, useT } from "@/components/i18n-provider";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useRef, useState, type FormEvent } from "react";
import { hostName, statusName } from "@/lib/i18n";
import { Empty, Notice } from "@/components/shared";
import { Button } from "@/components/ui/button";
import { Field, FieldGroup, FieldLabel, FieldLegend, FieldSet } from "@/components/ui/field";
import { APIError, canPlaceOnHost, change, GiB, hostListQuery, idempotencyKey, hostPlacementIssue, hostResourceFields, MiB, publicKeyLines, templatesQuery,
  type Accepted, type Host, type HostResources } from "@/lib/api";

export function NewWorkspaceForm({ initialHostID }: { initialHostID: string }) {
  const { t } = useT();
  const router = useRouter();
  const client = useQueryClient();
  const templates = useQuery(templatesQuery);
  const hosts = useQuery(hostListQuery);
  const template = templates.data?.[0];
  const [selectedHost, setHost] = useState(initialHostID);
  const [cores, setCores] = useState<string>();
  const [memory, setMemory] = useState<string>();
  const [disk, setDisk] = useState<string>();
  const request = useRef<{ payload: string; key: string } | null>(null);
  const host = hosts.data?.find(h => h.id === selectedHost);
  const eligible = (h: Host) => !!template && canPlaceOnHost(h, template.image);
  const resources = {
    cpu_milli: Math.round(Number(cores ?? (template?.cpu_milli ?? 0) / 1000) * 1000),
    memory_mb: Number(memory ?? template?.memory_mb ?? 0),
    disk_bytes: Math.round(Number(disk ?? (template?.disk_bytes ?? 0) / GiB) * GiB),
  };
  const mutation = useMutation({
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
    const repository = { url: f.get("url"), branch: f.get("branch") };
    const username = String(f.get("repo_username") ?? "");
    const token = String(f.get("repo_token") ?? "");
    const credential = username || token ? { username, token } : undefined;
    const body = {
      name:f.get("name"),template_id:template?.id,auto_start:f.has("auto"),
      ssh_public_keys:publicKeyLines(String(f.get("ssh_public_keys")??"")),
      repository:{...repository, credential},
      placement:{type:"manual",host_id:host.id}, resources,
    };
    const payload = JSON.stringify({ ...body, repository: { ...repository, username: credential?.username } });
    if (request.current?.payload !== payload) request.current = { payload, key: idempotencyKey() };
    mutation.mutate({key:request.current.key,body});
  };
  return <>
    <div className="title"><div><Link className="back link" href="/workspaces">{t("backWorkspaces")}</Link><h1>{t("newWorkspace")}</h1></div></div>
    <section className="panel max-w-3xl" aria-labelledby="workspace-configuration">
      <div className="section-head"><div><h2 id="workspace-configuration">{t("workspaceConfig")}</h2>
        {template && <p>{template.name} <span className="wide-code">· <code>{template.image}</code></span></p>}</div></div>
      {!template ? (templates.error ? <Notice error={templates.error} retry={()=>templates.refetch()}/> : <Empty>{t("loadingTemplate")}</Empty>) :
        <form className="form" onSubmit={submit}>
          <FieldGroup className="form">
            <FieldSet className="form">
              <FieldLegend className="mb-0">{t("placement")}</FieldLegend>
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
              {host && <CapacityPreview host={host} eligible={eligible(host)} requested={{
                cpu_milli: resources.cpu_milli, memory_bytes: resources.memory_mb * MiB, disk_bytes: resources.disk_bytes,
              }}/>}
            </FieldSet>
            <Field className="fields"><FieldLabel htmlFor="name">{t("workspaceName")}</FieldLabel><input className="field" id="name" name="name" maxLength={64} required placeholder={t("workspaceNameExample")}/></Field>
            <Field className="fields"><FieldLabel htmlFor="ssh-public-keys">{t("sshPublicKeys")} <span className="muted">{t("optional")}</span></FieldLabel>
              <textarea className="field" id="ssh-public-keys" name="ssh_public_keys" rows={3} spellCheck={false} maxLength={61440} aria-describedby="ssh-hint"/>
              <p id="ssh-hint" className="form-note"><Translation id="sshKeySetupHint" values={{ file: <code>.pub</code> }}/></p>
            </Field>
            <Field className="fields"><FieldLabel htmlFor="url">{t("repoUrl")} <span className="muted">{t("optional")}</span></FieldLabel>
              <input className="field" id="url" name="url" type="url" maxLength={1024} placeholder="https://…" aria-describedby="git-hint"/>
              <p id="git-hint" className="form-note">{t("repoUrlHint")}</p></Field>
            <FieldGroup className="form-split">
              <Field className="fields">
                <FieldLabel htmlFor="repo-username">{t("repositoryUsername")} <span className="muted">{t("optional")}</span></FieldLabel>
                <input className="field" id="repo-username" name="repo_username" autoCapitalize="none" autoComplete="off" spellCheck={false} maxLength={256}/>
              </Field>
              <Field className="fields">
                <FieldLabel htmlFor="repo-token">{t("repositoryToken")} <span className="muted">{t("optional")}</span></FieldLabel>
                <input className="field" id="repo-token" name="repo_token" type="password" autoComplete="new-password" spellCheck={false} maxLength={4096}/>
              </Field>
            </FieldGroup>
            <Field className="fields"><FieldLabel htmlFor="branch">{t("gitBranch")}</FieldLabel><input className="field" name="branch" id="branch" defaultValue="main" required maxLength={128}/></Field>
            <label className="label-inline"><input type="checkbox" name="auto" defaultChecked/> {t("keepWorkspaceRunning")}</label>
          </FieldGroup>
          {mutation.error && <Notice error={mutation.error}/>}
          <div className="actions"><Button disabled={!maySubmit || mutation.isPending}>{mutation.isPending ? t("queueingWorkspace") : t("createWorkspace")}</Button>
            <Button asChild variant="ghost"><Link href="/workspaces">{t("cancel")}</Link></Button></div>
        </form>}
    </section>
  </>;
}

function CapacityPreview({ host, eligible, requested }: { host: Host; eligible: boolean; requested: HostResources }) {
  const { locale, t } = useT();
  if (!eligible) return <p className="form-note" role="status">{t("hostIneligible", { name: hostName(t, host) })}</p>;
  const exceeded = hostResourceFields.filter(r => requested[r.key] > host.available[r.key]).map(r => t(r.label));
  return <section aria-label={t("hostCapacityPreview")}>
    <table className="data-table">
      <thead><tr><th scope="col">{t("capacity")}</th><th scope="col" className="numeric">{t("requested")}</th><th scope="col" className="numeric">{t("available")}</th></tr></thead>
      <tbody>{hostResourceFields.map(({ label, key, format }) => <tr key={key}>
        <th scope="row">{t(label)}</th><td className="numeric">{format(requested[key], locale)}</td><td className="numeric">{format(host.available[key], locale)}</td>
      </tr>)}</tbody>
    </table>
    {exceeded.length > 0 && <p className="form-note mt-2" role="status">{t("capacityExceeded", { resources: new Intl.ListFormat(locale).format(exceeded) })}</p>}
  </section>;
}
