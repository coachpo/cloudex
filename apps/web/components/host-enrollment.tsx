"use client";

import { Translation, useT } from "@/components/i18n-provider";
import { hostName } from "@/lib/i18n";
import Link from "next/link";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { change, hostListQuery, hostPath, hostQuery, stamp, type Host, type HostEnrollment } from "@/lib/api";
import { Button } from "./ui/button";
import { Field, FieldGroup, FieldLabel } from "./ui/field";
import { Notice } from "./shared";

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
    <p><Translation id="enrollBefore" values={{ host: <strong>{hostName(t, enrollment.host)}</strong>, time: stamp(locale, enrollment.expires_at) }}/></p>
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
        <Button type="button" variant="outline" onClick={copyToken}>{copied ? t("tokenCopied") : t("copyToken")}</Button>}
      <Button type="button" variant="outline" onClick={() => downloadCertificate(enrollment.ca_certificate)}>{t("downloadCa")}</Button>
    </div>
    <ol className="flex list-decimal flex-col gap-3 pl-5 small wide-code">
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

  function close() {
    mutation.reset();
    onClose();
  }

  return <section className="panel" aria-labelledby="enrollment-heading">
    <div className="section-head"><h2 id="enrollment-heading">{mutation.data ? t("connectHostAgent") : host ? t("issueAToken") : t("addHost")}</h2></div>
    {mutation.data ? <EnrollmentInstructions enrollment={mutation.data} onDone={close}/> :
      <form className="form" onSubmit={e => {
        e.preventDefault();
        mutation.mutate(String(new FormData(e.currentTarget).get("name") ?? "").trim());
      }}>
        {host ? <p className="small muted">{t("renewEnrollmentHint", { host: hostName(t, host) })}</p> :
          <FieldGroup className="form">
            <Field className="fields">
              <FieldLabel htmlFor="host-name">{t("hostName")}</FieldLabel>
              <input className="field" id="host-name" name="name" required maxLength={64} placeholder={t("hostNameExample")} autoFocus/>
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
