import { Eye, EyeOff, KeyRound, LockKeyhole, RadioTower, ShieldCheck } from "lucide-react";
import { useState, type FormEvent } from "react";
import { Navigate, useLocation, useNavigate } from "react-router";
import { Button, Field } from "../components/ui";
import { useAuth } from "../context/AuthContext";
import { ApiError } from "../lib/api";

export function LoginPage() {
  const auth = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [totp, setTotp] = useState("");
  const [recovery, setRecovery] = useState("");
  const [useRecovery, setUseRecovery] = useState(false);
  const [showPassword, setShowPassword] = useState(false);
  const [showTotp, setShowTotp] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const from = (location.state as { from?: { pathname?: string; search?: string; hash?: string } } | null)?.from;
  const returnTo = from?.pathname?.startsWith("/") && !from.pathname.startsWith("//") ? `${from.pathname}${from.search || ""}${from.hash || ""}` : "/";

  if (!auth.loading && auth.authenticated) return <Navigate to={auth.activationRequired ? "/activate" : returnTo} replace />;
  if (!auth.loading && auth.bootstrapRequired) return <Navigate to="/setup" replace />;

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setSubmitting(true);
    setError("");
    try {
      const result = await auth.login({ username: username.trim(), password, totp_code: useRecovery ? undefined : totp || undefined, recovery_code: useRecovery ? recovery || undefined : undefined });
      if (result.password_change_required || result.user.account_state === "pending_activation") {
        navigate("/activate", { replace: true });
        return;
      }
      navigate(returnTo, { replace: true });
    } catch (cause) {
      if (cause instanceof ApiError && ["totp_required", "invalid_totp"].includes(cause.code)) setShowTotp(true);
      setError(cause instanceof Error ? cause.message : "登录失败，请稍后重试");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <main className="login-page">
      <div className="login-atmosphere" aria-hidden><span /><span /><span /></div>
      <section className="login-intro">
        <div className="login-brand"><div className="brand-mark">L4</div><div><strong>撤离点</strong><span>L4D2 SERVER CONTROL</span></div></div>
        <div className="login-copy">
          <div className="eyebrow">MULTI-NODE COMMAND</div>
          <h1>所有服务器，<br /><em>一个安全出口。</em></h1>
          <p>跨 Windows 与 Linux 节点管理游戏实例、更新、玩家与备份。控制中心离线，也不会打断正在进行的战役。</p>
        </div>
        <div className="login-features">
          <div><RadioTower /><span><strong>主动连接</strong><small>节点无需暴露管理端口</small></span></div>
          <div><ShieldCheck /><span><strong>权限隔离</strong><small>每一步操作都有审计记录</small></span></div>
        </div>
      </section>
      <section className="login-panel">
        <form className="login-form" onSubmit={submit}>
          <div className="login-form__heading"><div className="login-icon"><LockKeyhole size={22} /></div><h2>进入控制中心</h2><p>使用管理员为你创建的账号登录</p></div>
          {error && <div className="form-error" role="alert">{error}</div>}
          <Field label="账号">
            <input autoComplete="username" value={username} onChange={(event) => setUsername(event.target.value)} placeholder="admin" required autoFocus />
          </Field>
          <Field label="密码">
            <div className="password-field"><input type={showPassword ? "text" : "password"} autoComplete="current-password" value={password} onChange={(event) => setPassword(event.target.value)} placeholder="输入密码" required /><button type="button" onClick={() => setShowPassword((visible) => !visible)} aria-label={showPassword ? "隐藏密码" : "显示密码"}>{showPassword ? <EyeOff size={18} /> : <Eye size={18} />}</button></div>
          </Field>
          {showTotp && <div className="second-factor"><Field label={useRecovery ? "一次性恢复码" : "双重验证码"} hint={useRecovery ? "输入启用 TOTP 时保存的恢复码，使用后立即失效" : "输入认证器中的 6 位动态验证码"}><div className="input-with-icon"><KeyRound size={17} />{useRecovery ? <input autoComplete="one-time-code" value={recovery} onChange={(event) => setRecovery(event.target.value.toUpperCase().replace(/[^A-Z2-9-]/g, ""))} placeholder="ABCD-EFGH-JKLM-NPQR" required /> : <input inputMode="numeric" pattern="[0-9]{6}" maxLength={6} autoComplete="one-time-code" value={totp} onChange={(event) => setTotp(event.target.value.replace(/\D/g, ""))} placeholder="000000" required />}</div></Field><button type="button" className="factor-switch" onClick={() => setUseRecovery((value) => !value)}>{useRecovery ? "改用认证器动态码" : "无法使用认证器？改用恢复码"}</button></div>}
          <Button className="login-submit" type="submit" loading={submitting}>安全登录 <span>→</span></Button>
          <p className="login-help">首次登录可在密码栏输入所有者提供的一次性密码。</p>
        </form>
      </section>
    </main>
  );
}
