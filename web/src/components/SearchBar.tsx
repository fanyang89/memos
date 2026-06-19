import { SearchIcon, SparklesIcon, TypeIcon } from "lucide-react";
import { useState } from "react";
import { useMemoFilterContext } from "@/contexts/MemoFilterContext";
import { cn } from "@/lib/utils";
import { useTranslate } from "@/utils/i18n";
import MemoDisplaySettingMenu from "./MemoDisplaySettingMenu";
import SemanticSearchResults from "./SemanticSearchResults";

type SearchMode = "substring" | "semantic";

const SearchBar = () => {
  const t = useTranslate();
  const { addFilter } = useMemoFilterContext();
  const [queryText, setQueryText] = useState("");
  const [mode, setMode] = useState<SearchMode>("substring");
  const [semanticQuery, setSemanticQuery] = useState("");

  const onTextChange = (event: React.FormEvent<HTMLInputElement>) => {
    setQueryText(event.currentTarget.value);
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key !== "Enter") return;
    e.preventDefault();
    const trimmedText = queryText.trim();
    if (trimmedText === "") return;

    if (mode === "semantic") {
      setSemanticQuery(trimmedText);
      return;
    }
    const words = trimmedText.split(/\s+/);
    words.forEach((word) => {
      addFilter({
        factor: "contentSearch",
        value: word,
      });
    });
    setQueryText("");
  };

  const toggleMode = () => {
    setMode((prev) => (prev === "substring" ? "semantic" : "substring"));
    setSemanticQuery("");
  };

  return (
    <div className="relative w-full h-auto flex flex-col justify-start items-start gap-1">
      <div className="relative w-full flex flex-row justify-start items-center">
        <SearchIcon className="absolute left-2 w-4 h-auto opacity-40 text-sidebar-foreground" />
        <input
          className={cn(
            "w-full text-sidebar-foreground leading-6 bg-sidebar border border-border text-sm rounded-lg p-1 pl-8 outline-0",
            mode === "semantic" && "pr-16",
          )}
          placeholder={mode === "semantic" ? t("memo.semantic-search-placeholder") : t("memo.search-placeholder")}
          value={queryText}
          onChange={onTextChange}
          onKeyDown={onKeyDown}
        />
        <button
          type="button"
          onClick={toggleMode}
          aria-label={t("memo.toggle-semantic-search")}
          title={t(mode === "semantic" ? "memo.search-mode-semantic" : "memo.search-mode-substring")}
          className={cn(
            "absolute right-8 flex items-center justify-center w-6 h-6 rounded transition-colors",
            mode === "semantic" ? "text-primary" : "text-muted-foreground hover:text-foreground",
          )}
        >
          {mode === "semantic" ? <SparklesIcon className="w-4 h-auto" /> : <TypeIcon className="w-4 h-auto" />}
        </button>
        <MemoDisplaySettingMenu className="absolute right-2 top-2 text-sidebar-foreground" />
      </div>
      {mode === "semantic" && semanticQuery !== "" && (
        <div className="w-full rounded-md border border-border bg-sidebar">
          <SemanticSearchResults query={semanticQuery} onClose={() => setQueryText("")} />
        </div>
      )}
    </div>
  );
};

export default SearchBar;
