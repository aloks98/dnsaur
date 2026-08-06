import { useState } from "react";
import { Outlet, useLocation } from "react-router";
import { cn } from "@e412/rnui-react";
import { isFullBleedRoute } from "../lib/nav";
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
      {/* Every page gets the shell's gutter except the dashboard and the
          query log, which are full-bleed grids of hairline-separated bands —
          their rules have to meet the viewport edges, not float inside a
          24px frame. `flex flex-col` is what lets those pages claim the
          remaining height, so their vertical rules run to the bottom of the
          window however few rows they have. */}
      {/* min-h-0 lets the flex child actually shrink; without it a long
          table forces the main element past the viewport and the internal
          scroller never engages. Padded routes keep the ordinary page
          scroll; full-bleed routes own their own scrolling, because the
          thing that should scroll there is one pane, not the chrome. */}
      <main
        className={cn(
          "flex min-h-0 flex-1 flex-col",
          isFullBleedRoute(pathname) ? "overflow-hidden" : "overflow-y-auto p-6",
        )}
      >
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
