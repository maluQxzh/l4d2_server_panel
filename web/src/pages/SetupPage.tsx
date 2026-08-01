import { Eye, EyeOff, RadioTower, ShieldCheck, UserRoundPlus } from "lucide-react";
import { useState, type FormEvent } from "react";
import { Navigate, useNavigate } from "react-router";
import { Button, Field } from "../components/ui";
import { useAuth } from "../context/AuthContext";
import { ApiError } from "../lib/api";

export function SetupPage() {
  const auth = useAuth();
  const navigate = useNavigate();
  const [username, setUsername] = useState("admin");
  const [displayName, setDisplayName] = useState("系统管理员");
  const [password, setPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [showPassword, setShowPassword] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");

  if (auth.loading) return <main className="app-loading"><div className="brand-mark">L4</div><span>正在检查初始化状态…</span></main>;
  if (auth.authenticated) return <Navigate to="/" replace />;
  if (!auth.bootstrapRequired) return <Navigate to="/login" replace />;

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setError("");
    if (password.length < 12) {
      setError("密码至少需要 12 个字符");
      return;
    }
    if (password !== confirmation) {
      setError("两次输入的密码不一致");
      return;
    }
    setSubmitting(true);
    try {
      await auth.bootstrapOwner({
        username: username.trim(),
        display_name: displayName.trim(),
        password,
        password_confirmation: confirmation,
      });
      navigate("/", { replace: true });
    } catch (cause) {
      if (cause instanceof ApiError && cause.code === "bootstrap_complete") {
        await auth.refreshBootstrap();
        navigate("/login", { replace: true });
        return;
      }
      setError(cause instanceof Error ? cause.message : "初始化失败，请稍后重试");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <main className="login-page login-page--setup">
      <div className="login-atmosphere" aria-hidden><span /><span /><span /></div>
      <section className="login-intro">
        <div className="login-brand"><div className="brand-mark">L4</div><div><strong>撤离点</strong><span>FIRST-RUN SETUP</span></div></div>
        <div className="login-copy">
          <div className="eyebrow">CONTROL PLANE READY</div>
          <h1>中心端已启动，<br /><em>创建首个所有者。</em></h1>
          <p>这是一次性初始化入口。创建成功后入口会立即关闭，并自动进入管理面板。</p>
        </div>
        <div className="login-features">
          <div><RadioTower /><span><strong>服务已经就绪</strong><small>数据库迁移和控制平面均已启动</small></span></div>
          <div><ShieldCheck /><span><strong>一次性初始化</strong><small>只有数据库无用户时才能创建</small></span></div>
        </div>
      </section>
      <section className="login-panel">
        <form className="login-form setup-form" onSubmit={submit}>
          <div className="login-form__heading"><div className="login-icon"><UserRoundPlus size={22} /></div><h2>创建首个所有者</h2><p>该账号拥有系统、安全和全部资源权限</p></div>
          <div className="setup-notice">请在可信网络中立即完成初始化。首个成功提交的账号将成为不可降级的根所有者。</div>
          {error && <div className="form-error" role="alert">{error}</div>}
          <Field label="账号">
            <input autoComplete="username" maxLength={128} value={username} onChange={(event) => setUsername(event.target.value)} required autoFocus />
          </Field>
          <Field label="显示名称">
            <input autoComplete="name" maxLength={128} value={displayName} onChange={(event) => setDisplayName(event.target.value)} required />
          </Field>
          <Field label="密码" hint="至少 12 个字符，建议使用密码管理器生成">
            <div className="password-field"><input aria-label="密码" type={showPassword ? "text" : "password"} autoComplete="new-password" minLength={12} maxLength={1024} value={password} onChange={(event) => setPassword(event.target.value)} required /><button type="button" onClick={() => setShowPassword((visible) => !visible)} aria-label={showPassword ? "隐藏密码" : "显示密码"}>{showPassword ? <EyeOff size={18} /> : <Eye size={18} />}</button></div>
          </Field>
          <Field label="确认密码">
            <input aria-label="确认密码" type={showPassword ? "text" : "password"} autoComplete="new-password" minLength={12} maxLength={1024} value={confirmation} onChange={(event) => setConfirmation(event.target.value)} required />
          </Field>
          <Button className="login-submit" type="submit" loading={submitting}>创建并进入面板 <span>→</span></Button>
        </form>
      </section>
    </main>
  );
}
