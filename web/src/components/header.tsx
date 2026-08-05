import { Search } from "lucide-react";
import { Button, Kbd, KbdGroup, SidebarTrigger } from "@e412/rnui-react";
import { PauseControl } from "./pause-control";

interface HeaderProps {
  onOpenCommandPalette: () => void;
}

// Deliberately thin: the header carries only what acts on the *current
// view* (rail toggle, command palette, the global blocking switch).
// Everything about the signed-in user — avatar, account menu, theme — lives
// in the sidebar footer instead (see sidebar-nav.tsx).
export function Header({ onOpenCommandPalette }: HeaderProps) {
  return (
    <header className="sticky top-0 z-20 flex h-14 shrink-0 items-center gap-3 border-b border-border bg-background/80 px-4 backdrop-blur-sm">
      <SidebarTrigger />

      <Button
        type="button"
        variant="outline"
        onClick={onOpenCommandPalette}
        className="h-8 w-64 justify-start gap-2 px-2.5 font-normal text-muted-foreground"
      >
        <Search className="size-4 shrink-0" />
        <span className="flex-1 text-left">Search commands…</span>
        <KbdGroup>
          <Kbd>⌘</Kbd>
          <Kbd>K</Kbd>
        </KbdGroup>
      </Button>

      <div className="ml-auto flex items-center gap-1.5">
        {/* Global pause (group 0) — see components/pause-control.tsx. The
            same component is reused per-group on the Filtering page's
            Groups & Clients tab (Task 10). */}
        <PauseControl />
      </div>
    </header>
  );
}
