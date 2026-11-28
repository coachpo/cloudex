"use client";

import { useT } from "@/components/i18n-provider";
import Link from "next/link";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent, type RefObject } from "react";
import { accountMutationKey, APIError, api, change, hostImageReady, hostListQuery, hostPath, readSettings, replaceSession, saveSetting, sessionQuery,
  settingsQuery, templatesQuery, workspaceImageDraft, publicKeyLines, type Session, type Settings } from "@/lib/api";
import { hostName } from "@/lib/i18n";
import { Loading, Notice, PageHeader, Status } from "@/components/shared";
import { GrantSettings } from "@/components/grant-settings";
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
      e.currentTarget.querySelector<HTMLInputElement>("#confirm-new-password")?.focus();
      return;
    }
    setMismatch(false);
    write.mutate({
      username: String(data.get("username")),
      current_password: String(data.get("current_password")),
      new_password: password || undefined,
    });
  }

  return <section className="panel" id="account" aria-labelledby="account-heading">
    <div className="section-head"><h2 id="account-heading">{t("account")}</h2></div>
    <form className="form" method="post" key={username} ref={form} onSubmit={submit} onChange={event => {
      if (!mismatch) return;
      const data = new FormData(event.currentTarget);
      if (data.get("new_password") === data.get("confirmation")) setMismatch(false);
    }}>
      <FieldGroup className="form-split">
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
          {mismatch && <p className="form-note" id="account-password-mismatch" role="alert">{t("passwordMismatch")}</p>}
        </Field>
      </FieldGroup>
      {write.error && <Notice error={write.error}/>}
      {write.isSuccess && <p className="confirm" role="status">{t("accountSaved")}</p>}
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
  return <div className="view-grid">
    <form className="form" onSubmit={e => {
      e.preventDefault();
      if (canSave) write.mutate(submitted);
    }}>
      <FieldGroup>
        <Field className="fields" data-disabled={write.isPending} data-invalid={invalid}>
          <FieldLabel htmlFor="settings-workspace-image">{t("defaultWorkspaceImage")}</FieldLabel>
          <input id="settings-workspace-image" className="field font-mono" value={value}
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
      {write.data && confirmed && <p className="confirm" role="status">{t("workspaceImageSaved")}</p>}
      <div className="actions">
        <Button disabled={!canSave}>{write.isPending ? t("saving") : t("saveWorkspaceImage")}</Button>
      </div>
    </form>
    {hosts.error && <Notice error={hosts.error} retry={() => hosts.refetch()}/>}
    {hosts.isPending && <Loading label={t("loadingHosts")} rows={2}/>}
    {!!hosts.data?.length && <div className="table-wrap border-t border-border"><table className="data-table" aria-label={t("workspaceImageReadiness")}>
      {submitted !== image && <caption className="small muted text-left break-all">{t("workspaceImageReadinessFor", { image })}</caption>}
      <thead><tr><th scope="col">{t("host")}</th><th scope="col">{t("workspaceImageReadiness")}</th></tr></thead>
      <tbody>{hosts.data.map(host => <tr key={host.id}>
        <td><Link className="entry-link" href={hostPath(host.id)}>{hostName(t, host)}</Link></td>
        <td><Status state={hostImageReady(host, image) ? "ready" : ""} label={t(hostImageReady(host, image) ? "state.ready" : "workspaceImageNotReady")}/></td>
      </tr>)}</tbody>
    </table></div>}
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
        ? await readSettings() : settings;
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
  return <section className="panel" id="access-addresses" aria-labelledby="access-addresses-heading">
    <div className="section-head"><h2 id="access-addresses-heading">{t("accessAddresses")}</h2></div>
    <form className="form" onSubmit={e => {
      e.preventDefault();
      if (draft && changed && !write.isPending && !unavailable) write.mutate(draft);
    }}>
      <FieldGroup className="form-split">
        <Field className="fields" data-disabled={write.isPending}>
          <FieldLabel htmlFor="settings-ssh-host">{t("settingsSshHost")}</FieldLabel>
          <input id="settings-ssh-host" className="field font-mono" placeholder={settings.ssh_bind} value={addresses.ssh_host}
            disabled={write.isPending} autoCapitalize="none" spellCheck={false} onChange={e => edit("ssh_host", e.target.value)}/>
        </Field>
        <Field className="fields" data-disabled={write.isPending}>
          <FieldLabel htmlFor="settings-agent-address">{t("settingsAgentAddress")}</FieldLabel>
          <input id="settings-agent-address" className="field font-mono" value={addresses.agent_public_address}
            disabled={write.isPending} autoCapitalize="none" spellCheck={false} onChange={e => edit("agent_public_address", e.target.value)}/>
        </Field>
      </FieldGroup>
      {write.error && <Notice error={write.error}/>}
      {unconfirmed && !confirmed && <p className="notice" role="alert">{t("settingSaveUnconfirmed")}</p>}
      {write.data?.acknowledged && confirmed && <p className="confirm" role="status">{t("accessAddressesSaved")}</p>}
      <div className="actions">
        <Button disabled={!changed || write.isPending || unavailable}>{write.isPending ? t("saving") : t("saveAccessAddresses")}</Button>
      </div>
    </form>
  </section>;
}

const sshKeysQuery = {
  queryKey: ["ssh-keys"],
  queryFn: ({ signal }: { signal: AbortSignal }) => api<{ public_keys: string[] }>("/settings/ssh-keys", { signal }),
};

function SSHKeySettings() {
  const { locale, t } = useT();
  const client = useQueryClient();
  const q = useQuery(sshKeysQuery);
  const [draft, setDraft] = useState<string>();
  const write = useMutation({
    mutationFn: (keys: string[]) => change("/settings/ssh-keys", "PUT", { public_keys: keys }),
    onSuccess: () => setDraft(undefined),
    onSettled: () => Promise.all([
      client.invalidateQueries({ queryKey: sshKeysQuery.queryKey }),
      client.invalidateQueries({ queryKey: ["access"] }),
    ]),
  });
  const saved = q.data?.public_keys.join("\n") ?? "";
  const value = draft ?? saved;
  const keys = publicKeyLines(value);
  const canSave = q.isSuccess && !write.isPending && (keys.join("\n") !== saved || write.isError);
  return <section id="ssh-keys" className="panel" aria-labelledby="ssh-keys-heading">
    <div className="section-head"><h2 id="ssh-keys-heading">{t("sshPublicKeys")}</h2>
      {q.data && <span className="count">{q.data.public_keys.length.toLocaleString(locale)}</span>}</div>
    {q.error && <Notice error={q.error} retry={() => q.refetch()}/>}
    {q.isPending && <Loading label={t("loading")} rows={2}/>}
    {q.data && <form className="form" onSubmit={e => { e.preventDefault(); if (canSave) write.mutate(keys); }}>
      <FieldGroup>
        <Field data-disabled={write.isPending}>
          <FieldLabel htmlFor="ssh-keys-input">{t("authorizedKeys")}</FieldLabel>
          <textarea id="ssh-keys-input" className="field font-mono" rows={5} spellCheck={false} value={value} maxLength={61440}
            disabled={write.isPending} onChange={e => {
              setDraft(e.target.value);
              if (write.isSuccess) write.reset();
            }} aria-describedby="ssh-keys-hint"/>
          <p id="ssh-keys-hint" className="form-note">{t("sshPublicKeysHint")}</p>
        </Field>
      </FieldGroup>
      {write.error && <Notice error={write.error}/>}
      {write.isSuccess && <p className="confirm" role="status">{t("sshKeysSaved")}</p>}
      <div className="actions"><Button disabled={!canSave}>{write.isPending ? t("saving") : t("saveSSHKeys")}</Button></div>
    </form>}
  </section>;
}

type CodexFiles = Pick<Settings, "codex_agents_md" | "codex_config_toml">;

function CodexSettings({ settings, observedAt, unavailable }: SettingsObservation & { settings: Settings }) {
  const { t } = useT();
  const client = useQueryClient();
  const [draft, setDraft] = useState<Partial<CodexFiles>>({});
  const fields = [
    { key: "codex_agents_md", name: "agents_md", label: "AGENTS.md" },
    { key: "codex_config_toml", name: "config_toml", label: "config.toml" },
  ] as const;
  const files = { ...settings, ...draft };
  const write = useMutation({
    mutationFn: async (edits: Partial<CodexFiles>) => {
      // Keep a file edited in another tab when this form has no draft for it.
      const current = edits.codex_agents_md === undefined || edits.codex_config_toml === undefined
        ? await readSettings() : settings;
      const submitted = {
        agents_md: edits.codex_agents_md ?? current.codex_agents_md,
        config_toml: edits.codex_config_toml ?? current.codex_config_toml,
      };
      const acknowledged = await saveSetting(client, settingsKey, () => change("/settings/codex", "PUT", submitted));
      return { acknowledged, submitted };
    },
    onSuccess: ({ acknowledged }) => { if (acknowledged) setDraft({}); },
  });
  const { reset } = write;
  const submitted = write.data?.submitted;
  const confirmed = !unavailable && observedAt >= write.submittedAt &&
    submitted?.agents_md === settings.codex_agents_md && submitted.config_toml === settings.codex_config_toml;
  const unconfirmed = write.data?.acknowledged === false;
  useEffect(() => {
    setDraft(current => {
      const agentsSaved = current.codex_agents_md === settings.codex_agents_md;
      const configSaved = current.codex_config_toml === settings.codex_config_toml;
      if (!agentsSaved && !configSaved) return current;
      const next = { ...current };
      if (agentsSaved) delete next.codex_agents_md;
      if (configSaved) delete next.codex_config_toml;
      return next;
    });
  }, [settings.codex_agents_md, settings.codex_config_toml]);
  useEffect(() => {
    if (unconfirmed && confirmed) {
      setDraft({});
      reset();
    }
  }, [unconfirmed, confirmed, reset]);
  const canSave = !unavailable && !write.isPending && fields.some(({ key }) => files[key] !== settings[key]);

  return <section className="panel" id="codex" aria-labelledby="codex-heading">
    <div className="section-head"><div>
      <h2 id="codex-heading">{t("codexSettings")}</h2>
      <p id="codex-settings-hint">{t("codexSettingsHint")}</p>
    </div></div>
    <form className="form" method="post" aria-labelledby="codex-heading" aria-describedby="codex-settings-hint"
      onSubmit={e => { e.preventDefault(); if (canSave) write.mutate(draft); }}>
      <FieldGroup className="halves">
        {fields.map(({ key, name, label }) => <Field key={key} data-disabled={write.isPending}>
          <FieldLabel htmlFor={key}>{label}</FieldLabel>
          <textarea id={key} name={name} className="field font-mono" rows={8} autoCapitalize="none" autoCorrect="off"
            spellCheck={false} disabled={write.isPending} value={files[key]} onChange={e => {
              const value = e.target.value;
              setDraft(previous => {
                const next = { ...previous };
                if (value === settings[key]) delete next[key];
                else next[key] = value;
                return next;
              });
              reset();
            }}/>
        </Field>)}
      </FieldGroup>
      {write.error && <Notice error={write.error}/>}
      {unconfirmed && !confirmed && <p className="notice" role="alert">{t("settingSaveUnconfirmed")}</p>}
      {write.data?.acknowledged && confirmed && <p className="confirm" role="status">{t("codexSettingsSaved")}</p>}
      <div className="actions">
        <Button type="submit" disabled={!canSave}>{write.isPending ? t("saving") : t("saveCodexSettings")}</Button>
      </div>
    </form>
  </section>;
}

function SettingsIndex({ container }: { container: RefObject<HTMLDivElement | null> }) {
  const { t } = useT();
  const [sections, setSections] = useState<{ id: string; label: string }[]>([]);
  useEffect(() => {
    const root = container.current;
    if (!root) return;
    const read = () => setSections(current => {
      const next = Array.from(root.querySelectorAll<HTMLElement>(":scope > section[id]"),
        section => ({ id: section.id, label: section.querySelector("h2")?.textContent ?? section.id }));
      return JSON.stringify(next) === JSON.stringify(current) ? current : next;
    });
    read();
    const observer = new MutationObserver(read);
    observer.observe(root, { childList: true, subtree: true, characterData: true });
    return () => observer.disconnect();
  }, [container]);
  return <nav className="settings-index" aria-label={t("settingsSections")}>
    {sections.map(section => <a key={section.id} href={"#" + section.id}>{section.label}</a>)}
  </nav>;
}

export default function SettingsPage() {
  const { t } = useT();
  const session = useQuery({ ...sessionQuery, enabled: false });
  const q=useQuery(settingsQuery);
  const sections = useRef<HTMLDivElement>(null);
  return <>
    <PageHeader title={t("settings")} description={t("settingsIntro")}/>
    {q.error && <Notice error={q.error} retry={() => q.refetch()}/>}
    <div className="settings-layout">
      <SettingsIndex container={sections}/>
      <div className="settings-sections" ref={sections}>
        {session.data && <AccountSettings username={session.data.username}/>}
        {(q.data || q.isPending) && <section className="panel" id="workspace-defaults" aria-labelledby="workspace-defaults-heading">
          <div className="section-head"><h2 id="workspace-defaults-heading">{t("workspaceDefaults")}</h2></div>
          {q.data ? <WorkspaceImageSettings image={q.data.workspace_image} observedAt={q.dataUpdatedAt} unavailable={q.isError}/> :
            <Loading label={t("loading")} rows={2}/>}
        </section>}
        <SSHKeySettings/>
        <GrantSettings/>
        {q.data && <CodexSettings settings={q.data} observedAt={q.dataUpdatedAt} unavailable={q.isError}/>}
        {q.data && <AccessSettings settings={q.data} observedAt={q.dataUpdatedAt} unavailable={q.isError}/>}
      </div>
    </div>
  </>;
}
