"use client";

import { useT } from "@/components/i18n-provider";
import Link from "next/link";
import { CircleAlert, Plus, type LucideIcon } from "lucide-react";
import { Fragment, type ReactNode, type Ref } from "react";
import { Button } from "@/components/ui/button";
import { APIError, hostWorkspacePath, noResponseMessage } from "@/lib/api";
import { localizeProblem, statusName } from "@/lib/i18n";

export function Brand() {
  return <svg viewBox="0 0 28 28" className="brand-mark" aria-hidden="true" fill="none" stroke="currentColor"
    strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">
    <rect x="2.85" y="2.85" width="22.3" height="22.3" rx="6.5"/>
    <path d="m8.6 10 4 4-4 4"/>
    <rect className="brand-fill" x="14.6" y="16.4" width="5.6" height="2.7" rx="1" stroke="none"/>
  </svg>;
}

export function PageHeader({ crumbs, current, title, status, description, meta, actions, titleRef, children }: {
  crumbs?: { href: string; label: string }[]; current?: string; title: ReactNode; status?: ReactNode;
  description?: ReactNode; meta?: ReactNode; actions?: ReactNode; titleRef?: Ref<HTMLHeadingElement>; children?: ReactNode;
}) {
  const { t } = useT();
  return <header className="page-head">
    {crumbs && <nav className="crumbs" aria-label={t("breadcrumb")}><ol>
      {crumbs.map(crumb => <li key={crumb.href}><Link href={crumb.href}>{crumb.label}</Link></li>)}
      {current && <li aria-current="page">{current}</li>}
    </ol></nav>}
    <div className="title-row">
      <div>
        <h1 ref={titleRef} tabIndex={titleRef ? -1 : undefined}>{title}{status}</h1>
        {description && <p className="lede">{description}</p>}
        {meta && <div className="meta">{meta}</div>}
      </div>
      {actions && <div className="actions">{actions}</div>}
    </div>
    {children}
  </header>;
}

export function MetaItem({ icon: Icon, children }: { icon: LucideIcon; children: ReactNode }) {
  return <span className="meta-item"><Icon aria-hidden="true" strokeWidth={1.75}/>{children}</span>;
}

export function Status({state, label}: {state: string; label?: string}) {
  const { t } = useT();
  return <span className="status" data-state={state}>{label ?? statusName(t, state)}</span>;
}

export function Notice({error, retry, announce = true}: {error: Error; retry?: () => void; announce?: boolean}) {
  const { locale, t } = useT();
  const detail = error instanceof APIError
    ? error.message === noResponseMessage ? t("requestNoResponse") : localizeProblem(locale, error.code, error.message)
    : error.message;
  return <div className="notice" role={announce ? "alert" : undefined}>
    <CircleAlert aria-hidden="true" strokeWidth={2}/>
    <p className="notice-body"><b>{t("requestFailed")}</b>{" "}{detail}</p>
    {retry && <Button type="button" variant="outline" size="xs" onClick={retry}>{t("retry")}</Button>}
  </div>;
}

export function Empty({ children, icon: Icon, title, large = false }: {
  children?: ReactNode; icon?: LucideIcon; title?: ReactNode; large?: boolean;
}) {
  return <div className={large ? "empty large" : "empty"}>
    {Icon && <Icon aria-hidden="true" strokeWidth={1.75}/>}
    {title && <strong>{title}</strong>}
    {children && <p>{children}</p>}
  </div>;
}

export function Loading({ label, rows = 3 }: { label: string; rows?: number }) {
  return <div className="loading" role="status" aria-live="polite">
    <span className="loading-label"><span className="spinner" aria-hidden="true"/>{label}</span>
    {rows > 0 && <div className="skeleton-rows" aria-hidden="true">
      {Array.from({ length: rows }, (_, index) => <span key={index} className="skeleton"/>)}
    </div>}
  </div>;
}

export function NotFoundContent() {
  const { t } = useT();
  return <section className="empty large" aria-labelledby="not-found-title">
    <code className="muted">404</code>
    <h1 id="not-found-title">{t("pageNotFound")}</h1>
    <Button asChild variant="outline"><Link href="/workspaces">{t("returnToWorkspaces")}</Link></Button>
  </section>;
}

export function NewWorkspace({ hostID }: { hostID?: string }) {
  const { t } = useT();
  return <Button asChild><Link href={hostID ? hostWorkspacePath("/workspaces/new", hostID) : "/workspaces/new"}>
    <Plus data-icon="inline-start"/>{t("newWorkspace")}</Link></Button>;
}

export function Facts({items}: {items: [string, ReactNode][]}) {
  return <dl className="fact-list">{items.map(([label,value]) => <Fragment key={label}><dt>{label}</dt><dd>{value ?? "—"}</dd></Fragment>)}</dl>;
}

export function Meter({ value, total, label, warnWhenFull = false, level }: {
  value: number; total: number; label: string; warnWhenFull?: boolean; level?: "low" | "critical";
}) {
  if (total <= 0) return null;
  const current = Math.min(total, Math.max(0, value));
  const share = current / total;
  return <div className="meter" role="meter" aria-label={label} aria-valuemin={0} aria-valuemax={total} aria-valuenow={current}
    data-level={level ?? (warnWhenFull && share >= .85 ? "high" : undefined)}>
    <span style={{ width: `${share * 100}%` }}/>
  </div>;
}
