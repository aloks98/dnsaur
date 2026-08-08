import { useState } from "react";
import { Outlet, useLocation } from "react-router";
import { CommandPalette } from "./command-palette";
import { ErrorBoundary } from "./error-boundary";
import { TopNav } from "./top-nav";

// Toaster lives at the App level (not here) — unauthenticated screens like
// Setup and Login need toasts too, and mounting it here as well as there
// would double every toast.
export function AppShell() {
  const [paletteOpen, setPaletteOpen] = useState(false);
  const { pathname } = useLocation();

  return (
    // h-screen, not min-h-screen: the full-bleed pages size a table to
    // "whatever is left of the window", and flex-1 can only resolve that
    // against a parent with a definite height. With min-h-screen the chain
    // is open-ended, which is why the query log had to hardcode a scroll
    // height and then stopped short of the bottom of the page.
    <div className="flex h-screen flex-col overflow-hidden bg-background">
      <TopNav onOpenCommandPalette={() => setPaletteOpen(true)} />
      {/* No gutter, and no scrolling here. Every screen is a full-bleed
          grid of hairline-separated bands whose rules have to meet the
          viewport edges rather than float inside a 24px frame, and each
          owns its own scrolling — the thing that should scroll is one pane,
          not the chrome.
          This used to branch per route while the screens were rebuilt one
          at a time; once Account was the last one left, the branch always
          took the same side.
          `flex flex-col` is what lets a page claim the remaining height, and
          min-h-0 lets it actually shrink — without it a long table forces
          <main> past the viewport and the internal scroller never engages. */}
      <main className="flex min-h-0 flex-1 flex-col overflow-hidden">
        {/* Keyed on the path: this shell is the persistent layout element,
            so one instance of the boundary outlives every sibling route
            change. Without the key, a page that throws once leaves
            "Something went wrong" pinned in the content area while the top
            nav happily changes the URL underneath it — every link looks
            broken until a reload. */}
        <ErrorBoundary key={pathname}>
          <Outlet />
        </ErrorBoundary>
      </main>
      {/* Always mounted, open or not: it owns the ⌘K / Ctrl+K listener. */}
      <CommandPalette open={paletteOpen} onOpenChange={setPaletteOpen} />
    </div>
  );
}
