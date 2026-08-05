# dnsaur Logo Assets Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the approved sauropod-tile logo as favicon, dashboard header logo, and README banner, per `docs/superpowers/specs/2026-08-05-logo-design.md`.

**Architecture:** The mark is a 64×64 SVG (rounded tile, clipped sauropod head/neck, four spikes). It exists in three carriers: a standalone favicon that switches palettes via an embedded `prefers-color-scheme` style; a React component that switches via the app's `useTheme()` store; and two static banner SVGs (light/dark) with the "dnsaur" wordmark converted to paths, swapped in the README via `<picture>`.

**Tech Stack:** SVG, React 19 + vitest + testing-library (existing `web/` setup, pnpm), opentype.js (new devDependency, banner generation only), sharp (verification only, not committed).

## Global Constraints

- Palettes exactly as specced. Light "badge": tile `#2FE26F`, body `#101010`, spike `#0A5B2C`, shade `#000000`, eye `#F2FBF5`, pupil `#101010`. Dark "forest": tile `#0F2B1C`, body `#2FE26F`, spike `#A9F5C7`, shade `#1FA84F`, eye `#F2FBF5`, pupil `#07140C`.
- Geometry is FINAL — copy the paths verbatim from this plan; do not redraw or "clean up" coordinates.
- Conventional commit messages (project-wide rule).
- Work on a branch cut from `feat/web-dashboard` (the sidebar the logo hooks into lives there): `git checkout -b feat/logo-assets`.
- Web commands run in `web/` with pnpm (`pnpm test`, `pnpm build`).
- Wordmark font: Nunito ExtraBold (OFL). If the download URL 404s, any OFL-licensed rounded geometric sans is acceptable (spec allowance) — note the substitution in the commit body.

### Shared artwork (reference for all tasks)

Every carrier draws these same five element groups inside a `viewBox="0 0 64 64"`, in this z-order:

1. tile: `<rect width="64" height="64" rx="10" />`
2. spikes (behind body), each `d="M -4.4 3.6 Q 0 -8 4.4 3.6 Q 0 6.4 -4.4 3.6 Z"` with transforms:
   `translate(25.3 8.2) rotate(-8)` · `translate(34.8 15.2) rotate(50)` · `translate(38.6 31.8) rotate(72)` · `translate(42 48.8) rotate(78)`
3. body: `d="M 24 66 C 24 50 22 40 24 30 C 23 27 20 25 17 22.5 C 13.5 20.5 13 15.5 16.5 12.5 C 19.5 8.6 28.5 8.2 33 11.6 C 36.2 15.2 36.5 20 35 25.5 C 39 38 43 50 45 66 Z"`
4. nostril: `<circle cx="15.2" cy="16.8" r="1" />` (shade color)
5. eye: `<circle cx="25" cy="16" r="4.5" />` (eye) + `<circle cx="23.6" cy="16.5" r="2.2" />` (pupil)

Spikes and body are wrapped in `<g clip-path="url(#<tile-clip-id>)">` where the clipPath contains a copy of the tile rect.

---

### Task 1: `DnsaurLogo` React component

**Files:**
- Create: `web/src/components/dnsaur-logo.tsx`
- Test: `web/src/components/dnsaur-logo.test.tsx`

**Interfaces:**
- Consumes: `useTheme` and `Theme` from `web/src/lib/theme.ts` (existing: `useTheme(): { theme: Theme; toggleTheme(): void }`, `type Theme = "light" | "dark"`).
- Produces: `export function DnsaurLogo(props: { size?: number; variant?: Theme; className?: string }): ReactElement` — Task 2 imports `{ DnsaurLogo }` from `./dnsaur-logo`.

- [ ] **Step 1: Write the failing test**

```tsx
// web/src/components/dnsaur-logo.test.tsx
import { expect, test } from "vitest";
import { render, screen } from "@testing-library/react";
import { DnsaurLogo } from "./dnsaur-logo";

test("renders an accessible image", () => {
  render(<DnsaurLogo />);
  expect(screen.getByRole("img", { name: /dnsaur logo/i })).toBeInTheDocument();
});

test("light variant paints the tile accent green with a black body", () => {
  const { container } = render(<DnsaurLogo variant="light" />);
  expect(container.querySelector('[data-part="tile"]')).toHaveAttribute("fill", "#2FE26F");
  expect(container.querySelector('[data-part="body"]')).toHaveAttribute("fill", "#101010");
});

test("dark variant paints the tile forest with a green body", () => {
  const { container } = render(<DnsaurLogo variant="dark" />);
  expect(container.querySelector('[data-part="tile"]')).toHaveAttribute("fill", "#0F2B1C");
  expect(container.querySelector('[data-part="body"]')).toHaveAttribute("fill", "#2FE26F");
});

test("two instances don't collide on clipPath ids", () => {
  const { container } = render(
    <>
      <DnsaurLogo />
      <DnsaurLogo />
    </>,
  );
  const ids = [...container.querySelectorAll("clipPath")].map((el) => el.id);
  expect(new Set(ids).size).toBe(ids.length);
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && pnpm vitest run src/components/dnsaur-logo.test.tsx`
Expected: FAIL — cannot resolve `./dnsaur-logo`.

- [ ] **Step 3: Write the component**

```tsx
// web/src/components/dnsaur-logo.tsx
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd web && pnpm vitest run src/components/dnsaur-logo.test.tsx`
Expected: 4 passing.

- [ ] **Step 5: Commit**

```bash
git add web/src/components/dnsaur-logo.tsx web/src/components/dnsaur-logo.test.tsx
git commit -m "feat(web): add DnsaurLogo mark component"
```

---

### Task 2: Sidebar header integration

**Files:**
- Modify: `web/src/components/sidebar-nav.tsx:141-146` (SidebarHeader brand block)
- Test: `web/src/components/sidebar-nav.test.tsx` (append one test)

**Interfaces:**
- Consumes: `DnsaurLogo` from Task 1 (`import { DnsaurLogo } from "./dnsaur-logo";`).
- Produces: nothing downstream.

- [ ] **Step 1: Write the failing test** — append to `sidebar-nav.test.tsx`, reusing its existing `renderSidebar` helper:

```tsx
test("shows the dnsaur logo in the header", async () => {
  renderSidebar();
  expect(await screen.findByRole("img", { name: /dnsaur logo/i })).toBeInTheDocument();
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && pnpm vitest run src/components/sidebar-nav.test.tsx -t "dnsaur logo"`
Expected: FAIL — no img role found.

- [ ] **Step 3: Add the logo to the header**

In `sidebar-nav.tsx`, add `import { DnsaurLogo } from "./dnsaur-logo";` with the other component imports, and change the SidebarHeader block:

```tsx
<SidebarHeader>
  <div className="flex items-center gap-2 px-2 py-1.5">
    <DnsaurLogo size={24} />
    <span className="font-heading text-lg font-semibold tracking-tight text-sidebar-foreground">
      <span className="text-primary">d</span>nsaur
    </span>
  </div>
</SidebarHeader>
```

(Keep the existing span exactly as-is; only the `<DnsaurLogo size={24} />` line is new. If the WIP top-bar redesign has already replaced SidebarHeader by the time this runs, put `<DnsaurLogo size={24} />` in that brand block instead, same placement rule: logo immediately before the wordmark.)

- [ ] **Step 4: Run the full sidebar test file (not just the new test) to catch regressions**

Run: `cd web && pnpm vitest run src/components/sidebar-nav.test.tsx`
Expected: all passing, including the new test.

- [ ] **Step 5: Commit**

```bash
git add web/src/components/sidebar-nav.tsx web/src/components/sidebar-nav.test.tsx
git commit -m "feat(web): show logo mark in sidebar header"
```

---

### Task 3: Dual-mode favicon

**Files:**
- Modify: `web/public/favicon.svg` (full replacement of the placeholder)

**Interfaces:**
- Consumes: nothing from other tasks (same artwork constants, embedded statically).
- Produces: nothing downstream.

- [ ] **Step 1: Replace `web/public/favicon.svg` with exactly:**

```xml
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">
  <!-- dnsaur mark. Palette + geometry locked by
       docs/superpowers/specs/2026-08-05-logo-design.md — keep in sync with
       web/src/components/dnsaur-logo.tsx and docs/assets/logo*.svg. -->
  <style>
    .tile { fill: #2FE26F }
    .body { fill: #101010 }
    .spike { fill: #0A5B2C }
    .shade { fill: #000000 }
    .eye { fill: #F2FBF5 }
    .pupil { fill: #101010 }
    @media (prefers-color-scheme: dark) {
      .tile { fill: #0F2B1C }
      .body { fill: #2FE26F }
      .spike { fill: #A9F5C7 }
      .shade { fill: #1FA84F }
      .pupil { fill: #07140C }
    }
  </style>
  <rect class="tile" width="64" height="64" rx="10" />
  <clipPath id="tile"><rect width="64" height="64" rx="10" /></clipPath>
  <g clip-path="url(#tile)">
    <g class="spike">
      <path d="M -4.4 3.6 Q 0 -8 4.4 3.6 Q 0 6.4 -4.4 3.6 Z" transform="translate(25.3 8.2) rotate(-8)" />
      <path d="M -4.4 3.6 Q 0 -8 4.4 3.6 Q 0 6.4 -4.4 3.6 Z" transform="translate(34.8 15.2) rotate(50)" />
      <path d="M -4.4 3.6 Q 0 -8 4.4 3.6 Q 0 6.4 -4.4 3.6 Z" transform="translate(38.6 31.8) rotate(72)" />
      <path d="M -4.4 3.6 Q 0 -8 4.4 3.6 Q 0 6.4 -4.4 3.6 Z" transform="translate(42 48.8) rotate(78)" />
    </g>
    <path class="body" d="M 24 66 C 24 50 22 40 24 30 C 23 27 20 25 17 22.5 C 13.5 20.5 13 15.5 16.5 12.5 C 19.5 8.6 28.5 8.2 33 11.6 C 36.2 15.2 36.5 20 35 25.5 C 39 38 43 50 45 66 Z" />
    <circle class="shade" cx="15.2" cy="16.8" r="1" />
    <circle class="eye" cx="25" cy="16" r="4.5" />
    <circle class="pupil" cx="23.6" cy="16.5" r="2.2" />
  </g>
</svg>
```

- [ ] **Step 2: Verify it rasterizes and survives 16px**

Run (scratchpad has sharp installed; adjust path if executing elsewhere):

```bash
cd /tmp/claude-1000/-home-aloks98-projects-dnsaur/558d5695-6a05-4cdd-9129-73d76e4ca78e/scratchpad \
  && node -e "
const sharp = require('sharp');
sharp('/home/aloks98/projects/dnsaur/web/public/favicon.svg', {density: 400})
  .resize(16, 16).png().toFile('favicon-16-check.png').then(() => console.log('ok'));
"
```

Expected: `ok`, and viewing `favicon-16-check.png` shows the light-mode (green tile) mark with the dino silhouette readable. (Static rasterizers render the light branch; the dark branch is checked in Step 3.)

- [ ] **Step 3: Verify the dashboard build embeds it and the dark branch works**

Run: `cd web && pnpm build`
Expected: build succeeds; `web/dist/favicon.svg` exists and is byte-identical to `web/public/favicon.svg`. Then open the served app (or the SVG file directly) in a browser and flip the OS/devtools `prefers-color-scheme` emulation: tile switches green→forest, body black→green.

- [ ] **Step 4: Commit**

```bash
git add web/public/favicon.svg
git commit -m "feat(web): replace placeholder favicon with dual-mode dnsaur mark"
```

---

### Task 4: README banner assets (mark + wordmark lockup)

**Files:**
- Create: `web/scripts/generate-logo-banner.mjs`
- Create: `docs/assets/logo.svg` (generated, committed)
- Create: `docs/assets/logo-dark.svg` (generated, committed)
- Modify: `web/package.json` (add `opentype.js` devDependency)

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: the two banner files Task 5's README references, at exactly `docs/assets/logo.svg` and `docs/assets/logo-dark.svg`.

- [ ] **Step 1: Add opentype.js**

Run: `cd web && pnpm add -D opentype.js`
Expected: appears in `web/package.json` devDependencies.

- [ ] **Step 2: Fetch Nunito ExtraBold (not committed)**

```bash
curl -fL -o /tmp/Nunito-ExtraBold.ttf \
  https://raw.githubusercontent.com/googlefonts/nunito/main/fonts/TTF/Nunito-ExtraBold.ttf
```

Expected: a ~300KB TTF. If this 404s, fetch any OFL rounded sans (e.g. Quicksand Bold from google/fonts) and note the substitution in the Step 5 commit body.

- [ ] **Step 3: Write the generator**

```js
// web/scripts/generate-logo-banner.mjs
// Regenerates docs/assets/logo.svg and logo-dark.svg from the locked mark
// geometry + a wordmark converted to paths. Usage:
//   node scripts/generate-logo-banner.mjs /tmp/Nunito-ExtraBold.ttf
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import opentype from "opentype.js";

const fontPath = process.argv[2];
if (!fontPath) throw new Error("usage: node generate-logo-banner.mjs <font.ttf>");

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");

const PALETTES = {
  light: { tile: "#2FE26F", body: "#101010", spike: "#0A5B2C", shade: "#000000", eye: "#F2FBF5", pupil: "#101010", ink: "#101010" },
  dark: { tile: "#0F2B1C", body: "#2FE26F", spike: "#A9F5C7", shade: "#1FA84F", eye: "#F2FBF5", pupil: "#07140C", ink: "#F2FBF5" },
};

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

const font = opentype.loadSync(fontPath);
const FONT_SIZE = 46;
const TEXT_X = 92;
const BASELINE_Y = 57;
const textPath = font.getPath("dnsaur", TEXT_X, BASELINE_Y, FONT_SIZE);
const textWidth = font.getAdvanceWidth("dnsaur", FONT_SIZE);
const width = Math.ceil(TEXT_X + textWidth + 12);

function banner(mode) {
  const p = PALETTES[mode];
  const spikes = SPIKE_TRANSFORMS.map(
    (t) => `      <path d="${SPIKE}" transform="${t}" />`,
  ).join("\n");
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${width} 80" role="img" aria-label="dnsaur">
  <!-- Generated by web/scripts/generate-logo-banner.mjs — do not hand-edit.
       Palette + geometry: docs/superpowers/specs/2026-08-05-logo-design.md -->
  <g transform="translate(8 8)">
    <rect width="64" height="64" rx="10" fill="${p.tile}" />
    <clipPath id="tile-${mode}"><rect width="64" height="64" rx="10" /></clipPath>
    <g clip-path="url(#tile-${mode})">
      <g fill="${p.spike}">
${spikes}
      </g>
      <path d="${BODY}" fill="${p.body}" />
      <circle cx="15.2" cy="16.8" r="1" fill="${p.shade}" />
      <circle cx="25" cy="16" r="4.5" fill="${p.eye}" />
      <circle cx="23.6" cy="16.5" r="2.2" fill="${p.pupil}" />
    </g>
  </g>
  <path d="${textPath.toPathData(2)}" fill="${p.ink}" />
</svg>
`;
}

writeFileSync(join(repoRoot, "docs/assets/logo.svg"), banner("light"));
writeFileSync(join(repoRoot, "docs/assets/logo-dark.svg"), banner("dark"));
console.log(`wrote docs/assets/logo.svg + logo-dark.svg (viewBox 0 0 ${width} 80)`);
```

- [ ] **Step 4: Generate and visually verify**

```bash
cd web && node scripts/generate-logo-banner.mjs /tmp/Nunito-ExtraBold.ttf
```

Expected: prints the two written files. Rasterize both at ~800px wide (sharp, as in Task 3 Step 2) and check: mark on the left, "dnsaur" wordmark vertically centered beside it, correct inks per mode, no clipped ascenders/descenders. If the wordmark's cap height looks off-center against the 64px tile, tune `BASELINE_Y` (±3) and regenerate — do not hand-edit the outputs.

- [ ] **Step 5: Commit**

```bash
git add web/scripts/generate-logo-banner.mjs web/package.json web/pnpm-lock.yaml docs/assets/logo.svg docs/assets/logo-dark.svg
git commit -m "feat(docs): add dnsaur banner lockups + generator script"
```

---

### Task 5: README + docs wiring

**Files:**
- Modify: `README.md` (top of file)
- Create: `docs/assets/README.md`

**Interfaces:**
- Consumes: `docs/assets/logo.svg` + `docs/assets/logo-dark.svg` from Task 4.
- Produces: nothing downstream.

- [ ] **Step 1: Swap the README heading for the banner**

Replace the first line of `README.md` (`# dnsaur`) with:

```html
<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo.svg" alt="dnsaur" width="360">
  </picture>
</p>
```

Leave the tagline paragraph below it untouched.

- [ ] **Step 2: Document the assets**

Create `docs/assets/README.md`:

```markdown
# Brand assets

The dnsaur mark is a sauropod-in-a-tile; design rationale and locked
geometry live in `docs/superpowers/specs/2026-08-05-logo-design.md`.

| File | Use |
|---|---|
| `logo.svg` | README banner, light color scheme (green tile, black dino) |
| `logo-dark.svg` | README banner, dark color scheme (forest tile, green dino) |
| `../../web/public/favicon.svg` | favicon; switches palettes via `prefers-color-scheme` |
| `../../web/src/components/dnsaur-logo.tsx` | in-app mark; follows the app theme toggle |

Palette (light / dark): tile `#2FE26F` / `#0F2B1C`, body `#101010` /
`#2FE26F`, spikes `#0A5B2C` / `#A9F5C7`, eye `#F2FBF5`, pupil `#101010` /
`#07140C`. `#2FE26F` is sampled from the WIP UI mock — when the final
theme token lands in `web/src/styles/`, update all four carriers together.

Regenerate the banners (never hand-edit them):

    cd web && node scripts/generate-logo-banner.mjs <path-to-Nunito-ExtraBold.ttf>
```

- [ ] **Step 3: Verify rendering**

Run: `grep -n "picture" README.md` — the block is present, paths are `docs/assets/...` (relative, no leading slash). Preview the README (e.g. `gh markdown-preview`, IDE preview, or push and check Forgejo) in light and dark.

- [ ] **Step 4: Commit**

```bash
git add README.md docs/assets/README.md
git commit -m "docs: add logo banner to README and document brand assets"
```

---

### Task 6: Full verification pass

**Files:** none new — checks only.

- [ ] **Step 1: Full web test suite**

Run: `cd web && pnpm test`
Expected: all green (includes Tasks 1–2 tests).

- [ ] **Step 2: Full build**

Run: `cd web && pnpm build && cd .. && go build ./cmd/dnsaur`
Expected: both succeed — the Go binary embeds `web/dist` including the new favicon.

- [ ] **Step 3: 16px raster sanity for every carrier**

Rasterize `web/public/favicon.svg` and both banner tiles at 16px (sharp, as in Task 3 Step 2) and confirm the dino silhouette + eye survive.

- [ ] **Step 4: Commit anything outstanding, then hand off**

No code expected here; if verification forced fixes, commit them with a conventional message. Then use superpowers:finishing-a-development-branch — PR via `fj` CLI (Forgejo is the git host), PR title in conventional-commit form, e.g. `feat: add dnsaur logo (favicon, app mark, README banner)`.
