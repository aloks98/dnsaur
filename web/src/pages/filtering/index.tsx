import { Separator, Tabs, TabsContent, TabsList, TabsTrigger } from "@e412/rnui-react";
import { ListsTab } from "./lists";

// Placeholder — Task 10 replaces these two panels with real per-domain/regex
// rule management and group/client assignment, reusing this same tab shell.
// Copy matches the app's other not-yet-built pages (pages/dns.tsx,
// pages/settings.tsx, pages/account.tsx): a plain, present-tense "X land
// here", no "coming soon" filler.
function RulesPlaceholder() {
  return (
    <p className="text-sm text-muted-foreground">
      Per-domain and regex allow/block rules land here.
    </p>
  );
}

function GroupsPlaceholder() {
  return (
    <p className="text-sm text-muted-foreground">
      Groups, clients, and per-group blocking controls land here.
    </p>
  );
}

export function Filtering() {
  return (
    <div className="flex flex-col gap-6">
      {/* Page chrome, set off from the tabbed content below with a hairline
          rule — the same "distinct structural zone" rhythm every other page
          in the app uses (see dashboard.tsx, pages/queries.tsx). */}
      <div className="flex flex-col gap-3">
        <div>
          <h1 className="text-2xl font-heading font-semibold text-foreground">Filtering</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Manage blocklists, allowlists, custom rules, and per-group policy.
          </p>
        </div>
      </div>

      <Separator />

      {/* "line" (an underlined nav-style bar), not the default filled-pill
          look — this switches between whole page sections, not a small
          in-card toggle, so it reads at the same visual weight as a page's
          own top-level navigation rather than a compact control. */}
      <Tabs defaultValue="lists">
        <TabsList variant="line">
          <TabsTrigger value="lists">Lists</TabsTrigger>
          <TabsTrigger value="rules">Rules</TabsTrigger>
          <TabsTrigger value="groups">Groups &amp; Clients</TabsTrigger>
        </TabsList>
        <TabsContent value="lists" className="pt-4">
          <ListsTab />
        </TabsContent>
        <TabsContent value="rules" className="pt-4">
          <RulesPlaceholder />
        </TabsContent>
        <TabsContent value="groups" className="pt-4">
          <GroupsPlaceholder />
        </TabsContent>
      </Tabs>
    </div>
  );
}
