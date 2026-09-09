import { QueryClientProvider, type QueryClient } from "@tanstack/react-query";
import { render, type RenderResult } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import type { ReactElement } from "react";
import { TooltipProvider } from "@e412/rnui-react";
import { makeQueryClient } from "../lib/query-client";
import { TOOLTIP_DELAY_MS } from "../lib/tooltip";

/**
 * The QueryClient is returned alongside the usual RenderResult, purely
 * additively, so a test asserting that something is *absent* can first wait
 * for the query that would have produced it to actually settle. A bare
 * `setTimeout` before a negative assertion passes on a slow machine for the
 * wrong reason: the response simply had not landed yet.
 */
export function renderWithProviders(
  ui: ReactElement,
  { route = "/" } = {},
): RenderResult & { queryClient: QueryClient } {
  const client = makeQueryClient();
  // A data router with one catch-all route, exactly as main.tsx mounts one:
  // descendant <Routes> still resolve, and useBlocker — which only a data
  // router provides — works here the same way it works in the app.
  const router = createMemoryRouter([{ path: "*", element: ui }], { initialEntries: [route] });
  return Object.assign(
    render(
      <QueryClientProvider client={client}>
        {/* The same provider app.tsx mounts, with the same delay: a tooltip
          test that waited out Base UI's own default instead would be timing
          something the app never does. */}
        <TooltipProvider delay={TOOLTIP_DELAY_MS}>
          <RouterProvider router={router} />
        </TooltipProvider>
      </QueryClientProvider>,
    ),
    { queryClient: client },
  );
}
