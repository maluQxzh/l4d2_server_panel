import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Ban, CheckCircle2, ChevronDown, CircleAlert, Clock3, Filter, LoaderCircle, RefreshCw, Search, XCircle } from "lucide-react";
import { useMemo, useState } from "react";
import { Button, Card, EmptyState, ErrorState, LoadingBlock, PageHeader, ProgressBar, StatusBadge } from "../components/ui";
import { useRealtime } from "../context/RealtimeContext";
import { useToast } from "../context/ToastContext";
import { api } from "../lib/api";
import { dateTime, jobLabels } from "../lib/format";
import type { Job } from "../lib/types";

export function JobsPage() {
  const [filter, setFilter] = useState("all");
  const [search, setSearch] = useState("");
  const [expanded, setExpanded] = useState<string>();
  const realtime = useRealtime();
  const jobs = useQuery({ queryKey: ["jobs"], queryFn: api.jobs, refetchInterval: realtime.state === "live" ? false : 10_000 });
  const list = useMemo(() => (jobs.data?.items || []).filter((job) => (filter === "all" || filter === "active" && ["queued", "dispatched", "running"].includes(job.status) || job.status === filter) && `${job.type} ${job.message} ${job.id}`.toLowerCase().includes(search.toLowerCase())), [filter, jobs.data, search]);
  const totals = useMemo(() => ({ active: (jobs.data?.items || []).filter((job) => ["queued", "dispatched", "running"].includes(job.status)).length, failed: (jobs.data?.items || []).filter((job) => job.status === "failed").length, succeeded: (jobs.data?.items || []).filter((job) => job.status === "succeeded").length }), [jobs.data]);
  return <div><PageHeader eyebrow="PERSISTENT JOB QUEUE" title="任务中心" description="所有长操作均可追踪、可恢复，并通过任务 ID 防止重复执行。" actions={<Button variant="secondary" onClick={() => void jobs.refetch()}><RefreshCw size={16} />刷新</Button>} />
    <section className="job-summary"><Card><LoaderCircle size={20} /><span>进行中</span><strong>{totals.active}</strong></Card><Card><CheckCircle2 size={20} /><span>已完成</span><strong>{totals.succeeded}</strong></Card><Card><CircleAlert size={20} /><span>失败</span><strong>{totals.failed}</strong></Card></section>
    <Card className="toolbar-card"><div className="search-box"><Search size={17} /><input value={search} onChange={(event) => setSearch(event.target.value)} placeholder="搜索任务类型、消息或 ID" aria-label="搜索任务" /></div><div className="filter-select"><Filter size={16} /><select value={filter} onChange={(event) => setFilter(event.target.value)}><option value="all">全部任务</option><option value="active">进行中</option><option value="succeeded">已完成</option><option value="failed">失败</option><option value="canceled">已取消</option></select></div></Card>
    {jobs.isPending ? <LoadingBlock label="正在同步任务队列…" /> : jobs.error ? <ErrorState error={jobs.error} retry={() => void jobs.refetch()} /> : list.length ? <Card className="jobs-card"><div className="jobs-table">{list.map((job) => <JobRow key={job.id} job={job} expanded={expanded === job.id} onToggle={() => setExpanded((value) => value === job.id ? undefined : job.id)} />)}</div></Card> : <Card><EmptyState title="没有匹配的任务" description="实例部署、更新、备份与玩家操作会在这里显示完整进度。" /></Card>}
  </div>;
}

function JobRow({ job, expanded, onToggle }: { job: Job; expanded: boolean; onToggle: () => void }) {
  const toast = useToast(); const client = useQueryClient();
  const cancel = useMutation({ mutationFn: () => api.cancelJob(job.id), onSuccess: () => { toast.push("取消请求已提交"); void client.invalidateQueries({ queryKey: ["jobs"] }); }, onError: (error) => toast.push(error.message, "error") });
  const active = ["queued", "dispatched", "running"].includes(job.status);
  const Icon = job.status === "succeeded" ? CheckCircle2 : job.status === "failed" ? XCircle : active ? LoaderCircle : Ban;
  return <div className={`job-row ${expanded ? "job-row--expanded" : ""}`}><button className="job-row__main" onClick={onToggle} aria-expanded={expanded}><div className={`job-status-icon job-status-icon--${job.status}`}><Icon className={job.status === "running" ? "spin" : ""} size={18} /></div><div className="job-row__name"><strong>{jobLabels[job.type] || job.type}</strong><span><code>{job.id.slice(0, 8)}</code>{job.message ? ` · ${job.message}` : ""}</span></div><StatusBadge status={job.status} /><div className="job-row__time"><Clock3 size={14} />{dateTime(job.created_at)}</div><ChevronDown className={expanded ? "rotate" : ""} size={17} /></button>{active && <div className="job-row__progress"><ProgressBar value={job.progress} /><span>{job.progress}%</span></div>}{expanded && <div className="job-row__details"><div><span>节点 ID</span><code>{job.node_id}</code></div><div><span>实例 ID</span><code>{job.instance_id || "—"}</code></div><div><span>发起人</span><strong>{job.requested_by || "系统"}</strong></div><div><span>开始时间</span><strong>{dateTime(job.started_at)}</strong></div><div><span>完成时间</span><strong>{dateTime(job.finished_at)}</strong></div>{active && <Button variant="danger" loading={cancel.isPending} onClick={() => cancel.mutate()}><Ban size={14} />取消任务</Button>}</div>}</div>;
}
