"use client";

import Link from "next/link";
import { useT } from "@/components/i18n-provider";

export default function NotFound() {
  const { t } = useT();
  return <main className="page">
    <section className="panel">
      <h1>{t("pageNotFound")}</h1>
      <Link className="link" href="/workspaces">{t("returnToWorkspaces")}</Link>
    </section>
  </main>;
}
