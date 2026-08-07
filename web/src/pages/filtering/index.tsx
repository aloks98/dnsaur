import { Outlet } from "react-router";

/**
 * Filtering is three real routes (`/filtering/lists`, `/filtering/rules`,
 * `/filtering/clients`), not a `<Tabs>` with local state — the shell's
 * second row is what switches between them, and a URL-less tab set could
 * neither be deep-linked, bookmarked, opened in a new tab, nor survive a
 * reload without snapping back to Lists.
 *
 * Which leaves this layout with nothing to hold. It used to add a "Filtering"
 * heading and a rule above the panel, both of which the chrome already says
 * — row 1 marks FILTERING, row 2 marks the tab. It renders the outlet
 * directly so a full-bleed panel is a direct flex child of <main> and can
 * claim the remaining height; a wrapper here would break that chain.
 */
export function FilteringLayout() {
  return <Outlet />;
}
