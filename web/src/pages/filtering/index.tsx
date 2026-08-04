import { Separator, Tabs, TabsContent, TabsList, TabsTrigger } from "@e412/rnui-react";
import { GroupsClientsTab } from "./groups-clients";
import { ListsTab } from "./lists";
import { RulesTab } from "./rules";

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
          <RulesTab />
        </TabsContent>
        <TabsContent value="groups" className="pt-4">
          <GroupsClientsTab />
        </TabsContent>
      </Tabs>
    </div>
  );
}
