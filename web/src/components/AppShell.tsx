import {
  Bell,
  Boxes,
  ChevronDown,
  CircleGauge,
  ClipboardList,
  LogOut,
  Menu,
  MapPinned,
  Server,
  Wrench,
  Settings,
  ShieldCheck,
  X,
} from "lucide-react";
import { useState, type ReactNode } from "react";
import { NavLink, useNavigate } from "react-router";
import { useAuth } from "../context/AuthContext";
import { useRealtime } from "../context/RealtimeContext";

const navigation = [
  { to: "/", label: "作战总览", icon: CircleGauge, end: true },
  { to: "/nodes", label: "节点", icon: Server },
  { to: "/environments", label: "运行环境", icon: Wrench },
  { to: "/instances", label: "游戏实例", icon: Boxes },
  { to: "/maps", label: "地图管理", icon: MapPinned },
  { to: "/jobs", label: "任务中心", icon: ClipboardList },
  { to: "/notifications", label: "通知", icon: Bell },
  { to: "/settings", label: "设置", icon: Settings },
];

export function AppShell({ children }: { children: ReactNode }) {
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [accountOpen, setAccountOpen] = useState(false);
  const auth = useAuth();
  const realtime = useRealtime();
  const navigate = useNavigate();

  const signOut = async () => {
    await auth.logout();
    navigate("/login", { replace: true });
  };

  return (
    <div className="app-shell">
      <a href="#main-content" className="skip-link">跳到主要内容</a>
      {sidebarOpen && <button className="sidebar-backdrop" aria-label="关闭导航" onClick={() => setSidebarOpen(false)} />}
      <aside className={`sidebar ${sidebarOpen ? "sidebar--open" : ""}`}>
        <div className="sidebar__brand">
          <div className="brand-mark" aria-hidden>L4</div>
          <div><strong>撤离点</strong><span>服务器控制中心</span></div>
          <button className="icon-button sidebar__close" onClick={() => setSidebarOpen(false)} aria-label="关闭导航"><X size={20} /></button>
        </div>
        <nav className="sidebar__nav" aria-label="主导航">
          <span className="sidebar__label">管理</span>
          {navigation.map(({ to, label, icon: Icon, end }) => (
            <NavLink key={to} to={to} end={end} onClick={() => setSidebarOpen(false)}>
              <Icon size={19} aria-hidden /><span>{label}</span>
            </NavLink>
          ))}
        </nav>
        <div className="sidebar__foot">
          <div className="integrity-card">
            <ShieldCheck size={19} />
            <div><strong>控制链路受保护</strong><span>Agent 仅建立出站连接</span></div>
          </div>
          <a className="public-link" href="/status" target="_blank" rel="noreferrer">查看公开状态页 <span>↗</span></a>
        </div>
      </aside>
      <div className="app-main">
        <header className="topbar">
          <button className="icon-button mobile-menu" onClick={() => setSidebarOpen(true)} aria-label="打开导航"><Menu size={21} /></button>
          <div className={`connection connection--${realtime.state}`} title={realtime.state === "live" ? "实时事件连接正常" : "实时连接不可用，已使用轮询"}>
            <span aria-hidden />
            {realtime.state === "live" ? "实时同步" : realtime.state === "connecting" ? "正在连接" : "轮询模式"}
          </div>
          <div className="topbar__spacer" />
          <NavLink className="icon-button notification-button" to="/notifications" aria-label="查看通知"><Bell size={19} /></NavLink>
          <div className="account-menu">
            <button className="account-menu__trigger" onClick={() => setAccountOpen((open) => !open)} aria-expanded={accountOpen}>
              <span className="avatar">{(auth.user?.display_name || auth.user?.username || "管").slice(0, 1).toUpperCase()}</span>
              <span className="account-menu__name"><strong>{auth.user?.display_name || auth.user?.username}</strong><small>{roleLabel(auth.user?.role)}</small></span>
              <ChevronDown size={16} aria-hidden />
            </button>
            {accountOpen && (
              <div className="account-menu__panel">
                <NavLink to="/settings" onClick={() => setAccountOpen(false)}><Settings size={16} />账号设置</NavLink>
                <button onClick={signOut}><LogOut size={16} />退出登录</button>
              </div>
            )}
          </div>
        </header>
        <main id="main-content" className="content">{children}</main>
      </div>
    </div>
  );
}

function roleLabel(role?: string) {
  return ({ owner: "所有者", admin: "管理员", operator: "操作员", viewer: "观察者" } as Record<string, string>)[role || ""] || "用户";
}
