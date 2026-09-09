import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Bell, Braces, Check, CheckCircle2, ChevronDown, ChevronUp, Clipboard, DownloadCloud, KeyRound, LockKeyhole, Mail, Plus, RefreshCw,
  ShieldAlert, ShieldCheck, Smartphone, Trash2, UserCog, Webhook,
} from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { useLocation } from "react-router";
import { Button, Card, EmptyState, ErrorState, Field, LoadingBlock, Modal, PageHeader, StatusBadge } from "../components/ui";
import { FileUploadInput } from "../components/FileUploadInput";
import { useAuth } from "../context/AuthContext";
import { useToast } from "../context/ToastContext";
import { hasAnyPermission, hasGlobalPermission, hasInstancePermission, hasNodePermission, hasRootSecurityPermission, normalizePermissionsAtScope, permissionAllowedAtScope, permissionCatalog, rolePresetPermissions, tokenScopesForGlobal, tokenScopesForInstance, tokenScopesForNode } from "../lib/account-permissions";
import { api, ApiError } from "../lib/api";
import { dateTime } from "../lib/format";
import type { Permission, Role, RoleBinding, SystemUpdatePhase, User } from "../lib/types";

export function SettingsPage() {
  const auth = useAuth();
  const location = useLocation();
  const setupKeyIsInitialPassword = Boolean((location.state as { setupKeyIsInitialPassword?: boolean } | null)?.setupKeyIsInitialPassword);
  const canManageUsers = hasRootSecurityPermission(auth.user);
  const users = useQuery({ queryKey: ["users"], queryFn: api.users, enabled: canManageUsers });
  const [compact, setCompact] = useState(() => localStorage.getItem("l4d2-density") === "compact");
  const [totpOpen, setTotpOpen] = useState(false);
  const [tokenOpen, setTokenOpen] = useState(false);
  const [channelKind, setChannelKind] = useState<"smtp" | "webhook" | null>(null);
  const [managedUser, setManagedUser] = useState<User | "new" | null>(null);

  useEffect(() => {
    document.documentElement.dataset.density = compact ? "compact" : "comfortable";
    localStorage.setItem("l4d2-density", compact ? "compact" : "comfortable");
  }, [compact]);

  return <div>
    <PageHeader eyebrow="CONTROL SETTINGS" title="用户与设置" description="管理账号安全、访问权限、通知渠道和本机显示偏好。" />
    {setupKeyIsInitialPassword && <div className="alert-banner" role="alert"><KeyRound size={20} /><div><strong>初始化密钥也是 admin 的当前密码</strong><span>请立即使用下方“更改密码”设置新密码，并妥善删除此前保存的初始化密钥。</span></div></div>}
    <div className="settings-grid settings-grid--profile">
      <Card><div className="profile-card"><div className="avatar avatar--large">{(auth.user?.display_name || auth.user?.username || "管").slice(0, 1)}</div><div><span className="eyebrow">CURRENT ACCOUNT</span><h2>{auth.user?.display_name || auth.user?.username}</h2><p>@{auth.user?.username} · 上次登录 {dateTime(auth.user?.last_login_at)}</p></div><StatusBadge status={auth.user?.role || "viewer"} /></div></Card>
      <Card className="security-summary"><div><ShieldCheck size={22} /><span><strong>账号安全</strong><small>密码使用 Argon2id 哈希存储</small></span></div><div className={auth.user?.totp_enabled ? "good" : "warning"}><Smartphone size={20} /><span><strong>双重验证</strong><small>{auth.user?.totp_enabled ? "已启用 TOTP" : "建议尽快启用"}</small></span></div><Button variant="secondary" onClick={() => auth.user && setManagedUser(auth.user)} disabled={!canManageUsers} title={!canManageUsers ? "请联系所有者重置密码" : undefined}><LockKeyhole size={16} />更改密码</Button></Card>
    </div>

    <section className="settings-section">
      <div className="section-heading"><div><h2>团队成员</h2><p>只有所有者可查看和修改团队账号；资源范围由权限绑定与令牌范围共同收敛。</p></div>{canManageUsers && <Button onClick={() => setManagedUser("new")}><Plus size={16} />添加用户</Button>}</div>
      <Card>{!canManageUsers ? <EmptyState title="成员管理仅所有者可用" description="你的实例操作权限仍按当前角色正常生效。" /> : users.isPending ? <LoadingBlock label="正在读取用户…" /> : users.error ? users.error instanceof ApiError && [404, 501].includes(users.error.status) ? <EmptyState title="用户管理接口尚未开放" description="当前账号可正常使用。" /> : <ErrorState error={users.error} retry={() => void users.refetch()} /> : <div className="user-list">{users.data.items.map((user) => <div className={`user-row ${user.disabled ? "user-row--disabled" : ""}`} key={user.id}><div className="avatar">{(user.display_name || user.username).slice(0, 1)}</div><div><strong>{user.display_name || user.username}{user.bootstrap_owner ? " · 根所有者" : ""}</strong><span>@{user.username} · {bindingSummary(user.role_bindings)}</span></div><StatusBadge status={user.disabled ? "disabled" : user.account_state === "pending_activation" ? "pending_activation" : user.role} /><span>{user.account_state === "pending_activation" ? "等待首次登录" : user.totp_enabled ? <><CheckCircle2 size={15} />2FA</> : "未启用 2FA"}</span><time>{dateTime(user.last_login_at)}</time><Button variant="ghost" onClick={() => setManagedUser(user)}><UserCog size={16} />管理</Button></div>)}</div>}</Card>
    </section>

    <div className="settings-grid">
      <Card><div className="card-heading"><div><span className="eyebrow">PERSONAL SECURITY</span><h2>账号安全</h2></div><KeyRound size={21} /></div><div className="settings-list"><button onClick={() => setTotpOpen(true)}><span><Smartphone size={18} /><span><strong>TOTP 双重验证</strong><small>{auth.user?.totp_enabled ? "已启用；恢复码每个只能使用一次" : "使用认证器保护登录"}</small></span></span><em>{auth.user?.totp_enabled ? "查看状态" : "启用"}</em></button><button onClick={() => setTokenOpen(true)}><span><KeyRound size={18} /><span><strong>API Token</strong><small>创建带权限、资源范围与有效期的令牌</small></span></span><em>管理</em></button><button onClick={() => { if (window.confirm("确认退出当前登录会话？")) void auth.logout(); }}><span><LockKeyhole size={18} /><span><strong>登录会话</strong><small>撤销当前设备的 HttpOnly 会话 Cookie</small></span></span><em>退出</em></button></div></Card>
      <Card><div className="card-heading"><div><span className="eyebrow">NOTIFICATION DELIVERY</span><h2>通知渠道</h2></div><Bell size={21} /></div><div className="settings-list"><button onClick={() => setChannelKind("smtp")} disabled={!hasGlobalPermission(auth.user, "notifications.manage")}><span><Mail size={18} /><span><strong>SMTP 邮件</strong><small>强制 STARTTLS，凭据加密保存</small></span></span><em>配置</em></button><button onClick={() => setChannelKind("webhook")} disabled={!hasGlobalPermission(auth.user, "notifications.manage")}><span><Webhook size={18} /><span><strong>Webhook</strong><small>HMAC-SHA256 签名与持久化重试</small></span></span><em>配置</em></button></div></Card>
      <Card><div className="card-heading"><div><span className="eyebrow">DISPLAY</span><h2>界面偏好</h2></div><Braces size={21} /></div><label className="preference-row"><span><strong>紧凑布局</strong><small>在表格中显示更多信息</small></span><button type="button" role="switch" aria-checked={compact} className={`switch ${compact ? "switch--on" : ""}`} onClick={() => setCompact((value) => !value)}><span /></button></label><Field label="语言"><select value="zh-CN" disabled><option>简体中文</option></select></Field></Card>
    </div>
    {hasGlobalPermission(auth.user, "audit.read_global") && <GlobalAuditCard />}
    {canManageUsers && <CenterUpdateCard />}
    <TotpModal open={totpOpen} enabled={Boolean(auth.user?.totp_enabled)} onClose={() => setTotpOpen(false)} />
    <APITokenModal open={tokenOpen} user={auth.user || null} onClose={() => setTokenOpen(false)} />
    <NotificationChannelModal kind={channelKind} onClose={() => setChannelKind(null)} />
    <UserModal target={canManageUsers ? managedUser : null} actor={auth.user || null} onClose={() => setManagedUser(null)} />
  </div>;
}

function GlobalAuditCard() {
  const audit = useQuery({ queryKey: ["audit", "global"], queryFn: () => api.audit() });
  const [expanded, setExpanded] = useState(false);
  const items = audit.data?.items || [];
  const visibleItems = expanded ? items : items.slice(0, 5);
  return <section className="settings-section"><div className="section-heading"><div><h2>全局审计</h2><p>查看中心端所有资源的安全事件；正文已由服务端脱敏。</p></div>{!audit.isPending && <Button variant="secondary" onClick={() => void audit.refetch()}><RefreshCw size={16} />刷新</Button>}</div><Card>{audit.isPending ? <LoadingBlock label="正在读取全局审计记录…" /> : audit.error ? <ErrorState error={audit.error} retry={() => void audit.refetch()} /> : items.length ? <><div className="audit-timeline" id="global-audit-timeline">{visibleItems.map((event) => <div className="audit-event" key={event.id}><span className="audit-event__line" /><div className="audit-event__dot" /><time>{dateTime(event.created_at)}</time><div><strong>{event.actor_name || "系统"} · {event.action}</strong><span>{event.resource_type}{event.resource_id ? ` #${event.resource_id.slice(0, 8)}` : ""}{event.remote_ip ? ` · ${event.remote_ip}` : ""}</span>{event.metadata && Object.keys(event.metadata).length > 0 && <code>{JSON.stringify(event.metadata)}</code>}</div></div>)}</div>{items.length > 5 && <div className="audit-timeline__footer"><span>共 {items.length} 条审计记录</span><Button variant="ghost" aria-expanded={expanded} aria-controls="global-audit-timeline" onClick={() => setExpanded((value) => !value)}>{expanded ? <ChevronUp size={15} /> : <ChevronDown size={15} />}{expanded ? "收起，仅显示最近 5 条" : `展开其余 ${items.length - 5} 条`}</Button></div>}</> : <EmptyState title="还没有全局审计记录" description="登录、权限和资源写操作会保留不可编辑的安全记录。" />}</Card></section>;
}

const updatePhaseLabels: Record<SystemUpdatePhase, string> = {
  validating: "正在校验升级包",
  stopping: "正在停止中心端服务",
  backing_up: "正在创建完整快照",
  installing: "正在替换程序文件",
  migrating: "正在迁移数据库",
  starting: "正在启动新版本",
  health_check: "正在执行健康检查",
  rolling_back: "升级失败，正在自动回滚",
};

const successfulUpdateReloadDelayMs = 1000;
const dockerReleaseVersionPattern = /^v?\d+\.\d+\.\d+$/;
const windowsReleaseVersionPattern = /^v?\d+\.\d+\.\d+(?:-([0-9A-Za-z.-]+))?$/;

function isOrderedWindowsVersion(value: string) {
  const match = windowsReleaseVersionPattern.exec(value);
  return Boolean(match && !(match[1]?.split(".").some((identifier) => !identifier || (/^\d+$/.test(identifier) && identifier.length > 1 && identifier.startsWith("0")))));
}

function CenterUpdateCard() {
  const toast = useToast();
  const client = useQueryClient();
  const overview = useQuery({ queryKey: ["system-updates"], queryFn: api.systemUpdates, retry: 1 });
  const [file, setFile] = useState<File | null>(null);
  const [uploadProgress, setUploadProgress] = useState(0);
  const [jobID, setJobID] = useState("");
  const handledTerminalJobID = useRef("");
  const activeID = jobID || overview.data?.active?.job_id || "";
  const current = useQuery({
    queryKey: ["system-update", activeID],
    queryFn: () => api.systemUpdate(activeID),
    enabled: Boolean(activeID),
    retry: true,
    retryDelay: 1000,
    refetchInterval: (query) => ["succeeded", "failed", "canceled"].includes(query.state.data?.status || "") ? false : 1000,
  });
  const dockerDeployment = overview.data?.deployment === "docker-compose";
  const packageFormat = overview.data?.package_format || "zip";
  const currentVersion = overview.data?.current_version || "";
  const currentVersionIsOrdered = dockerDeployment ? dockerReleaseVersionPattern.test(currentVersion) : isOrderedWindowsVersion(currentVersion);
  const upload = useMutation({
    mutationFn: async () => {
      if (!file) throw new Error(`请选择 .${packageFormat} 升级包`);
      return api.uploadSystemUpdate(file, setUploadProgress);
    },
    onSuccess: (result) => {
      setJobID(result.job_id);
      setUploadProgress(100);
      toast.push("升级包已校验，中心端将短暂离线");
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const state = current.data || overview.data?.active;

  useEffect(() => {
    if (!state || !["succeeded", "failed", "canceled"].includes(state.status) || handledTerminalJobID.current === state.job_id) return;
    handledTerminalJobID.current = state.job_id;
    void client.invalidateQueries({ queryKey: ["system-updates"] });
    if (state.status === "succeeded") {
      toast.push(`中心端已升级到 ${state.target_version || "新版本"}，页面即将刷新`);
      window.setTimeout(() => window.location.reload(), successfulUpdateReloadDelayMs);
    } else {
      toast.push(state.rollback === "succeeded" ? "升级失败，已完整恢复旧版本" : "升级及自动回滚失败，请按恢复文档处理", "error");
    }
    setJobID("");
    setFile(null);
  }, [client, state?.job_id, state?.status]);

  if (overview.isPending || overview.error || !overview.data?.supported) return null;
  const busy = upload.isPending || Boolean(state && ["queued", "running"].includes(state.status));
  const history = overview.data.history || [];
  return <section className="settings-section center-update-section">
    <div className="section-heading"><div><h2>中心端升级</h2><p>当前版本 {overview.data.current_version} · {dockerDeployment ? "仅接受与 Docker 主机架构匹配的中心端 .tar.gz 镜像包" : "仅接受可信来源的 Windows x64 便携 ZIP"}，不验证发布者签名。</p></div><DownloadCloud size={22} /></div>
    <Card><div className="center-update-panel">
      <div className="center-update-picker">
        <Field label="升级包" hint={`${!currentVersionIsOrdered ? "开发版本可替换为其他开发构建或正式版本" : "允许升级或同版本重装，禁止降级"}；最大 ${dockerDeployment ? "1 GiB" : "512 MiB"}`}><FileUploadInput label="升级包" accept={packageFormat === "tar.gz" ? ".tar.gz,application/gzip,application/x-gzip" : ".zip,application/zip"} minBytes={1} maxBytes={dockerDeployment ? 1024 ** 3 : 512 * 1024 ** 2} fileName={file?.name || ""} disabled={busy} onFile={(selected) => { setFile(selected); setUploadProgress(0); }} /></Field>
        <Button disabled={!file || busy} loading={upload.isPending} onClick={() => { if (file && window.confirm(`确认使用“${file.name}”升级中心端？${dockerDeployment ? "容器镜像将获得 Docker Socket 的宿主机高权限；" : ""}服务会短暂离线，失败时将自动完整回滚。`)) upload.mutate(); }}><RefreshCw size={16} />一键升级</Button>
      </div>
      {upload.isPending && <div className="center-update-progress" role="progressbar" aria-valuenow={uploadProgress}><span style={{ width: `${uploadProgress}%` }} /><small>正在上传 {uploadProgress}%</small></div>}
      {state && ["queued", "running"].includes(state.status) && <div className="center-update-running"><span className="spinner" /><div><strong>{updatePhaseLabels[state.phase] || state.phase}</strong><small>页面会等待服务恢复；升级成功后将自动刷新，请勿关闭页面或操作中心端容器。</small></div></div>}
      {state?.status === "failed" && <div className="form-warning">{state.rollback === "succeeded" ? "升级失败，旧版本和数据库已经恢复。" : "自动回滚失败，服务可能保持停止。"}{state.error ? ` ${state.error}` : ""}</div>}
      {history.length > 0 && <div className="center-update-history"><strong>最近升级</strong>{history.slice(0, 3).map((item) => <div key={item.job_id}><StatusBadge status={item.status} /><span>{item.current_version} → {item.target_version || "未知"}</span><time>{dateTime(item.completed_at || item.updated_at)}</time><small>{item.status === "failed" ? `回滚：${item.rollback === "succeeded" ? "成功" : "失败"}` : "健康检查通过"}</small>{item.status === "failed" && item.error && <code className="center-update-history__error" title={item.error}>失败诊断：{item.error}</code>}</div>)}</div>}
    </div></Card>
  </section>;
}

type EditableBinding = Pick<RoleBinding, "role" | "permissions" | "scope_type" | "node_id" | "instance_id">;

function UserModal({ target, actor, onClose }: { target: User | "new" | null; actor: User | null; onClose: () => void }) {
  const open = target != null;
  const creating = target === "new";
  const existing = !creating && target ? target : null;
  const client = useQueryClient();
  const toast = useToast();
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: api.nodes, enabled: open });
  const instances = useQuery({ queryKey: ["instances"], queryFn: api.instances, enabled: open });
  const [username, setUsername] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const [disabled, setDisabled] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [deleteConfirmation, setDeleteConfirmation] = useState("");
  const [bindings, setBindings] = useState<EditableBinding[]>([{ role: "viewer", permissions: rolePresetPermissions("viewer"), scope_type: "global" }]);
  const [issuedCredential, setIssuedCredential] = useState<{ username: string; oneTimePassword: string; expiresAt: string } | null>(null);

  useEffect(() => {
    if (!target) return;
    setUsername(creating ? "" : existing?.username || "");
    setDisplayName(creating ? "" : existing?.display_name || "");
    setPassword("");
    setDeleting(false);
    setDeleteConfirmation("");
    setIssuedCredential(null);
    setDisabled(Boolean(existing?.disabled));
    setBindings(creating ? [{ role: "viewer", permissions: rolePresetPermissions("viewer"), scope_type: "global" }] : existing?.role_bindings === undefined ? [{ role: existing?.role || "viewer", permissions: rolePresetPermissions(existing?.role || "viewer"), scope_type: "global" }] : existing.role_bindings.map(cleanBinding));
  }, [creating, existing, target]);

  const normalizedBindings = bindings.map(cleanBinding);
  const bindingKeys = normalizedBindings.map((item) => `${item.scope_type}:${item.node_id || item.instance_id || "global"}`);
  const originalBindings: EditableBinding[] = existing?.role_bindings === undefined
    ? existing ? [{ role: existing.role, permissions: rolePresetPermissions(existing.role), scope_type: "global" }] : []
    : existing.role_bindings.map(cleanBinding);
  const emptyBindingsInvalid = normalizedBindings.length === 0 && (creating || originalBindings.length > 0);
  const bindingsValid = !emptyBindingsInvalid && normalizedBindings.every((item) => Boolean(item.permissions?.length) && (item.scope_type === "global" || (item.scope_type === "node" ? Boolean(item.node_id) : Boolean(item.instance_id)))) && new Set(bindingKeys).size === bindingKeys.length;
  const bindingsChanged = existing ? JSON.stringify(normalizedBindings) !== JSON.stringify(originalBindings) : true;
  const changed = creating || displayName.trim() !== existing?.display_name || Boolean(password) || disabled !== Boolean(existing?.disabled) || bindingsChanged;
  const valid = username.trim() && displayName.trim() && (!password || password.length >= 12) && bindingsValid && changed;

  const save = useMutation({
    mutationFn: async () => {
      if (creating) return api.createUser({ username: username.trim(), display_name: displayName.trim(), role_bindings: normalizedBindings });
      if (!existing) throw new Error("用户无效");
      const payload: Parameters<typeof api.updateUser>[1] = {};
      if (displayName.trim() !== existing.display_name) payload.display_name = displayName.trim();
      if (password) payload.password = password;
      if (disabled !== Boolean(existing.disabled)) payload.disabled = disabled;
      if (bindingsChanged) payload.role_bindings = normalizedBindings;
      return api.updateUser(existing.id, payload);
    },
    onSuccess: (result) => {
      void client.invalidateQueries({ queryKey: ["users"] });
      if (creating && "one_time_password" in result) {
        setIssuedCredential({ username: username.trim(), oneTimePassword: result.one_time_password, expiresAt: result.expires_at });
        toast.push("团队账号已创建，请立即复制一次性密码");
        return;
      }
      toast.push(existing?.id === actor?.id && (password || bindingsChanged) ? "账号已更新，请重新登录" : "团队账号已更新");
      if (existing?.id === actor?.id && (password || bindingsChanged)) void client.invalidateQueries({ queryKey: ["session"] });
      onClose();
    },
    onError: (error) => toast.push(error.message, "error"),
  });

  const reissue = useMutation({
    mutationFn: () => existing ? api.reissueUserActivation(existing.id) : Promise.reject(new Error("用户无效")),
    onSuccess: (result) => {
      setIssuedCredential({ username: existing?.username || "", oneTimePassword: result.one_time_password, expiresAt: result.expires_at });
      toast.push("新的一次性密码已生成，旧密码已经失效");
    },
    onError: (error) => toast.push(error.message, "error"),
  });

  const updateBinding = (index: number, value: EditableBinding) => setBindings((items) => items.map((item, itemIndex) => itemIndex === index ? cleanBinding(value) : item));
  const remove = useMutation({
    mutationFn: () => existing ? api.deleteUser(existing.id) : Promise.reject(new Error("用户无效")),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["users"] });
      void client.invalidateQueries({ queryKey: ["audit"] });
      toast.push("账号已永久删除");
      onClose();
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  const busy = save.isPending || reissue.isPending || remove.isPending;
  const protectedOwner = Boolean(existing?.bootstrap_owner);
  const deleteProtected = protectedOwner || existing?.id === actor?.id;
  const availableRoles: Role[] = actor?.role === "owner" ? ["owner", "admin", "operator", "viewer"] : ["admin", "operator", "viewer"];
  const close = () => {
    if (busy) return;
    if (issuedCredential && !window.confirm("一次性密码关闭后无法再次查看。确认已经妥善复制？")) return;
    setIssuedCredential(null);
    onClose();
  };
  if (deleting && existing) return <Modal open={open} onClose={close} title="永久删除账号" description="删除后无法恢复，请核对目标账号。" footer={<><Button variant="secondary" disabled={busy} onClick={() => setDeleting(false)}>返回</Button><Button variant="danger" loading={remove.isPending} disabled={busy || deleteProtected || deleteConfirmation !== existing.username} onClick={() => remove.mutate()}><Trash2 size={14} />确认永久删除</Button></>}>
    <div className="form-stack"><div className="form-warning">将永久删除 {existing.display_name || existing.username}（{existing.username}）的账号、登录凭据和权限绑定，并立即撤销全部会话与 API Token。游戏资源、任务和历史审计记录会保留。</div><Field label="输入用户名以确认删除" hint={existing.username}><input aria-label="输入用户名以确认删除" value={deleteConfirmation} onChange={(event) => setDeleteConfirmation(event.target.value)} disabled={busy} autoComplete="off" /></Field></div>
  </Modal>;
  if (issuedCredential) return <Modal open={open} onClose={close} title="一次性密码已生成" description="请通过可信渠道交给用户；该密码关闭后无法再次查看。" footer={<Button onClick={close}>我已妥善保存</Button>}>
    <div className="activation-credential"><div><span>用户名</span><code>{issuedCredential.username}</code></div><div><span>一次性密码</span><code>{issuedCredential.oneTimePassword}</code></div><Button variant="secondary" onClick={() => void navigator.clipboard.writeText(`用户名：${issuedCredential.username}\n一次性密码：${issuedCredential.oneTimePassword}`)}><Clipboard size={15} />复制账号信息</Button><small>有效期至 {dateTime(issuedCredential.expiresAt)}。首次登录后只能设置新密码，不能访问其他功能。</small></div>
  </Modal>;
  return <Modal open={open} onClose={close} title={creating ? "添加团队成员" : `管理 ${existing?.display_name || existing?.username || "用户"}`} description="角色用于一键填充预设，实际授权以每个范围勾选的具体权限为准。" footer={<>{existing && <Button variant="danger" disabled={busy || deleteProtected} title={protectedOwner ? "首启根所有者不可删除" : existing.id === actor?.id ? "不能删除当前登录账号" : undefined} onClick={() => { setDeleteConfirmation(""); setDeleting(true); }}><Trash2 size={14} />删除账号</Button>}{existing?.account_state === "pending_activation" && <Button variant="secondary" onClick={() => reissue.mutate()} loading={reissue.isPending} disabled={busy}><RefreshCw size={14} />重新生成一次性密码</Button>}<Button variant="secondary" onClick={close} disabled={busy}>取消</Button><Button onClick={() => save.mutate()} loading={save.isPending} disabled={!valid || busy}>{creating ? "创建账号并生成密码" : "保存更改"}</Button></>}>
    <div className="form-stack"><div className="form-grid"><Field label="用户名"><input value={username} onChange={(event) => setUsername(event.target.value)} disabled={!creating} maxLength={128} autoComplete="username" /></Field><Field label="显示名称"><input value={displayName} onChange={(event) => setDisplayName(event.target.value)} maxLength={128} /></Field></div>{creating ? <div className="setup-notice">账号创建后由系统生成一次性密码，有效期 24 小时；管理员不需要也不能指定用户的正式密码。</div> : existing?.account_state === "pending_activation" ? <div className="setup-notice">该账号尚未完成首次登录。可以重新生成一次性密码，旧密码会立即失效。</div> : <Field label="重置密码（可留空）" hint="至少 12 个字符；修改密码会撤销该账号的全部会话和 API Token"><input type="password" value={password} onChange={(event) => setPassword(event.target.value)} minLength={12} maxLength={1024} autoComplete="new-password" /></Field>}{!creating && <label className={`risk-check ${protectedOwner || existing?.id === actor?.id ? "risk-check--disabled" : ""}`}><input type="checkbox" checked={disabled} onChange={(event) => setDisabled(event.target.checked)} disabled={protectedOwner || existing?.id === actor?.id} /><LockKeyhole size={18} /><span><strong>停用账号</strong><small>{protectedOwner ? "首启根所有者不可停用" : existing?.id === actor?.id ? "不能停用当前登录账号" : "立即撤销该账号的全部会话和 API Token"}</small></span></label>}<div className="binding-editor"><div className="binding-editor__heading"><div><strong>资源权限绑定</strong><span>匹配范围的权限取并集；不支持显式拒绝</span></div><Button type="button" variant="secondary" onClick={() => setBindings((items) => [...items, { role: "viewer", permissions: rolePresetPermissions("viewer").filter((permission) => permissionAllowedAtScope(permission, "instance")), scope_type: "instance" }])} disabled={protectedOwner || bindings.length >= 50}><Plus size={14} />添加范围</Button></div>{bindings.length === 0 && <div className="form-warning">{emptyBindingsInvalid ? "至少保留一条权限绑定；如需撤销访问，请停用账号或调整为最小权限。" : "该账号当前没有权限绑定，因此不能访问任何受保护资源。可添加范围后再授予权限。"}</div>}{bindings.map((binding, index) => <div className="binding-block" key={`${index}-${binding.scope_type}`}><div className="binding-row"><Field label="角色预设"><select value={binding.role} disabled={protectedOwner} onChange={(event) => { const role = event.target.value as Role; const scope = role === "owner" ? "global" : binding.scope_type; updateBinding(index, { ...binding, role, scope_type: scope, permissions: rolePresetPermissions(role).filter((permission) => permissionAllowedAtScope(permission, scope)) }); }}>{availableRoles.map((role) => <option value={role} key={role}>{roleName(role)}</option>)}</select></Field><Field label="范围"><select value={binding.scope_type} disabled={protectedOwner || binding.role === "owner"} onChange={(event) => { const scope = event.target.value as EditableBinding["scope_type"]; updateBinding(index, { ...binding, scope_type: scope, permissions: (binding.permissions || []).filter((permission) => permissionAllowedAtScope(permission, scope)) }); }}><option value="global">全局</option><option value="node">单个节点</option><option value="instance">单个实例</option></select></Field>{binding.scope_type === "node" ? <Field label="节点"><select value={binding.node_id || ""} disabled={protectedOwner} onChange={(event) => updateBinding(index, { ...binding, node_id: event.target.value })}><option value="">请选择节点</option>{(nodes.data?.items || []).map((item) => <option value={item.id} key={item.id}>{item.name}</option>)}</select></Field> : binding.scope_type === "instance" ? <Field label="实例"><select value={binding.instance_id || ""} disabled={protectedOwner} onChange={(event) => updateBinding(index, { ...binding, instance_id: event.target.value })}><option value="">请选择实例</option>{(instances.data?.items || []).map((item) => <option value={item.id} key={item.id}>{item.name}</option>)}</select></Field> : <div className="binding-global-note"><ShieldCheck size={16} />适用于全部节点与实例</div>}<Button type="button" variant="danger" aria-label="删除权限绑定" onClick={() => setBindings((items) => items.filter((_, itemIndex) => itemIndex !== index))} disabled={protectedOwner}><Trash2 size={14} /></Button></div><PermissionCheckboxes binding={binding} disabled={protectedOwner || binding.role === "owner"} onChange={(permissions) => updateBinding(index, { ...binding, permissions })} /></div>)}{bindings.length > 0 && !bindingsValid && <div className="form-warning">每条绑定至少选择一个权限，并选择对应资源；同一资源不能重复绑定。</div>}</div></div>
  </Modal>;
}

function cleanBinding(binding: EditableBinding): EditableBinding {
  const permissions = normalizePermissionsAtScope(binding.permissions === undefined ? rolePresetPermissions(binding.role) : binding.permissions, binding.scope_type);
  if (binding.scope_type === "node") return { role: binding.role, permissions, scope_type: "node", node_id: binding.node_id || "" };
  if (binding.scope_type === "instance") return { role: binding.role, permissions, scope_type: "instance", instance_id: binding.instance_id || "" };
  return { role: binding.role, permissions, scope_type: "global" };
}

function PermissionCheckboxes({ binding, disabled, onChange }: { binding: EditableBinding; disabled: boolean; onChange: (permissions: Permission[]) => void }) {
  const available = permissionCatalog.filter((item) => item.id !== "root_security" && permissionAllowedAtScope(item.id, binding.scope_type));
  const selected = new Set(binding.permissions || []);
  const preset = rolePresetPermissions(binding.role).filter((permission) => permission !== "root_security" && permissionAllowedAtScope(permission, binding.scope_type)).sort();
  const customized = JSON.stringify([...selected].sort()) !== JSON.stringify(preset);
  const groups = [...new Set(available.map((item) => item.group))];
  const baseRequired = (permission: Permission) => {
    if (permission === "nodes.read") return available.some((item) => item.class !== "read" && selected.has(item.id) && (item.scope === "node" || binding.scope_type === "node"));
    if (permission === "instances.read") return available.some((item) => item.class !== "read" && selected.has(item.id) && (item.scope === "instance" || binding.scope_type === "instance"));
    return false;
  };
  const toggle = (permission: Permission, checked: boolean) => {
    const next = new Set(selected);
    if (checked) {
      next.add(permission);
      const definition = permissionCatalog.find((item) => item.id === permission);
      if (definition?.class !== "read" && (definition?.scope === "node" || binding.scope_type === "node") && permissionAllowedAtScope("nodes.read", binding.scope_type)) next.add("nodes.read");
      if (definition?.class !== "read" && (definition?.scope === "instance" || binding.scope_type === "instance") && permissionAllowedAtScope("instances.read", binding.scope_type)) next.add("instances.read");
    } else next.delete(permission);
    onChange(normalizePermissionsAtScope([...next], binding.scope_type));
  };
  return <div className="permission-editor"><div className="permission-editor__summary"><span><strong>{selected.size} 项权限</strong>{customized && <em>已自定义</em>}</span><Button type="button" variant="ghost" disabled={disabled} onClick={() => onChange(preset)}>恢复角色预设</Button></div>{groups.map((group) => <fieldset key={group}><legend>{group}</legend><div className="permission-grid">{available.filter((item) => item.group === group).map((item) => <label key={item.id} title={item.description}><input type="checkbox" checked={selected.has(item.id)} disabled={disabled || baseRequired(item.id)} onChange={(event) => toggle(item.id, event.target.checked)} /><span><strong>{item.label}</strong><small>{item.description || item.id}</small></span></label>)}</div></fieldset>)}</div>;
}

function roleName(role: Role) {
  return role === "owner" ? "所有者" : role === "admin" ? "管理员" : role === "operator" ? "操作员" : "观察者";
}

function bindingSummary(bindings?: RoleBinding[]) {
  if (bindings === undefined) return "全局角色";
  if (bindings.length === 0) return "无权限绑定";
  const global = bindings.find((item) => item.scope_type === "global");
  const customized = bindings.some((binding) => binding.permissions !== undefined
    && JSON.stringify([...binding.permissions].sort()) !== JSON.stringify(rolePresetPermissions(binding.role).filter((permission) => permissionAllowedAtScope(permission, binding.scope_type)).sort()));
  const suffix = customized ? " · 自定义权限" : "";
  return global ? `${roleName(global.role)} · 全局${suffix}` : `${bindings.length} 个资源范围${suffix}`;
}

function NotificationChannelModal({ kind, onClose }: { kind: "smtp" | "webhook" | null; onClose: () => void }) {
  const client = useQueryClient();
  const toast = useToast();
  const channels = useQuery({ queryKey: ["notification-channels"], queryFn: api.notificationChannels, enabled: Boolean(kind) });
  const [name, setName] = useState("");
  const [secret, setSecret] = useState("");
  const [url, setURL] = useState("");
  const [host, setHost] = useState("");
  const [port, setPort] = useState(587);
  const [username, setUsername] = useState("");
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const create = useMutation({
    mutationFn: () => {
      if (!kind) throw new Error("通知类型无效");
      const config: Record<string, unknown> = kind === "webhook" ? { url } : { host, port, username, from, to: to.split(",").map((item) => item.trim()).filter(Boolean) };
      return api.createNotificationChannel({ name: name.trim(), kind, enabled: true, config, secret });
    },
    onSuccess: () => { toast.push("通知渠道已创建"); void client.invalidateQueries({ queryKey: ["notification-channels"] }); setName(""); setSecret(""); },
    onError: (error) => toast.push(error.message, "error"),
  });
  const remove = useMutation({
    mutationFn: api.deleteNotificationChannel,
    onSuccess: () => { toast.push("通知渠道已删除，待发送记录已跳过"); void client.invalidateQueries({ queryKey: ["notification-channels"] }); },
    onError: (error) => toast.push(error.message, "error"),
  });
  useEffect(() => {
    if (!kind) return;
    setName(kind === "smtp" ? "管理员邮件" : "运维 Webhook");
    setSecret(""); setURL(""); setHost(""); setPort(587); setUsername(""); setFrom(""); setTo("");
  }, [kind]);
  const matching = (channels.data?.items || []).filter((item) => item.kind === kind);
  const valid = kind === "webhook" ? name.trim() && /^https:\/\//i.test(url) && secret.length >= 16 : name.trim() && host && from && to;
  return <Modal open={Boolean(kind)} onClose={onClose} title={kind === "smtp" ? "SMTP 邮件渠道" : "Webhook 渠道"} description={kind === "smtp" ? "只连接支持 STARTTLS 的 SMTP 服务；密码以部署主密钥加密。" : "只允许无查询参数的 HTTPS 地址；每次投递附带时间戳和 HMAC-SHA256 签名。"} footer={<><Button variant="secondary" onClick={onClose}>关闭</Button><Button onClick={() => create.mutate()} loading={create.isPending} disabled={!valid}><Plus size={16} />添加渠道</Button></>}>
    <div className="form-stack"><Field label="渠道名称"><input value={name} onChange={(event) => setName(event.target.value)} maxLength={128} /></Field>{kind === "webhook" ? <><Field label="HTTPS 地址" hint="禁止凭据、query、fragment 和重定向"><input type="url" value={url} onChange={(event) => setURL(event.target.value)} placeholder="https://ops.example.com/l4d2-alerts" /></Field><Field label="签名密钥" hint="至少 16 个字符，仅加密保存"><input type="password" value={secret} onChange={(event) => setSecret(event.target.value)} autoComplete="new-password" /></Field></> : <><div className="form-grid"><Field label="SMTP 主机"><input value={host} onChange={(event) => setHost(event.target.value)} placeholder="smtp.example.com" /></Field><Field label="端口"><input type="number" min={1} max={65535} value={port} onChange={(event) => setPort(Number(event.target.value))} /></Field></div><Field label="用户名"><input value={username} onChange={(event) => setUsername(event.target.value)} autoComplete="username" /></Field><Field label="密码 / SMTP Token"><input type="password" value={secret} onChange={(event) => setSecret(event.target.value)} autoComplete="new-password" /></Field><div className="form-grid"><Field label="发件地址"><input type="email" value={from} onChange={(event) => setFrom(event.target.value)} placeholder="panel@example.com" /></Field><Field label="收件地址" hint="多个地址用逗号分隔"><input value={to} onChange={(event) => setTo(event.target.value)} placeholder="ops@example.com" /></Field></div></>}<div className="channel-list"><strong>已配置的同类渠道</strong>{channels.isPending ? <LoadingBlock /> : matching.length ? matching.map((item) => <div className="channel-row" key={item.id}><span className={`health-dot health-dot--${item.enabled ? "online" : "offline"}`} /><div><strong>{item.name}</strong><small>{item.kind === "webhook" ? String(item.config.url || "") : `${String(item.config.host || "")}:${String(item.config.port || 587)}`} · {item.has_secret ? "凭据已加密" : "无凭据"}</small></div><Button variant="danger" onClick={() => { if (window.confirm(`确认删除“${item.name}”？`)) remove.mutate(item.id); }} loading={remove.isPending}><Trash2 size={14} />删除</Button></div>) : <span>尚未配置</span>}</div></div>
  </Modal>;
}

function TotpModal({ open, enabled, onClose }: { open: boolean; enabled: boolean; onClose: () => void }) {
  const client = useQueryClient();
  const toast = useToast();
  const [setup, setSetup] = useState<{ secret: string; otpauth_url: string } | null>(null);
  const [code, setCode] = useState("");
  const [recoveryCodes, setRecoveryCodes] = useState<string[]>([]);
  const [copied, setCopied] = useState(false);
  const begin = useMutation({ mutationFn: api.setupTotp, onSuccess: setSetup, onError: (error) => toast.push(error.message, "error") });
  const confirm = useMutation({
    mutationFn: () => api.confirmTotp(code),
    onSuccess: (result) => {
      setRecoveryCodes(result.recovery_codes || []);
      void client.invalidateQueries({ queryKey: ["session"] });
      toast.push("双重验证已启用，请立即保存恢复码");
    },
    onError: (error) => toast.push(error.message, "error"),
  });
  useEffect(() => { if (!open) { setSetup(null); setCode(""); setRecoveryCodes([]); setCopied(false); } }, [open]);

  const copy = async (value: string) => {
    await navigator.clipboard.writeText(value);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1800);
  };
  const close = () => {
    if (recoveryCodes.length && !window.confirm("恢复码关闭后不会再次显示。确认已经保存？")) return;
    onClose();
  };
  return <Modal open={open} onClose={close} title="TOTP 双重验证" description="认证器密钥和恢复码只在当前流程中显示。" footer={!enabled && !setup ? <Button onClick={() => begin.mutate()} loading={begin.isPending}><Smartphone size={16} />生成认证器密钥</Button> : setup && !recoveryCodes.length ? <Button onClick={() => confirm.mutate()} loading={confirm.isPending} disabled={!/^\d{6}$/.test(code)}><Check size={16} />验证并启用</Button> : <Button variant="secondary" onClick={close}>完成</Button>}>
    {enabled && !setup ? <div className="security-success"><CheckCircle2 /><div><strong>TOTP 已启用</strong><span>登录时可使用 6 位动态码；此前保存的恢复码每个只能使用一次。系统不会再次显示旧恢复码。</span></div></div> : begin.isPending ? <LoadingBlock label="正在生成密钥…" /> : !setup ? <div className="form-note"><strong>开始前请准备认证器</strong><span>支持标准 TOTP 的应用均可使用。启用后会生成 10 个一次性恢复码。</span></div> : recoveryCodes.length ? <div className="form-stack"><div className="form-warning"><ShieldCheck size={18} />这是恢复码唯一一次明文显示，请保存到密码管理器。</div><div className="recovery-code-grid">{recoveryCodes.map((item) => <code key={item}>{item}</code>)}</div><Button variant="secondary" onClick={() => void copy(recoveryCodes.join("\n"))}>{copied ? <Check size={16} /> : <Clipboard size={16} />}{copied ? "已复制" : "复制全部恢复码"}</Button></div> : <div className="form-stack"><Field label="认证器密钥" hint="无法扫描时，在认证器中手工输入"><div className="secret-line"><code>{setup.secret}</code><Button type="button" variant="ghost" onClick={() => void copy(setup.secret)}><Clipboard size={15} />复制</Button></div></Field><details className="technical-details"><summary>显示 otpauth 地址</summary><code>{setup.otpauth_url}</code></details><Field label="6 位动态验证码"><input autoFocus inputMode="numeric" pattern="[0-9]{6}" maxLength={6} value={code} onChange={(event) => setCode(event.target.value.replace(/\D/g, ""))} placeholder="000000" /></Field></div>}
  </Modal>;
}

function APITokenModal({ open, user, onClose }: { open: boolean; user: User | null; onClose: () => void }) {
  const toast = useToast();
  const client = useQueryClient();
  const tokens = useQuery({ queryKey: ["api-tokens"], queryFn: api.apiTokens, enabled: open });
  const canReadNodes = hasAnyPermission(user, "nodes.read");
  const canReadInstances = hasAnyPermission(user, "instances.read");
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: api.nodes, enabled: open && canReadNodes });
  const instances = useQuery({ queryKey: ["instances"], queryFn: api.instances, enabled: open && canReadInstances });
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const globalScopes = useMemo(() => tokenScopesForGlobal(user), [user]);
  const [scope, setScope] = useState<"read" | "operate" | "manage">("read");
  const [resource, setResource] = useState<"global" | "node" | "instance">("global");
  const [resourceID, setResourceID] = useState("");
  const [days, setDays] = useState(90);
  const [secret, setSecret] = useState("");
  const nodeOptions = useMemo(() => (nodes.data?.items || []).filter((item) => hasNodePermission(user, item.id, "nodes.read") && tokenScopesForNode(user, item.id).length > 0), [nodes.data, user]);
  const instanceOptions = useMemo(() => (instances.data?.items || []).filter((item) => hasInstancePermission(user, item, "instances.read") && tokenScopesForInstance(user, item).length > 0), [instances.data, user]);
  const selectedInstance = instanceOptions.find((item) => item.id === resourceID);
  const allowedScopes = resource === "global" ? globalScopes
    : resource === "node" && resourceID ? tokenScopesForNode(user, resourceID)
      : resource === "instance" && selectedInstance ? tokenScopesForInstance(user, selectedInstance) : [];
  const canDelegate = globalScopes.length > 0 || nodeOptions.length > 0 || instanceOptions.length > 0;
  const delegateOptionsLoading = (canReadNodes && nodes.isPending) || (canReadInstances && instances.isPending);
  const create = useMutation({
    mutationFn: () => {
      if (!allowedScopes.includes(scope)) throw new Error("当前资源范围不能委派所选权限");
      return api.createApiToken({
        name: name.trim(), scopes: [scope],
        node_id: resource === "node" ? resourceID : undefined,
        instance_id: resource === "instance" ? resourceID : undefined,
        expires_at: new Date(Date.now() + days * 86_400_000).toISOString(),
      });
    },
    onSuccess: (result) => { setSecret(result.token); void client.invalidateQueries({ queryKey: ["api-tokens"] }); toast.push("API Token 已创建，只会显示一次"); },
    onError: (error) => toast.push(error.message, "error"),
  });
  const revoke = useMutation({ mutationFn: api.revokeApiToken, onSuccess: () => { void client.invalidateQueries({ queryKey: ["api-tokens"] }); toast.push("API Token 已撤销"); }, onError: (error) => toast.push(error.message, "error") });
  useEffect(() => { if (!open) { setCreating(false); setName(""); setResource("global"); setResourceID(""); setSecret(""); } }, [open]);
  useEffect(() => {
    if (!open || resource !== "global" || globalScopes.length) return;
    if (canReadNodes) setResource("node");
    else if (canReadInstances) setResource("instance");
  }, [canReadInstances, canReadNodes, globalScopes.length, open, resource]);
  useEffect(() => {
    if (allowedScopes.length && !allowedScopes.includes(scope)) setScope(allowedScopes[allowedScopes.length - 1]);
  }, [allowedScopes, scope]);

  const close = () => {
    if (secret && !window.confirm("令牌关闭后不会再次显示。确认已经保存？")) return;
    onClose();
  };
  const resourceOptions = resource === "node" ? nodeOptions : instanceOptions;
  return <Modal open={open} onClose={close} title="API Token" description="令牌受账号角色、权限范围、资源范围和到期时间四重限制。" footer={<><Button variant="secondary" onClick={close}>关闭</Button>{!creating && !secret && <Button onClick={() => setCreating(true)} disabled={!canDelegate} title={!canDelegate ? "当前账号没有可委派权限" : undefined}><Plus size={16} />新建令牌</Button>}{creating && !secret && <Button onClick={() => create.mutate()} loading={create.isPending} disabled={!canDelegate || !allowedScopes.includes(scope) || !name.trim() || (resource !== "global" && !resourceID)}>创建</Button>}</>}>
    {!canDelegate && !delegateOptionsLoading && <div className="form-warning"><ShieldAlert size={18} />当前账号没有可委派权限，不能创建新的 API Token；已有令牌仍可在下方撤销。</div>}
    {secret ? <div className="form-stack"><div className="form-warning"><ShieldCheck size={18} />令牌只显示这一次，请立即复制到安全位置。</div><div className="token-secret"><code>{secret}</code><Button variant="secondary" onClick={() => void navigator.clipboard.writeText(secret)}><Clipboard size={16} />复制</Button></div></div> : creating ? <div className="form-stack"><Field label="名称"><input autoFocus value={name} onChange={(event) => setName(event.target.value)} maxLength={100} placeholder="自动化维护脚本" /></Field><div className="form-grid"><Field label="权限范围"><select value={scope} disabled={!allowedScopes.length} onChange={(event) => setScope(event.target.value as typeof scope)}>{!allowedScopes.length && <option value="">请先选择资源</option>}{allowedScopes.map((item) => <option value={item} key={item}>{item === "read" ? "只读" : item === "operate" ? "操作" : "管理"}</option>)}</select></Field><Field label="有效期"><select value={days} onChange={(event) => setDays(Number(event.target.value))}><option value={30}>30 天</option><option value={90}>90 天</option><option value={180}>180 天</option><option value={365}>1 年</option></select></Field></div><Field label="资源范围"><select value={resource} onChange={(event) => { setResource(event.target.value as typeof resource); setResourceID(""); }}>{globalScopes.length > 0 && <option value="global">全部允许资源</option>}{canReadNodes && <option value="node">单个节点及其实例</option>}{canReadInstances && <option value="instance">单个实例</option>}</select></Field>{resource !== "global" && <Field label={resource === "node" ? "节点" : "实例"}><select value={resourceID} onChange={(event) => setResourceID(event.target.value)}><option value="">请选择</option>{resourceOptions.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select></Field>}</div> : tokens.isPending ? <LoadingBlock label="正在读取令牌…" /> : tokens.error ? <ErrorState error={tokens.error} retry={() => void tokens.refetch()} /> : tokens.data.items.length ? <div className="token-list">{tokens.data.items.map((token) => <div className="token-row" key={token.id}><div><strong>{token.name}</strong><span>{token.scopes.join(" / ")} · {token.instance_id ? "实例范围" : token.node_id ? "节点范围" : "全局范围"}</span></div><div><small>到期</small><time>{dateTime(token.expires_at)}</time></div><div><small>最近使用</small><time>{dateTime(token.last_used_at)}</time></div><Button variant="danger" aria-label={`撤销 ${token.name}`} onClick={() => { if (window.confirm(`确认撤销“${token.name}”？`)) revoke.mutate(token.id); }} loading={revoke.isPending}><Trash2 size={15} />撤销</Button></div>)}</div> : <EmptyState title="还没有 API Token" description="为自动化创建最小权限、最小资源范围且会自动过期的令牌。" />}
  </Modal>;
}
