import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router";

import { App } from "./app";
import { ErrorBoundary } from "./components/error-boundary";
import { makeQueryClient } from "./lib/query-client";
import "./styles/app.css";

const queryClient = makeQueryClient();

// App mounts its own boundary around both auth branches; this one exists
// solely for what happens *above* it — a throw in App's own render body, or
// in the providers — which no boundary inside App can catch, and which React
// 19 would otherwise answer by unmounting the root into a blank page.
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <ErrorBoundary>
          <App />
        </ErrorBoundary>
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
