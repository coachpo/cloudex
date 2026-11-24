"use client";

import { useT } from "@/components/i18n-provider";
import Link from "next/link";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { accountMutationKey, APIError, api, change, hostImageReady, hostListQuery, hostPath, replaceSession, saveSetting, sessionQuery,
  settingsQuery, templatesQuery, workspaceImageDraft, type Session, type Settings } from "@/lib/api";
import { hostName } from "@/lib/i18n";
import { Empty, Facts, Notice } from "@/components/shared";
import { Button } from "@/components/ui/button";
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field";

const settingsKey = settingsQuery.queryKey;

function AccountSettings({ username }: { username: string }) {
  const { t } = useT();
  const client = useQueryClient();
  const form = useRef<HTMLFormElement>(null);
  const [mismatch, setMismatch] = useState(false);
  const write = useMutation({
    gcTime: 0,
    mutationKey: accountMutationKey,
    mutationFn: (account: { username: string; current_password: string; new_password?: string }) =>
      change<Session>("/auth/account", "PUT", account),
    onMutate: () => client.cancelQueries({ queryKey: sessionQuery.queryKey, exact: true }),
    onSuccess: (session) => {
      form.current?.reset();
      return replaceSession(client, session);
    },
  });

  function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const data = new FormData(e.currentTarget);
    const password = String(data.get("new_password"));
    if (password !== data.get("confirmation")) {
      setMismatch(true);
      write.reset();
      return;
    }
    setMismatch(false);
    write.mutate({
      username: String(data.get("username")),
      current_password: String(data.get("current_password")),
      new_password: password || undefined,
    });
  }

  return <section className="panel">
    <div className="section-head"><h2>{t("account")}</h2></div>
    <form className="form" key={username} ref={form} onSubmit={submit}>
      <FieldGroup className="form">
        <Field className="fields">
          <FieldLabel htmlFor="account-username">{t("username")}</FieldLabel>
          <input id="account-username" className="field" name="username" autoComplete="username" defaultValue={username} required/>
        </Field>
        <Field className="fields">
          <FieldLabel htmlFor="current-password">{t("currentPassword")}</FieldLabel>
          <input id="current-password" className="field" name="current_password" type="password" autoComplete="current-password" required/>
        </Field>
        <Field className="fields">
          <FieldLabel htmlFor="new-password">{t("optionalNewPassword")}</FieldLabel>
          <input id="new-password" className="field" name="new_password" type="password" autoComplete="new-password"/>
        </Field>
        <Field className="fields" data-invalid={mismatch}>
          <FieldLabel htmlFor="confirm-new-password">{t("confirmNewPassword")}</FieldLabel>
          <input id="confirm-new-password" className="field" name="confirmation" type="password" autoComplete="new-password"
            aria-invalid={mismatch} aria-describedby={mismatch ? "account-password-mismatch" : undefined}/>
          {mismatch && <p className="small" id="account-password-mismatch" role="alert">{t("passwordMismatch")}</p>}
        </Field>
      </FieldGroup>
      {write.error && <Notice error={write.error}/>}
      {write.isSuccess && <p className="small muted" role="status">{t("accountSaved")}</p>}
      <div className="actions"><Button disabled={write.isPending}>{write.isPending ? t("saving") : t("saveAccount")}</Button></div>
    </form>
  </section>;
}

type SettingsObservation = { observedAt: number; unavailable: boolean };

function WorkspaceImageSettings({ image, observedAt, unavailable }: SettingsObservation & { image: string }) {
  const { t } = useT();
  const client = useQueryClient();
  const hosts = useQuery(hostListQuery);
  const [draft, setDraft] = useState<string>();
  const value = workspaceImageDraft(draft, image) ?? image;
  const submitted = value.trim();
  const write = useMutation({
    mutationFn: async (image: string) => {
      const acknowledged = await saveSetting(client, settingsKey, () => change("/settings/workspace-image", "PUT", { image }));
      await Promise.all([
        client.invalidateQueries({ queryKey: hostListQuery.queryKey }),
        client.invalidateQueries({ queryKey: templatesQuery.queryKey }),
      ]);
      return acknowledged;
    },
    onSuccess: (acknowledged) => { if (acknowledged) setDraft(undefined); },
  });
  const { reset } = write;
  const confirmed = !unavailable && observedAt >= write.submittedAt && write.variables === image;
  const unconfirmed = write.data === false;
  useEffect(() => {
    setDraft(current => workspaceImageDraft(current, image));
  }, [image]);
  useEffect(() => {
    if (unconfirmed && confirmed) {
      setDraft(undefined);
      reset();
    }
  }, [unconfirmed, confirmed, reset]);
  const invalid = write.error instanceof APIError && write.error.code === "INVALID_WORKSPACE_IMAGE";
  const canSave = !!submitted && submitted !== image && !write.isPending && !unavailable;
  return <div className="view-grid mb-3">
    <form className="form" onSubmit={e => {
      e.preventDefault();
      if (canSave) write.mutate(submitted);
    }}>
      <FieldGroup>
        <Field className="fields" data-disabled={write.isPending} data-invalid={invalid}>
          <FieldLabel htmlFor="settings-workspace-image">{t("defaultWorkspaceImage")}</FieldLabel>
          <input id="settings-workspace-image" className="field" value={value}
            autoCapitalize="none" autoComplete="off" spellCheck={false} required disabled={write.isPending}
            aria-invalid={invalid} aria-describedby="workspace-image-hint"
            onChange={e => {
              setDraft(workspaceImageDraft(e.target.value, image));
              reset();
            }}/>
          <p id="workspace-image-hint" className="form-note">{t("workspaceImageHint")}</p>
        </Field>
      </FieldGroup>
      {write.error && <Notice error={write.error}/>}
      {unconfirmed && !confirmed && <p className="notice" role="alert">{t("settingSaveUnconfirmed")}</p>}
      {write.data && confirmed && <p className="small muted" role="status">{t("workspaceImageSaved")}</p>}
      <div className="actions">
        <Button disabled={!canSave}>{write.isPending ? t("saving") : t("saveWorkspaceImage")}</Button>
      </div>
    </form>
    {hosts.error && <Notice error={hosts.error} retry={() => hosts.refetch()}/>}
    {hosts.isPending && <Empty>{t("loadingHosts")}</Empty>}
    {!!hosts.data?.length && <table className="data-table" aria-label={t("workspaceImageReadiness")}>
      {submitted !== image && <caption className="small muted text-left break-all">{t("workspaceImageReadinessFor", { image })}</caption>}
      <thead><tr><th scope="col">{t("host")}</th><th scope="col">{t("workspaceImageReadiness")}</th></tr></thead>
      <tbody>{hosts.data.map(host => <tr key={host.id}>
        <td><Link className="entry-link link" href={hostPath(host.id)}>{hostName(t, host)}</Link></td>
        <td>{t(hostImageReady(host, image) ? "state.ready" : "workspaceImageNotReady")}</td>
      </tr>)}</tbody>
    </table>}
  </div>;
}

type AccessAddresses = Pick<Settings, "ssh_host" | "agent_public_address">;

function AccessSettings({ settings, observedAt, unavailable }: SettingsObservation & { settings: Settings }) {
  const { t } = useT();
  const client = useQueryClient();
  const [draft, setDraft] = useState<Partial<AccessAddresses>>();
  const addresses = {
    ssh_host: draft?.ssh_host ?? settings.ssh_host,
    agent_public_address: draft?.agent_public_address ?? settings.agent_public_address,
  };
  const changed = addresses.ssh_host !== settings.ssh_host || addresses.agent_public_address !== settings.agent_public_address;
  const write = useMutation({
    mutationFn: async (edits: Partial<AccessAddresses>) => {
      // The PUT requires both fields, including any field edited only in another tab.
      const current = edits.ssh_host === undefined || edits.agent_public_address === undefined
        ? await api<AccessAddresses>("/settings") : settings;
      const submitted = {
        ssh_host: edits.ssh_host ?? current.ssh_host,
        agent_public_address: edits.agent_public_address ?? current.agent_public_address,
      };
      const acknowledged = await saveSetting(client, settingsKey, () => change("/settings/access", "PUT", submitted));
      return { acknowledged, submitted };
    },
    onSuccess: ({ acknowledged }) => { if (acknowledged) setDraft(undefined); },
  });
  const { reset } = write;
  const submitted = write.data?.submitted;
  const confirmed = !unavailable && observedAt >= write.submittedAt &&
    submitted?.ssh_host === settings.ssh_host && submitted.agent_public_address === settings.agent_public_address;
  const unconfirmed = write.data?.acknowledged === false;
  useEffect(() => {
    if (unconfirmed && confirmed) {
      setDraft(undefined);
      reset();
    }
  }, [unconfirmed, confirmed, reset]);
  function edit(key: keyof AccessAddresses, value: string) {
    setDraft(current => ({ ...current, [key]: value === settings[key] ? undefined : value }));
    reset();
  }
  return <section className="panel" aria-labelledby="access-addresses-heading">
    <div className="section-head"><h2 id="access-addresses-heading">{t("accessAddresses")}</h2></div>
    <form className="form" onSubmit={e => {
      e.preventDefault();
      if (draft && changed && !write.isPending && !unavailable) write.mutate(draft);
    }}>
      <FieldGroup className="form">
        <Field className="fields" data-disabled={write.isPending}>
          <FieldLabel htmlFor="settings-ssh-host">{t("settingsSshHost")}</FieldLabel>
          <input id="settings-ssh-host" className="field" placeholder={settings.ssh_bind} value={addresses.ssh_host}
            disabled={write.isPending} autoCapitalize="none" spellCheck={false} onChange={e => edit("ssh_host", e.target.value)}/>
        </Field>
        <Field className="fields" data-disabled={write.isPending}>
          <FieldLabel htmlFor="settings-agent-address">{t("settingsAgentAddress")}</FieldLabel>
          <input id="settings-agent-address" className="field" value={addresses.agent_public_address}
            disabled={write.isPending} autoCapitalize="none" spellCheck={false} onChange={e => edit("agent_public_address", e.target.value)}/>
        </Field>
      </FieldGroup>
      {write.error && <Notice error={write.error}/>}
      {unconfirmed && !confirmed && <p className="notice" role="alert">{t("settingSaveUnconfirmed")}</p>}
      {write.data?.acknowledged && confirmed && <p className="small muted" role="status">{t("accessAddressesSaved")}</p>}
      <div className="actions">
        <Button disabled={!changed || write.isPending || unavailable}>{write.isPending ? t("saving") : t("saveAccessAddresses")}</Button>
      </div>
    </form>
  </section>;
}

export default function SettingsPage() {
  const { t } = useT();
  const session = useQuery({ ...sessionQuery, enabled: false });
  const q=useQuery(settingsQuery);
  return <>
    <div className="title"><div><h1>{t("settings")}</h1><p>{t("settingsIntro")}</p></div></div>
    {q.error && <Notice error={q.error} retry={() => q.refetch()}/>}
    <div className="two-up">
      {session.data && <AccountSettings username={session.data.username}/>}
      <section className="panel"><div className="section-head"><h2>{t("workspaceDefaults")}</h2></div>
        {q.data && <WorkspaceImageSettings image={q.data.workspace_image} observedAt={q.dataUpdatedAt} unavailable={q.isError}/>}
        <Facts items={[[t("cliAccount"),<code>codex login --device-auth</code>]]}/>
        <p className="small muted mt-3">{t("managedCodexSignIn")}</p>
      </section>
      {q.data && <AccessSettings settings={q.data} observedAt={q.dataUpdatedAt} unavailable={q.isError}/>}
    </div>
  </>;
}
