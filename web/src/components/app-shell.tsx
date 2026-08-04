import { useState } from "react";
import { Outlet } from "react-router";
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

  return (
    <SidebarProvider>
      <SidebarNav />
      <SidebarInset>
        <Header onOpenCommandPalette={() => setPaletteOpen(true)} />
        <main className="flex-1 p-6">
          <ErrorBoundary>
            <Outlet />
          </ErrorBoundary>
        </main>
      </SidebarInset>
      <CommandPalette open={paletteOpen} onOpenChange={setPaletteOpen} />
    </SidebarProvider>
  );
}
