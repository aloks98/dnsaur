import { useId } from "react";
import { type Theme, useTheme } from "../lib/theme";

// Palette + geometry are locked by docs/superpowers/specs/2026-08-05-logo-design.md.
// Keep in sync with web/public/favicon.svg and docs/assets/logo*.svg.
const PALETTES = {
  light: { tile: "#2FE26F", body: "#101010", spike: "#0A5B2C", shade: "#000000", eye: "#F2FBF5", pupil: "#101010" },
  dark: { tile: "#0F2B1C", body: "#2FE26F", spike: "#A9F5C7", shade: "#1FA84F", eye: "#F2FBF5", pupil: "#07140C" },
} as const satisfies Record<Theme, Record<string, string>>;

const SPIKE = "M -4.4 3.6 Q 0 -8 4.4 3.6 Q 0 6.4 -4.4 3.6 Z";
const SPIKE_TRANSFORMS = [
  "translate(25.3 8.2) rotate(-8)",
  "translate(34.8 15.2) rotate(50)",
  "translate(38.6 31.8) rotate(72)",
  "translate(42 48.8) rotate(78)",
];
const BODY =
  "M 24 66 C 24 50 22 40 24 30 C 23 27 20 25 17 22.5 C 13.5 20.5 13 15.5 16.5 12.5 " +
  "C 19.5 8.6 28.5 8.2 33 11.6 C 36.2 15.2 36.5 20 35 25.5 C 39 38 43 50 45 66 Z";

export function DnsaurLogo({
  size = 24,
  variant,
  className,
}: {
  size?: number;
  /** Force a palette; defaults to the live app theme. */
  variant?: Theme;
  className?: string;
}) {
  const { theme } = useTheme();
  const clipId = useId();
  const p = PALETTES[variant ?? theme];
  return (
    <svg
      viewBox="0 0 64 64"
      width={size}
      height={size}
      role="img"
      aria-label="dnsaur logo"
      className={className}
    >
      <rect data-part="tile" width="64" height="64" rx="10" fill={p.tile} />
      <clipPath id={clipId}>
        <rect width="64" height="64" rx="10" />
      </clipPath>
      <g clipPath={`url(#${clipId})`}>
        <g fill={p.spike}>
          {SPIKE_TRANSFORMS.map((transform) => (
            <path key={transform} d={SPIKE} transform={transform} />
          ))}
        </g>
        <path data-part="body" d={BODY} fill={p.body} />
        <circle cx="15.2" cy="16.8" r="1" fill={p.shade} />
        <circle cx="25" cy="16" r="4.5" fill={p.eye} />
        <circle cx="23.6" cy="16.5" r="2.2" fill={p.pupil} />
      </g>
    </svg>
  );
}
