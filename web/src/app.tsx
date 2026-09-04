import { Navigate, Route, Routes } from "react-router";
import { Spinner, Toaster, TooltipProvider } from "@e412/rnui-react";
import { AppShell } from "./components/app-shell";
import { ApiUnreachableBanner } from "./components/api-unreachable-banner";
import { ErrorBoundary } from "./components/error-boundary";
import { useMe, useSetupState } from "./hooks/use-auth";
import { useTheme } from "./lib/theme";
import { TOOLTIP_DELAY_MS } from "./lib/tooltip";
import { Account } from "./pages/account";
import { Dashboard } from "./pages/dashboard";
import { FilteringLayout } from "./pages/filtering";
import { GroupsClientsTab } from "./pages/filtering/groups-clients";
import { ListsTab } from "./pages/filtering/lists";
import { RulesTab } from "./pages/filtering/rules";
import { Login } from "./pages/login";
import { NotFound } from "./pages/not-found";
import { QueryLog } from "./pages/queries";
import { SettingsPage } from "./pages/settings";
import { Setup } from "./pages/setup";
import { TSIGKeys } from "./pages/tsig-keys";
import { ZoneDetail } from "./pages/zones/detail";
import { ZonesList } from "./pages/zones/list";

function FullPageSpinner() {
  return (
    <div className="flex min-h-screen items-center justify-center bg-background">
      <Spinner className="size-8 text-muted-foreground" />
    </div>
  );
}

/**
 * Rendered once useMe() has failed (no active session). Distinguishes a
 * real "please sign in" state from "the API is unreachable" by consulting
 * useSetupState() — both auth/me and setup fail together when the backend
 * itself can't be reached at all.
 */
function UnauthenticatedGate() {
  const setupState = useSetupState();

  if (setupState.isPending) {
    return <FullPageSpinner />;
  }

  if (setupState.isError) {
    return (
      <main className="flex min-h-screen items-center justify-center bg-background p-6">
        <div className="w-full max-w-md">
          <ApiUnreachableBanner onRetry={() => setupState.refetch()} />
        </div>
      </main>
    );
  }

  return setupState.data.setup_required ? <Setup /> : <Login />;
}

export function App() {
  const me = useMe();
  const { theme } = useTheme();

  return (
    // Wraps the tree rather than sitting beside the Toaster below, which is
    // the one thing about this provider that is not a formality: it hands
    // its delay down through context, so one mounted as a sibling of the
    // routes would be inert and every tooltip in the app would quietly keep
    // Base UI's own default.
    <TooltipProvider delay={TOOLTIP_DELAY_MS}>
      {/* The outer net. AppShell has its own per-route boundary (so one
          broken page keeps the chrome usable), but that one covers only the
          Outlet: the always-mounted top nav and command palette, and the
          entire unauthenticated branch below, sit outside it. React 19
          unmounts the whole root on an uncaught render error, so without
          this any throw in those places is a white page with no way back. */}
      <ErrorBoundary>
        {me.isPending ? (
          <FullPageSpinner />
        ) : me.isError ? (
          <UnauthenticatedGate />
        ) : (
          <Routes>
            <Route element={<AppShell />}>
              <Route index element={<Dashboard />} />
              <Route path="queries" element={<QueryLog />} />
              {/* Filtering's three panels are routes, not local tab state,
                  so each is deep-linkable and survives a reload. `/filtering`
                  itself is only an entry point — it forwards to the first
                  panel rather than rendering a fourth, empty thing. */}
              <Route path="filtering" element={<FilteringLayout />}>
                <Route index element={<Navigate to="/filtering/lists" replace />} />
                <Route path="lists" element={<ListsTab />} />
                <Route path="rules" element={<RulesTab />} />
                <Route path="clients" element={<GroupsClientsTab />} />
              </Route>
              <Route path="zones">
                <Route index element={<ZonesList />} />
                <Route path=":id" element={<ZoneDetail />} />
              </Route>
              <Route path="settings" element={<SettingsPage />} />
              {/* System's second tab (see lib/nav.ts): the keys that
                  authenticate zone transfers, not a Settings section. */}
              <Route path="tsig-keys" element={<TSIGKeys />} />
              <Route path="account" element={<Account />} />
              {/* A real screen, not a redirect. Silently rewriting an
                  unknown URL to `/` meant a broken link and a working one
                  looked identical — the page you asked for never existed and
                  nothing said so. */}
              <Route path="*" element={<NotFound />} />
            </Route>
          </Routes>
        )}
      </ErrorBoundary>
      {/* One Toaster for the whole app — mounted here (not per-shell) so
          unauthenticated screens (Setup, Login) can toast too. */}
      <Toaster theme={theme} />
    </TooltipProvider>
  );
}
