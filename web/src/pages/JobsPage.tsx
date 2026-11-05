import { liveRefreshInterval, refreshQueries } from "../lib/query-refresh";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Ban, CheckCircle2, ChevronDown, CircleAlert, Clock3, Filter, LoaderCircle, RadioTower, RefreshCw, Search, XCircle } from "lucide-react";
import { useEffect, useState } from "react";
import { Button, Card, EmptyState, ErrorState, LoadingBlock, PageHeader, ProgressBar, StatusBadge } from "../components/ui";
import { useAuth } from "../context/AuthContext";
import { useRealtime } from "../context/RealtimeContext";
import { useToast } from "../context/ToastContext";
import { api } from "../lib/api";
import { hasAnyPermission, hasJobPermission, hasRootSecurityPermission } from "../lib/account-permissions";
import { dateTime, jobLabel } from "../lib/format";
import type { Job, TerminalOutcomeConflict } from "../lib/types";

export function JobsPage() {
  const { user } = useAuth();
  const [filter, setFilter] = useState("all");
  const [view, setView] = useState<"operations" | "background">("operations");
  const [search, setSearch] = useState("");
  const [submittedSearch, setSubmittedSearch] = useState("");
  const [cursors, setCursors] = useState<string[]>([""]);
  const cursor = cursors[cursors.length - 1];
  useEffect(() => {
    if (search.trim() === submittedSearch) return;
    const timer = window.setTimeout(() => { setSubmittedSearch(search.trim()); setCursors([""]); }, 300);
    return () => window.clearTimeout(timer);
  }, [search, submittedSearch]);
  const [expanded, setExpanded] = useState<string>();
  const realtime = useRealtime();
  const canReadJobs = hasAnyPermission(user, "jobs.read");
  const canResolveTerminalConflicts = hasRootSecurityPermission(user);
  const jobs = useQuery({
    queryKey: ["jobs", view, filter, submittedSearch, cursor],
    queryFn: () => api.jobList({ background: view === "operations" ? "exclude" : "only", backgroundSummary: true,
      ...(filter !== "all" ? { status: filter } : {}), ...(submittedSearch ? { search: submittedSearch } : {}), ...(cursor ? { cursor } : {}) }),
    enabled: canReadJobs,
    refetchInterval: realtime.state === "live" ? liveRefreshInterval : 10_000,
  });
  const terminalConflicts = useQuery({
    queryKey: ["job-terminal-conflicts"],
    queryFn: api.terminalOutcomeConflicts,
    enabled: canResolveTerminalConflicts,
    refetchInterval: 10_000,
  });
  const list = jobs.data?.items || [];
  const totals = jobs.data?.summary || { active: "—", succeeded: "—", failed: "—" };
  const background = jobs.data?.background_summary;
  const backgroundState = background?.failed ? `${background.failed} 个异常` : background ? `${background.targets} 个目标` : "—";
  const backgroundDetail = background?.latest_at ? `最近 ${dateTime(background.latest_at)}` : "暂无探测记录";
  if (!canReadJobs) return <div><PageHeader eyebrow="PERSISTENT JOB QUEUE" title="任务中心" description="用户操作与后台探测分开显示，重要任务不会再被周期性状态查询挤出列表。" /><Card><EmptyState title="任务清单已隐藏" description="当前账号没有查看任务队列的权限。已授权的实例操作仍可在对应页面执行。" /></Card></div>;
  return <div><PageHeader eyebrow="PERSISTENT JOB QUEUE" title="任务中心" description="用户操作与后台探测分开显示，重要任务不会再被周期性状态查询挤出列表。" actions={<Button variant="secondary" onClick={() => { void jobs.refetch(); if (canResolveTerminalConflicts) void terminalConflicts.refetch(); }}><RefreshCw size={16} />刷新</Button>} />
    {canResolveTerminalConflicts && terminalConflicts.error ? <Card className="terminal-conflict-card"><ErrorState error={terminalConflicts.error} retry={() => void terminalConflicts.refetch()} /></Card> : canResolveTerminalConflicts && terminalConflicts.data?.items.length ? <TerminalConflictPanel conflicts={terminalConflicts.data.items} /> : null}
    <section className="job-summary"><Card><LoaderCircle size={20} /><span>进行中</span><strong>{totals.active}</strong></Card><Card><CheckCircle2 size={20} /><span>已完成</span><strong>{totals.succeeded}</strong></Card><Card><CircleAlert size={20} /><span>失败</span><strong>{totals.failed}</strong></Card><Card className={background?.failed ? "job-background-card job-background-card--failed" : "job-background-card"}><RadioTower size={20} /><span>后台探测<em>{backgroundDetail}</em></span><strong>{backgroundState}</strong></Card></section>
    <Card className="toolbar-card"><div className="search-box"><Search size={17} /><input value={search} onChange={(event) => { setSearch(event.target.value); }} placeholder="搜索任务类型代码或 ID" aria-label="搜索任务" maxLength={60} /></div><div className="filter-select"><RadioTower size={16} /><select aria-label="任务分类" value={view} onChange={(event) => { setView(event.target.value as "operations" | "background"); setCursors([""]); setExpanded(undefined); }}><option value="operations">重要任务</option><option value="background">后台探测</option></select></div><div className="filter-select"><Filter size={16} /><select aria-label="任务状态" value={filter} onChange={(event) => { setFilter(event.target.value); setCursors([""]); }}><option value="all">全部状态</option><option value="active">进行中</option><option value="succeeded">已完成</option><option value="failed">失败</option><option value="canceled">已取消</option></select></div></Card>
    {jobs.isPending ? <LoadingBlock label="正在同步任务队列…" /> : jobs.error ? <ErrorState error={jobs.error} retry={() => void jobs.refetch()} /> : list.length ? <Card className="jobs-card"><div className="jobs-table">{list.map((job) => <JobRow key={job.id} job={job} expanded={expanded === job.id} canCancel={hasJobPermission(user, job, "jobs.cancel")} canViewDetails={hasJobPermission(user, job, "jobs.details")} onToggle={() => setExpanded((value) => value === job.id ? undefined : job.id)} />)}</div></Card> : <Card><EmptyState title="没有匹配的任务" description={view === "operations" ? "后台状态查询已单独归类；实例部署、更新、备份与玩家操作会显示在这里。" : "当前没有可查看的后台探测记录。"} /></Card>}
    <div className="form-actions"><Button variant="secondary" disabled={cursors.length === 1 || jobs.isFetching} onClick={() => { setCursors((current) => current.slice(0, -1)); setExpanded(undefined); }}>上一页</Button><span>第 {cursors.length} 页</span><Button variant="secondary" disabled={!jobs.data?.next_cursor || jobs.isFetching} onClick={() => { setCursors((current) => [...current, jobs.data!.next_cursor!]); setExpanded(undefined); }}>下一页</Button></div>
  </div>;
}

function TerminalConflictPanel({ conflicts }: { conflicts: TerminalOutcomeConflict[] }) {
  return <Card className="terminal-conflict-card">
    <div className="terminal-conflict-card__heading"><CircleAlert size={19} /><div><strong>检测到任务最终结果冲突</strong><span>中心端记录保持不变；确认后仅清除 Agent 保存的冲突上报，不会改写已经完成的任务。</span></div></div>
    <div>{conflicts.map((conflict) => <TerminalConflictRow key={`${conflict.job_id}:${conflict.reported_fingerprint}`} conflict={conflict} />)}</div>
  </Card>;
}

function TerminalConflictRow({ conflict }: { conflict: TerminalOutcomeConflict }) {
  const toast = useToast();
  const client = useQueryClient();
  const resolve = useMutation({
    mutationFn: () => api.resolveTerminalOutcomeConflict(conflict.job_id, conflict.reported_fingerprint),
    onSuccess: (result) => {
      toast.push(result.acknowledgement_delivered ? "冲突已确认，节点已收到清理通知" : "冲突已确认，节点重连后会自动清理");
      void refreshQueries(client, { queryKey: ["job-terminal-conflicts"] });
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  return <div className="terminal-conflict-row">
    <div><strong>任务 <code>{conflict.job_id}</code></strong><span>节点 <code>{conflict.node_id}</code>{conflict.instance_id ? <> · 实例 <code>{conflict.instance_id}</code></> : null}</span><small>中心端：{terminalStatusLabel(conflict.stored_status)} · Agent：{terminalStatusLabel(conflict.reported_status)} · 已拦截 {conflict.occurrences} 次 · 最近 {dateTime(conflict.last_seen_at)}</small></div>
    <Button variant="danger" loading={resolve.isPending} onClick={() => resolve.mutate()}>保留中心端结果并确认</Button>
  </div>;
}

function terminalStatusLabel(status: TerminalOutcomeConflict["stored_status"]) {
  return ({ succeeded: "成功", failed: "失败", canceled: "已取消" } as const)[status];
}

function JobRow({ job, expanded, canCancel, canViewDetails, onToggle }: { job: Job; expanded: boolean; canCancel: boolean; canViewDetails: boolean; onToggle: () => void }) {
  const toast = useToast(); const client = useQueryClient();
  const cancel = useMutation({ mutationFn: () => api.cancelJob(job.id), onSuccess: () => { toast.push("取消请求已提交"); void refreshQueries(client, { queryKey: ["jobs"] }); }, onError: (error) => toast.push(error.message, "error") });
  const active = ["queued", "dispatched", "running"].includes(job.status);
  const failure = jobFailureDetails(job);
  const Icon = job.status === "succeeded" ? CheckCircle2 : job.status === "failed" ? XCircle : active ? LoaderCircle : Ban;
  return <div className={`job-row ${expanded ? "job-row--expanded" : ""}`}><button className="job-row__main" onClick={onToggle} aria-expanded={expanded}><div className={`job-status-icon job-status-icon--${job.status}`}><Icon className={job.status === "running" ? "spin" : ""} size={18} /></div><div className="job-row__name"><strong>{jobLabel(job)}</strong><span><code>{job.id.slice(0, 8)}</code>{job.message ? ` · ${job.message}` : ""}</span></div><StatusBadge status={job.status} /><div className="job-row__time"><Clock3 size={14} />{dateTime(job.created_at)}</div><ChevronDown className={expanded ? "rotate" : ""} size={17} /></button>{active && <div className="job-row__progress"><ProgressBar value={job.progress} /><span>{job.progress}%</span></div>}{expanded && <div className="job-row__details"><div><span>节点 ID</span><code>{job.node_id}</code></div><div><span>关联实例 ID</span><code>{job.instance_id || job.scope_instance_id || "—"}</code></div><div><span>发起人</span><strong>{job.requested_by ? job.requested_by_display_name || "未知账号" : "系统"}</strong></div><div><span>开始时间</span><strong>{dateTime(job.started_at)}</strong></div><div><span>完成时间</span><strong>{dateTime(job.finished_at)}</strong></div>{active && canCancel && <Button variant="danger" loading={cancel.isPending} onClick={() => cancel.mutate()}><Ban size={14} />取消任务</Button>}{canViewDetails && failure && <div className="job-row__failure"><span>失败阶段 · {mapFailureStageLabel(failure.stage)} · {failure.code}</span><strong>{failure.summary}</strong><p>{failure.remediation}</p>{failure.source && <code>{failure.source}</code>}<small>{failure.retryable ? "可在原因消除后直接重试" : "需要修复来源或节点配置后重试"}</small></div>}{canViewDetails && (job.payload || job.result) && <div className="job-row__technical">{job.payload && <div><span>任务参数</span><code>{JSON.stringify(job.payload)}</code></div>}{job.result && <div><span>执行结果</span><code>{JSON.stringify(job.result)}</code></div>}</div>}</div>}</div>;
}

type JobFailureDetails = { stage: string; code: string; summary: string; remediation: string; retryable: boolean; source?: string };

function jobFailureDetails(job: Job): JobFailureDetails | undefined {
  const value = job.result?.failure;
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
  const failure = value as Record<string, unknown>;
  if (typeof failure.stage !== "string" || typeof failure.code !== "string" || typeof failure.summary !== "string" || typeof failure.remediation !== "string" || typeof failure.retryable !== "boolean") return undefined;
  return { stage: failure.stage, code: failure.code, summary: failure.summary, remediation: failure.remediation, retryable: failure.retryable, source: typeof failure.source === "string" ? failure.source : undefined };
}

function mapFailureStageLabel(stage: string) {
  return ({ preflight: "预检", staging: "暂存", metadata: "分享信息", download: "下载", archive: "归档", vpk: "VPK", publish: "发布" } as Record<string, string>)[stage] || stage;
}
