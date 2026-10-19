import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Zap } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  cancelRegistrarJob,
  createRegistrarJob,
  getRegistrarJob,
  getRegistrarStatus,
  isRegistrarJobActive,
  registrarJobQueryKey,
  registrarStatusQueryKey,
  type RegistrarJobStatus,
  type RegistrarStatus,
} from "@/features/accounts/registrar-api";
import { cn } from "@/shared/lib/cn";

const REGISTRAR_JOB_POLL_INTERVAL_MS = 2_000;
const REGISTRAR_STATUS_STALE_MS = 30_000;

/** 账号页工具栏入口：按钮 + 自动补号进度弹窗。 */
export function RegistrarAction() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const [jobId, setJobId] = useState<string | null>(null);
  const [count, setCount] = useState("1");
  const [domain, setDomain] = useState("");
  const refreshedJobRef = useRef<string | null>(null);

  const statusQuery = useQuery({
    queryKey: registrarStatusQueryKey,
    queryFn: getRegistrarStatus,
    staleTime: REGISTRAR_STATUS_STALE_MS,
    retry: 1,
  });
  const status = statusQuery.data;
  const maxCount = Math.max(1, status?.maxCount ?? 1);
  const domains = status?.domains ?? [];
  const available = status !== undefined && status.available && status.enabled;

  // 只在弹窗打开且已有任务时轮询；终态由 refetchInterval 返回 false 自动停止，卸载时 react-query 会清掉定时器。
  const jobQuery = useQuery({
    queryKey: registrarJobQueryKey(jobId),
    queryFn: () => getRegistrarJob(jobId ?? ""),
    enabled: open && jobId !== null,
    retry: 2,
    refetchInterval: (query) => {
      const current = query.state.data;
      return current && isRegistrarJobActive(current.status) ? REGISTRAR_JOB_POLL_INTERVAL_MS : false;
    },
  });
  const job = jobId === null ? null : jobQuery.data ?? null;
  const jobRunning = job !== null && isRegistrarJobActive(job.status);

  useEffect(() => {
    if (!job || isRegistrarJobActive(job.status)) return;
    if (refreshedJobRef.current === job.jobId) return;
    refreshedJobRef.current = job.jobId;
    if (job.imported <= 0) return;
    void queryClient.invalidateQueries({ queryKey: ["accounts"] });
    void queryClient.invalidateQueries({ queryKey: ["accounts", "summary"] });
  }, [job, queryClient]);

  const createMutation = useMutation({
    mutationFn: (input: { count: number; domain: string }) => createRegistrarJob(input.domain ? { count: input.count, domains: [input.domain] } : { count: input.count }),
    onSuccess: (next) => {
      queryClient.setQueryData(registrarJobQueryKey(next.jobId), next);
      setJobId(next.jobId);
      toast.success(t("accounts.registrar.started"));
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("accounts.registrar.startFailed")),
  });
  const cancelMutation = useMutation({
    mutationFn: (id: string) => cancelRegistrarJob(id),
    onSuccess: (next) => {
      queryClient.setQueryData(registrarJobQueryKey(next.jobId), next);
      toast.success(t("accounts.registrar.canceled"));
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("accounts.registrar.cancelFailed")),
  });

  const parsedCount = Number(count.trim());
  const countValid = Number.isInteger(parsedCount) && parsedCount >= 1 && parsedCount <= maxCount;
  const unavailableKey = registrarUnavailableKey(statusQuery.isPending, statusQuery.isError, status);

  const openDialog = () => {
    // 保留上一次任务：运行中重新打开弹窗时继续跟随进度，未运行时再重置输入。
    if (!jobRunning) {
      setCount("1");
      setDomain(status?.domains[0] ?? "");
    }
    createMutation.reset();
    cancelMutation.reset();
    setOpen(true);
  };

  return (
    <>
      <Tooltip>
        <TooltipTrigger asChild>
          <span tabIndex={0} className="inline-flex">
            <Button type="button" variant="secondary" size="sm" disabled={!available} onClick={openDialog}>
              <Zap />
              {t("accounts.registrar.action")}
            </Button>
          </span>
        </TooltipTrigger>
        <TooltipContent>{t(unavailableKey ?? "accounts.registrar.description")}</TooltipContent>
      </Tooltip>
      <Dialog open={open} onOpenChange={(next) => { if (!next && (createMutation.isPending || cancelMutation.isPending)) return; setOpen(next); }}>
        <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-[520px]">
          <DialogHeader className="pr-8">
            <DialogTitle>{t("accounts.registrar.title")}</DialogTitle>
            <DialogDescription>{t("accounts.registrar.description")}</DialogDescription>
          </DialogHeader>

          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-2">
              <Label htmlFor="registrar-count">{t("accounts.registrar.count")}</Label>
              <Input
                id="registrar-count"
                type="number"
                min={1}
                max={maxCount}
                step={1}
                inputMode="numeric"
                value={count}
                disabled={jobRunning}
                onChange={(event) => setCount(event.target.value)}
              />
              <p className="text-xs leading-5 text-muted-foreground">{t("accounts.registrar.countHelp", { max: maxCount })}</p>
            </div>
            <div className="space-y-2">
              <Label htmlFor="registrar-domain">{t("accounts.registrar.domain")}</Label>
              <Input
                id="registrar-domain"
                value={domain}
                disabled={jobRunning}
                placeholder={t("accounts.registrar.domainPlaceholder")}
                onChange={(event) => setDomain(event.target.value)}
              />
              <p className="text-xs leading-5 text-muted-foreground">{t("accounts.registrar.domainHelp")}</p>
            </div>
          </div>

          {domains.length > 0 ? (
            <div className="flex flex-wrap items-center gap-1.5">
              <span className="text-xs text-muted-foreground">{t("accounts.registrar.domainSuggestions")}</span>
              {domains.map((item) => (
                <Button
                  key={item}
                  type="button"
                  variant={item === domain ? "default" : "secondary"}
                  size="sm"
                  disabled={jobRunning}
                  onClick={() => setDomain(item)}
                >
                  {item}
                </Button>
              ))}
            </div>
          ) : null}

          {job ? (
            <div className="space-y-3 rounded-md bg-muted/45 px-3 py-3">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex flex-wrap items-center gap-2">
                  <RegistrarStatusBadge status={job.status} />
                  <span className="text-xs tabular-nums text-muted-foreground">{t("accounts.registrar.progress", { created: job.created, count: job.count, imported: job.imported, failed: job.failed })}</span>
                </div>
                {jobRunning ? <span className="flex items-center gap-1.5 text-xs text-muted-foreground"><Spinner className="size-3.5" />{t("accounts.registrar.runningHint")}</span> : null}
              </div>

              {job.lastError ? (
                <p className="break-all rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-xs leading-5 text-destructive">
                  <span className="font-medium">{t("accounts.registrar.lastError")}</span>
                  <span className="ml-1">{job.lastError}</span>
                </p>
              ) : null}

              <div className="space-y-1.5">
                <p className="text-xs font-medium">{t("accounts.registrar.items")}</p>
                {job.items.length === 0 ? <p className="text-xs text-muted-foreground">{t("accounts.registrar.itemsEmpty")}</p> : (
                  <ul className="max-h-56 divide-y overflow-y-auto rounded-md bg-background">
                    {job.items.map((item, index) => (
                      <li key={`${item.email}-${index}`} className="flex items-start justify-between gap-3 px-3 py-2">
                        <div className="min-w-0">
                          <p className="truncate text-xs font-medium" title={item.email}>{item.email || "-"}</p>
                          {item.reason ? <p className="mt-0.5 break-all text-[11px] leading-4 text-muted-foreground">{item.reason}</p> : null}
                        </div>
                        <Badge variant={item.status === "failed" ? "destructive" : "secondary"} className="shrink-0">{item.status || "-"}</Badge>
                      </li>
                    ))}
                  </ul>
                )}
              </div>

              {!jobRunning && job.imported > 0 ? <p className="text-xs leading-5 text-emerald-700 dark:text-emerald-400">{t("accounts.registrar.refreshed")}</p> : null}
            </div>
          ) : null}

          <DialogFooter className="gap-2">
            {jobRunning ? (
              <Button type="button" variant="secondary" size="sm" disabled={cancelMutation.isPending} onClick={() => { if (job) cancelMutation.mutate(job.jobId); }}>
                {cancelMutation.isPending ? <Spinner /> : null}
                {t("accounts.registrar.cancelAction")}
              </Button>
            ) : (
              <Button type="button" size="sm" disabled={!countValid || createMutation.isPending} onClick={() => createMutation.mutate({ count: parsedCount, domain: domain.trim() })}>
                {createMutation.isPending ? <Spinner /> : null}
                {t("accounts.registrar.start")}
              </Button>
            )}
            <Button type="button" variant="secondary" size="sm" onClick={() => setOpen(false)}>{t("common.close")}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}

function registrarUnavailableKey(pending: boolean, failed: boolean, status?: RegistrarStatus): string | null {
  if (pending) return "common.loading";
  if (failed || !status) return "accounts.registrar.statusFailed";
  if (!status.available) return "accounts.registrar.unavailable";
  if (!status.enabled) return "accounts.registrar.disabled";
  return null;
}

function RegistrarStatusBadge({ status }: { status: RegistrarJobStatus }) {
  const { t } = useTranslation();
  const variant = status === "failed" ? "destructive" : status === "done" ? "outline" : "secondary";
  return (
    <Badge
      variant={variant}
      className={cn(
        status === "done" && "border-emerald-500/40 text-emerald-700 dark:text-emerald-400",
        status === "running" && "border-sky-500/40 text-sky-700 dark:text-sky-400",
      )}
    >
      {t(`accounts.registrar.status.${status}`)}
    </Badge>
  );
}
