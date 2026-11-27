"use client";

import { useQueryClient } from "@tanstack/react-query";
import { useRef, useState, type FormEvent } from "react";
import { APIError, change, hostListQuery, hostPath, hostQuery, workspaceOrHostNameMaxLength, type Host } from "@/lib/api";
import { useT } from "./i18n-provider";
import { Notice } from "./shared";
import { Button } from "./ui/button";
import { Field, FieldLabel } from "./ui/field";

type HostKey = { host_key: string; fingerprint: string };

export function HostSSHForm({ host, pending, setPending, onClose }: {
  host?: Host; pending: boolean; setPending: (pending: boolean) => void; onClose: () => void;
}) {
  const { t } = useT();
  const client = useQueryClient();
  const [address, setAddress] = useState(host?.ssh_address ?? "");
  const [probe, setProbe] = useState<HostKey>();
  const [auth, setAuth] = useState<"password" | "private_key">("private_key");
  const [error, setError] = useState<Error>();
  const submitButton = useRef<HTMLButtonElement>(null);
  const keyChanged = probe && host?.ssh_host_key && host.ssh_host_key !== probe.host_key;

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (pending) return;
    const fields = new FormData(event.currentTarget);
    const field = (name: string) => String(fields.get(name) ?? "");
    setPending(true);
    setError(undefined);
    try {
      if (!probe) {
        setProbe(await change<HostKey>("/hosts/ssh/probe", "POST", { address: address.trim() }));
        return;
      }
      const identity = {
        address: address.trim(), username: field("username").trim(), host_key: probe.host_key,
        ...(auth === "password" ? { password: field("password") }
          : { private_key: field("private_key"), passphrase: field("passphrase") }),
      };
      const updated = host
        ? await change<Host>(hostPath(host.id) + "/ssh", "PUT", identity)
        : await change<Host>("/hosts/ssh", "POST", { ...identity, name: field("name").trim() });
      void client.invalidateQueries({ queryKey: hostQuery(updated.id).queryKey, exact: true });
      void client.invalidateQueries({ queryKey: hostListQuery.queryKey });
      onClose();
    } catch (err) {
      if (err instanceof APIError && err.code === "SSH_HOST_KEY_CHANGED") setProbe(undefined);
      setError(err instanceof Error ? err : new Error(String(err)));
    } finally {
      setPending(false);
    }
  }

  function resetProbe() {
    setProbe(undefined);
    setError(undefined);
  }

  return <form className="form" method="post" aria-labelledby="enrollment-heading" onSubmit={submit}>
    {!host && <Field className="fields">
      <FieldLabel htmlFor="ssh-host-name">{t("hostName")}</FieldLabel>
      <input className="field" id="ssh-host-name" name="name" required maxLength={workspaceOrHostNameMaxLength} readOnly={pending} placeholder={t("hostNameExample")}/>
    </Field>}
    <Field className="fields">
      <FieldLabel htmlFor="ssh-host-address">{t("hostSSHAddress")}</FieldLabel>
      <input className="field font-mono" id="ssh-host-address" name="address" value={address} required readOnly={!!host || pending}
        autoCapitalize="none" autoCorrect="off" spellCheck={false} placeholder="server.example.com:22"
        onChange={e => { setAddress(e.target.value); resetProbe(); }}/>
    </Field>
    {probe && <>
      <div className="fields subtle-panel">
        <p className="small muted">{t("hostKeyFingerprint")}</p>
        <code className="break-all text-[13px]" dir="ltr">{probe.host_key.split(" ", 1)[0]} {probe.fingerprint}</code>
      </div>
      {keyChanged && <p className="notice" role="alert">{t("hostKeyWasChanged")}</p>}
      <label className="label-inline font-medium"><input name="verified" type="checkbox" required autoFocus disabled={pending}/> {t("hostKeyVerified")}</label>
      <Field className="fields">
        <FieldLabel htmlFor="ssh-host-user">{t("username")}</FieldLabel>
        <input className="field" id="ssh-host-user" name="username" required defaultValue={host?.ssh_user} readOnly={!!host || pending}
          autoComplete="section-remote username" autoCapitalize="none" autoCorrect="off" spellCheck={false}/>
      </Field>
      <fieldset disabled={pending} className="grid gap-2">
        <legend className="mb-2 font-medium">{t("sshAuthentication")}</legend>
        <div className="choice-group">
          <label className="choice"><input type="radio" name="authentication" checked={auth === "private_key"} onChange={() => setAuth("private_key")}/> {t("sshPrivateKey")}</label>
          <label className="choice"><input type="radio" name="authentication" checked={auth === "password"} onChange={() => setAuth("password")}/> {t("password")}</label>
        </div>
      </fieldset>
      {auth === "password" ? <Field className="fields">
        <FieldLabel htmlFor="ssh-host-password">{t("password")}</FieldLabel>
        <input className="field" id="ssh-host-password" type="password" name="password" autoComplete="section-remote current-password" required readOnly={pending}/>
      </Field> : <>
        <Field className="fields">
          <FieldLabel htmlFor="ssh-host-key">{t("sshPrivateKey")}</FieldLabel>
          <textarea className="field font-mono" id="ssh-host-key" name="private_key" rows={4} autoComplete="off" autoCapitalize="none"
            autoCorrect="off" spellCheck={false} required readOnly={pending}/>
        </Field>
        <Field className="fields">
          <FieldLabel htmlFor="ssh-host-passphrase">{t("sshPassphrase")} <span className="optional">{t("optional")}</span></FieldLabel>
          <input className="field" id="ssh-host-passphrase" name="passphrase" type="password" autoComplete="section-key current-password" readOnly={pending}/>
        </Field>
      </>}
    </>}
    {error && <Notice error={error}/>}
    <div className="actions">
      <Button ref={submitButton} type="submit" disabled={pending}>{pending ? t("hostSSHConnecting") :
        probe ? t(host ? "saveHostSSH" : "addHost") : t("checkHostKey")}</Button>
      {probe && <Button type="button" variant="outline" disabled={pending} onClick={() => {
        resetProbe();
        submitButton.current?.focus();
      }}>{t("checkHostKeyAgain")}</Button>}
      <Button type="button" variant="ghost" disabled={pending} onClick={onClose}>{t("cancel")}</Button>
    </div>
  </form>;
}
