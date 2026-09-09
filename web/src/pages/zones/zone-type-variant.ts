import type { BadgeProps } from "@e412/rnui-react";
import type { Zone } from "../../api/types";

/**
 * The badge a zone's type wears, on the list and on its own page.
 *
 * A category tag, not a verdict — the exact mapping from the artboard, and
 * deliberately not "each type gets a fresh colour": stub shares the neutral
 * `secondary` variant rather than being given one of its own.
 *
 * One map for both screens. Two copies drifted apart the moment either was
 * edited, and a zone that was `warning-light` on the list and something else
 * on its own page reads as two different kinds of zone.
 */
export const ZONE_TYPE_VARIANT: Record<Zone["type"], NonNullable<BadgeProps["variant"]>> = {
  primary: "primary-light",
  secondary: "info-light",
  stub: "secondary",
  forwarder: "warning-light",
  internal: "secondary",
};
