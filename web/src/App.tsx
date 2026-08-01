import { Navigate, Outlet, Route, Routes, useLocation } from "react-router";
import { lazy, Suspense, type ComponentType } from "react";
import { AppShell } from "./components/AppShell";
import { LoadingBlock } from "./components/ui";
import { useAuth } from "./context/AuthContext";
import { RealtimeProvider } from "./context/RealtimeContext";

function lazyNamed<T extends ComponentType<object>>(loader: () => Promise<Record<string, T>>, name: string) {
  return lazy(async () => ({ default: (await loader())[name] }));
}

const DashboardPage = lazyNamed(() => import("./pages/DashboardPage"), "DashboardPage");
const InstanceDetailPage = lazyNamed(() => import("./pages/InstanceDetailPage"), "InstanceDetailPage");
const InstancesPage = lazyNamed(() => import("./pages/InstancesPage"), "InstancesPage");
const JobsPage = lazyNamed(() => import("./pages/JobsPage"), "JobsPage");
const LoginPage = lazyNamed(() => import("./pages/LoginPage"), "LoginPage");
const NodesPage = lazyNamed(() => import("./pages/NodesPage"), "NodesPage");
const NotificationsPage = lazyNamed(() => import("./pages/NotificationsPage"), "NotificationsPage");
const PublicStatusPage = lazyNamed(() => import("./pages/PublicStatusPage"), "PublicStatusPage");
const SettingsPage = lazyNamed(() => import("./pages/SettingsPage"), "SettingsPage");

function ProtectedLayout() {
  const auth = useAuth();
  const location = useLocation();
  if (auth.loading) return <div className="app-loading"><div className="brand-mark">L4</div><LoadingBlock label="正在建立安全会话…" /></div>;
  if (!auth.authenticated) return <Navigate to="/login" replace state={{ from: location }} />;
  return <RealtimeProvider><AppShell><Outlet /></AppShell></RealtimeProvider>;
}

function NotFound() {
  return <div className="not-found"><span>404</span><h1>这条撤离路线不存在</h1><p>返回管理面板，确认你访问的地址。</p><a className="button button--primary" href="/">返回仪表盘</a></div>;
}

export function App() {
  return (
    <Suspense fallback={<div className="app-loading"><LoadingBlock label="正在装载界面…" /></div>}>
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route path="/status" element={<PublicStatusPage />} />
        <Route element={<ProtectedLayout />}>
          <Route index element={<DashboardPage />} />
          <Route path="nodes" element={<NodesPage />} />
          <Route path="instances" element={<InstancesPage />} />
          <Route path="instances/:instanceId" element={<InstanceDetailPage />} />
          <Route path="jobs" element={<JobsPage />} />
          <Route path="notifications" element={<NotificationsPage />} />
          <Route path="settings" element={<SettingsPage />} />
        </Route>
        <Route path="*" element={<NotFound />} />
      </Routes>
    </Suspense>
  );
}
