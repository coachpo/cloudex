"use client";

import { Translation, useT } from "@/components/i18n-provider";
import { hostName } from "@/lib/i18n";
import Link from "next/link";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { Check, Clock, Copy, Download, KeyRound, RadioTower } from "lucide-react";
import { change, hostListQuery, hostPath, hostQuery, stamp, workspaceOrHostNameMaxLength, type Host, type HostEnrollment } from "@/lib/api";
import { Button } from "./ui/button";
import { Field, FieldGroup, FieldLabel } from "./ui/field";
import { Notice } from "./shared";
import { HostSSHForm } from "./host-ssh-form";

function downloadCertificate(pem: string) {
  const url = URL.createObjectURL(new Blob([pem], { type: "application/x-pem-file" }));
  const link = document.createElement("a");
  link.href = url;
  link.download = "agent-ca.pem";
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

function EnrollmentInstructions({ enrollment, onDone }: { enrollment: HostEnrollment; onDone: () => void }) {
  const { locale, t } = useT();
  const [copied, setCopied] = useState(false);
  async function copyToken() {
    try {
      await navigator.clipboard.writeText(enrollment.enrollment_token);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  }

  return <div className="form">
    <p className="notice info"><Clock aria-hidden="true"/><span className="notice-body"><Translation id="enrollBefore" values={{ host: <strong>{hostName(t, enrollment.host)}</strong>, time: stamp(locale, enrollment.expires_at) }}/></span></p>
    <FieldGroup className="form">
      <Field className="fields">
        <FieldLabel htmlFor="enrollment-token">{t("enrollmentToken")}</FieldLabel>
        <input id="enrollment-token" className="field font-mono" value={enrollment.enrollment_token} readOnly onFocus={e => e.currentTarget.select()}/>
      </Field>
      {enrollment.agent_address && <Field className="fields">
        <FieldLabel htmlFor="agent-address">{t("controlPlaneAddress")}</FieldLabel>
        <input id="agent-address" className="field font-mono" value={enrollment.agent_address} readOnly onFocus={e => e.currentTarget.select()}/>
      </Field>}
    </FieldGroup>
    <div className="actions">
      {typeof navigator !== "undefined" && navigator.clipboard &&
        <Button type="button" variant="outline" onClick={copyToken}>{copied ? <Check data-icon="inline-start"/> : <Copy data-icon="inline-start"/>}{copied ? t("tokenCopied") : t("copyToken")}</Button>}
      <Button type="button" variant="outline" onClick={() => downloadCertificate(enrollment.ca_certificate)}><Download data-icon="inline-start"/>{t("downloadCa")}</Button>
    </div>
    <ol className="steps wide-code">
      <li><Translation id="saveCaFile" values={{ certificate: <code>agent-ca.pem</code>, compose: <code>compose.agent.yaml</code> }}/>
        <pre className="log mt-2"><code>docker compose -f compose.agent.yaml build workspace-image</code></pre></li>
      <li><Translation id="setEnrollmentEnv" values={{ address: <code>CLOUDEX_AGENT_ADDRESS</code>, token: <code>CLOUDEX_AGENT_ENROLLMENT_TOKEN</code>,
        value: enrollment.agent_address ? <code>{enrollment.agent_address}</code> : t("reachableControlPlane") }}/></li>
      <li>{t("startHostAgent")}
        <pre className="log mt-2"><code>docker compose -f compose.agent.yaml up -d --build</code></pre></li>
    </ol>
    <div className="actions">
      <Button asChild><Link href={hostPath(enrollment.host.id)} onClick={onDone}>{t("viewHost")}</Link></Button>
      <Button type="button" variant="ghost" onClick={onDone}>{t("done")}</Button>
    </div>
  </div>;
}

export function HostEnrollmentPanel({ host, onClose }: { host?: Host; onClose: () => void }) {
  const { t } = useT();
  const client = useQueryClient();
  const [method, setMethod] = useState<"agent" | "ssh">(host?.connection_type ?? "ssh");
  const [sshPending, setSSHPending] = useState(false);
  const heading = useRef<HTMLHeadingElement>(null);
  const mutation = useMutation({
    gcTime: 0,
    mutationFn: (name: string) => host
      ? change<HostEnrollment>(hostPath(host.id) + "/enrollment", "POST")
      : change<HostEnrollment>("/hosts", "POST", { name }),
    onSuccess: ({ host }) => {
      void client.invalidateQueries({ queryKey: hostQuery(host.id).queryKey, exact: true });
      void client.invalidateQueries({ queryKey: hostListQuery.queryKey });
    },
  });
  const enrollment = mutation.data;
  useEffect(() => { heading.current?.focus(); }, [enrollment]);

  function close() {
    mutation.reset();
    onClose();
  }

  return <section className="panel enrollment" aria-labelledby="enrollment-heading">
    <div className="section-head"><h2 ref={heading} id="enrollment-heading" tabIndex={-1}>{enrollment ? t("connectHostAgent") : host ? t(method === "ssh" ? "updateHostSSH" : "issueAToken") : t("addHost")}</h2></div>
    {!host && !mutation.data && <fieldset className="mb-5 grid gap-2" disabled={sshPending || mutation.isPending}>
      <legend className="mb-2 font-medium">{t("connectionMethod")}</legend>
      <div className="choice-group">
        <label className="choice"><input type="radio" name="connection" checked={method === "ssh"} onChange={() => setMethod("ssh")}/><KeyRound size={16} aria-hidden="true"/> {t("sshDirect")}</label>
        <label className="choice"><input type="radio" name="connection" checked={method === "agent"} onChange={() => setMethod("agent")}/><RadioTower size={16} aria-hidden="true"/> Agent</label>
      </div>
    </fieldset>}
    {method === "ssh" ? <HostSSHForm host={host} pending={sshPending} setPending={setSSHPending} onClose={close}/> :
      mutation.data ? <EnrollmentInstructions enrollment={mutation.data} onDone={close}/> :
      <form className="form" onSubmit={e => {
        e.preventDefault();
        mutation.mutate(String(new FormData(e.currentTarget).get("name") ?? "").trim());
      }}>
        {host ? <p className="muted">{t("renewEnrollmentHint", { host: hostName(t, host) })}</p> :
          <FieldGroup className="form">
            <Field className="fields">
              <FieldLabel htmlFor="host-name">{t("hostName")}</FieldLabel>
              <input className="field" id="host-name" name="name" required maxLength={workspaceOrHostNameMaxLength} placeholder={t("hostNameExample")}/>
            </Field>
          </FieldGroup>}
        {mutation.error && <Notice error={mutation.error}/>}
        <div className="actions">
          <Button type="submit" disabled={mutation.isPending}>{mutation.isPending ? t("issuingToken") : t("issueEnrollmentToken")}</Button>
          <Button type="button" variant="ghost" disabled={mutation.isPending} onClick={close}>{t("cancel")}</Button>
        </div>
      </form>}
  </section>;
}
