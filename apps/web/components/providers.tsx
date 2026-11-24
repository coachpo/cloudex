"use client";

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";
import { APIError } from "@/lib/api";

export function Providers({ children }: { children: ReactNode }) {
  const [client] = useState(() => new QueryClient({ defaultOptions: { queries: {
    staleTime: 1500, retry: (count, error) => count < 1 && (!(error instanceof APIError) || error.status >= 500),
  } } }));
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}
