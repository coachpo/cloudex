"use client";

import { useT } from "@/components/i18n-provider";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useIsMutating, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Boxes, CircuitBoard, LogOut, Settings2 } from "lucide-react";
import { useEffect, type ReactNode } from "react";
import { accountMutationKey, APIError, change, sessionQuery } from "@/lib/api";
import { Brand, Empty, Notice } from "./shared";
import { ThemeSwitcher } from "./theme-switcher";

const links = [
  { to: "/workspaces", label: "workspaces", icon: Boxes },
  { to: "/hosts", label: "hosts", icon: CircuitBoard },
  { to: "/settings", label: "settings", icon: Settings2 },
] as const;

export function Shell({children}: {children: ReactNode}) {
  const { t } = useT();
  const path = usePathname();
  const router = useRouter();
  const client = useQueryClient();
  // An account update replaces the cookie and revokes the previous session together.
  const checkingSession = useIsMutating({ mutationKey: accountMutationKey }) === 0;
  const session = useQuery({ ...sessionQuery, enabled: checkingSession, refetchInterval: 20000 });
  const denied = checkingSession && session.error instanceof APIError && session.error.status === 401;
  useEffect(() => { if (denied) router.replace("/login"); }, [denied, router]);
  const logout = useMutation({
    mutationFn: () => change("/auth/logout", "POST"),
    onSuccess: () => { client.clear(); router.replace("/login"); },
  });
  return <div className="shell">
    <nav className="rail" aria-label={t("mainNavigation")}>
      <Link className="rail-link rail-home" href="/workspaces" title="Cloudex" aria-label="Cloudex"><Brand/></Link>
      <div className="rail-items">{links.map(({to,label,icon: Icon}) =>
        <Link href={to} key={to} className="rail-link" title={t(label)} aria-label={t(label)} aria-current={path.startsWith(to) ? "page" : undefined}>
          <Icon size={18} strokeWidth={1.55} aria-hidden="true"/><span>{t(label)}</span>
        </Link>)}</div>
      <div className="rail-bottom"><button className="rail-link" title={t("signOut")} aria-label={t("signOut")}
        disabled={!session.data || logout.isPending} onClick={() => logout.mutate()}><LogOut size={17}/></button></div>
    </nav>
    <header className="topbar"><Link className="product" href="/workspaces">Cloudex</Link>
      <span className="topbar-account">{session.data?.username}</span>
      <ThemeSwitcher/>
      {session.data && <button className="mobile-signout" onClick={() => logout.mutate()} disabled={logout.isPending} aria-label={t("signOut")}><LogOut size={17}/></button>}
    </header>
    <main className="page" id="main">
      {logout.error && <Notice error={logout.error} retry={() => logout.mutate()}/>}
      {session.data && !denied ? children : <><h1>{t("console")}</h1>
        {session.error && !denied ? <Notice error={session.error} retry={() => session.refetch()}/> :
          <Empty>{denied ? t("openingLogin") : t("checkingSession")}</Empty>}</>}
    </main>
  </div>;
}
