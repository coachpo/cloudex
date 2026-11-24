"use client";

import { useT } from "@/components/i18n-provider";
import { hostName, localizeSSHMessage, type MessageKey } from "@/lib/i18n";
import Link from "next/link";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { useEffect, useState } from "react";
import { Empty, Facts, Notice, Status } from "./shared";
import { HostEnrollmentPanel } from "./host-enrollment";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription,
  AlertDialogFooter, AlertDialogHeader, AlertDialogTitle, AlertDialogTrigger } from "./ui/alert-dialog";
import { Button } from "./ui/button";
import { Field, FieldError, FieldGroup, FieldLabel } from "./ui/field";
import { api, byteSize, canPlaceOnHost, change, GiB, hostImageReady, hostListQuery, hostPath, hostQuery, hostReady, hostResourceFields, hostWorkspacePath, mutationOutcomeUnknown, saveSetting, settingsQuery, stamp, wsPath,
  type Host, type HostScheduling, type HostContainer, type Image } from "@/lib/api";

export function HostDetail({ id }: { id: string }) {
  const { locale, t } = useT();
  const client = useQueryClient();
  const host = useQuery(hostQuery(id));
  const reconnect = useMutation({
    mutationFn: () => change<Host>(hostPath(id) + "/ssh/reconnect", "POST"),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: hostQuery(id).queryKey, exact: true });
      void client.invalidateQueries({ queryKey: hostListQuery.queryKey });
    },
  });
  const settings = useQuery(settingsQuery);
  const [issuing, setIssuing] = useState(false);
  const info = host.data;
  const image = settings.data?.workspace_image;
  return <>
    <div className="title">
      <div><Link className="back link" href="/hosts">{t("backHosts")}</Link>
        <h1>{info ? hostName(t, info) : t("host")}{info && <Status state={info.status}/>}</h1>
        {info?.last_seen && <p>{t("lastSeenTime", { time: stamp(locale, info.last_seen) })}</p>}
      </div>
      {info && <div className="actions">
        <Button asChild variant="outline"><Link href={hostWorkspacePath("/workspaces", id)}>{t("viewWorkspaces")}</Link></Button>
        {image && canPlaceOnHost(info, image) && <Button asChild><Link href={hostWorkspacePath("/workspaces/new", id)}>
          <Plus data-icon="inline-start"/>{t("newWorkspace")}
        </Link></Button>}
        {info.connection_type === "agent" && info.id !== "local" && info.scheduling_state !== "disabled" && (info.status === "enrolling" || info.status === "offline") &&
          <Button disabled={issuing} type="button" onClick={() => setIssuing(true)}>{t("issueNewToken")}</Button>}
        {info.connection_type === "ssh" && info.scheduling_state !== "disabled" && (info.status === "offline" || info.status === "degraded") &&
          <Button type="button" variant="outline" onClick={() => reconnect.mutate()} disabled={reconnect.isPending}>
            {t(reconnect.isPending ? "hostSSHConnecting" : "retryHostSSH")}
          </Button>}
        {info.connection_type === "ssh" && <Button disabled={issuing} type="button" variant="outline" onClick={() => setIssuing(true)}>{t("updateHostSSH")}</Button>}
      </div>}
    </div>
    {host.error && <Notice error={host.error} retry={() => host.refetch()}/>}
    {reconnect.error && <Notice error={reconnect.error}/>}
    {reconnect.isSuccess && (info?.status === "offline" || info?.status === "degraded") && info.scheduling_state !== "disabled" &&
      <p className="small muted" role="status">{t("hostReconnectRequested")}</p>}
    {info?.connection_error && <p role="alert" className="notice">{localizeSSHMessage(locale, info.connection_error)}</p>}
    {issuing && info && <HostEnrollmentPanel key={info.id} host={info} onClose={() => setIssuing(false)}/>}
    {info ? <>
      <HostSchedulingPanel host={info}/>
      <div className="two-up">
        <div className="view-grid">
          <Capacity key={info.id} info={info}/>
          <HostContainers host={info}/>
          <HostImages host={info}/>
        </div>
        <div className="view-grid">
          <section className="panel" aria-labelledby="engine-heading">
            <div className="section-head"><h2 id="engine-heading">{t("dockerEngine")}</h2></div>
            <Facts items={[
              [t("version"), info.engine || null],
              [t("osArch"), [info.os, info.arch].filter(Boolean).join(" · ") || null],
              info.connection_type === "ssh"
                ? [t("sshDirect"), `${info.ssh_user}@${info.ssh_address}`]
                : [t("agentVersion"), info.agent_version || null],
              [t("runningContainers"), info.engine ? t("runningContainerCount", { running: info.running_containers.toLocaleString(locale), total: info.total_containers.toLocaleString(locale) }) : null],
              [t("defaultWorkspaceImage"), image ? <><code>{image}</code>
                <span className="entry-secondary">{hostImageReady(info, image) ? t("state.ready") : t("workspaceImageNotReady")}</span></> : null],
            ]}/>
          </section>
          <OperationLimit key={info.id} info={info} observedAt={host.dataUpdatedAt} unavailable={host.isError}/>
        </div>
      </div>
    </> : host.isPending && <Empty>{t("loadingHost")}</Empty>}
  </>;
}

const schedulingDescription: Record<HostScheduling, MessageKey> = {
  enabled: "scheduleEnabled",
  cordoned: "scheduleCordoned",
  disabled: "scheduleDisabled",
};

function HostSchedulingPanel({ host }: { host: Host }) {
  const { t } = useT();
  const client = useQueryClient();
  const mode = useMutation({
    mutationFn: (mode: HostScheduling) => change<Host>(hostPath(host.id) + "/mode", "PUT", { mode }),
    onSuccess: async (updated) => {
      const queryKey = hostQuery(host.id).queryKey;
      await client.cancelQueries({ queryKey, exact: true });
      // A mode receipt can update scheduling without replacing newer inventory or settings.
      client.setQueryData<Host>(queryKey, current => {
        if (!current || updated.scheduling_version <= current.scheduling_version) return;
        const disabled = updated.scheduling_state === "disabled";
        return {
          ...current,
          scheduling_state: updated.scheduling_state,
          scheduling_version: updated.scheduling_version,
          reachable: disabled ? updated.reachable : current.reachable,
          status: disabled ? updated.status : current.status,
        };
      });
      await Promise.all([
        client.invalidateQueries({ queryKey, exact: true }),
        client.invalidateQueries({ queryKey: hostListQuery.queryKey }),
      ]);
    },
  });
  const scheduling = host.scheduling_state;
  return <section className="panel view-grid" aria-labelledby="scheduling-heading">
    <div className="section-head mb-0">
      <div><h2 id="scheduling-heading">{t("hostScheduling")}</h2><p>{t(schedulingDescription[scheduling])}</p></div>
      <Status state={scheduling}/>
    </div>
    <div className="actions">
      {scheduling === "enabled" && <Button type="button" onClick={() => mode.mutate("cordoned")} disabled={mode.isPending}>
        {t("cordonHost")}
      </Button>}
      {scheduling !== "enabled" &&
        <Button type="button" variant="outline" onClick={() => mode.mutate("enabled")} disabled={mode.isPending}>
          {t("resumeScheduling")}
        </Button>}
      {scheduling !== "disabled" && <AlertDialog>
        <AlertDialogTrigger asChild><Button type="button" variant="ghost" disabled={mode.isPending}>{t("disableHost")}</Button></AlertDialogTrigger>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("disableHostTitle", { name: hostName(t, host) })}</AlertDialogTitle>
            <AlertDialogDescription>{t("disableHostEffects")}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("keepHost")}</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={() => mode.mutate("disabled")}>{t("disableHost")}</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>}
    </div>
    {mode.error && <Notice error={mode.error}/>}
  </section>;
}

function OperationLimit({ info, observedAt, unavailable }: { info: Host; observedAt: number; unavailable: boolean }) {
  const { locale, t } = useT();
  const client = useQueryClient();
  const [draft, setDraft] = useState<string>();
  const target = info.max_operations_target;
  const actual = info.max_operations;
  const value = draft ?? String(target);
  const limit = Number(value);
  const valid = Number.isInteger(limit) && limit >= 1 && limit <= 100;
  const invalid = draft !== undefined && !valid;
  const hostKey = hostQuery(info.id).queryKey;
  const save = useMutation({
    mutationFn: (max_operations: number) => saveSetting(client, hostKey,
      () => change(hostPath(info.id) + "/max-operations", "PUT", { max_operations })),
    onSuccess: (acknowledged) => {
      if (acknowledged) setDraft(undefined);
      void client.invalidateQueries({ queryKey: hostListQuery.queryKey });
    },
  });
  const { reset } = save;
  const confirmed = !unavailable && observedAt >= save.submittedAt && save.variables === target;
  const unconfirmed = save.data === false;
  useEffect(() => {
    if (unconfirmed && confirmed) {
      setDraft(undefined);
      reset();
    }
  }, [unconfirmed, confirmed, reset]);
  return <section className="panel" aria-labelledby="operation-limit-heading">
    <div className="section-head"><h2 id="operation-limit-heading">{t("hostOperationLimit")}</h2></div>
    <Facts items={[
      [t("hostOperationTarget"), target.toLocaleString(locale)],
      [t("hostOperationActual"), info.last_seen ? actual.toLocaleString(locale) : null],
    ]}/>
    {target !== actual && <p className="small muted mt-2" role="status">
      {t(info.last_seen ? "operationLimitApplying" : "operationLimitWaitingForHost")}
    </p>}
    <form className="form mt-3" noValidate onSubmit={e => {
      e.preventDefault();
      if (valid && limit !== target && !save.isPending && !unavailable) save.mutate(limit);
    }}>
      <Field className="fields" data-invalid={invalid} data-disabled={save.isPending}>
        <FieldLabel htmlFor="host-operation-limit">{t("hostOperationNewLimit")}</FieldLabel>
        <div className="actions">
          <input id="host-operation-limit" className="field max-w-24" type="number" min={1} max={100} step={1}
            value={value} disabled={save.isPending} aria-invalid={invalid}
            aria-describedby={invalid ? "host-operation-limit-error" : undefined} onChange={e => {
              setDraft(e.target.value);
              reset();
            }}/>
          <Button type="submit" variant="outline" disabled={!valid || limit === target || save.isPending || unavailable}>
            {save.isPending ? t("saving") : t("saveOperationLimit")}
          </Button>
        </div>
        {invalid && <FieldError id="host-operation-limit-error">{t("error.INVALID_MAX_OPERATIONS")}</FieldError>}
      </Field>
      {save.error && <Notice error={save.error}/>}
      {unconfirmed && !confirmed && <p className="notice" role="alert">{t("settingSaveUnconfirmed")}</p>}
    </form>
  </section>;
}

function capacityGiB(bytes: number) {
  const gib = bytes / GiB;
  // Ten decimal places are enough to round-trip any safe byte count.
  for (let decimals = 0; decimals < 10; decimals++) {
    const value = gib.toFixed(decimals);
    if (capacityBytes(value) === bytes) return value;
  }
  return gib.toFixed(10).replace(/\.?0+$/, "");
}

function capacityBytes(gib: string) {
  const decimal = /^(\d*)(?:\.(\d*))?$/.exec(gib.trim());
  if (!decimal || (!decimal[1] && !decimal[2])) return undefined;

  // Half-byte boundaries in GiB have at most 31 decimal places.
  const fraction = decimal[2]?.slice(0, 31) ?? "";
  const scale = 10n ** BigInt(fraction.length);
  const fractionBytes = (BigInt(fraction) * BigInt(GiB) + scale / 2n) / scale;
  const bytes = Number(decimal[1]) * GiB + Number(fractionBytes);
  return Number.isSafeInteger(bytes) ? bytes : undefined;
}

type HostCapacity = Pick<Host, "allocatable" | "reserved" | "available">;

function Capacity({ info }: { info: Host }) {
  const { locale, t } = useT();
  const client = useQueryClient();
  const [draft, setDraft] = useState<string>();
  const path = hostPath(info.id) + "/capacity";
  const capacityKey = ["host-capacity", info.id];
  // Mode changes can publish a Host snapshot that predates a capacity save.
  const capacity = useQuery({
    queryKey: capacityKey,
    queryFn: ({ signal }) => api<HostCapacity>(path, { signal }),
    placeholderData: info,
    refetchInterval: 5000,
  });
  const { allocatable, reserved, available } = capacity.data ?? info;
  const current = allocatable.disk_bytes;
  const gib = draft ?? capacityGiB(current);
  const bytes = capacityBytes(gib);
  const invalid = draft !== undefined && bytes === undefined;
  const save = useMutation({
    mutationFn: async (bytes: number) => {
      let unconfirmed = false;
      try {
        await change<Host>(path, "PUT", { disk_bytes: bytes });
      } catch (error) {
        if (!(error instanceof Error) || !mutationOutcomeUnknown(error)) throw error;
        unconfirmed = true;
      }

      void client.invalidateQueries({ queryKey: hostQuery(info.id).queryKey, exact: true });
      void client.invalidateQueries({ queryKey: hostListQuery.queryKey });
      // An observer showing placeholder data can still have its first read in flight.
      await client.cancelQueries({ queryKey: capacityKey, exact: true });
      const refreshed = await client.invalidateQueries({ queryKey: capacityKey, exact: true }, { throwOnError: true })
        .then(() => client.getQueryData<HostCapacity>(capacityKey), () => undefined);
      return {
        refreshed: refreshed !== undefined,
        confirmed: !unconfirmed || refreshed?.allocatable.disk_bytes === bytes,
      };
    },
    onSuccess: ({ refreshed, confirmed }) => { if (refreshed && confirmed) setDraft(undefined); },
  });
  const { reset: resetSave } = save;
  const unconfirmed = save.data?.confirmed === false;
  const confirmedByPoll = unconfirmed && capacity.isSuccess && capacity.dataUpdatedAt >= save.submittedAt && save.variables === current;
  useEffect(() => {
    if (confirmedByPoll) {
      setDraft(undefined);
      resetSave();
    }
  }, [confirmedByPoll, resetSave]);
  const resources = info.last_seen ? hostResourceFields : hostResourceFields.filter(({ key }) => key === "disk_bytes");
  return <section className="panel" aria-labelledby="capacity-heading">
    <div className="section-head"><div><h2 id="capacity-heading">{t("workspaceCapacity")}</h2>
      {info.last_seen && !hostReady(info) && <p>{t("lastReportedCapacity")}</p>}
      {!info.last_seen && <p>{t("capacityPending")}</p>}</div></div>
    <table className="data-table">
      <thead><tr><th scope="col">{t("resource")}</th><th scope="col" className="numeric on-wide">{t("allocatable")}</th>
        <th scope="col" className="numeric">{t("reserved")}</th><th scope="col" className="numeric">{t("available")}</th></tr></thead>
      <tbody>{resources.map(({ label, key, format }) => <tr key={key}>
        <th scope="row">{t(label)}<span className="entry-secondary mobile-meta">{t("allocatableAmount", { amount: format(allocatable[key], locale) })}</span></th>
        <td className="numeric on-wide">{format(allocatable[key], locale)}</td>
        <td className="numeric">{format(reserved[key], locale)}</td>
        <td className="numeric">{format(available[key], locale)}</td>
      </tr>)}</tbody>
    </table>
    {capacity.error && <Notice error={capacity.error} retry={() => capacity.refetch()}/>}
    <form className="mt-3" onSubmit={e => {
      e.preventDefault();
      if (bytes !== undefined && bytes !== current && !save.isPending) save.mutate(bytes);
    }}>
      <FieldGroup>
        <Field className="fields" data-disabled={save.isPending} data-invalid={invalid}>
          <FieldLabel htmlFor="host-disk-capacity">{t("hostDiskCapacityGiB")}</FieldLabel>
          <div className="actions">
            <input className="field max-w-36" id="host-disk-capacity" inputMode="decimal" required
              aria-invalid={invalid} aria-describedby={invalid ? "host-disk-capacity-error" : undefined}
              value={gib} disabled={save.isPending} onChange={e => {
                setDraft(e.target.value);
                if (save.error || unconfirmed) resetSave();
              }}/>
            <Button type="submit" variant="outline" disabled={bytes === undefined || bytes === current || save.isPending}>
              {save.isPending ? t("saving") : t("saveCapacity")}
            </Button>
          </div>
          {invalid && <FieldError id="host-disk-capacity-error" className="break-all">
            {t("hostDiskCapacityInvalid", { max: capacityGiB(Number.MAX_SAFE_INTEGER) })}
          </FieldError>}
        </Field>
      </FieldGroup>
      {save.error && <Notice error={save.error}/>}
      {unconfirmed && !confirmedByPoll && <p className="notice" role="alert">{t("hostDiskCapacityUnconfirmed")}</p>}
    </form>
  </section>;
}
function HostContainers({ host }: { host: Host }) {
  const { t } = useT();
  const c = useQuery({ queryKey: ["host-containers", host.id], queryFn: () => api<HostContainer[]>(hostPath(host.id) + "/containers"),
    refetchInterval: 6000, enabled: hostReady(host) });
  return <section className="panel" aria-labelledby="containers-heading">
      <div className="section-head"><h2 id="containers-heading">{t("containerInventory")}</h2></div>
      {!hostReady(host) ? <Empty>{t("containersUnavailable")}</Empty> :
        c.error ? <Notice error={c.error} retry={() => c.refetch()}/> :
        !c.data ? <Empty>{t("loadingContainers")}</Empty> : !c.data.length ? <Empty>{t("noHostContainers")}</Empty> :
        <table className="data-table"><thead><tr><th scope="col">{t("container")}</th><th scope="col">{t("state")}</th><th scope="col" className="on-wide">{t("scope")}</th></tr></thead>
          <tbody>{c.data.map(v => <tr key={v.id}><td>
            {v.workspace_id ? <Link className="entry-link link" href={wsPath(v.workspace_id)}>{v.name}</Link> :
              <span className="entry-link">{v.name || v.id.slice(0, 12)}</span>}
            <span className="entry-secondary">{v.image}</span>
            <span className="entry-secondary mobile-meta">{v.workspace_id ? t("managed") : t("readOnly")}</span></td>
            <td><Status state={v.state}/></td><td className="on-wide">{v.workspace_id ? t("managed") : t("readOnly")}</td>
          </tr>)}</tbody></table>}
  </section>;
}

function HostImages({ host }: { host: Host }) {
  const { locale, t } = useT();
  const i = useQuery({ queryKey: ["host-images", host.id], queryFn: () => api<Image[]>(hostPath(host.id) + "/images"),
    refetchInterval: 12000, enabled: hostReady(host) });
  return <section className="panel" aria-labelledby="images-heading">
      <div className="section-head"><h2 id="images-heading">{t("localImages")}</h2></div>
      {!hostReady(host) ? <Empty>{t("imagesUnavailable")}</Empty> :
        i.error ? <Notice error={i.error} retry={() => i.refetch()}/> :
        !i.data ? <Empty>{t("loadingImages")}</Empty> : !i.data.length ? <Empty>{t("noLocalImages")}</Empty> :
        <table className="data-table"><thead><tr><th scope="col">{t("tags")}</th><th scope="col" className="on-wide">{t("imageId")}</th>
          <th scope="col" className="numeric">{t("size")}</th></tr></thead>
          <tbody>{i.data.map(v => {
            const shortID = v.id.replace(/^sha256:/, "").slice(0, 12);
            return <tr key={v.id}><td className="wide-code">{v.tags?.join(", ") || t("untagged")}
              <span className="entry-secondary mobile-meta"><code>{shortID}</code></span></td>
              <td className="on-wide"><code>{shortID}</code></td><td className="numeric whitespace-nowrap">{byteSize(v.size_bytes, locale)}</td></tr>;
          })}</tbody></table>}
  </section>;
}
