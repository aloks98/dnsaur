import { QueryClient } from "@tanstack/react-query";

export function makeQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        retry: 1,
        staleTime: 10_000,
        refetchOnWindowFocus: false,
      },
      mutations: { retry: false },
    },
  });
}
