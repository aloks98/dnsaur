import type { ComponentPropsWithoutRef, ElementType, ReactNode } from "react";
import { cn } from "@e412/rnui-react";

/**
 * dnsaur's one visual idea for "this needs warning emphasis": a
 * `bg-warning/8` wash with an inset left bar in `--warning`. It had been
 * hand-rolled at three call sites before this existed — the same drift
 * stale-data-alert.tsx's comment describes ("copy-pasted six times before,
 * drifting in wording") — and had already started to diverge: two sites at
 * a 3px inset, one at 2px.
 *
 * Covers exactly two of those three sites: the encryption-downgrade banner
 * (encryption-downgrade-banner.tsx) and upstreams-field's transport-switch
 * notice. Both are standalone warning notices — they exist only when
 * something is wrong, and carrying that warning is their whole job.
 *
 * The Settings save bar (settings.tsx) is not built on this component even
 * though it wears the same tint: it is always present, showing save state,
 * and merely *gains* warning emphasis when the form is dirty. A component
 * named `WarningStrip` would misdescribe it. It reuses just the tint —
 * `WARNING_STRIP_TINT` below — in its own `cn(...)` call instead.
 *
 * Not the rnui `Alert`: `Alert` is a card with a title, description and
 * action, meant to sit inside page content (see stale-data-alert.tsx,
 * api-unreachable-banner.tsx). These three sites are thinner strips that
 * often *are* the content — a shell-wide banner, a save bar, a nested
 * field notice — not cards with that internal structure.
 *
 * Deliberately does not own the element or ARIA role: the banner needs
 * `<div role="alert">`, the notice needs `<output>`, and the two are not
 * interchangeable. `as` picks the tag; everything else — `role`,
 * `aria-*`, `className`, children — passes straight through to it. The
 * size split (`roomy` vs `tight`) is likewise kept rather than normalised
 * away: the artboard specifies a 3px inset with roomy padding for
 * full-bleed bars and a 2px inset with tight padding for a notice nested
 * inside a field's own padding, and both are intentional.
 */

const WARNING_STRIP_PADDING = {
  roomy: "px-5 py-2.5",
  tight: "px-2.5 py-1.5",
} as const;

export type WarningStripSize = keyof typeof WARNING_STRIP_PADDING;

/**
 * The tint half of the treatment — `bg-warning/8` plus the roomy (3px)
 * inset bar — factored out for settings.tsx's save bar, which wants this
 * exact wash but isn't a `WarningStrip` (see the module comment above).
 * `WarningStrip` itself builds on this same constant for its `roomy` size,
 * so there is one definition of the roomy tint, not two.
 */
export const WARNING_STRIP_TINT = "bg-warning/8 shadow-[inset_3px_0_0_var(--warning)]";

const WARNING_STRIP_TONE: Record<WarningStripSize, string> = {
  roomy: WARNING_STRIP_TINT,
  tight: "bg-warning/8 shadow-[inset_2px_0_0_var(--warning)]",
};

type WarningStripProps<T extends ElementType> = {
  /** The element (and its semantics) the call site needs — e.g. `"div"`
   * for a `role="alert"` banner, `"output"` for a live status notice. */
  as: T;
  size: WarningStripSize;
  children: ReactNode;
} & Omit<ComponentPropsWithoutRef<T>, "as" | "children">;

export function WarningStrip<T extends ElementType = "div">({
  as,
  size,
  className,
  children,
  ...rest
}: WarningStripProps<T>) {
  // `as` stays generic in the public props (so callers get real
  // element/attribute checking — a `<output>` can't be handed `href`, a
  // `<div>` can't be handed `value`), but there is no single prop type that
  // both satisfies every element JSX supports and describes `...rest`
  // generically, so the render below widens to `ElementType`.
  const Component = as as ElementType;
  return (
    <Component
      className={cn(WARNING_STRIP_TONE[size], WARNING_STRIP_PADDING[size], className)}
      {...rest}
    >
      {children}
    </Component>
  );
}
