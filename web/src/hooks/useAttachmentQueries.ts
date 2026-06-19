import { create } from "@bufbuild/protobuf";
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { attachmentServiceClient } from "@/connect";
import {
  type Attachment,
  BatchDeleteAttachmentsRequestSchema,
  type ListAttachmentsRequest,
  ListAttachmentsRequestSchema,
  type SearchAttachmentsRequest,
  SearchAttachmentsRequestSchema,
} from "@/types/proto/api/v1/attachment_service_pb";

// Query keys factory
export const attachmentKeys = {
  all: ["attachments"] as const,
  lists: () => [...attachmentKeys.all, "list"] as const,
  list: (filters?: Partial<ListAttachmentsRequest>) => [...attachmentKeys.lists(), filters] as const,
  searches: () => [...attachmentKeys.all, "search"] as const,
  search: (request: Partial<SearchAttachmentsRequest>) => [...attachmentKeys.searches(), request] as const,
  details: () => [...attachmentKeys.all, "detail"] as const,
  detail: (name: string) => [...attachmentKeys.details(), name] as const,
};

// Hook to fetch attachments
export function useAttachments() {
  return useQuery({
    queryKey: attachmentKeys.lists(),
    queryFn: async () => {
      const { attachments } = await attachmentServiceClient.listAttachments(create(ListAttachmentsRequestSchema, {}));
      return attachments;
    },
  });
}

export function useInfiniteAttachments(request: Partial<ListAttachmentsRequest> = {}, options?: { enabled?: boolean }) {
  return useInfiniteQuery({
    queryKey: attachmentKeys.list(request),
    queryFn: async ({ pageParam }) => {
      const response = await attachmentServiceClient.listAttachments(
        create(ListAttachmentsRequestSchema, {
          ...request,
          pageToken: pageParam || "",
        } as Record<string, unknown>),
      );
      return response;
    },
    initialPageParam: "",
    getNextPageParam: (lastPage) => lastPage.nextPageToken || undefined,
    staleTime: 1000 * 60,
    gcTime: 1000 * 60 * 5,
    enabled: options?.enabled ?? true,
  });
}

export function useSearchAttachments(request: Partial<SearchAttachmentsRequest> = {}, options?: { enabled?: boolean }) {
  const query = request.query?.trim() ?? "";

  return useQuery({
    queryKey: attachmentKeys.search({ ...request, query }),
    enabled: (options?.enabled ?? true) && query !== "",
    queryFn: async () => {
      return attachmentServiceClient.searchAttachments(
        create(SearchAttachmentsRequestSchema, {
          ...request,
          query,
        } as Record<string, unknown>),
      );
    },
    retry: false,
    staleTime: 1000 * 60,
  });
}

// Hook to create/upload attachment
export function useCreateAttachment() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (attachment: Attachment) => {
      const result = await attachmentServiceClient.createAttachment({ attachment });
      return result;
    },
    onSuccess: () => {
      // Invalidate attachments list
      queryClient.invalidateQueries({ queryKey: attachmentKeys.lists() });
    },
  });
}

// Hook to delete attachment
export function useDeleteAttachment() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (name: string) => {
      await attachmentServiceClient.deleteAttachment({ name });
      return name;
    },
    onSuccess: (name) => {
      // Remove from cache
      queryClient.removeQueries({ queryKey: attachmentKeys.detail(name) });
      // Invalidate lists
      queryClient.invalidateQueries({ queryKey: attachmentKeys.lists() });
    },
  });
}

export function useBatchDeleteAttachments() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (names: string[]) => {
      await attachmentServiceClient.batchDeleteAttachments(create(BatchDeleteAttachmentsRequestSchema, { names }));
      return names;
    },
    onSuccess: (names) => {
      for (const name of names) {
        queryClient.removeQueries({ queryKey: attachmentKeys.detail(name) });
      }
      queryClient.invalidateQueries({ queryKey: attachmentKeys.lists() });
    },
  });
}
