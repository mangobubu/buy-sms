# 短信聚合管理台前端

Vue 3、TypeScript、Vite 与 Element Plus 构建的管理端。生产构建产物为 `dist/`，由 Go 服务嵌入并与 API 一体启动。

## 本地开发

```bash
npm ci
npm run dev
```

开发服务器使用 `5173` 端口，并将 `/api` 转发至 `VITE_DEV_API_TARGET`（默认 `http://127.0.0.1:8080`）。生产验证：

```bash
npm run typecheck
npm run build
```

## 随机后台入口

前端不调用任何匿名接口获取后台入口，防止随机路径通过公共 API 暴露。

- 推荐由 Go 在返回 `index.html` 前注入 `window.__APP_CONFIG__ = { adminPath: "/随机路径" }`。
- 未注入时（主要用于本地开发），前端只从当前 URL 的首段提取 `adminSlug`。
- 验证码、登录和其他 API 请求均携带 `X-Admin-Path`；登录 body 也携带 `adminPath`。
- Go 生产服务只对配置的后台前缀及明确存在的 SPA 子路由返回 `index.html`。`/`、错误后台前缀、未知 API 和未知页面必须直接返回 HTTP 404，不能使用全局 SPA fallback。
- Vue 内部的未知子路由显示独立 404 页面，不跳转首页或仪表盘。

## API 约定

统一前缀为 `/api`，业务接口使用 `Authorization: Bearer <token>`。前端同时兼容直接 JSON 与 `{ "data": ... }` 包装响应。401 会清除会话并返回当前随机入口下的登录页。

### 登录

| 方法 | 路径 | 约定 |
| --- | --- | --- |
| GET | `/public/captcha` | `{ id, image }`，`image` 为 data URL；验证码比较不区分大小写 |
| POST | `/public/login` | body `{ username, password, captchaId, captcha, adminPath }`；返回 `{ token, user, expiresAt? }` |
| GET | `/auth/me` | 当前用户 |
| POST | `/auth/logout` | 注销当前会话 |
| POST | `/auth/change-password` | body `{ currentPassword, newPassword }` |

验证码和登录接口虽然不使用 Bearer Token，但必须由服务端校验随机入口与 `X-Admin-Path`，并应用验证码、频率限制等登录链路保护。

### 业务

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/dashboard` | 仪表盘汇总 |
| GET | `/providers` | 供应商配置列表 |
| PUT | `/providers/:id` | 更新 API 地址、密钥、Webhook 启用状态、供应商启用状态和轮询间隔 |
| GET | `/catalog/countries?provider=` | 可购买国家 |
| GET | `/catalog/services?provider=&country=` | 可购买服务 |
| GET | `/catalog/quote?provider=&country=&service=` | 实时报价 |
| POST | `/orders` | 购买号码；必须携带 16–128 字符的 `Idempotency-Key` 请求头 |
| GET | `/orders` | 分页订单列表 |
| GET | `/orders/:id` | 订单详情 |
| POST | `/orders/:id/complete` | 完成并结算 |
| POST | `/orders/:id/cancel` | 取消号码 |
| GET | `/users` | 用户列表 |
| POST | `/users` | 创建用户 |
| PUT | `/users/:id` | 更新用户 |

供应商规范代码为 `herosms`、`smsbower`、`smspool`。完整 DTO 位于 `src/types/api.ts`。

## 持续收码规则

- Webhook 或第三方轮询都只在后端运行，浏览器只查询本平台数据库。
- `pending`、`active`、`receiving` 等非终态订单每 5 秒从本平台 API 自动刷新；页面重新可见时立即刷新。
- 收到第一条验证码后继续刷新，多条短信按服务端消息 ID 分别展示，不覆盖历史记录。
- `settled`、`completed`、`cancelled`、`expired`、`failed` 为终态。
- 号码和每条验证码、每条短信全文均支持独立复制。
- API Key 和 Webhook Token 只写不回显；空值代表保留服务端已有密钥。
