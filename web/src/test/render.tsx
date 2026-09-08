import { QueryClientProvider, type QueryClient } from "@tanstack/react-query";
import { render, type RenderResult } from "@testing-library/react";
import { MemoryRouter } from "react-router";
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
  return Object.assign(
    render(
      <QueryClientProvider client={client}>
        {/* The same provider app.tsx mounts, with the same delay: a tooltip
          test that waited out Base UI's own default instead would be timing
          something the app never does. */}
        <TooltipProvider delay={TOOLTIP_DELAY_MS}>
          <MemoryRouter initialEntries={[route]}>{ui}</MemoryRouter>
        </TooltipProvider>
      </QueryClientProvider>,
    ),
    { queryClient: client },
  );
}
