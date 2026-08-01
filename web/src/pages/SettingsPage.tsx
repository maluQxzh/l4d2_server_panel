import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Bell, Braces, Check, CheckCircle2, Clipboard, KeyRound, LockKeyhole, Mail, Plus, ShieldCheck,
  Smartphone, Trash2, UserCog, Webhook,
} from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { Button, Card, EmptyState, ErrorState, Field, LoadingBlock, Modal, PageHeader, StatusBadge } from "../components/ui";
import { useAuth } from "../context/AuthContext";
import { useToast } from "../context/ToastContext";
import { hasRootSecurityPermission } from "../lib/account-permissions";
import { api, ApiError } from "../lib/api";
import { dateTime } from "../lib/format";
import type { Role, RoleBinding, User } from "../lib/types";

export function SettingsPage() {
  const auth = useAuth();
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
    <div className="settings-grid settings-grid--profile">
      <Card><div className="profile-card"><div className="avatar avatar--large">{(auth.user?.display_name || auth.user?.username || "管").slice(0, 1)}</div><div><span className="eyebrow">CURRENT ACCOUNT</span><h2>{auth.user?.display_name || auth.user?.username}</h2><p>@{auth.user?.username} · 上次登录 {dateTime(auth.user?.last_login_at)}</p></div><StatusBadge status={auth.user?.role || "viewer"} /></div></Card>
      <Card className="security-summary"><div><ShieldCheck size={22} /><span><strong>账号安全</strong><small>密码使用 Argon2id 哈希存储</small></span></div><div className={auth.user?.totp_enabled ? "good" : "warning"}><Smartphone size={20} /><span><strong>双重验证</strong><small>{auth.user?.totp_enabled ? "已启用 TOTP" : "建议尽快启用"}</small></span></div><Button variant="secondary" onClick={() => auth.user && setManagedUser(auth.user)} disabled={!canManageUsers} title={!canManageUsers ? "请联系所有者重置密码" : undefined}><LockKeyhole size={16} />更改密码</Button></Card>
    </div>

    <section className="settings-section">
      <div className="section-heading"><div><h2>团队成员</h2><p>只有所有者可查看和修改团队账号；资源范围由权限绑定与令牌范围共同收敛。</p></div>{canManageUsers && <Button onClick={() => setManagedUser("new")}><Plus size={16} />添加用户</Button>}</div>
      <Card>{!canManageUsers ? <EmptyState title="成员管理仅所有者可用" description="你的实例操作权限仍按当前角色正常生效。" /> : users.isPending ? <LoadingBlock label="正在读取用户…" /> : users.error ? users.error instanceof ApiError && [404, 501].includes(users.error.status) ? <EmptyState title="用户管理接口尚未开放" description="当前账号可正常使用。" /> : <ErrorState error={users.error} retry={() => void users.refetch()} /> : <div className="user-list">{users.data.items.map((user) => <div className={`user-row ${user.disabled ? "user-row--disabled" : ""}`} key={user.id}><div className="avatar">{(user.display_name || user.username).slice(0, 1)}</div><div><strong>{user.display_name || user.username}{user.bootstrap_owner ? " · 根所有者" : ""}</strong><span>@{user.username} · {bindingSummary(user.role_bindings)}</span></div><StatusBadge status={user.disabled ? "disabled" : user.role} /><span>{user.totp_enabled ? <><CheckCircle2 size={15} />2FA</> : "未启用 2FA"}</span><time>{dateTime(user.last_login_at)}</time><Button variant="ghost" onClick={() => setManagedUser(user)}><UserCog size={16} />管理</Button></div>)}</div>}</Card>
    </section>

    <div className="settings-grid">
      <Card><div className="card-heading"><div><span className="eyebrow">PERSONAL SECURITY</span><h2>账号安全</h2></div><KeyRound size={21} /></div><div className="settings-list"><button onClick={() => setTotpOpen(true)}><span><Smartphone size={18} /><span><strong>TOTP 双重验证</strong><small>{auth.user?.totp_enabled ? "已启用；恢复码每个只能使用一次" : "使用认证器保护登录"}</small></span></span><em>{auth.user?.totp_enabled ? "查看状态" : "启用"}</em></button><button onClick={() => setTokenOpen(true)}><span><KeyRound size={18} /><span><strong>API Token</strong><small>创建带权限、资源范围与有效期的令牌</small></span></span><em>管理</em></button><button onClick={() => { if (window.confirm("确认退出当前登录会话？")) void auth.logout(); }}><span><LockKeyhole size={18} /><span><strong>登录会话</strong><small>撤销当前设备的 HttpOnly 会话 Cookie</small></span></span><em>退出</em></button></div></Card>
      <Card><div className="card-heading"><div><span className="eyebrow">NOTIFICATION DELIVERY</span><h2>通知渠道</h2></div><Bell size={21} /></div><div className="settings-list"><button onClick={() => setChannelKind("smtp")} disabled={!['owner', 'admin'].includes(auth.user?.role || '')}><span><Mail size={18} /><span><strong>SMTP 邮件</strong><small>强制 STARTTLS，凭据加密保存</small></span></span><em>配置</em></button><button onClick={() => setChannelKind("webhook")} disabled={!['owner', 'admin'].includes(auth.user?.role || '')}><span><Webhook size={18} /><span><strong>Webhook</strong><small>HMAC-SHA256 签名与持久化重试</small></span></span><em>配置</em></button></div></Card>
      <Card><div className="card-heading"><div><span className="eyebrow">DISPLAY</span><h2>界面偏好</h2></div><Braces size={21} /></div><label className="preference-row"><span><strong>紧凑布局</strong><small>在表格中显示更多信息</small></span><button type="button" role="switch" aria-checked={compact} className={`switch ${compact ? "switch--on" : ""}`} onClick={() => setCompact((value) => !value)}><span /></button></label><Field label="语言"><select value="zh-CN" disabled><option>简体中文</option></select></Field></Card>
    </div>
    <TotpModal open={totpOpen} enabled={Boolean(auth.user?.totp_enabled)} onClose={() => setTotpOpen(false)} />
    <APITokenModal open={tokenOpen} role={auth.user?.role || "viewer"} onClose={() => setTokenOpen(false)} />
    <NotificationChannelModal kind={channelKind} onClose={() => setChannelKind(null)} />
    <UserModal target={canManageUsers ? managedUser : null} actor={auth.user || null} onClose={() => setManagedUser(null)} />
  </div>;
}

type EditableBinding = Pick<RoleBinding, "role" | "scope_type" | "node_id" | "instance_id">;

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
  const [bindings, setBindings] = useState<EditableBinding[]>([{ role: "viewer", scope_type: "global" }]);

  useEffect(() => {
    if (!target) return;
    setUsername(creating ? "" : existing?.username || "");
    setDisplayName(creating ? "" : existing?.display_name || "");
    setPassword("");
    setDisabled(Boolean(existing?.disabled));
    setBindings(creating ? [{ role: "viewer", scope_type: "global" }] : (existing?.role_bindings?.length ? existing.role_bindings.map(cleanBinding) : [{ role: existing?.role || "viewer", scope_type: "global" }]));
  }, [creating, existing, target]);

  const normalizedBindings = bindings.map(cleanBinding);
  const bindingKeys = normalizedBindings.map((item) => `${item.scope_type}:${item.node_id || item.instance_id || "global"}`);
  const bindingsValid = normalizedBindings.length > 0 && normalizedBindings.every((item) => item.scope_type === "global" || (item.scope_type === "node" ? Boolean(item.node_id) : Boolean(item.instance_id))) && new Set(bindingKeys).size === bindingKeys.length;
  const originalBindings: EditableBinding[] = existing?.role_bindings?.length
    ? existing.role_bindings.map(cleanBinding)
    : existing ? [{ role: existing.role, scope_type: "global" }] : [];
  const bindingsChanged = existing ? JSON.stringify(normalizedBindings) !== JSON.stringify(originalBindings) : true;
  const changed = creating || displayName.trim() !== existing?.display_name || Boolean(password) || disabled !== Boolean(existing?.disabled) || bindingsChanged;
  const valid = username.trim() && displayName.trim() && (creating ? password.length >= 12 : !password || password.length >= 12) && bindingsValid && changed;

  const save = useMutation({
    mutationFn: async () => {
      if (creating) return api.createUser({ username: username.trim(), display_name: displayName.trim(), password, role_bindings: normalizedBindings });
      if (!existing) throw new Error("用户无效");
      const payload: Parameters<typeof api.updateUser>[1] = {};
      if (displayName.trim() !== existing.display_name) payload.display_name = displayName.trim();
      if (password) payload.password = password;
      if (disabled !== Boolean(existing.disabled)) payload.disabled = disabled;
      if (bindingsChanged) payload.role_bindings = normalizedBindings;
      return api.updateUser(existing.id, payload);
    },
    onSuccess: () => {
      toast.push(creating ? "团队账号已创建" : existing?.id === actor?.id && (password || bindingsChanged) ? "账号已更新，请重新登录" : "团队账号已更新");
      void client.invalidateQueries({ queryKey: ["users"] });
      if (existing?.id === actor?.id && (password || bindingsChanged)) void client.invalidateQueries({ queryKey: ["session"] });
      onClose();
    },
    onError: (error) => toast.push(error.message, "error"),
  });

  const updateBinding = (index: number, value: EditableBinding) => setBindings((items) => items.map((item, itemIndex) => itemIndex === index ? cleanBinding(value) : item));
  const protectedOwner = Boolean(existing?.bootstrap_owner);
  const availableRoles: Role[] = actor?.role === "owner" ? ["owner", "admin", "operator", "viewer"] : ["admin", "operator", "viewer"];
  return <Modal open={open} onClose={onClose} title={creating ? "添加团队成员" : `管理 ${existing?.display_name || existing?.username || "用户"}`} description="权限绑定可以限定到全局、单个节点或单个实例；同一资源只允许一条绑定。" footer={<><Button variant="secondary" onClick={onClose}>取消</Button><Button onClick={() => save.mutate()} loading={save.isPending} disabled={!valid}>{creating ? "创建账号" : "保存更改"}</Button></>}>
    <div className="form-stack"><div className="form-grid"><Field label="用户名"><input value={username} onChange={(event) => setUsername(event.target.value)} disabled={!creating} maxLength={128} autoComplete="username" /></Field><Field label="显示名称"><input value={displayName} onChange={(event) => setDisplayName(event.target.value)} maxLength={128} /></Field></div><Field label={creating ? "初始密码" : "重置密码（可留空）"} hint="至少 12 个字符；修改密码会撤销该账号的全部会话和 API Token"><input type="password" value={password} onChange={(event) => setPassword(event.target.value)} minLength={12} maxLength={1024} autoComplete="new-password" /></Field>{!creating && <label className={`risk-check ${protectedOwner || existing?.id === actor?.id ? "risk-check--disabled" : ""}`}><input type="checkbox" checked={disabled} onChange={(event) => setDisabled(event.target.checked)} disabled={protectedOwner || existing?.id === actor?.id} /><LockKeyhole size={18} /><span><strong>停用账号</strong><small>{protectedOwner ? "首启根所有者不可停用" : existing?.id === actor?.id ? "不能停用当前登录账号" : "立即撤销该账号的全部会话和 API Token"}</small></span></label>}<div className="binding-editor"><div className="binding-editor__heading"><div><strong>资源权限绑定</strong><span>最高显示角色不扩大具体资源权限</span></div><Button type="button" variant="secondary" onClick={() => setBindings((items) => [...items, { role: "viewer", scope_type: "instance" }])} disabled={protectedOwner || bindings.length >= 50}><Plus size={14} />添加范围</Button></div>{bindings.map((binding, index) => <div className="binding-row" key={`${index}-${binding.scope_type}`}><Field label="角色"><select value={binding.role} disabled={protectedOwner} onChange={(event) => { const role = event.target.value as Role; updateBinding(index, role === "owner" ? { role, scope_type: "global" } : { ...binding, role }); }}>{availableRoles.map((role) => <option value={role} key={role}>{roleName(role)}</option>)}</select></Field><Field label="范围"><select value={binding.scope_type} disabled={protectedOwner || binding.role === "owner"} onChange={(event) => updateBinding(index, { role: binding.role, scope_type: event.target.value as EditableBinding["scope_type"] })}><option value="global">全局</option><option value="node">单个节点</option><option value="instance">单个实例</option></select></Field>{binding.scope_type === "node" ? <Field label="节点"><select value={binding.node_id || ""} disabled={protectedOwner} onChange={(event) => updateBinding(index, { ...binding, node_id: event.target.value })}><option value="">请选择节点</option>{(nodes.data?.items || []).map((item) => <option value={item.id} key={item.id}>{item.name}</option>)}</select></Field> : binding.scope_type === "instance" ? <Field label="实例"><select value={binding.instance_id || ""} disabled={protectedOwner} onChange={(event) => updateBinding(index, { ...binding, instance_id: event.target.value })}><option value="">请选择实例</option>{(instances.data?.items || []).map((item) => <option value={item.id} key={item.id}>{item.name}</option>)}</select></Field> : <div className="binding-global-note"><ShieldCheck size={16} />适用于全部节点与实例</div>}<Button type="button" variant="danger" aria-label="删除权限绑定" onClick={() => setBindings((items) => items.filter((_, itemIndex) => itemIndex !== index))} disabled={protectedOwner || bindings.length === 1}><Trash2 size={14} /></Button></div>)}{!bindingsValid && <div className="form-warning">请选择每条绑定的资源，且不要重复绑定同一资源。</div>}</div></div>
  </Modal>;
}

function cleanBinding(binding: EditableBinding): EditableBinding {
  if (binding.scope_type === "node") return { role: binding.role, scope_type: "node", node_id: binding.node_id || "" };
  if (binding.scope_type === "instance") return { role: binding.role, scope_type: "instance", instance_id: binding.instance_id || "" };
  return { role: binding.role, scope_type: "global" };
}

function roleName(role: Role) {
  return role === "owner" ? "所有者" : role === "admin" ? "管理员" : role === "operator" ? "操作员" : "观察者";
}

function bindingSummary(bindings?: RoleBinding[]) {
  if (!bindings?.length) return "全局角色";
  const global = bindings.find((item) => item.scope_type === "global");
  return global ? `${roleName(global.role)} · 全局` : `${bindings.length} 个资源范围`;
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

function APITokenModal({ open, role, onClose }: { open: boolean; role: Role; onClose: () => void }) {
  const toast = useToast();
  const client = useQueryClient();
  const tokens = useQuery({ queryKey: ["api-tokens"], queryFn: api.apiTokens, enabled: open });
  const nodes = useQuery({ queryKey: ["nodes"], queryFn: api.nodes, enabled: open });
  const instances = useQuery({ queryKey: ["instances"], queryFn: api.instances, enabled: open });
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [scope, setScope] = useState<"read" | "operate" | "manage">(role === "viewer" ? "read" : "operate");
  const [resource, setResource] = useState<"global" | "node" | "instance">("global");
  const [resourceID, setResourceID] = useState("");
  const [days, setDays] = useState(90);
  const [secret, setSecret] = useState("");
  const allowedScopes = useMemo(() => role === "viewer" ? ["read"] : role === "operator" ? ["read", "operate"] : ["read", "operate", "manage"], [role]);
  const create = useMutation({
    mutationFn: () => api.createApiToken({
      name: name.trim(), scopes: [scope],
      node_id: resource === "node" ? resourceID : undefined,
      instance_id: resource === "instance" ? resourceID : undefined,
      expires_at: new Date(Date.now() + days * 86_400_000).toISOString(),
    }),
    onSuccess: (result) => { setSecret(result.token); void client.invalidateQueries({ queryKey: ["api-tokens"] }); toast.push("API Token 已创建，只会显示一次"); },
    onError: (error) => toast.push(error.message, "error"),
  });
  const revoke = useMutation({ mutationFn: api.revokeApiToken, onSuccess: () => { void client.invalidateQueries({ queryKey: ["api-tokens"] }); toast.push("API Token 已撤销"); }, onError: (error) => toast.push(error.message, "error") });
  useEffect(() => { if (!open) { setCreating(false); setName(""); setResource("global"); setResourceID(""); setSecret(""); } }, [open]);

  const close = () => {
    if (secret && !window.confirm("令牌关闭后不会再次显示。确认已经保存？")) return;
    onClose();
  };
  const resourceOptions = resource === "node" ? nodes.data?.items || [] : instances.data?.items || [];
  return <Modal open={open} onClose={close} title="API Token" description="令牌受账号角色、权限范围、资源范围和到期时间四重限制。" footer={<><Button variant="secondary" onClick={close}>关闭</Button>{!creating && !secret && <Button onClick={() => setCreating(true)}><Plus size={16} />新建令牌</Button>}{creating && !secret && <Button onClick={() => create.mutate()} loading={create.isPending} disabled={!name.trim() || (resource !== "global" && !resourceID)}>创建</Button>}</>}>
    {secret ? <div className="form-stack"><div className="form-warning"><ShieldCheck size={18} />令牌只显示这一次，请立即复制到安全位置。</div><div className="token-secret"><code>{secret}</code><Button variant="secondary" onClick={() => void navigator.clipboard.writeText(secret)}><Clipboard size={16} />复制</Button></div></div> : creating ? <div className="form-stack"><Field label="名称"><input autoFocus value={name} onChange={(event) => setName(event.target.value)} maxLength={100} placeholder="自动化维护脚本" /></Field><div className="form-grid"><Field label="权限范围"><select value={scope} onChange={(event) => setScope(event.target.value as typeof scope)}>{allowedScopes.map((item) => <option value={item} key={item}>{item === "read" ? "只读" : item === "operate" ? "操作" : "管理"}</option>)}</select></Field><Field label="有效期"><select value={days} onChange={(event) => setDays(Number(event.target.value))}><option value={30}>30 天</option><option value={90}>90 天</option><option value={180}>180 天</option><option value={365}>1 年</option></select></Field></div><Field label="资源范围"><select value={resource} onChange={(event) => { setResource(event.target.value as typeof resource); setResourceID(""); }}><option value="global">全部允许资源</option><option value="node">单个节点及其实例</option><option value="instance">单个实例</option></select></Field>{resource !== "global" && <Field label={resource === "node" ? "节点" : "实例"}><select value={resourceID} onChange={(event) => setResourceID(event.target.value)}><option value="">请选择</option>{resourceOptions.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select></Field>}</div> : tokens.isPending ? <LoadingBlock label="正在读取令牌…" /> : tokens.error ? <ErrorState error={tokens.error} retry={() => void tokens.refetch()} /> : tokens.data.items.length ? <div className="token-list">{tokens.data.items.map((token) => <div className="token-row" key={token.id}><div><strong>{token.name}</strong><span>{token.scopes.join(" / ")} · {token.instance_id ? "实例范围" : token.node_id ? "节点范围" : "全局范围"}</span></div><div><small>到期</small><time>{dateTime(token.expires_at)}</time></div><div><small>最近使用</small><time>{dateTime(token.last_used_at)}</time></div><Button variant="danger" aria-label={`撤销 ${token.name}`} onClick={() => { if (window.confirm(`确认撤销“${token.name}”？`)) revoke.mutate(token.id); }} loading={revoke.isPending}><Trash2 size={15} />撤销</Button></div>)}</div> : <EmptyState title="还没有 API Token" description="为自动化创建最小权限、最小资源范围且会自动过期的令牌。" />}
  </Modal>;
}
