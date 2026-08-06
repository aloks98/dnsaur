import { QueryClientProvider } from "@tanstack/react-query";
import { render, type RenderResult } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import type { ReactElement } from "react";
import { makeQueryClient } from "../lib/query-client";

export function renderWithProviders(ui: ReactElement, { route = "/" } = {}): RenderResult {
  const client = makeQueryClient();
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[route]}>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}
