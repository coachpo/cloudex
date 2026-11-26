"use client";

import { useT } from "@/components/i18n-provider";
import { useRouter } from "next/navigation";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Brand, Loading, Notice } from "@/components/shared";
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
      e.currentTarget.querySelector<HTMLInputElement>("#confirmation")?.focus();
      return;
    }
    setMismatch(false);
    login.mutate({ username: String(data.get("username")), password });
  }
  return <main className="auth">
    <div className="auth-aside">
      <div className="grid-lines" aria-hidden="true"/>
      <span className="brand"><Brand/><span>Cloudex</span></span>
      <p className="auth-statement">{t("metadataDescription")}</p>
    </div>
    <div className="auth-main relative">
      <div className="auth-tools"><ThemeSwitcher/></div>
      <section className="auth-card" aria-labelledby={required === undefined || setup.error ? undefined : "auth-heading"}>
        <span className="brand auth-brand"><Brand/><span>Cloudex</span></span>
        {setup.error ? <Notice error={setup.error} retry={() => setup.refetch()}/> :
          required === undefined ? <Loading label={t("checkingSetup")} rows={0}/> : <>
          <h1 id="auth-heading">{required ? t("createAdministrator") : t("signIn")}</h1>
          <form key={String(required)} className="form" onSubmit={submit} onChange={event => {
            if (!mismatch) return;
            const data = new FormData(event.currentTarget);
            if (data.get("password") === data.get("confirmation")) setMismatch(false);
          }}>
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
                {mismatch && <p className="form-note" id="setup-password-mismatch" role="alert">{t("passwordMismatch")}</p>}
              </Field>}
            </FieldGroup>
            {login.error && <Notice error={login.error}/>}
            <Button size="lg" className="w-full" disabled={login.isPending}>{required
              ? (login.isPending ? t("creatingAdministrator") : t("createAdministrator"))
              : (login.isPending ? t("signingIn") : t("signIn"))}</Button>
          </form>
        </>}
      </section>
    </div>
  </main>;
}
