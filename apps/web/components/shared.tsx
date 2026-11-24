"use client";

import { useT } from "@/components/i18n-provider";
import Link from "next/link";
import { Database, Plus } from "lucide-react";
import { Fragment, type ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { APIError, hostWorkspacePath, noResponseMessage } from "@/lib/api";
import { localizeProblem, statusName } from "@/lib/i18n";

export function Brand() {
  return <svg viewBox="0 0 28 28" className="brand-mark" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.35" strokeLinejoin="round">
    <path d="M14 2.75 24.5 8.6 14 14.5 3.5 8.6 14 2.75Z M3.5 13.9 14 19.75l10.5-5.85 M3.5 19.2 14 25.1l10.5-5.9"/>
    <path d="M14 8.55v.1"/>
  </svg>;
}

export function Status({state, label}: {state: string; label?: string}) {
  const { t } = useT();
  return <span className="status" data-state={state}>{label ?? statusName(t, state)}</span>;
}
export function Notice({error, retry}: {error: Error; retry?: () => void}) {
  const { locale, t } = useT();
  const detail = error instanceof APIError
    ? error.message === noResponseMessage ? t("requestNoResponse") : localizeProblem(locale, error.code, error.message)
    : error.message;
  return <div className="notice" role="alert"><b>{t("requestFailed")}</b> {detail}
    {retry && <Button type="button" variant="ghost" size="sm" onClick={retry}>{t("retry")}</Button>}</div>;
}
export function Empty({children}: {children: ReactNode}) {return <p className="empty">{children}</p>;}
export function NewWorkspace({ hostID }: { hostID?: string }) {
  const { t } = useT();
  return <Button asChild><Link href={hostID ? hostWorkspacePath("/workspaces/new", hostID) : "/workspaces/new"}>
    <Plus data-icon="inline-start"/>{t("newWorkspace")}</Link></Button>;
}
export function Facts({items}: {items: [string, ReactNode][]}) {
  return <dl className="fact-list">{items.map(([label,value]) => <Fragment key={label}><dt>{label}</dt><dd>{value ?? "—"}</dd></Fragment>)}</dl>;
}
export function VolumeLegend({state}: {state: string}) {
  return <div className="actions"><Database size={14} aria-hidden="true"/><span className="muted small">{state}</span></div>;
}
