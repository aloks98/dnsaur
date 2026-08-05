import { Navigate, Route, Routes } from "react-router";
import { Spinner, Toaster } from "@e412/rnui-react";
import { AppShell } from "./components/app-shell";
import { ApiUnreachableBanner } from "./components/api-unreachable-banner";
import { ErrorBoundary } from "./components/error-boundary";
import { useMe, useSetupState } from "./hooks/use-auth";
import { useTheme } from "./lib/theme";
import { Account } from "./pages/account";
import { Dashboard } from "./pages/dashboard";
import { LocalDns } from "./pages/dns";
import { Filtering } from "./pages/filtering";
import { Login } from "./pages/login";
import { QueryLog } from "./pages/queries";
import { SettingsPage } from "./pages/settings";
import { Setup } from "./pages/setup";

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
    <>
      {/* The outer net. AppShell has its own per-route boundary (so one
          broken page keeps the chrome usable), but that one covers only the
          Outlet: the always-mounted sidebar/header/command palette, and the
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
              <Route path="filtering" element={<Filtering />} />
              <Route path="dns" element={<LocalDns />} />
              <Route path="settings" element={<SettingsPage />} />
              <Route path="account" element={<Account />} />
              <Route path="*" element={<Navigate to="/" replace />} />
            </Route>
          </Routes>
        )}
      </ErrorBoundary>
      {/* One Toaster for the whole app — mounted here (not per-shell) so
          unauthenticated screens (Setup, Login) can toast too. */}
      <Toaster theme={theme} />
    </>
  );
}
