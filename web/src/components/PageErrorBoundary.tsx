import { Component, type ReactNode } from "react";

// Reload also replaces stale hashed chunks after a Control/web deployment.
export class PageErrorBoundary extends Component<{ children: ReactNode; resetKey: string }, { failed: boolean }> {
  state = { failed: false };

  static getDerivedStateFromError() { return { failed: true }; }

  componentDidUpdate(previous: Readonly<{ children: ReactNode; resetKey: string }>) {
    if (this.state.failed && previous.resetKey !== this.props.resetKey) this.setState({ failed: false });
  }

  render() {
    if (!this.state.failed) return this.props.children;
    return <div className="app-loading"><div className="card" role="alert">
      <h1>页面暂时无法显示</h1>
      <p>请重新加载页面。已提交的任务会继续执行。</p>
      <div className="form-actions"><button className="button button--primary" onClick={() => window.location.reload()}>重新加载</button><a className="button button--secondary" href="/">返回首页</a></div>
    </div></div>;
  }
}
