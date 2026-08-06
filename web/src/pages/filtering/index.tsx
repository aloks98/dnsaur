import { Outlet } from "react-router";
import { Separator } from "@e412/rnui-react";

/**
 * Filtering is three real routes (`/filtering/lists`, `/filtering/rules`,
 * `/filtering/clients`), not a `<Tabs>` with local state — the shell's
 * second row is what switches between them now, and a URL-less tab set could
 * neither be deep-linked, bookmarked, opened in a new tab, nor survive a
 * reload without snapping back to Lists.
 *
 * This layout keeps only what all three share: the page's own heading and
 * the hairline that sets it off from the panel below.
 */
export function FilteringLayout() {
  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-heading font-semibold text-foreground">Filtering</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          Manage blocklists, allowlists, custom rules, and per-group policy.
        </p>
      </div>

      <Separator />

      <Outlet />
    </div>
  );
}
