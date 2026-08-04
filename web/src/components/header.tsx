import { LogOut, Pause, Search } from "lucide-react";
import { toast } from "sonner";
import {
  Avatar,
  AvatarFallback,
  Button,
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
  Kbd,
  KbdGroup,
  Separator,
  SidebarTrigger,
} from "@e412/rnui-react";
import { useLogout, useMe } from "../hooks/use-auth";
import { ThemeToggle } from "./theme-toggle";

interface HeaderProps {
  onOpenCommandPalette: () => void;
}

function initialsFromUsername(username: string): string {
  return username.slice(0, 2).toUpperCase();
}

export function Header({ onOpenCommandPalette }: HeaderProps) {
  const me = useMe();
  const logout = useLogout();

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
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled
          title="Pause DNS resolution — coming soon"
        >
          <Pause />
          Pause
        </Button>

        <ThemeToggle />

        <Separator orientation="vertical" className="mx-1 h-6" />

        <DropdownMenu>
          <DropdownMenuTrigger
            aria-label="Account menu"
            className="flex items-center rounded-full outline-none transition-opacity hover:opacity-80 focus-visible:ring-3 focus-visible:ring-ring/50"
          >
            <Avatar size="sm">
              <AvatarFallback>
                {me.data ? initialsFromUsername(me.data.username) : "?"}
              </AvatarFallback>
            </Avatar>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuLabel>{me.data?.username ?? "Account"}</DropdownMenuLabel>
            <DropdownMenuSeparator />
            <DropdownMenuItem
              variant="destructive"
              onClick={() =>
                logout.mutate(undefined, {
                  onError: () => toast.error("Couldn't sign out — try again"),
                })
              }
            >
              <LogOut />
              Log out
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </header>
  );
}
