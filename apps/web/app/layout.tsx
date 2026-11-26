import type { Metadata } from "next";
import { cookies, headers } from "next/headers";
import type { ReactNode } from "react";
import "@fontsource-variable/ibm-plex-sans";
import "@fontsource-variable/jetbrains-mono";
import "@xterm/xterm/css/xterm.css";
import "./globals.css";
import { Providers } from "@/components/providers";
import { I18nProvider } from "@/components/i18n-provider";
import { ThemeProvider } from "@/components/theme-provider";
import { message, resolveLocale } from "@/lib/i18n";
import { parseTheme, themeCookie } from "@/lib/theme";

async function localeForRequest() {
  return resolveLocale((await headers()).get("accept-language"));
}

export async function generateMetadata(): Promise<Metadata> {
  return { title: "Cloudex", description: message(await localeForRequest(), "metadataDescription") };
}

export default async function RootLayout({children}: {children: ReactNode}) {
  const [locale, cookieStore] = await Promise.all([localeForRequest(), cookies()]);
  const theme = parseTheme(cookieStore.get(themeCookie)?.value);
  return <html lang={locale} data-theme={theme}>
    <body>
      <I18nProvider locale={locale}>
        <ThemeProvider initialTheme={theme}><Providers>{children}</Providers></ThemeProvider>
      </I18nProvider>
    </body>
  </html>;
}
