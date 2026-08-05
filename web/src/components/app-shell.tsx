import { useState } from "react";
import { Outlet, useLocation } from "react-router";
import { SidebarInset, SidebarProvider } from "@e412/rnui-react";
import { CommandPalette } from "./command-palette";
import { ErrorBoundary } from "./error-boundary";
import { Header } from "./header";
import { SidebarNav } from "./sidebar-nav";

// Toaster lives at the App level (not here) — unauthenticated screens like
// Setup and Login need toasts too, and mounting it here as well as there
// would double every toast.
export function AppShell() {
  const [paletteOpen, setPaletteOpen] = useState(false);
  const { pathname } = useLocation();

  return (
    <SidebarProvider>
      <SidebarNav />
      <SidebarInset>
        <Header onOpenCommandPalette={() => setPaletteOpen(true)} />
        <main className="flex-1 p-6">
          {/* Keyed on the path: this shell is the persistent layout element,
              so one instance of the boundary outlives every sibling route
              change. Without the key, a page that throws once leaves
              "Something went wrong" pinned in the content area while the
              sidebar happily changes the URL underneath it — every link
              looks broken until a reload. */}
          <ErrorBoundary key={pathname}>
            <Outlet />
          </ErrorBoundary>
        </main>
      </SidebarInset>
      <CommandPalette open={paletteOpen} onOpenChange={setPaletteOpen} />
    </SidebarProvider>
  );
}
