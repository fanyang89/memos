import { FileAudioIcon, FileStackIcon, ImageIcon, SearchIcon, SparklesIcon } from "lucide-react";
import type { ComponentType } from "react";
import { Button } from "@/components/ui/button";
import type { AttachmentLibraryStats, AttachmentLibraryTab } from "@/hooks/useAttachmentLibrary";
import { cn } from "@/lib/utils";
import { useTranslate } from "@/utils/i18n";

interface AttachmentLibraryToolbarProps {
  activeTab: AttachmentLibraryTab;
  onTabChange: (tab: AttachmentLibraryTab) => void;
  searchQuery: string;
  onSearchQueryChange: (query: string) => void;
  stats: AttachmentLibraryStats;
}

const TAB_CONFIG: Array<{
  key: AttachmentLibraryTab;
  labelKey: "media" | "documents" | "audio";
  icon: ComponentType<{ className?: string }>;
  count: (stats: AttachmentLibraryStats) => number;
}> = [
  { key: "media", labelKey: "media", icon: ImageIcon, count: (stats) => stats.media },
  { key: "audio", labelKey: "audio", icon: FileAudioIcon, count: (stats) => stats.audio },
  { key: "documents", labelKey: "documents", icon: FileStackIcon, count: (stats) => stats.documents },
];

const AttachmentLibraryToolbar = ({ activeTab, onTabChange, searchQuery, onSearchQueryChange, stats }: AttachmentLibraryToolbarProps) => {
  const t = useTranslate();

  return (
    <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
      <div className="-mx-1 overflow-x-auto px-1 [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
        <div className="flex min-w-max items-center gap-1.5">
          {TAB_CONFIG.map((tab) => {
            const Icon = tab.icon;
            const isActive = activeTab === tab.key;

            return (
              <Button
                key={tab.key}
                type="button"
                variant="ghost"
                className={cn(
                  "h-9 rounded-md px-2.5 text-sm font-medium sm:px-3",
                  isActive ? "bg-muted/60 text-foreground shadow-none" : "text-muted-foreground hover:bg-muted/40 hover:text-foreground",
                )}
                onClick={() => onTabChange(tab.key)}
              >
                <Icon className="h-4 w-4" />
                <span>{t(`attachment-library.tabs.${tab.labelKey}`)}</span>
                <span
                  className={cn(
                    "rounded-full px-1.5 py-0.5 text-[11px]",
                    isActive ? "bg-background text-muted-foreground" : "bg-muted/50 text-muted-foreground",
                  )}
                >
                  {tab.count(stats)}
                </span>
              </Button>
            );
          })}
        </div>
      </div>

      <label className="relative flex h-9 w-full items-center sm:max-w-xs">
        <SearchIcon className="absolute left-2.5 h-4 w-4 text-muted-foreground" />
        <input
          value={searchQuery}
          onChange={(event) => onSearchQueryChange(event.currentTarget.value)}
          disabled={activeTab !== "media"}
          placeholder={t("attachment-library.search.placeholder")}
          className="h-full w-full rounded-md border border-border bg-background pl-8 pr-8 text-sm text-foreground outline-none transition-colors placeholder:text-muted-foreground disabled:cursor-not-allowed disabled:opacity-50"
        />
        <SparklesIcon className="absolute right-2.5 h-4 w-4 text-muted-foreground" />
      </label>
    </div>
  );
};

export default AttachmentLibraryToolbar;
