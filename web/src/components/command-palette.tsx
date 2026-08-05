import { useEffect } from "react";
import { useNavigate } from "react-router";
import {
  Command,
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@e412/rnui-react";
import { navItems } from "./sidebar-nav";

interface CommandPaletteProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function CommandPalette({ open, onOpenChange }: CommandPaletteProps) {
  const navigate = useNavigate();

  useEffect(() => {
    function onKeyDown(event: KeyboardEvent) {
      if (event.key.toLowerCase() === "k" && (event.metaKey || event.ctrlKey)) {
        event.preventDefault();
        onOpenChange(!open);
      }
    }
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [open, onOpenChange]);

  function goTo(to: string) {
    navigate(to);
    onOpenChange(false);
  }

  return (
    <CommandDialog
      open={open}
      onOpenChange={onOpenChange}
      title="Command palette"
      description="Jump to a page in dnsaur"
    >
      {/* rnui's CommandDialog is only the Dialog shell — unlike shadcn's, it
          does NOT wrap its children in the cmdk root. Every Command* part
          below reads cmdk's store off a context that defaults to undefined,
          so without this <Command> the palette throws "Cannot read properties
          of undefined (reading 'subscribe')" the moment it opens. */}
      <Command>
        <CommandInput placeholder="Jump to a page…" />
        <CommandList>
          <CommandEmpty>No matching pages.</CommandEmpty>
          <CommandGroup heading="Pages">
            {navItems.map((item) => {
              const Icon = item.icon;
              return (
                <CommandItem key={item.to} value={item.label} onSelect={() => goTo(item.to)}>
                  <Icon />
                  {item.label}
                </CommandItem>
              );
            })}
          </CommandGroup>
        </CommandList>
      </Command>
    </CommandDialog>
  );
}
