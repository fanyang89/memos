import { SparklesIcon } from "lucide-react";
import { Link } from "react-router-dom";
import { useMemo } from "@/hooks/useMemoQueries";
import { useSearchMemos } from "@/hooks/useMemoSearch";
import { cn } from "@/lib/utils";
import { useTranslate } from "@/utils/i18n";

interface SemanticSearchResultsProps {
  query: string;
  onClose?: () => void;
}

/**
 * SemanticSearchResults renders the hits for a semantic-search query inline.
 * Each row fetches its memo lazily via the cached detail query.
 */
const SemanticSearchResults = ({ query, onClose }: SemanticSearchResultsProps) => {
  const t = useTranslate();
  const { data, isLoading, error } = useSearchMemos(query);

  const results = data?.results ?? [];

  if (isLoading) {
    return <p className="px-2 py-3 text-xs text-muted-foreground">{t("common.searching")}</p>;
  }
  if (error) {
    return <p className="px-2 py-3 text-xs text-muted-foreground">{t("memo.semantic-search-unavailable")}</p>;
  }
  if (results.length === 0) {
    return <p className="px-2 py-3 text-xs text-muted-foreground">{t("memo.no-results")}</p>;
  }

  return (
    <ul className="flex flex-col gap-1 py-1">
      {results.map((result) => (
        <SemanticSearchResultRow key={result.memo} name={result.memo} similarity={result.similarity} onClose={onClose} />
      ))}
    </ul>
  );
};

interface RowProps {
  name: string;
  similarity: number;
  onClose?: () => void;
}

const SemanticSearchResultRow = ({ name, similarity, onClose }: RowProps) => {
  const t = useTranslate();
  const { data: memo } = useMemo(name);
  const snippet = memo?.content?.slice(0, 80) ?? "";
  const pct = Math.round((similarity > 1 ? 1 : similarity) * 100);

  return (
    <li>
      <Link
        to={`/${name}`}
        onClick={onClose}
        className={cn("flex flex-col gap-0.5 rounded-md px-2 py-1.5 text-sm", "hover:bg-muted transition-colors")}
      >
        <span className="line-clamp-1 text-foreground">{snippet || name}</span>
        <span className="flex items-center gap-1 text-xs text-muted-foreground">
          <SparklesIcon className="w-3 h-auto" />
          {t("memo.semantic-similarity", { percent: pct })}
        </span>
      </Link>
    </li>
  );
};

export default SemanticSearchResults;
