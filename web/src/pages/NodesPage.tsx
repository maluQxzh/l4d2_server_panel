import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Archive, Copy, FolderSearch, KeyRound, Laptop, MoreHorizontal, Plus, RefreshCw, RotateCw, Server, ShieldAlert, Trash2 } from "lucide-react";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { Button, Card, EmptyState, ErrorState, Field, LoadingBlock, MetricBar, Modal, PageHeader, StatusBadge } from "../components/ui";
import { useAuth } from "../context/AuthContext";
import { useRealtime } from "../context/RealtimeContext";
import { useToast } from "../context/ToastContext";
import { api } from "../lib/api";
import { hasAnyPermission, hasGlobalPermission } from "../lib/account-permissions";
import { bytes, dateTime, percent, relativeTime } from "../lib/format";
import { canAccessNode } from "../lib/instance-lifecycle";
import type { NodeDiscoveryResult, NodeRecord, QuarantineEntry } from "../lib/types";

export function NodesPage() {
  const { user } = useAuth();
  const canEnroll = hasGlobalPermission(user, "nodes.enroll");
  const canReadNodes = hasAnyPermission(user, "nodes.read");
  const [enrollOpen, setEnrollOpen] = useState(false);
  const [selected, setSelected] = useState<NodeRecord | null>(null);
  const realtime = useRealtime();
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: api.nodes, enabled: canReadNodes, refetchInterval: realtime.state === "live" ? false : 15_000 });

  return (
    <div>
      <PageHeader eyebrow="NODE FLEET" title="节点" description="管理承载游戏实例的 Windows 与 Linux 主机。" actions={<>{canReadNodes && <Button variant="secondary" onClick={() => void nodes.refetch()}><RefreshCw size={16} />刷新</Button>}{canEnroll && <Button onClick={() => setEnrollOpen(true)}><Plus size={17} />接入节点</Button>}</>} />
      {!canReadNodes ? <Card><EmptyState title="可接入新节点" description="当前账号不能读取节点列表；你仍可创建一次性注册令牌接入新主机。" action={canEnroll ? <Button onClick={() => setEnrollOpen(true)}><Plus size={16} />接入节点</Button> : undefined} /></Card> : nodes.isPending ? <LoadingBlock label="正在检查节点心跳…" /> : nodes.error ? <ErrorState error={nodes.error} retry={() => void nodes.refetch()} /> : nodes.data.items.length === 0 ? <Card><EmptyState title="还没有节点" description="创建一次性注册令牌，在 Windows 或 Linux 主机上安装 Agent。" action={canEnroll ? <Button onClick={() => setEnrollOpen(true)}><Plus size={16} />接入第一个节点</Button> : undefined} /></Card> : (
        <div className="node-grid">
          {nodes.data.items.map((node) => (
            <Card className={`node-card node-card--${node.status}`} key={node.id}>
              <div className="node-card__top"><div className="node-os-icon">{node.os.toLowerCase().includes("windows") ? <Laptop /> : <Server />}</div><div><h2>{node.name}</h2><span>{node.os || "未知系统"} · {node.arch}</span></div><StatusBadge status={node.status} pulse={node.status === "online"} /><button className="icon-button" aria-label={`管理 ${node.name}`} onClick={() => setSelected(node)}><MoreHorizontal size={18} /></button></div>
              <div className="node-card__address"><span>AGENT</span><code>{node.agent_version || "等待注册"}</code><span>最后心跳</span><strong>{relativeTime(node.last_seen_at)}</strong></div>
              <div className="node-card__metrics">
                <MetricBar label="CPU" value={node.cpu_percent} detail={`${Math.round(node.cpu_percent)}%`} />
                <MetricBar label="内存" value={percent(node.memory_used, node.memory_total)} detail={`${bytes(node.memory_used)} / ${bytes(node.memory_total)}`} />
                <MetricBar label="磁盘" value={percent(node.disk_used, node.disk_total)} detail={`${bytes(node.disk_used)} / ${bytes(node.disk_total)}`} />
              </div>
              <div className="node-card__foot"><span><span className={`health-dot health-dot--${node.status}`} />{node.status === "online" ? "指标持续上报" : "等待 Agent 重新连接"}</span><code>{node.address || "未报告地址"}</code></div>
            </Card>
          ))}
        </div>
      )}
      <EnrollmentModal open={canEnroll && enrollOpen} onClose={() => setEnrollOpen(false)} />
      <NodeManageModal node={selected} onClose={() => setSelected(null)} />
    </div>
  );
}

function EnrollmentModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [name, setName] = useState("");
  const [token, setToken] = useState("");
  const [expires, setExpires] = useState("");
  const toast = useToast();
  const enroll = useMutation({
    mutationFn: () => api.createEnrollment(name.trim()),
    onSuccess: (result) => { setToken(result.token); setExpires(result.expires_at || ""); },
  });
  const close = () => { setName(""); setToken(""); enroll.reset(); onClose(); };
  const submit = (event: FormEvent) => { event.preventDefault(); if (name.trim()) enroll.mutate(); };
  const copy = async (value: string) => { await navigator.clipboard.writeText(value); toast.push("已复制到剪贴板"); };

  return <Modal open={open} onClose={close} title="接入新节点" description="令牌仅显示一次，Agent 完成注册后自动失效。" footer={token ? <Button onClick={close}>完成</Button> : <><Button variant="secondary" onClick={close}>取消</Button><Button form="enroll-form" type="submit" loading={enroll.isPending}>生成注册令牌</Button></>}>
    {token ? <div className="enrollment-result"><div className="success-seal"><KeyRound size={24} /></div><h3>注册令牌已生成</h3><p>在目标主机安装 Agent 时填入下方令牌。{expires && `令牌将于 ${new Date(expires).toLocaleString("zh-CN")} 失效。`}</p><div className="copy-value"><code>{token}</code><button className="icon-button" onClick={() => void copy(token)} aria-label="复制令牌"><Copy size={18} /></button></div><div className="command-hint"><strong>安全提示</strong><span>不要通过公开聊天发送令牌。节点接入后，可在此页面确认系统与 Agent 版本。</span></div></div> : <form id="enroll-form" onSubmit={submit}><Field label="节点名称" hint="使用容易识别的机房或主机名称。"><input value={name} onChange={(event) => setName(event.target.value)} placeholder="例如：香港-01" required /></Field><div className="platform-note"><Server size={20} /><div><strong>兼容平台</strong><span>Windows 10/11、Windows Server 2019+、Ubuntu 22.04/24.04 与 Debian 12 amd64</span></div></div>{enroll.error && <div className="form-error" role="alert">{enroll.error.message}</div>}</form>}
  </Modal>;
}

function NodeManageModal({ node, onClose }: { node: NodeRecord | null; onClose: () => void }) {
  const { user } = useAuth();
  const toast = useToast();
  const client = useQueryClient();
  const [discovery, setDiscovery] = useState<NodeDiscoveryResult | null>(null);
  const [newCredential, setNewCredential] = useState("");
  const [publicAddress, setPublicAddress] = useState("");
  const canUpdate = Boolean(node && canAccessNode(user, node.id, "nodes.update"));
  const canRotate = Boolean(node && canAccessNode(user, node.id, "nodes.credentials.rotate"));
  const canDelete = Boolean(node && canAccessNode(user, node.id, "nodes.delete"));
  const canDiscover = Boolean(node && canAccessNode(user, node.id, "nodes.discover"));
  const canViewQuarantine = Boolean(node && canAccessNode(user, node.id, "nodes.quarantine.read"));
  const canPurgeQuarantine = Boolean(node && canAccessNode(user, node.id, "nodes.quarantine.purge"));
  useEffect(() => { setPublicAddress(node?.public_address || ""); }, [node?.id, node?.public_address]);
  const close = () => {
    setDiscovery(null);
    setNewCredential("");
    onClose();
  };
  const discover = useMutation({
    mutationFn: () => api.discoverNodeInstances(node!.id),
    onSuccess: (result) => {
      setDiscovery(result);
      toast.push(result.candidates.length ? `发现 ${result.candidates.length} 个 L4D2 安装` : "扫描完成，未发现可识别安装");
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const rotate = useMutation({
    mutationFn: () => api.rotateNodeCredential(node!.id),
    onSuccess: (result) => {
      setNewCredential(result.credential);
      void client.invalidateQueries({ queryKey: ["nodes"] });
      toast.push("节点凭据已轮换；Agent 已断开连接");
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const saveNode = useMutation({
    mutationFn: () => api.updateNode(node!.id, { name: node!.name, public_address: publicAddress.trim() }),
    onSuccess: (updated) => {
      setPublicAddress(updated.public_address || "");
      void client.invalidateQueries({ queryKey: ["nodes"] });
      void client.invalidateQueries({ queryKey: ["nodes", updated.id] });
      toast.push(updated.public_address ? "公开连接地址已保存" : "公开连接地址已清除");
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const remove = useMutation({
    mutationFn: () => api.deleteNode(node!.id),
    onSuccess: () => {
      toast.push("节点及中心端关联数据已删除");
      for (const key of [["nodes"], ["instances"], ["environments"], ["maps"], ["jobs"], ["notifications"]]) {
        void client.invalidateQueries({ queryKey: key });
      }
      close();
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const rotateCredential = () => {
    if (window.confirm("轮换会立即断开 Agent。确认已准备好更新 agent.json 并重启 Agent 吗？")) rotate.mutate();
  };
  const deleteNode = () => {
    if (window.confirm(`确认删除节点“${node?.name}”？中心端的实例、运行环境、地图、任务和相关记录会一并删除；Agent 主机上的文件不会被删除。`)) remove.mutate();
  };
  return <Modal open={Boolean(node)} onClose={close} title={node?.name || "节点管理"} description="管理节点地址和凭据；SteamCMD 与游戏路径请在运行环境中手动登记。" footer={<><Button variant="secondary" onClick={close}>关闭</Button><Button variant="danger" loading={remove.isPending} disabled={!canDelete} title={!canDelete ? "当前账号没有删除节点权限" : undefined} onClick={deleteNode}><Trash2 size={16} />删除节点</Button></>}>
    {node && <div className="manage-node form-stack">
      <div className="detail-pairs"><div><span>实例根目录</span><code>{node.instance_root || "未上报"}</code></div><div><span>Agent 版本</span><strong>{node.agent_version || "未知"}</strong></div><div><span>主机地址</span><strong>{node.address || "未上报"}</strong></div><div><span>公开连接地址</span><strong>{publicAddress || "未设置"}</strong></div><div><span>运行架构</span><strong>{node.os} / {node.arch}</strong></div></div>
      <section className="node-public-address">
        <div className="node-section-heading"><div><strong>公开状态页连接地址</strong><span>填写玩家可访问的域名或 IP，不含协议、路径和端口；IPv6 请使用方括号。</span></div></div>
        <form className="node-address-form" onSubmit={(event) => { event.preventDefault(); saveNode.mutate(); }}>
          <Field label="公开连接地址" hint="例如 game.example.com、203.0.113.10 或 [2001:db8::10]；留空可清除。"><input value={publicAddress} onChange={(event) => setPublicAddress(event.target.value)} placeholder="game.example.com" /></Field>
          <Button type="submit" variant="secondary" loading={saveNode.isPending} disabled={!canUpdate}>保存公开地址</Button>
        </form>
      </section>
      {canDiscover && <section className="node-discovery">
        <div className="node-section-heading"><div><strong>发现已有游戏安装</strong><span>只扫描 Agent 配置的实例根目录；不会跟随符号链接或 Windows 重解析点。</span></div><Button variant="secondary" loading={discover.isPending} disabled={node.status !== "online" || rotate.isSuccess} onClick={() => discover.mutate()}><FolderSearch size={15} />扫描安装</Button></div>
        {discover.isPending && <LoadingBlock label="正在安全扫描实例根目录…" />}
        {discovery && !discover.isPending && <><div className="discovery-summary"><span>检查 {discovery.scanned_directories} 个目录</span><span>跳过 {discovery.skipped_links} 个链接</span>{discovery.truncated && <strong>已达到扫描上限</strong>}</div>{discovery.candidates.length ? <div className="discovery-list">{discovery.candidates.map((candidate) => <div className="discovery-candidate discovery-candidate--readonly" key={`${candidate.root_index}:${candidate.path}`}><span className="discovery-radio" /><span><strong>{candidate.name}</strong><code>根目录 {candidate.root_index} · {candidate.path}</code><small>{candidate.evidence.join(" · ")}</small></span><span>{candidate.managed ? "已登记" : candidate.likely_running ? `可能运行中${candidate.pid ? ` · PID ${candidate.pid}` : ""}` : "可登记"}</span></div>)}</div> : <EmptyState title="未发现可识别的安装" description="候选目录需要包含 srcds 可执行文件和 left4dead2 目录。" />}</>}
      </section>}
      <NodeQuarantineSection node={node} canView={canViewQuarantine} canPurge={canPurgeQuarantine} />
      <section className="node-credential-risk">
        <div className="danger-note"><ShieldAlert size={20} /><div><strong>节点凭据属于高风险设置</strong><span>轮换会立即断开 Agent。新凭据只在这里返回一次，必须写入该节点的 agent.json 后重启 Agent。</span></div></div>
        {newCredential ? <div className="credential-result"><strong>新节点凭据</strong><code>{newCredential}</code><span>请现在更新 agent.json 的 credential 字段并重启 Agent；关闭窗口后无法再次查看。</span></div> : <Button variant="danger" loading={rotate.isPending} disabled={!canRotate} onClick={rotateCredential}><RotateCw size={15} />轮换节点凭据</Button>}
      </section>
      <div className="danger-note"><Trash2 size={20} /><div><strong>删除会清理中心端关联数据</strong><span>请先停止该节点上的游戏进程。删除不会移除 Agent 主机上的游戏文件，重新接入需要新注册令牌。</span></div></div>
    </div>}
  </Modal>;
}

function NodeQuarantineSection({ node, canView, canPurge }: { node: NodeRecord; canView: boolean; canPurge: boolean }) {
  const toast = useToast();
  const client = useQueryClient();
  const token = useRef("");
  const purgeKey = useRef("");
  const [selected, setSelected] = useState<QuarantineEntry | null>(null);
  const [typedName, setTypedName] = useState("");
  const [tokenExpiresAt, setTokenExpiresAt] = useState("");
  const entries = useQuery({
    queryKey: ["nodes", node.id, "quarantine"],
    queryFn: () => api.quarantineEntries(node.id),
    enabled: canView,
  });
  const resetConfirmation = () => {
    token.current = "";
    purgeKey.current = "";
    setTypedName("");
    setTokenExpiresAt("");
    setSelected(null);
    confirmation.reset();
    purge.reset();
  };
  const confirmation = useMutation({
    mutationFn: async () => {
      if (!selected) throw new Error("未选择隔离项");
      const result = await api.createQuarantinePurgeConfirmation(node.id, selected.id);
      token.current = result.confirmation_token;
      purgeKey.current = `purge-quarantine:${selected.id}:${crypto.randomUUID()}`;
      return result.expires_at;
    },
    onSuccess: (expiresAt) => setTokenExpiresAt(expiresAt),
    onError: (error) => toast.push(error.message, "error"),
  });
  const purge = useMutation({
    mutationFn: async () => {
      if (!selected || !token.current || !purgeKey.current) throw new Error("一次性确认已失效，请重新获取");
      return api.purgeQuarantineEntry(node.id, selected.id, token.current, purgeKey.current);
    },
    onSuccess: () => {
      toast.push("隔离文件彻底清理任务已提交");
      void client.invalidateQueries({ queryKey: ["nodes", node.id, "quarantine"] });
      void client.invalidateQueries({ queryKey: ["jobs"] });
      resetConfirmation();
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  useEffect(() => {
    if (!tokenExpiresAt) return;
    const remaining = Date.parse(tokenExpiresAt) - Date.now();
    if (remaining <= 0) {
      token.current = "";
      purgeKey.current = "";
      setTypedName("");
      setTokenExpiresAt("");
      return;
    }
    const timeout = window.setTimeout(() => {
      token.current = "";
      purgeKey.current = "";
      setTypedName("");
      setTokenExpiresAt("");
    }, Math.min(remaining, 0x7fffffff));
    return () => window.clearTimeout(timeout);
  }, [tokenExpiresAt]);
  const now = Date.now();
  const tokenValid = Boolean(token.current && tokenExpiresAt && Date.parse(tokenExpiresAt) > now);
  const choose = (entry: QuarantineEntry) => {
    token.current = "";
    purgeKey.current = "";
    setTypedName("");
    setTokenExpiresAt("");
    confirmation.reset();
    purge.reset();
    setSelected(entry);
  };
  const closeConfirmation = () => {
    if (confirmation.isPending || purge.isPending) {
      toast.push("正在创建或提交一次性确认，请稍候", "error");
      return;
    }
    resetConfirmation();
  };
  return <section className="node-quarantine">
    <div className="node-section-heading"><div><strong>实例文件隔离区</strong><span>隔离项至少保留 7 天。到期后仍需生成一次性确认并输入完整隔离项名称，系统不会自动彻底清理。</span></div><Archive size={18} /></div>
    {!canView ? <div className="form-note"><strong>没有查看隔离目录权限</strong><span>请联系所有者分配对应节点权限。</span></div> : entries.isPending ? <LoadingBlock label="正在读取隔离清单…" /> : entries.error ? <ErrorState error={entries.error} retry={() => void entries.refetch()} /> : entries.data.items.length ? <div className="quarantine-list">{entries.data.items.map((entry) => {
      const expired = Date.parse(entry.retention_expires_at) <= now;
      return <div className="quarantine-row" key={entry.id}><div><strong>{entry.instance_name || entry.entry_name}</strong><code>{entry.entry_name}</code><span>隔离于 {dateTime(entry.quarantined_at)} · {expired ? `保留期已于 ${dateTime(entry.retention_expires_at)} 到期` : `最早可于 ${dateTime(entry.retention_expires_at)} 清理`}</span></div><Button variant="danger" disabled={!canPurge || !expired || Boolean(entry.purge_job_id)} title={!canPurge ? "没有永久清理权限" : !expired ? "7 天最低保留期尚未结束" : entry.purge_job_id ? "清理任务已提交" : "需要二次确认"} onClick={() => choose(entry)}><Trash2 size={14} />{entry.purge_job_id ? "清理中" : "彻底清理"}</Button></div>;
    })}</div> : <EmptyState title="隔离区为空" description="隔离删除的实例目录会在这里保留至少 7 天。" />}
    <Modal open={Boolean(selected)} onClose={closeConfirmation} title="彻底清理隔离文件" description="此操作不可恢复；一次性确认仅绑定当前管理员和隔离项，并在 5 分钟后失效。" footer={<><Button variant="secondary" onClick={closeConfirmation} disabled={confirmation.isPending || purge.isPending}>取消</Button>{!tokenValid ? <Button variant="danger" onClick={() => { setTypedName(""); confirmation.mutate(); }} loading={confirmation.isPending}>生成一次性确认</Button> : <Button variant="danger" onClick={() => purge.mutate()} loading={purge.isPending} disabled={typedName !== selected?.entry_name}>确认彻底清理</Button>}</>}>
      {selected && <div className="form-stack"><div className="danger-note"><ShieldAlert size={20} /><div><strong>将永久删除节点上的隔离目录</strong><span>该目录已超过最低保留期。请输入完整隔离项名称“{selected.entry_name}”继续；文件无法从面板恢复。</span></div></div>{tokenValid ? <><Field label="隔离项名称确认"><input value={typedName} onChange={(event) => setTypedName(event.target.value)} autoComplete="off" /></Field><div className="form-note"><strong>确认有效期</strong><span>一次性确认将在 {dateTime(tokenExpiresAt)} 失效，确认值不会显示或保存在界面状态中。</span></div></> : <div className="form-note"><strong>二次确认</strong><span>先生成 5 分钟有效的一次性确认，再输入完整名称。生成动作和最终清理都会写入审计记录。</span></div>}</div>}
    </Modal>
  </section>;
}
