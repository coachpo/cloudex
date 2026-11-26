import * as React from "react"
import { cva, type VariantProps } from "class-variance-authority"
import { cn } from "@/lib/utils"
import { Slot } from "radix-ui"

const buttonVariants = cva(
  "inline-flex shrink-0 items-center justify-center gap-2 rounded-md text-sm font-medium whitespace-nowrap select-none transition-[background-color,border-color,color,box-shadow,transform] duration-150 ease-[cubic-bezier(.2,.8,.2,1)] active:translate-y-px active:duration-75 motion-reduce:transition-none motion-reduce:active:translate-y-0 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring disabled:pointer-events-none disabled:opacity-45 aria-invalid:border-destructive [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
  {
    variants: {
      variant: {
        default: "bg-primary text-primary-foreground shadow-[inset_0_-1px_0_rgb(0_0_0/.12)] hover:bg-[var(--action-hover)]",
        danger: "bg-destructive text-[var(--danger-ink)] hover:bg-[var(--danger-hover)]",
        destructive:
          "border border-[color-mix(in_srgb,var(--danger)_45%,transparent)] bg-surface text-destructive hover:bg-[var(--danger-soft)] hover:border-destructive",
        outline:
          "border border-[var(--line-strong)] bg-surface text-foreground hover:bg-accent hover:border-[var(--ink-muted)]",
        secondary:
          "bg-secondary text-secondary-foreground hover:bg-accent",
        ghost:
          "text-muted-foreground hover:bg-accent hover:text-foreground",
        link: "text-[var(--link)] underline-offset-4 hover:underline",
      },
      size: {
        default: "h-9 px-4 has-[>svg]:px-3 max-sm:h-11",
        xs: "h-7 gap-1 rounded-sm px-2 text-[13px] has-[>svg]:px-1.5 [&_svg:not([class*='size-'])]:size-3.5 max-sm:h-9",
        sm: "h-8 gap-1.5 px-3 has-[>svg]:px-2.5 max-sm:h-10",
        lg: "h-10 px-6 has-[>svg]:px-4 max-sm:h-11",
        icon: "size-9 max-sm:size-11",
        "icon-xs": "size-7 rounded-sm [&_svg:not([class*='size-'])]:size-3.5",
        "icon-sm": "size-8 max-sm:size-11",
        "icon-lg": "size-10 max-sm:size-11",
      },
    },
    defaultVariants: {
      variant: "default",
      size: "default",
    },
  }
)

function Button({
  className,
  variant = "default",
  size = "default",
  asChild = false,
  ...props
}: React.ComponentProps<"button"> &
  VariantProps<typeof buttonVariants> & {
    asChild?: boolean
  }) {
  const Comp = asChild ? Slot.Root : "button"

  return (
    <Comp
      data-slot="button"
      data-variant={variant}
      data-size={size}
      className={cn(buttonVariants({ variant, size, className }))}
      {...props}
    />
  )
}

export { Button, buttonVariants }
