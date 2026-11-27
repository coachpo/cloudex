"use client";

import { useId, useRef, type ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { useT } from "./i18n-provider";

export function GrantActionDialog({ label, title, children, action, disabled, destructive }: {
  label: string;
  title: string;
  children: ReactNode;
  action: () => void;
  disabled?: boolean;
  destructive?: boolean;
}) {
  const { t } = useT();
  const dialog = useRef<HTMLDialogElement>(null);
  const labelID = useId();
  const descriptionID = useId();

  return <>
    <Button type="button" variant={destructive ? "destructive" : "outline"} size="sm"
      onClick={() => dialog.current?.showModal()} disabled={disabled}>{label}</Button>
    <dialog ref={dialog} className="grant-action-dialog" aria-labelledby={labelID} aria-describedby={descriptionID}
      onClose={event => {
        const element = event.currentTarget;
        const confirmed = element.returnValue === "confirm";
        element.returnValue = "";
        if (confirmed && !disabled) action();
      }}>
      <h1 id={labelID}>{title}</h1>
      <div className="muted" id={descriptionID}>{children}</div>
      <form method="dialog" className="actions justify-end">
        {/* Use the native attribute so focus is set when the dialog opens. */}
        <Button type="submit" value="cancel" variant="outline" ref={button => button?.setAttribute("autofocus", "")}>{t("cancel")}</Button>
        <Button type="submit" value="confirm" variant={destructive ? "destructive" : "default"}
          disabled={disabled}>{label}</Button>
      </form>
    </dialog>
  </>;
}
