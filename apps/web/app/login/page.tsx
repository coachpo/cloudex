"use client";

import { useT } from "@/components/i18n-provider";
import { useRouter } from "next/navigation";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Brand, Empty, Notice } from "@/components/shared";
import { ThemeSwitcher } from "@/components/theme-switcher";
import { Button } from "@/components/ui/button";
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field";
import { api, APIError, change, sessionQuery, type Session } from "@/lib/api";

export default function Login() {
  const { t } = useT();
  const router = useRouter();
  const client = useQueryClient();
  const setup = useQuery({ queryKey: ["setup"], queryFn: () => api<{ required: boolean }>("/auth/setup") });
  const required = setup.data?.required;
  const [mismatch, setMismatch] = useState(false);
  const login = useMutation({
    gcTime: 0,
    mutationFn: (credentials: { username: string; password: string }) =>
      change<Session>(required ? "/auth/setup" : "/auth/login", "POST", credentials),
    onSuccess: (session) => {
      client.clear();
      client.setQueryData(sessionQuery.queryKey, session);
      router.replace("/workspaces");
    },
    onError: (error) => {
      if (error instanceof APIError && (error.code === "SETUP_REQUIRED" || error.code === "SETUP_COMPLETED")) {
        void setup.refetch();
      }
    },
  });
  function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const data = new FormData(e.currentTarget);
    const password = String(data.get("password"));
    if (required && password !== data.get("confirmation")) {
      setMismatch(true);
      login.reset();
      return;
    }
    setMismatch(false);
    login.mutate({ username: String(data.get("username")), password });
  }
  return <main className="login-page">
    <div className="login-brand"><Brand/><span className="product">Cloudex</span><div className="ml-auto"><ThemeSwitcher/></div></div>
    <section className="panel login-panel">
      {setup.error ? <Notice error={setup.error} retry={() => setup.refetch()}/> :
        required === undefined ? <Empty>{t("checkingSetup")}</Empty> : <>
        <h1>{required ? t("createAdministrator") : t("signIn")}</h1>
        <form key={String(required)} className="form" onSubmit={submit}>
          <FieldGroup className="form">
            <Field className="fields">
              <FieldLabel htmlFor="username">{t("username")}</FieldLabel>
              <input className="field" name="username" id="username" autoComplete="username" required/>
            </Field>
            <Field className="fields">
              <FieldLabel htmlFor="password">{t("password")}</FieldLabel>
              <input className="field" name="password" id="password" type="password"
                autoComplete={required ? "new-password" : "current-password"} required/>
            </Field>
            {required && <Field className="fields" data-invalid={mismatch}>
              <FieldLabel htmlFor="confirmation">{t("confirmPassword")}</FieldLabel>
              <input className="field" name="confirmation" id="confirmation" type="password" autoComplete="new-password"
                aria-invalid={mismatch} aria-describedby={mismatch ? "setup-password-mismatch" : undefined} required/>
              {mismatch && <p className="small" id="setup-password-mismatch" role="alert">{t("passwordMismatch")}</p>}
            </Field>}
          </FieldGroup>
          {login.error && <Notice error={login.error}/>}
          <Button disabled={login.isPending}>{required
            ? (login.isPending ? t("creatingAdministrator") : t("createAdministrator"))
            : (login.isPending ? t("signingIn") : t("signIn"))}</Button>
        </form>
      </>}
    </section>
  </main>;
}
