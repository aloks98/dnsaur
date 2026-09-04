import { QueryClientProvider } from "@tanstack/react-query";
import { render, type RenderResult } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import type { ReactElement } from "react";
import { TooltipProvider } from "@e412/rnui-react";
import { makeQueryClient } from "../lib/query-client";
import { TOOLTIP_DELAY_MS } from "../lib/tooltip";

export function renderWithProviders(ui: ReactElement, { route = "/" } = {}): RenderResult {
  const client = makeQueryClient();
  return render(
    <QueryClientProvider client={client}>
      {/* The same provider app.tsx mounts, with the same delay: a tooltip
          test that waited out Base UI's own default instead would be timing
          something the app never does. */}
      <TooltipProvider delay={TOOLTIP_DELAY_MS}>
        <MemoryRouter initialEntries={[route]}>{ui}</MemoryRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}
