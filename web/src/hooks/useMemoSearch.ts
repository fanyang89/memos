import { create } from "@bufbuild/protobuf";
import { useQuery } from "@tanstack/react-query";
import { memoServiceClient } from "@/connect";
import type { SearchMemosResponse } from "@/types/proto/api/v1/memo_service_pb";
import { SearchMemosRequestSchema } from "@/types/proto/api/v1/memo_service_pb";

export const memoSearchKeys = {
  all: ["memos", "search"] as const,
  query: (query: string, topK: number) => [...memoSearchKeys.all, query, topK] as const,
};

export interface UseMemoSearchOptions {
  topK?: number;
  enabled?: boolean;
}

/**
 * useSearchMemos runs the semantic-search RPC for the given query.
 *
 * Disabled by default (the toggle in SearchBar controls `enabled`). When the
 * instance has not configured an embedding provider the RPC returns
 * FailedPrecondition, which is surfaced via `error` so callers can show a hint.
 */
export const useSearchMemos = (query: string, options: UseMemoSearchOptions = {}) => {
  const { topK = 20, enabled = true } = options;
  const trimmed = query.trim();
  return useQuery<SearchMemosResponse>({
    queryKey: memoSearchKeys.query(trimmed, topK),
    enabled: enabled && trimmed !== "",
    queryFn: async () => {
      const request = create(SearchMemosRequestSchema, { query: trimmed, topK });
      return memoServiceClient.searchMemos(request);
    },
    retry: false,
  });
};
