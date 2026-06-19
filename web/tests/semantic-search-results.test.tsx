import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

vi.mock("@/hooks/useMemoSearch", () => ({
  useSearchMemos: vi.fn(),
}));

vi.mock("@/hooks/useMemoQueries", () => ({
  useMemo: () => ({ data: { name: "memos/x", content: "hello world content here" } }),
}));

vi.mock("@/utils/i18n", () => ({
  useTranslate: () => (key: string) => key,
}));

vi.mock("react-router-dom", () => ({
  Link: ({ children, to }: { children: React.ReactNode; to: string }) => (
    <a data-testid="memo-link" href={to}>
      {children}
    </a>
  ),
}));

import SemanticSearchResults from "@/components/SemanticSearchResults";
import { useSearchMemos } from "@/hooks/useMemoSearch";

const mockedUseSearchMemos = vi.mocked(useSearchMemos);

const renderResults = () => {
  const queryClient = new QueryClient();
  render(
    <QueryClientProvider client={queryClient}>
      <SemanticSearchResults query="anything" />
    </QueryClientProvider>,
  );
};

describe("<SemanticSearchResults>", () => {
  it("shows the unavailable hint when semantic search is not configured (FailedPrecondition)", () => {
    mockedUseSearchMemos.mockReturnValue({
      data: undefined,
      isLoading: false,
      error: new Error("FailedPrecondition"),
    } as never);
    renderResults();
    expect(screen.getByText("memo.semantic-search-unavailable")).toBeInTheDocument();
  });

  it("renders the no-results message when the search returns empty", () => {
    mockedUseSearchMemos.mockReturnValue({
      data: { results: [] },
      isLoading: false,
      error: null,
    } as never);
    renderResults();
    expect(screen.getByText("memo.no-results")).toBeInTheDocument();
  });

  it("renders matched memos with a similarity line", () => {
    mockedUseSearchMemos.mockReturnValue({
      data: { results: [{ memo: "memos/x", similarity: 0.82 }] },
      isLoading: false,
      error: null,
    } as never);
    renderResults();
    expect(screen.getByText("hello world content here")).toBeInTheDocument();
    expect(screen.getByText("memo.semantic-similarity")).toBeInTheDocument();
  });
});
