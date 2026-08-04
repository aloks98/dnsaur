import { useState } from "react";
import { Outlet } from "react-router";
import { SidebarInset, SidebarProvider, Toaster } from "@e412/rnui-react";
import { useTheme } from "../lib/theme";
import { CommandPalette } from "./command-palette";
import { ErrorBoundary } from "./error-boundary";
import { Header } from "./header";
import { SidebarNav } from "./sidebar-nav";

export function AppShell() {
  const [paletteOpen, setPaletteOpen] = useState(false);
  const { theme } = useTheme();

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
      <Toaster theme={theme} />
    </SidebarProvider>
  );
}
