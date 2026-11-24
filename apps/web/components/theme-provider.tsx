"use client";

import { createContext, useContext, useState, type ReactNode } from "react";
import { themeCookie, type Theme } from "@/lib/theme";

const ThemeContext = createContext<{ theme: Theme; selectTheme: (theme: Theme) => void }>({
  theme: "system",
  selectTheme: () => {},
});

export function ThemeProvider({ initialTheme, children }: { initialTheme: Theme; children: ReactNode }) {
  const [theme, setTheme] = useState(initialTheme);

  function selectTheme(value: Theme) {
    document.documentElement.dataset.theme = value;
    document.cookie = `${themeCookie}=${value}; Path=/; Max-Age=31536000; SameSite=Lax`;
    setTheme(value);
  }

  return <ThemeContext.Provider value={{ theme, selectTheme }}>{children}</ThemeContext.Provider>;
}

export function useTheme() {
  return useContext(ThemeContext);
}
