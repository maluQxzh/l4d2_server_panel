import { useMutation, useQuery, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { BellRing, CheckCheck, CircleAlert, Info, RefreshCw, ShieldAlert } from "lucide-react";
import { Button, Card, EmptyState, ErrorState, LoadingBlock, PageHeader } from "../components/ui";
import { useAuth } from "../context/AuthContext";
import { api } from "../lib/api";
import { dateTime } from "../lib/format";
import type { ListResponse, NotificationDeliveryRecord, NotificationRecord } from "../lib/types";

export function NotificationsPage() {
  const auth = useAuth();
  const notifications = useQuery({ queryKey: ["notifications"], queryFn: api.notifications, refetchInterval: 30_000 });
  const deliveries = useQuery({ queryKey: ["notification-deliveries"], queryFn: api.notificationDeliveries, enabled: ["owner", "admin"].includes(auth.user?.role || ""), refetchInterval: 30_000 });
  return <div><PageHeader eyebrow="ALERT INBOX" title="通知" description="查看节点离线、磁盘预警、更新和任务失败等重要事件。" />{notifications.isPending ? <LoadingBlock label="正在读取通知…" /> : notifications.error ? <ErrorState error={notifications.error} retry={() => void notifications.refetch()} /> : notifications.data.items.length ? <Card className="notification-list">{notifications.data.items.map((item) => <NotificationRow key={item.id} item={item} />)}</Card> : <Card><EmptyState title="没有未处理通知" description="系统状态良好。新告警会通过实时事件自动出现在这里。" /></Card>}{["owner", "admin"].includes(auth.user?.role || "") && <DeliveryCard query={deliveries} />}</div>;
}

function DeliveryCard({ query }: { query: UseQueryResult<ListResponse<NotificationDeliveryRecord>, Error> }) {
  const client = useQueryClient();
  const retry = useMutation({ mutationFn: api.retryNotificationDelivery, onSuccess: () => void client.invalidateQueries({ queryKey: ["notification-deliveries"] }) });
  return <Card className="delivery-card"><div className="card-heading"><div><span className="eyebrow">DELIVERY OUTBOX</span><h2>外部投递记录</h2></div><BellRing size={19} /></div>{query.isPending ? <LoadingBlock label="正在读取投递记录…" /> : query.error ? <ErrorState error={query.error} retry={() => void query.refetch()} /> : query.data?.items.length ? <div className="delivery-list">{query.data.items.slice(0, 30).map((item) => <div className="delivery-row" key={item.id}><span className={`status status--${item.status === "succeeded" ? "online" : item.status === "failed" ? "failed" : "pending"}`}>{item.status}</span><div><strong>{item.channel_name}</strong><small>{item.last_error || `HTTP ${item.response_code || "—"}`}</small></div><div><small>尝试 {item.attempt_count}/6</small><time>{dateTime(item.delivered_at || item.next_attempt_at)}</time></div>{item.status === "failed" && <Button variant="secondary" onClick={() => retry.mutate(item.id)} loading={retry.isPending}><RefreshCw size={14} />重试</Button>}</div>)}</div> : <EmptyState title="还没有外部投递" description="配置 SMTP 或 Webhook 后，新通知的重试记录会显示在这里。" />}</Card>;
}

function NotificationRow({ item }: { item: NotificationRecord }) {
  const client = useQueryClient();
  const read = useMutation({ mutationFn: () => api.markNotificationRead(item.id), onSuccess: () => void client.invalidateQueries({ queryKey: ["notifications"] }) });
  const Icon = item.level === "critical" ? ShieldAlert : item.level === "warning" ? CircleAlert : Info;
  return <article className={`notification-row notification-row--${item.level} ${item.read ? "notification-row--read" : ""}`}><div className="notification-row__icon"><Icon size={20} /></div><div><div><strong>{item.title}</strong><time>{dateTime(item.created_at)}</time></div><p>{item.message}</p></div>{!item.read && <Button variant="ghost" onClick={() => read.mutate()} loading={read.isPending}><CheckCheck size={15} />标为已读</Button>}</article>;
}
