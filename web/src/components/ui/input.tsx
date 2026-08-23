import * as React from "react"

import { cn } from "@/lib/utils"

function Input({ className, type, ...props }: React.ComponentProps<"input">) {
  return (
    <input
      type={type}
      data-slot="input"
      className={cn(
        "h-9 w-full min-w-0 rounded-md border border-input bg-transparent px-3 py-1 text-base shadow-xs transition-[color,box-shadow] selection:bg-primary selection:text-primary-foreground file:inline-flex file:h-7 file:border-0 file:bg-transparent file:text-sm file:font-medium file:text-foreground placeholder:text-muted-foreground disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50 md:text-sm dark:bg-input/30",
        // The ring is `index.css`'s single `:focus-visible` outline. What is
        // left here is the field's own state: a teal edge while it has focus,
        // and a red one when it is announced invalid.
        "focus-visible:border-ring",
        // The width is not decoration: upstream declares it once on the focus
        // ring and lets the invalid state reuse it, and this file replaced that
        // focus ring with the product's own outline. Without it the invalid
        // state names a ring colour and draws a ring of no width at all — the
        // red edge is the border alone, which is also what a plain field wears
        // when its edge happens to be dark.
        "aria-invalid:border-destructive aria-invalid:ring-[3px] aria-invalid:ring-destructive/20 dark:aria-invalid:ring-destructive/40",
        className
      )}
      {...props}
    />
  )
}

export { Input }
