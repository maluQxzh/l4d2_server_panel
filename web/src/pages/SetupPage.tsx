import { Eye, EyeOff, KeyRound, RadioTower, RefreshCw, ShieldCheck, Terminal } from "lucide-react";
import { useRef, useState, type FormEvent } from "react";
import { Navigate, useNavigate } from "react-router";
import { Button, Field } from "../components/ui";
import { useAuth } from "../context/AuthContext";
import { ApiError } from "../lib/api";

export function SetupPage() {
  const auth = useAuth();
  const navigate = useNavigate();
  const [setupKey, setSetupKey] = useState("");
  const [showKey, setShowKey] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState("");
  const bootstrapInFlight = useRef(false);

  if (auth.loading) return <main className="app-loading"><div className="brand-mark">L4</div><span>正在检查初始化状态…</span></main>;
  // bootstrapOwner publishes the authenticated session before this handler
  // navigates to settings. Keep the in-flight page mounted so the generic
  // authenticated redirect cannot win that race.
  if (auth.authenticated && !bootstrapInFlight.current) return <Navigate to="/" replace />;
  if (!auth.bootstrapRequired) return <Navigate to="/login" replace />;

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setError("");
    if (!setupKey.trim()) {
      setError("请输入中心端生成的初始化密钥");
      return;
    }
    bootstrapInFlight.current = true;
    setSubmitting(true);
    try {
      await auth.bootstrapOwner({
        setup_key: setupKey.trim(),
      });
      // Confirm that the one-time bootstrap gate is closed before entering a
      // protected route; otherwise its guard can briefly route back to setup.
      await auth.refreshBootstrap();
      navigate("/settings", { replace: true, state: { setupKeyIsInitialPassword: true } });
    } catch (cause) {
      bootstrapInFlight.current = false;
      if (cause instanceof ApiError && cause.code === "bootstrap_complete") {
        await auth.refreshBootstrap();
        navigate("/login", { replace: true });
        return;
      }
      if (cause instanceof ApiError && ["invalid_setup_key", "invalid_setup_key_format"].includes(cause.code)) {
        setError("初始化密钥无效，请检查后重试");
        return;
      }
      if (cause instanceof ApiError && cause.code === "setup_key_not_configured") {
        await auth.refreshBootstrap();
        setError("中心端尚未生成初始化密钥");
        return;
      }
      setError(cause instanceof Error ? cause.message : "初始化失败，请稍后重试");
    } finally {
      setSubmitting(false);
    }
  };

  const refresh = async () => {
    setChecking(true);
    setError("");
    try {
      await auth.refreshBootstrap();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "检查初始化状态失败");
    } finally {
      setChecking(false);
    }
  };

  return (
    <main className="login-page login-page--setup">
      <div className="login-atmosphere" aria-hidden><span /><span /><span /></div>
      <section className="login-intro">
        <div className="login-brand"><div className="brand-mark">L4</div><div><strong>撤离点</strong><span>FIRST-RUN SETUP</span></div></div>
        <div className="login-copy">
          <div className="eyebrow">CONTROL PLANE READY</div>
          <h1>中心端已启动，<br /><em>验证初始化密钥。</em></h1>
          <p>密钥只能在中心端主机上生成。验证成功后入口会关闭，并自动进入系统设置。</p>
        </div>
        <div className="login-features">
          <div><RadioTower /><span><strong>服务已经就绪</strong><small>数据库迁移和控制平面均已启动</small></span></div>
          <div><ShieldCheck /><span><strong>一次性初始化</strong><small>密钥验证成功后自动创建根所有者</small></span></div>
        </div>
      </section>
      <section className="login-panel">
        <form className="login-form setup-form" onSubmit={submit}>
          <div className="login-form__heading"><div className="login-icon"><KeyRound size={22} /></div><h2>输入初始化密钥</h2><p>验证后将创建 admin 根所有者</p></div>
          <div className="setup-notice">初始化密钥同时是 admin 的初始密码。进入设置后请立即修改密码，并将 19090 明文端口限制到可信代理来源。</div>
          {error && <div className="form-error" role="alert">{error}</div>}
          {!auth.bootstrapKeyConfigured ? <div className="setup-key-missing">
            <div><Terminal size={18} /><span><strong>请先在中心端主机生成密钥</strong><small>命令只会在当前终端显示一次明文密钥。</small></span></div>
            <code>docker compose exec -T app setup-key issue</code>
            <code>&amp; "$env:ProgramFiles\L4D2Panel\Control\New-L4D2PanelSetupKey.ps1"</code>
            <Button type="button" variant="secondary" onClick={() => void refresh()} loading={checking}><RefreshCw size={16} />我已生成，重新检查</Button>
          </div> : <Field label="初始化密钥" hint="格式为 l4d2p_setup_ 开头的一次性字符串">
            <div className="password-field"><input aria-label="初始化密钥" type={showKey ? "text" : "password"} autoComplete="off" maxLength={128} value={setupKey} onChange={(event) => setSetupKey(event.target.value)} required autoFocus /><button type="button" onClick={() => setShowKey((visible) => !visible)} aria-label={showKey ? "隐藏密钥" : "显示密钥"}>{showKey ? <EyeOff size={18} /> : <Eye size={18} />}</button></div>
          </Field>}
          <Button className="login-submit" type="submit" loading={submitting} disabled={!auth.bootstrapKeyConfigured}>验证并进入设置 <span>→</span></Button>
        </form>
      </section>
    </main>
  );
}
