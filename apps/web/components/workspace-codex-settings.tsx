"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { useT } from "@/components/i18n-provider";
import { Empty, Notice, Status } from "@/components/shared";
import { Button } from "@/components/ui/button";
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field";
import {
  APIError, change, codexConfigPath, codexConfigQuery, saveSetting,
  type CodexConfigFields, type WorkspaceCodexConfig,
} from "@/lib/api";

const managedFiles = [["agents_md", "AGENTS.md"], ["config_toml", "config.toml"], ["auth_json", "auth.json"]] as const;
type Draft = { revision: number; edits: Partial<CodexConfigFields> };
type ConfigWrite = CodexConfigFields & { revision: number };

function configFields({ managed, agents_md, config_toml, auth_json }: CodexConfigFields): CodexConfigFields {
  return { managed, agents_md, config_toml, auth_json };
}

function matchesWrite(config: WorkspaceCodexConfig, write: ConfigWrite) {
  return config.revision > write.revision && config.managed === write.managed &&
    managedFiles.every(([key]) => config[key] === write[key]);
}

export function WorkspaceCodexSettings({ id, busy }: { id: string; busy: boolean }) {
  const { t } = useT();
  const client = useQueryClient();
  const queryOptions = codexConfigQuery(id);
  const query = useQuery(queryOptions);
  const config = query.data;
  const [draft, setDraft] = useState<Draft>();

  const save = useMutation({
    mutationKey: queryOptions.queryKey,
    mutationFn: (payload: ConfigWrite) => saveSetting(client, queryOptions.queryKey,
      () => change<WorkspaceCodexConfig>(codexConfigPath(id), "PUT", payload)),
    onSuccess: (acknowledged) => { if (acknowledged) setDraft(undefined); },
    onError: (error) => {
      if (error instanceof APIError && error.code === "CODEX_CONFIG_CHANGED") return query.refetch();
    },
  });
  const confirmed = !query.isError && !!config && !!save.variables && matchesWrite(config, save.variables);
  const unconfirmed = save.data === false;
  const { reset } = save;
  useEffect(() => {
    if (unconfirmed && confirmed) {
      setDraft(undefined);
      reset();
    }
  }, [unconfirmed, confirmed, reset]);

  const fields = config && { ...configFields(config), ...draft?.edits };
  const changed = !!draft && (fields?.managed || config?.managed);
  const stale = draft && draft.revision !== config?.revision;
  const blocked = busy || query.isError || save.isPending;

  function edit<K extends keyof CodexConfigFields>(key: K, value: CodexConfigFields[K]) {
    if (!config) return;
    setDraft(current => {
      const edits = { ...current?.edits };
      if (value === config[key]) delete edits[key];
      else edits[key] = value;
      return Object.keys(edits).length ? { revision: current?.revision ?? config.revision, edits } : undefined;
    });
    reset();
  }

  function applyWrite() {
    if (!config || !fields || !draft || !changed || blocked || stale) return;
    const nextFields = fields.managed ? fields : { ...configFields(config), managed: false };
    save.mutate({ revision: draft.revision, ...nextFields });
  }

  return <section className="panel" aria-labelledby="workspace-codex-settings">
    <div className="section-head flex-wrap">
      <h2 id="workspace-codex-settings">{t("workspaceCodexSettings")}</h2>
      {config?.managed && !query.isError && config.applied_revision !== config.revision && <span role="status">
        <Status state="queued" label={t("codexConfigPending")}/>
      </span>}
    </div>
    {query.error && <Notice error={query.error} retry={() => query.refetch()}/>}
    {!config && !query.error && <Empty>{t("codexConfigLoading")}</Empty>}
    {config && fields && <form className="form" onSubmit={event => { event.preventDefault(); applyWrite(); }}>
      <FieldGroup className="form">
        <Field className="fields" data-disabled={blocked}>
          <FieldLabel htmlFor="manage-codex-files" className="label-inline">
            <input type="checkbox" id="manage-codex-files" checked={fields.managed} disabled={blocked}
              onChange={event => edit("managed", event.target.checked)}/>
            {t("manageCodexFiles")}
          </FieldLabel>
        </Field>
        {fields.managed && managedFiles.map(([key, filename]) => <Field key={key} className="fields" data-disabled={blocked}>
          <FieldLabel htmlFor={`codex-${key}`}>{filename}</FieldLabel>
          <textarea id={`codex-${key}`} className="field font-mono" rows={7} spellCheck={false}
            autoCapitalize="none" autoComplete="off" value={fields[key]} disabled={blocked}
            onChange={event => edit(key, event.target.value)}/>
        </Field>)}
      </FieldGroup>

      {save.error && !stale && <Notice error={save.error}/>}
      {unconfirmed && !confirmed && <p className="notice" role="alert">{t("settingSaveUnconfirmed")}</p>}
      {stale && <div className="actions" role="status">
        <p className="form-note">{t("codexConfigReviewDraftHint")}</p>
        <Button type="button" variant="outline" disabled={blocked}
          onClick={() => {
            const edits = Object.fromEntries(Object.entries(draft.edits)
              .filter(([key, value]) => value !== config[key as keyof CodexConfigFields]));
            setDraft(Object.keys(edits).length ? { revision: config.revision, edits } : undefined);
            reset();
          }}>{t("codexConfigReviewDraft")}</Button>
      </div>}
      <div className="actions">
        <Button disabled={!changed || blocked || !!stale}>{save.isPending ? t("saving") : t("saveCodexConfig")}</Button>
      </div>
    </form>}
  </section>;
}
