import { liveRefreshInterval } from "../lib/query-refresh";
import { instanceAddress, instancePlayers } from "../lib/instance-display";
import { useQuery } from "@tanstack/react-query";
import { Activity, ArrowRight, Boxes, CircleAlert, Clock3, Cpu, HardDrive, Server, Users } from "lucide-react";
import { Link } from "react-router";
import { Area, AreaChart, ResponsiveContainer, Tooltip } from "recharts";
import { Card, EmptyState, ErrorState, LoadingBlock, MetricBar, PageHeader, ProgressBar, StatusBadge } from "../components/ui";
import { useAuth } from "../context/AuthContext";
import { useRealtime } from "../context/RealtimeContext";
import { api } from "../lib/api";
import { hasAnyPermission } from "../lib/account-permissions";
import { availableInstanceUpdates, fleetNetworkTotals } from "../lib/dashboard";
import { bytes, dateTime, jobLabel, percent } from "../lib/format";

export function DashboardPage() {
  const { user } = useAuth();
  const realtime = useRealtime();
  const polling = realtime.state === "live" ? liveRefreshInterval : 15_000;
  const canReadNodes = hasAnyPermission(user, "nodes.read");
  const canReadInstances = hasAnyPermission(user, "instances.read");
  const canReadJobs = hasAnyPermission(user, "jobs.read");
  const canReadMetrics = hasAnyPermission(user, "metrics.read");
  const canReadNotifications = hasAnyPermission(user, "notifications.read");
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: api.nodes, enabled: canReadNodes, refetchInterval: polling });
  const instances = useQuery({ queryKey: ["instances"], queryFn: api.instances, enabled: canReadInstances, refetchInterval: polling });
  const jobs = useQuery({ queryKey: ["jobs"], queryFn: api.jobs, enabled: canReadJobs, refetchInterval: polling });
  const metricInstance = instances.data?.items.find((item) => item.status === "running") || instances.data?.items[0];
  const metricInstanceID = metricInstance?.id;
  const history = useQuery({
    queryKey: ["metrics", "dashboard", metricInstanceID],
    queryFn: () => api.metrics({ instanceId: metricInstanceID, resolution: "1m", from: new Date(Date.now() - 24 * 60 * 60 * 1000).toISOString() }),
    enabled: canReadInstances && canReadMetrics && Boolean(metricInstanceID),
    refetchInterval: 60_000,
  });

  const pending = canReadNodes && nodes.isPending || canReadInstances && instances.isPending || canReadJobs && jobs.isPending;
  const queryError = canReadNodes && nodes.error || canReadInstances && instances.error || canReadJobs && jobs.error;
  if (pending) return <><PageHeader eyebrow="COMMAND OVERVIEW" title="作战总览" /><LoadingBlock label="正在汇总可查看资源…" /></>;
  if (queryError) return <><PageHeader eyebrow="COMMAND OVERVIEW" title="作战总览" /><ErrorState error={queryError} retry={() => { if (canReadNodes) void nodes.refetch(); if (canReadInstances) void instances.refetch(); if (canReadJobs) void jobs.refetch(); }} /></>;

  const nodeList = nodes.data?.items || [];
  const instanceList = instances.data?.items || [];
  const jobList = jobs.data?.items || [];
  const onlineNodes = nodeList.filter((node) => node.status === "online").length;
  const running = instanceList.filter((instance) => instance.status === "running").length;
  const players = instanceList.reduce((sum, instance) => sum + instancePlayers(instance), 0);
  const activeJobs = jobList.filter((job) => ["queued", "dispatched", "running"].includes(job.status));
  const failedJobs = jobList.filter((job) => job.status === "failed").length;
  const offlineNodes = nodeList.filter((node) => node.status === "offline").length;
  const availableUpdates = availableInstanceUpdates(instanceList);
  const cpu = nodeList.length ? Math.round(nodeList.reduce((sum, node) => sum + node.cpu_percent, 0) / nodeList.length) : 0;
  const memoryUsed = nodeList.reduce((sum, node) => sum + node.memory_used, 0);
  const memoryTotal = nodeList.reduce((sum, node) => sum + node.memory_total, 0);
  const diskUsed = nodeList.reduce((sum, node) => sum + node.disk_used, 0);
  const diskTotal = nodeList.reduce((sum, node) => sum + node.disk_total, 0);
  const network = fleetNetworkTotals(nodeList);
  const attentionHref = failedJobs > 0 && canReadJobs ? "/jobs" : offlineNodes > 0 && canReadNodes ? "/nodes" : canReadNotifications ? "/notifications" : canReadInstances ? "/instances" : undefined;
  const chartData = history.data?.items.map((point) => ({ label: dateTime(point.collected_at), players: point.players })) || [];

  return (
    <div className="dashboard-page">
      <PageHeader eyebrow="COMMAND OVERVIEW" title="作战总览" description="仅汇总当前账号有权查看的节点、实例和任务。" actions={<div className="header-clock"><Clock3 size={16} />{new Intl.DateTimeFormat("zh-CN", { month: "long", day: "numeric", weekday: "short" }).format(new Date())}</div>} />

      {(failedJobs > 0 || offlineNodes > 0 || availableUpdates > 0) && (
        <div className="alert-banner">
          <CircleAlert size={21} />
          <div><strong>有项目需要处理</strong><span>{offlineNodes} 个节点离线，{failedJobs} 个任务执行失败，{availableUpdates} 个实例可更新。</span></div>
          {attentionHref && <Link to={attentionHref}>查看详情 <ArrowRight size={15} /></Link>}
        </div>
      )}

      <section className="stat-grid" aria-label="关键状态">
        {canReadNodes && <Card className="stat-card stat-card--green"><div className="stat-card__icon"><Server /></div><div><span>在线节点</span><strong>{onlineNodes}<small> / {nodeList.length}</small></strong><em>{nodeList.length ? Math.round((onlineNodes / nodeList.length) * 100) : 0}% 可用</em></div></Card>}
        {canReadInstances && <Card className="stat-card stat-card--amber"><div className="stat-card__icon"><Boxes /></div><div><span>运行实例</span><strong>{running}<small> / {instanceList.length}</small></strong><em>{canReadJobs ? `${activeJobs.length} 个任务进行中` : "仅显示可见实例"}</em></div></Card>}
        {canReadInstances && <Card className="stat-card stat-card--blue"><div className="stat-card__icon"><Users /></div><div><span>在线玩家</span><strong>{players}</strong><em>{instanceList.reduce((sum, item) => sum + item.max_players, 0)} 个总席位</em></div></Card>}
        {canReadNodes && <Card className="stat-card stat-card--slate"><div className="stat-card__icon"><Cpu /></div><div><span>平均 CPU</span><strong>{cpu}<small>%</small></strong><em>{nodeList.length} 个节点采样</em></div></Card>}
      </section>

      {!canReadNodes && !canReadInstances && !canReadJobs && <Card><EmptyState title="暂无可汇总资源" description="当前账号没有节点、实例或任务的查看权限；可从左侧进入其他已授权功能。" /></Card>}

      {(canReadInstances || canReadNodes || canReadMetrics) && <section className="dashboard-grid">
        {canReadInstances && (
        <Card className="operations-card">
          <div className="card-heading"><div><span className="eyebrow">LIVE INSTANCES</span><h2>活跃实例</h2></div><Link to="/instances">查看全部 <ArrowRight size={15} /></Link></div>
          {instanceList.length === 0 ? <EmptyState title="还没有游戏实例" description="接入节点后创建你的第一个 L4D2 专用服务器。" action={<Link className="button button--primary" to="/instances">创建实例</Link>} /> : (
            <div className="instance-rows">
              {instanceList.slice(0, 6).map((instance) => (
                <Link to={`/instances/${instance.id}`} className="instance-row" key={instance.id}>
                  <span className={`server-glyph server-glyph--${instance.status}`}><Activity size={18} /></span>
                  <div className="instance-row__name"><strong>{instance.name}</strong><small>{instance.map || "尚未载入地图"} · {instance.mode || instance.config?.mode || "未知模式"}</small>{instance.update_available && <em className="instance-update-available">可更新至 {instance.available_build || "最新公开构建"}</em>}</div>
                  <StatusBadge status={instance.status} pulse={instance.status === "running"} />
                  <div className="instance-row__players"><Users size={15} /><strong>{instancePlayers(instance)}</strong><span>/{instance.max_players}</span></div>
                  <span className="instance-row__address">{instanceAddress(instance)}</span>
                  <ArrowRight className="row-arrow" size={16} />
                </Link>
              ))}
            </div>
          )}
        </Card>
        )}

        {(canReadNodes || canReadInstances) && (
        <Card className="capacity-card">
          <div className="card-heading"><div><span className="eyebrow">FLEET CAPACITY</span><h2>集群容量</h2></div><HardDrive size={20} /></div>
          {canReadInstances && <div className="capacity-players" aria-label="集群当前在线玩家"><span>当前在线玩家<strong>{players}<small> 人</small></strong></span><small>全部 {instanceList.length} 个可见实例合计 · 与实例列表同步</small></div>}
          {canReadInstances && canReadMetrics && metricInstance && <div className="capacity-chart">
            <p className="capacity-chart__caption">{metricInstance.name} · 近 24 小时玩家趋势（单实例）</p>
            {history.isPending ? <p className="capacity-chart__caption" role="status">正在读取历史数据…</p> : history.error ? <p className="capacity-chart__caption" role="status">历史数据暂时无法读取</p> : !chartData.length ? <p className="capacity-chart__caption">暂无历史玩家数据</p> :
            <ResponsiveContainer width="100%" height={100}>
              <AreaChart data={chartData}><defs><linearGradient id="player-fill" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stopColor="#9bd235" stopOpacity={0.4} /><stop offset="1" stopColor="#9bd235" stopOpacity={0} /></linearGradient></defs><Tooltip contentStyle={{ background: "#111816", border: "1px solid #2a3531", borderRadius: 8 }} labelFormatter={(_, payload) => `${metricInstance.name} · ${payload[0]?.payload?.label || ""}`} /><Area type="monotone" name="历史玩家数" dataKey="players" stroke="#9bd235" fill="url(#player-fill)" strokeWidth={2} /></AreaChart>
            </ResponsiveContainer>
            }
          </div>}
          {canReadNodes && <div className="capacity-metrics">
            <MetricBar label="内存" value={percent(memoryUsed, memoryTotal)} detail={`${bytes(memoryUsed)} / ${bytes(memoryTotal)}`} />
            <MetricBar label="磁盘" value={percent(diskUsed, diskTotal)} detail={`${bytes(diskUsed)} / ${bytes(diskTotal)}`} />
            <MetricBar label="CPU" value={cpu} detail={`${cpu}% 平均使用率`} />
            <div className="network-totals" aria-label="节点网络累计流量"><span>网络接收<strong>{bytes(network.rx)}</strong></span><span>网络发送<strong>{bytes(network.tx)}</strong></span></div>
          </div>}
        </Card>
        )}
      </section>}

      {(canReadNodes || canReadJobs) && <section className="dashboard-grid dashboard-grid--lower">
        {canReadNodes && (
        <Card>
          <div className="card-heading"><div><span className="eyebrow">NODE HEALTH</span><h2>节点健康</h2></div><Link to="/nodes">管理节点 <ArrowRight size={15} /></Link></div>
          {nodeList.length === 0 ? <EmptyState title="等待节点接入" description="生成注册令牌，并在 Windows 或 Linux 主机上运行 Agent。" /> : <div className="node-mini-grid">{nodeList.slice(0, 4).map((node) => <div className="node-mini" key={node.id}><div><strong>{node.name}</strong><StatusBadge status={node.status} /></div><small>{node.os} · {node.arch}</small><div className="node-mini__metrics"><span>CPU {Math.round(node.cpu_percent)}%</span><ProgressBar value={node.cpu_percent} /><span>MEM {percent(node.memory_used, node.memory_total)}%</span><ProgressBar value={percent(node.memory_used, node.memory_total)} /></div></div>)}</div>}
        </Card>
        )}
        {canReadJobs && (
        <Card>
          <div className="card-heading"><div><span className="eyebrow">RECENT JOBS</span><h2>最近任务</h2></div><Link to="/jobs">任务中心 <ArrowRight size={15} /></Link></div>
          {jobList.length === 0 ? <EmptyState title="暂无任务" description="创建实例或执行维护操作后，进度会显示在这里。" /> : <div className="job-mini-list">{jobList.slice(0, 5).map((job) => <div className="job-mini" key={job.id}><div className={`job-icon job-icon--${job.status}`}><ClipboardIcon /></div><div><strong>{jobLabel(job)}</strong><small>{job.message || dateTime(job.created_at)}</small></div><div><StatusBadge status={job.status} />{["queued", "dispatched", "running"].includes(job.status) && <ProgressBar value={job.progress} />}</div></div>)}</div>}
        </Card>
        )}
      </section>}
    </div>
  );
}

function ClipboardIcon() { return <span aria-hidden>›_</span>; }
