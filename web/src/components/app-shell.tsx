import { useState } from "react";
import { Outlet, useLocation } from "react-router";
import { cn } from "@e412/rnui-react";
import { DASHBOARD_PATH } from "../lib/nav";
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
      {/* Every page gets the shell's gutter except the dashboard, which is a
          full-bleed grid of hairline-separated bands — its rules have to
          meet the viewport edges, not float inside a 24px frame. `flex
          flex-col` is what lets that page claim the remaining height, so
          its vertical rule runs to the bottom of the window however few
          rows it has. */}
      <main className={cn("flex flex-1 flex-col", pathname !== DASHBOARD_PATH && "p-6")}>
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
