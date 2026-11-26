"use client";

import { useT } from "@/components/i18n-provider";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useIsMutating, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Boxes, LogOut, Server, Settings2 } from "lucide-react";
import { useEffect, type ReactNode } from "react";
import { accountMutationKey, APIError, change, sessionQuery } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Brand, Loading, Notice } from "./shared";
import { ThemeSwitcher } from "./theme-switcher";

const links = [
  { to: "/workspaces", label: "workspaces", icon: Boxes },
  { to: "/hosts", label: "hosts", icon: Server },
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
  const username = session.data?.username;
  return <div className="app">
    <a className="skip-link" href="#main">{t("skipToMain")}</a>
    <header className="app-header">
      <div className="app-header-inner">
        <Link className="brand" href="/workspaces"><Brand/><span>Cloudex</span></Link>
        <nav className="app-nav" aria-label={t("mainNavigation")}>
          {links.map(({ to, label, icon: Icon }) =>
            <Link href={to} key={to} className="nav-link" aria-current={path.startsWith(to) ? "page" : undefined}>
              <Icon strokeWidth={1.75} aria-hidden="true"/><span>{t(label)}</span>
            </Link>)}
        </nav>
        <div className="header-end">
          {username && <span className="session"><span className="avatar" aria-hidden="true">{username.slice(0, 1)}</span><span>{username}</span></span>}
          <ThemeSwitcher/>
          <span className="header-divider" aria-hidden="true"/>
          <Button type="button" variant="ghost" size="icon-sm" title={t("signOut")} aria-label={t("signOut")}
            disabled={!session.data || logout.isPending} onClick={() => logout.mutate()}><LogOut strokeWidth={1.75}/></Button>
        </div>
      </div>
    </header>
    <main className="page" id="main" tabIndex={-1}>
      {logout.error && <Notice error={logout.error} retry={() => logout.mutate()}/>}
      {session.data && !denied ? children : <>
        <h1>{t("console")}</h1>
        {session.error && !denied ? <Notice error={session.error} retry={() => session.refetch()}/> :
          <Loading label={denied ? t("openingLogin") : t("checkingSession")} rows={0}/>}
      </>}
    </main>
  </div>;
}
