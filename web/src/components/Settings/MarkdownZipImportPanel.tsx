import { useState } from "react";
import { toast } from "react-hot-toast";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { getRequestToken } from "@/connect";
import { useTranslate } from "@/utils/i18n";
import { SettingList, SettingListItem, SettingPanel } from "./SettingList";

type ImportMode = "skip" | "overwrite";
type ImportVisibility = "PRIVATE" | "PROTECTED" | "PUBLIC";

interface ImportIssue {
  file: string;
  target?: string;
  message: string;
}

interface ImportItem {
  file: string;
  memo_id: string;
  action: string;
  error?: string;
}

interface ImportReport {
  created: number;
  updated: number;
  skipped: number;
  failed: number;
  attachments: number;
  missing_assets: ImportIssue[];
  missing_links: ImportIssue[];
  failures: ImportIssue[];
  items: ImportItem[];
}

const MarkdownZipImportPanel = () => {
  const t = useTranslate();
  const [file, setFile] = useState<File>();
  const [visibility, setVisibility] = useState<ImportVisibility>("PRIVATE");
  const [mode, setMode] = useState<ImportMode>("skip");
  const [dryRun, setDryRun] = useState(false);
  const [loading, setLoading] = useState(false);
  const [report, setReport] = useState<ImportReport>();

  const handleImport = async () => {
    if (!file) {
      toast.error(t("setting.memo.import-markdown-zip-file-required"));
      return;
    }

    const formData = new FormData();
    formData.set("file", file);
    formData.set("visibility", visibility);
    formData.set("mode", mode);
    formData.set("dry_run", dryRun ? "true" : "false");

    setLoading(true);
    try {
      const token = await getRequestToken();
      const response = await fetch("/api/v1/import/markdown/zip", {
        method: "POST",
        credentials: "include",
        headers: token ? { Authorization: `Bearer ${token}` } : undefined,
        body: formData,
      });
      if (!response.ok) {
        const message = await response.text();
        throw new Error(message || response.statusText);
      }
      const nextReport = (await response.json()) as ImportReport;
      setReport(nextReport);
      toast.success(dryRun ? t("setting.memo.import-markdown-zip-dry-run-complete") : t("setting.memo.import-markdown-zip-complete"));
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t("setting.memo.import-markdown-zip-failed"));
    } finally {
      setLoading(false);
    }
  };

  const issues = report ? [...report.failures, ...report.missing_assets, ...report.missing_links].slice(0, 8) : [];

  return (
    <SettingPanel
      header={
        <>
          <div className="text-sm font-medium text-foreground">{t("setting.memo.import-markdown-zip-title")}</div>
          <p className="mt-1 text-xs leading-5 text-muted-foreground">{t("setting.memo.import-markdown-zip-description")}</p>
        </>
      }
      footer={
        <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
          <span className="text-xs text-muted-foreground">{t("setting.memo.import-markdown-zip-note")}</span>
          <Button onClick={handleImport} disabled={!file || loading}>
            {loading
              ? t("setting.memo.import-markdown-zip-importing")
              : dryRun
                ? t("setting.memo.import-markdown-zip-preview")
                : t("setting.memo.import-markdown-zip-submit")}
          </Button>
        </div>
      }
    >
      <SettingList className="rounded-none border-0">
        <SettingListItem
          label={t("setting.memo.import-markdown-zip-file")}
          description={file?.name || t("setting.memo.import-markdown-zip-file-description")}
        >
          <Input className="max-w-72" type="file" accept=".zip,application/zip" onChange={(event) => setFile(event.target.files?.[0])} />
        </SettingListItem>
        <SettingListItem label={t("setting.memo.import-markdown-zip-visibility")}>
          <Select value={visibility} onValueChange={(value) => setVisibility(value as ImportVisibility)}>
            <SelectTrigger className="w-36">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="PRIVATE">PRIVATE</SelectItem>
              <SelectItem value="PROTECTED">PROTECTED</SelectItem>
              <SelectItem value="PUBLIC">PUBLIC</SelectItem>
            </SelectContent>
          </Select>
        </SettingListItem>
        <SettingListItem
          label={t("setting.memo.import-markdown-zip-mode")}
          description={t("setting.memo.import-markdown-zip-mode-description")}
        >
          <Select value={mode} onValueChange={(value) => setMode(value as ImportMode)}>
            <SelectTrigger className="w-36">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="skip">skip</SelectItem>
              <SelectItem value="overwrite">overwrite</SelectItem>
            </SelectContent>
          </Select>
        </SettingListItem>
        <SettingListItem
          label={t("setting.memo.import-markdown-zip-dry-run")}
          description={t("setting.memo.import-markdown-zip-dry-run-description")}
        >
          <Switch checked={dryRun} onCheckedChange={setDryRun} />
        </SettingListItem>
      </SettingList>

      {report && (
        <div className="border-t border-border px-3 py-3">
          <div className="grid grid-cols-2 gap-2 text-xs sm:grid-cols-5">
            <ReportStat label={t("setting.memo.import-markdown-zip-created")} value={report.created} />
            <ReportStat label={t("setting.memo.import-markdown-zip-updated")} value={report.updated} />
            <ReportStat label={t("setting.memo.import-markdown-zip-skipped")} value={report.skipped} />
            <ReportStat label={t("setting.memo.import-markdown-zip-failed-count")} value={report.failed} />
            <ReportStat label={t("setting.memo.import-markdown-zip-attachments")} value={report.attachments} />
          </div>
          {issues.length > 0 && (
            <div className="mt-3 rounded-md border border-border bg-muted/20 px-3 py-2">
              <div className="text-xs font-medium text-foreground">{t("setting.memo.import-markdown-zip-issues")}</div>
              <div className="mt-2 flex flex-col gap-1 text-xs text-muted-foreground">
                {issues.map((issue, index) => (
                  <div key={`${issue.file}-${issue.target}-${index}`} className="break-all">
                    {issue.file}
                    {issue.target ? ` -> ${issue.target}` : ""}: {issue.message}
                  </div>
                ))}
              </div>
            </div>
          )}
        </div>
      )}
    </SettingPanel>
  );
};

const ReportStat = ({ label, value }: { label: string; value: number }) => (
  <div className="rounded-md border border-border bg-muted/20 px-2 py-2">
    <div className="font-mono text-base text-foreground">{value}</div>
    <div className="text-muted-foreground">{label}</div>
  </div>
);

export default MarkdownZipImportPanel;
