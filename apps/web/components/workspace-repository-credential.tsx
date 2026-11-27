"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useRef } from "react";
import { useT } from "@/components/i18n-provider";
import { Loading, Notice } from "@/components/shared";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription,
  AlertDialogFooter, AlertDialogHeader, AlertDialogTitle, AlertDialogTrigger } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field";
import { change, repositoryCredentialPath, repositoryCredentialQuery, saveSetting } from "@/lib/api";

export function WorkspaceRepositoryCredential({ id, busy }: { id: string; busy: boolean }) {
  const { t } = useT();
  const client = useQueryClient();
  const token = useRef<HTMLInputElement>(null);
  const options = repositoryCredentialQuery(id);
  const query = useQuery(options);
  const status = query.data;
  const write = useMutation({
    mutationKey: options.queryKey,
    mutationFn: (credential: { username: string; token: string } | null) =>
      saveSetting(client, options.queryKey, () => credential
        ? change(repositoryCredentialPath(id), "PUT", credential)
        : change<void>(repositoryCredentialPath(id), "DELETE")),
    gcTime: 0,
    onSuccess: acknowledged => { if (acknowledged && token.current) token.current.value = ""; },
  });
  const disabled = busy || query.isError || write.isPending;

  return <section className="panel" aria-labelledby="workspace-repository-credential">
    <div className="section-head"><h2 id="workspace-repository-credential">{t("repositoryCredential")}</h2></div>
    {query.error && <Notice error={query.error} retry={() => query.refetch()}/>}
    {!status && !query.error && <Loading label={t("repositoryCredentialLoading")} rows={2}/>}
    {status && <form className="form" method="post" onSubmit={event => {
      event.preventDefault();
      if (disabled) return;
      const fields = new FormData(event.currentTarget);
      write.mutate({ username: String(fields.get("username")), token: String(fields.get("token")) });
    }}>
      <p className="muted">
        {status.configured ? t("repositoryCredentialConfigured", { username: status.username }) : t("repositoryCredentialMissing")}
      </p>
      <FieldGroup className="form-split">
        <Field className="fields" data-disabled={disabled}>
          <FieldLabel htmlFor="repo-credential-username">{t("repositoryUsername")}</FieldLabel>
          <input className="field" id="repo-credential-username" name="username" key={status.username} defaultValue={status.username}
            autoCapitalize="none" autoComplete="off" spellCheck={false} required maxLength={256} disabled={disabled}
            onChange={() => write.reset()}/>
        </Field>
        <Field className="fields" data-disabled={disabled}>
          <FieldLabel htmlFor="repo-credential-token">{t("repositoryToken")}</FieldLabel>
          <input className="field" id="repo-credential-token" name="token" type="password" ref={token}
            autoComplete="new-password" spellCheck={false} required maxLength={4096} disabled={disabled}
            onChange={() => write.reset()}/>
        </Field>
      </FieldGroup>
      <p className="form-note">{t("repositoryCredentialRebuild")}</p>
      {write.error && <Notice error={write.error}/>}
      {write.data === false && <p className="notice" role="alert">{t("settingSaveUnconfirmed")}</p>}
      {write.data && <p className="confirm" role="status">
        {t(write.variables === null ? "repositoryCredentialRemoved" : "repositoryCredentialSaved")}
      </p>}
      <div className="actions">
        <Button disabled={disabled}>{write.isPending && write.variables !== null ? t("saving") : t("saveRepositoryCredential")}</Button>
        {status.configured && <AlertDialog>
          <AlertDialogTrigger asChild>
            <Button type="button" variant="outline" disabled={disabled}>{t("removeRepositoryCredential")}</Button>
          </AlertDialogTrigger>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{t("removeRepositoryCredential")}</AlertDialogTitle>
              <AlertDialogDescription>{t("removeRepositoryCredentialHint")}</AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel type="button">{t("cancel")}</AlertDialogCancel>
              <AlertDialogAction type="button" variant="danger" disabled={disabled} onClick={() => write.mutate(null)}>
                {t("removeRepositoryCredential")}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>}
      </div>
    </form>}
  </section>;
}
