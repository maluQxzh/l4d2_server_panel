import { Eye, EyeOff, KeyRound, LockKeyhole, ShieldCheck } from "lucide-react";
import { useState, type FormEvent } from "react";
import { Navigate, useNavigate } from "react-router";
import { Button, Field, LoadingBlock } from "../components/ui";
import { useAuth } from "../context/AuthContext";

export function ActivationPage() {
  const auth = useAuth();
  const navigate = useNavigate();
  const [password, setPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [showPassword, setShowPassword] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");

  if (auth.loading) return <div className="app-loading"><LoadingBlock label="正在验证激活会话…" /></div>;
  if (!auth.authenticated) return <Navigate to="/login" replace />;
  if (!auth.activationRequired) return <Navigate to="/" replace />;

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setError("");
    if (password.length < 12) {
      setError("新密码至少需要 12 个字符");
      return;
    }
    if (password !== confirmation) {
      setError("两次输入的密码不一致");
      return;
    }
    setSubmitting(true);
    try {
      await auth.completeActivation(password);
      navigate("/", { replace: true });
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "账号激活失败，请重新登录后再试");
    } finally {
      setSubmitting(false);
    }
  };

  return <main className="login-page activation-page">
    <div className="login-atmosphere" aria-hidden><span /><span /><span /></div>
    <section className="login-intro">
      <div className="login-brand"><div className="brand-mark">L4</div><div><strong>撤离点</strong><span>ACCOUNT ACTIVATION</span></div></div>
      <div className="login-copy"><div className="eyebrow">FIRST SIGN-IN</div><h1>最后一步，<br /><em>掌握你的账号。</em></h1><p>一次性密码已经完成身份交接。设置只有你知道的新密码后，系统才会开放服务器与团队资源。</p></div>
      <div className="login-features"><div><ShieldCheck /><span><strong>受限会话</strong><small>改密前无法访问任何业务接口</small></span></div><div><KeyRound /><span><strong>一次性凭据</strong><small>新密码保存后立即销毁</small></span></div></div>
    </section>
    <section className="login-panel">
      <form className="login-form" onSubmit={submit}>
        <div className="login-form__heading"><div className="login-icon"><LockKeyhole size={22} /></div><h2>设置你的新密码</h2><p>账号 @{auth.user?.username} · 完成后将自动进入控制中心</p></div>
        <div className="setup-notice">新密码至少 12 个字符。建议使用密码管理器生成并保存独立密码。</div>
        {error && <div className="form-error" role="alert">{error}</div>}
        <Field label="新密码"><div className="password-field"><input type={showPassword ? "text" : "password"} autoComplete="new-password" value={password} onChange={(event) => setPassword(event.target.value)} minLength={12} maxLength={1024} required autoFocus /><button type="button" onClick={() => setShowPassword((visible) => !visible)} aria-label={showPassword ? "隐藏密码" : "显示密码"}>{showPassword ? <EyeOff size={18} /> : <Eye size={18} />}</button></div></Field>
        <Field label="确认新密码"><input type={showPassword ? "text" : "password"} autoComplete="new-password" value={confirmation} onChange={(event) => setConfirmation(event.target.value)} minLength={12} maxLength={1024} required /></Field>
        <Button className="login-submit" type="submit" loading={submitting}>保存密码并进入 <span>→</span></Button>
        <button type="button" className="activation-logout" onClick={() => void auth.logout()}>退出并稍后激活</button>
      </form>
    </section>
  </main>;
}
