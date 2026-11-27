"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import Link from "next/link";
import { useState, type ReactNode } from "react";
import { useT } from "@/components/i18n-provider";
import { GrantActionDialog } from "@/components/grant-action-dialog";
import { Facts, Loading, Notice, Status } from "@/components/shared";
import { Button } from "@/components/ui/button";
import { Field, FieldLabel } from "@/components/ui/field";
import { change, changeGrantBinding, grantBindingPath, type GrantBinding } from "@/lib/api";
import {
  gatewayCredentialPath, grantBindingQuery, grantChannel, grantName,
  grantsQuery, grantStatusLabels, type ManagedGrant,
} from "@/lib/grants";
import { grantProblem, type MessageKey } from "@/lib/i18n";

const readinessLabels = {
  unbound: "grantBindingUnbound",
  pending: "grantBindingPending",
  ready: "grantBindingReady",
  unavailable: "grantUnavailable",
} satisfies Record<GrantBinding["readiness"], MessageKey>;

function bindingName(grant: ManagedGrant) {
  const identity = grant.upstream_workspace_id || (grant.channel === "chatgpt" ? grant.client_id : "");
  return [grantName(grant), grantChannel(grant.channel), identity].filter(Boolean).join(" · ");
}

export function WorkspaceGrantSettings({ id, busy }: { id: string; busy: boolean }) {
  const { t } = useT();
  const client = useQueryClient();
  const grantQuery = useQuery(grantsQuery);
  const grantList = grantQuery.data?.grants ?? [];
  const queryOptions = grantBindingQuery(id);
  const query = useQuery(queryOptions);
  const binding = query.data;
  const current = binding?.grant_id ?? "";
  const currentGrant = grantList.find(grant => grant.id === current);
  const [draft, setDraft] = useState<{ grantID: string; revision: number }>();
  const staleDraft = !!draft && draft.revision !== binding?.binding_revision;
  const selected = staleDraft ? current : draft?.grantID ?? current;
  const selectedGrant = grantList.find(grant => grant.id === selected);

  const refreshViews = () => Promise.all([
    client.invalidateQueries({ queryKey: queryOptions.queryKey, exact: true }),
    client.invalidateQueries({ queryKey: grantsQuery.queryKey }),
  ]);
  const save = useMutation({
    mutationKey: queryOptions.queryKey,
    mutationFn: ({ grantID, revision }: NonNullable<typeof draft>) => changeGrantBinding(id, grantID, revision),
    onSuccess: () => setDraft(undefined),
    onSettled: refreshViews,
  });
  const rotate = useMutation({
    mutationKey: queryOptions.queryKey,
    mutationFn: () => change<GrantBinding>(gatewayCredentialPath(id), "POST"),
    onSettled: refreshViews,
  });

  const pending = busy || query.isError || save.isPending || rotate.isPending;
  const selectable = !selected || (!grantQuery.isError && selectedGrant?.status === "active");
  const changeEffect = current ? (selected ? "grantBindingReplaceEffect" : "grantBindingClearEffect") : "grantBindingStartEffect";
  const canSave = !pending && !!draft && !staleDraft && selected !== current && selectable;
  const details: [string, ReactNode][] = [];
  if (current) details.push([t("grantBindingCurrent"), currentGrant ? bindingName(currentGrant) : current]);
  if (binding) details.push([t("grantGatewayStatus"), <span role="status"><Status state={binding.readiness === "ready" ? "succeeded" : binding.readiness}
    label={t(readinessLabels[binding.readiness])}/></span>]);
  if (currentGrant && currentGrant.status !== "active") details.push([t("grantBindingCurrentStatus"), t(grantStatusLabels[currentGrant.status])]);

  return <section className="panel view-grid" id="workspace-grant" aria-labelledby="workspace-grant-heading">
    <div className="section-head mb-0">
      <h2 id="workspace-grant-heading">{t("grantBindingHeading")}</h2>
      <Link href="/settings#grants" className="link">{t("grantManage")}</Link>
    </div>
    {query.error && <Notice error={query.error} retry={() => query.refetch()}/>}
    {grantQuery.error && <Notice error={grantQuery.error} retry={() => grantQuery.refetch()}/>}
    {save.error && <Notice error={save.error}/>}
    {rotate.error && <Notice error={rotate.error}/>}
    {save.isSuccess && !draft && <p className="confirm" role="status">{t(save.variables.grantID ? "grantBindingSaved" : "grantBindingCleared")}</p>}
    {rotate.isSuccess && <p className="confirm" role="status">{t("grantRotateDone")}</p>}
    {query.isPending && <Loading label={t("loading")}/>}
    {binding && <>
      <Facts items={details}/>
      {binding.reason && <p className="form-note">{grantProblem(t, binding.reason)}</p>}
      {staleDraft && <p className="form-note" role="status">{t("grantBindingChanged")}</p>}
      {(!!current || grantList.length > 0) && <form className="form" method="post" action={"/api/v1" + grantBindingPath(id)}
        aria-labelledby="workspace-grant-heading" onSubmit={event => {
          event.preventDefault();
          if (!canSave) return;
          rotate.reset();
          save.mutate(draft);
        }}>
        <Field className="fields">
          <FieldLabel htmlFor="workspace-authorization">{t("grantBindingChoice")}</FieldLabel>
          <select className="field" id="workspace-authorization" name="grant_id" value={selected} disabled={pending}
            aria-describedby={selected !== current ? "workspace-grant-effect" : undefined}
            onChange={event => {
              setDraft({ grantID: event.target.value, revision: binding.binding_revision });
              save.reset();
              rotate.reset();
            }}>
            <option value="">{t("grantBindingUnbound")}</option>
            {current && !currentGrant && <option value={current} disabled>{current}</option>}
            {grantList.map(grant => <option key={grant.id} value={grant.id} disabled={grant.status !== "active"}>
              {bindingName(grant)}{grant.status === "active" ? "" : " · " + t(grantStatusLabels[grant.status])}
            </option>)}
          </select>
          {selected !== current && <p className="form-note" id="workspace-grant-effect">
            {t(changeEffect)}
          </p>}
        </Field>
        <div className="actions"><Button type="submit" disabled={!canSave}>{save.isPending ? t("saving") : t("grantBindingApply")}</Button></div>
      </form>}
      {current && binding.readiness === "ready" && <div className="actions">
        <GrantActionDialog label={t("grantRotate")} title={t("grantRotateTitle")} disabled={pending}
          action={() => { save.reset(); rotate.mutate(); }}><p>{t("grantRotateEffect")}</p></GrantActionDialog>
      </div>}
    </>}
  </section>;
}
