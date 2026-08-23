import { cn } from "@/lib/utils"

/**
 * The shape of an answer that has not arrived.
 *
 * The generated default is `animate-pulse bg-accent`; this wears `.skeleton`
 * from `index.css` instead — the shimmer that sweeps left to right, which
 * reads as work in progress rather than as a box blinking. Same substitution
 * the CSS-variable block makes for the rest of the primitives: what is pulled
 * in keeps its API and wears Drive's clothes, so there is one placeholder in
 * the product and not two. Reduced motion drops the sweep and leaves the box.
 */
function Skeleton({ className, ...props }: React.ComponentProps<"div">) {
  return <div data-slot="skeleton" className={cn("skeleton rounded-md", className)} {...props} />
}

export { Skeleton }
