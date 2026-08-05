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
    <div className="flex min-h-screen flex-col bg-background">
      <TopNav onOpenCommandPalette={() => setPaletteOpen(true)} />
      <main className="flex-1 p-6">
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
