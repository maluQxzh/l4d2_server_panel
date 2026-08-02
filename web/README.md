# 撤离点 Web 前端

React 19 + TypeScript + Vite 的 L4D2 多节点管理界面。所有业务数据来自 `/api/v1`，不包含演示数据或浏览器端伪造状态。

## 本地开发

```bash
npm ci
npm run dev
```

开发服务器默认把 `/api` 与 `/agent` 代理到 `http://127.0.0.1:19090`。可复制 `.env.example` 并通过 `VITE_DEV_PROXY` 修改目标。

## 验证与构建

```bash
npm test
npm run build
```

生产文件写入 `dist/`。Web 服务器需要对非 API 路径启用 SPA fallback 到 `index.html`，同时保留 `/api/v1/events` 的 WebSocket 升级。

## 接口约定

- 首次初始化：`GET|POST /api/v1/auth/bootstrap`，仅数据库无用户时可创建首个所有者
- 认证：`/api/v1/auth/login|me|logout`
- 资源：`/api/v1/nodes`、`instances`、`jobs`、`audit`
- 实时事件：`/api/v1/events?cursor=N`
- 公开状态：`/api/v1/public/servers`
- 实例长操作：`POST /api/v1/instances/{id}/actions`

登录后写请求自动携带 `X-CSRF-Token` 与 `Idempotency-Key`。实时连接不可用时，核心列表会自动降级为定时轮询。
