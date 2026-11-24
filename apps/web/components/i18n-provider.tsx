"use client";

import { createContext, Fragment, useCallback, useContext, type ReactNode } from "react";
import { interpolation, message, type Locale, type MessageKey } from "@/lib/i18n";

const LocaleContext = createContext<Locale>("en");

export function I18nProvider({ locale, children }: { locale: Locale; children: ReactNode }) {
  return <LocaleContext.Provider value={locale}>{children}</LocaleContext.Provider>;
}

export function useT() {
  const locale = useContext(LocaleContext);
  const t = useCallback((key: MessageKey, values?: Record<string, string | number>) => message(locale, key, values), [locale]);
  return { locale, t };
}

export function Translation({ id, values }: { id: MessageKey; values: Record<string, ReactNode> }) {
  const { t } = useT();
  return <>{t(id).split(interpolation).map((part, index) => index % 2
    ? <Fragment key={index}>{values[part]}</Fragment> : part)}</>;
}
