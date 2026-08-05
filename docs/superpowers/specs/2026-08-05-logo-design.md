# dnsaur logo — design spec

Date: 2026-08-05
Status: approved direction (G1/G2 dual-mode), pending implementation

## Concept

A friendly "calm guardian" sauropod mascot, delivered as a contained
rounded-square tile: the dinosaur's head and neck rise diagonally from the
bottom-right corner like a periscope, watching over the network. Soft spike
plates run along the spine — a nod to DNS resolution hops without literal
network clichés. Chosen over a full-body silhouette and round-plate
variants after side-by-side comparison at real raster sizes (16/32/64/256
px) across ~15 drafts.

## The mark

- Canvas: 64×64 viewBox, rounded-square tile `rx="10"`, artwork clipped to
  the tile.
- Geometry: lean neck (base spans x≈24–45 at the tile bottom), rounded
  head with flat-ish top, large white eye with dark pupil, small nostril
  dot, four soft spikes (`M -4.4 3.6 Q 0 -8 4.4 3.6 Q 0 6.4 -4.4 3.6 Z`,
  translated/rotated onto the spine) drawn behind the body, sunk deep
  enough that their curved bases tuck fully under the silhouette with no
  gaps at any zoom.
- Master source of truth is the SVG committed to the repo (see
  Deliverables); raster exports derive from it.

## Palette and dual-mode behavior

The product UI direction (WIP mock, 2026-08-05) is a terminal-style
black-and-spring-green design. The logo uses two arrangements of that
palette and switches with the viewer's color scheme:

**Light mode — "badge" (G2):** accent-green tile, black dinosaur.

| Token | Hex | Use |
|---|---|---|
| tile | `#2FE26F` | rounded-square background |
| body | `#101010` | dinosaur head/neck |
| spike | `#0A5B2C` | spine spikes |
| eye / pupil | `#F2FBF5` / `#101010` | eye |

**Dark mode — "forest" (G1):** deep forest tile, accent-green dinosaur.

| Token | Hex | Use |
|---|---|---|
| tile | `#0F2B1C` | rounded-square background |
| body | `#2FE26F` | dinosaur head/neck |
| shade | `#1FA84F` | nostril |
| spike | `#A9F5C7` | spine spikes |
| eye / pupil | `#F2FBF5` / `#07140C` | eye |

Note: `#2FE26F` is sampled from the WIP UI mock. When the web theme's
final green token lands in `web/src/styles/`, the logo SVGs should adopt
that exact value (single source: keep the hexes in sync manually and note
it in the asset header comment).

## Deliverables

1. **Favicon** — replace `web/public/favicon.svg` with a single SVG that
   embeds a `prefers-color-scheme` media query in an internal `<style>`
   block, rendering G2 in light and G1 in dark (supported by modern
   browsers for SVG favicons).
2. **Dashboard header logo** — small (~24px) tile mark beside the "dnsaur"
   wordmark in the app shell (currently
   `web/src/components/sidebar-nav.tsx` SidebarHeader, ~line 141). If the
   WIP top-bar redesign has landed by implementation time, the logo goes in
   its brand block instead; otherwise wire it into the existing
   SidebarHeader now and let the redesign carry it over. Inline SVG
   component driven by the app's
   theme state (`data-theme`), not the OS media query, so it follows the
   in-app theme toggle.
3. **README banner** — `docs/assets/logo.svg` (G2) and
   `docs/assets/logo-dark.svg` (G1), each: tile mark + lowercase "dnsaur"
   wordmark in a rounded geometric sans converted to paths (default:
   Nunito ExtraBold; any OFL-licensed rounded sans is acceptable if
   Nunito can't be fetched at build time). Referenced
   from README via `<picture>` + `prefers-color-scheme` so GitHub/Forgejo
   swap automatically.
4. **Docs** — README gains the banner; `docs/` notes the logo assets and
   palette where relevant (per the keep-docs-updated convention).

## Out of scope

- Re-theming the dashboard (the black/green UI redesign is its own
  workstream; this spec only consumes its palette).
- Raster/ICO favicon fallbacks, social/OG images, animated variants —
  follow-ups if ever needed.

## Verification

- Rasterize the final SVGs at true 16 px and 32 px and visually confirm
  the silhouette, eye, and spikes survive in both modes (done for all
  drafts with sharp; repeat for finals).
- Favicon visibly switches between G1/G2 when toggling OS color scheme.
- Dashboard builds (`pnpm build`) and the header renders the logo in both
  in-app themes.
- README banner renders on the Forgejo instance and the GitHub mirror in
  both color schemes.
