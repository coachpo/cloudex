"use client";

import { ChevronDown, Monitor, Moon, Sun } from "lucide-react";
import { useRef } from "react";
import { useT } from "@/components/i18n-provider";
import { useTheme } from "@/components/theme-provider";
import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuRadioGroup, DropdownMenuRadioItem,
  DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { parseTheme } from "@/lib/theme";

const themes = {
  system: { label: "themeSystem", icon: Monitor },
  light: { label: "themeLight", icon: Sun },
  dark: { label: "themeDark", icon: Moon },
} as const;

export function ThemeSwitcher() {
  const { t } = useT();
  const { theme, selectTheme } = useTheme();
  const trigger = useRef<HTMLButtonElement>(null);
  const { label, icon: Icon } = themes[theme];
  const selected = t(label);

  return <DropdownMenu modal={false}>
    <DropdownMenuTrigger asChild>
      <Button ref={trigger} type="button" variant="ghost" size="sm" className="max-sm:min-h-11"
        aria-label={t("themeSelection", { theme: selected })}>
        <Icon data-icon="inline-start" strokeWidth={1.55} aria-hidden="true"/>
        {selected}
        <ChevronDown data-icon="inline-end" strokeWidth={1.55} aria-hidden="true"/>
      </Button>
    </DropdownMenuTrigger>
    <DropdownMenuContent align="end" collisionPadding={8} onKeyDownCapture={event => {
      if (event.key !== "Tab") return;
      // Radix cancels Tab in menus. Moving focus to the trigger dismisses this
      // menu and lets the browser continue tabbing from the trigger's position.
      event.stopPropagation();
      trigger.current?.focus({ preventScroll: true });
    }}>
      <DropdownMenuRadioGroup value={theme} onValueChange={value => selectTheme(parseTheme(value))}>
        {Object.entries(themes).map(([value, { label, icon: OptionIcon }]) =>
          <DropdownMenuRadioItem key={value} value={value}>
            <OptionIcon strokeWidth={1.55} aria-hidden="true"/>{t(label)}
          </DropdownMenuRadioItem>)}
      </DropdownMenuRadioGroup>
    </DropdownMenuContent>
  </DropdownMenu>;
}
