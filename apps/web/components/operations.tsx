"use client";

import { useT } from "@/components/i18n-provider";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { Empty, Notice, Status } from "./shared";
import { statusName, type MessageKey, type Translate } from "@/lib/i18n";
import { active, change, stamp, type Operation } from "@/lib/api";

const verbs: Record<string, MessageKey | undefined> = { create: "create", start: "start", stop: "stop", rebuild: "rebuild", reset: "resetData", delete: "delete" };

export function operationName(t: Translate, type: string) {
  const key = verbs[type];
  return key ? t(key) : statusName(t, type);
}

export function Operations({ rows, names, canCancel = false }: {rows: Operation[]; names?: Map<string,string>; canCancel?: boolean}) {
  const { locale, t } = useT();
  const client = useQueryClient();
  const router = useRouter();
  const cancel = useMutation({
    mutationFn: (o: Operation) => change("/operations/" + o.id + "/cancel", "POST"),
    onSuccess: (_, o) => {
      client.invalidateQueries({queryKey:["operations"]});
      // Cancelling an undispatched create also cancels the workspace.
      if (o.type === "create") {
        client.invalidateQueries({queryKey:["workspaces"]});
        router.replace("/workspaces");
      }
    },
  });
  if (!rows.length) return <Empty>{t("noOperations")}</Empty>;
  return <>
    <ol className="operations">{rows.map(o => <li key={o.id}>
      <div className="op-row"><strong>{o.apply_ssh ? t("applySshSettings") : operationName(t, o.type)}
        {names && <span className="muted"> · {names.has(o.workspace_id) ?
          <Link className="link" href={"/workspaces/" + o.workspace_id}>{names.get(o.workspace_id)}</Link> :
          <code>{o.workspace_id.slice(0,8)}</code>}</span>}</strong>
        <Status state={o.status}/></div>
      <div className="op-row muted small"><span>{o.target_generation > 0 ? t("generationNumber", { number: o.target_generation.toLocaleString(locale) }) + " · " : ""}{stamp(locale, o.created_at)}</span>
        {canCancel && active(o.status) && o.status === "queued" && o.attempts === 0 && !o.apply_ssh &&
          <Button variant="ghost" size="xs" disabled={cancel.isPending} onClick={() => cancel.mutate(o)}>{t("cancel")}</Button>}
      </div>
      {o.error && <p className="op-error small">{o.error}</p>}
    </li>)}</ol>
    {cancel.error && <Notice error={cancel.error}/>}
  </>;
}
