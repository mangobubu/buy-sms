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
| GET | `/providers/balances` | 查询供应商实时余额与启用状态 |
| PUT | `/providers/:id` | 更新 API 地址、密钥、Webhook 启用状态、供应商启用状态和轮询间隔 |
| GET | `/catalog/services?provider=` | 可购买服务；兼容可选 `country` 参数 |
| GET | `/catalog/countries?provider=&service=&tier=` | 按服务查询可购买国家；`tier` 仅用于 SMSBower |
| GET | `/catalog/quote?provider=&country=&service=&tier=` | 实时报价；`tier` 仅用于 SMSBower |
| GET | `/catalog/durations?provider=herosms&country=&service=` | HeroSMS 当前服务与国家可购买的短时、长租时长及对应价格和库存 |
| POST | `/orders` | 购买号码；body 可带 SMSBower `tier` 或 HeroSMS `duration`，且必须携带 16–128 字符的 `Idempotency-Key` 请求头 |
| GET | `/orders` | 分页订单列表；每项通过可选的 `expiresAt`（RFC 3339）返回供应商给出的号码截止时间 |
| GET | `/orders/:id` | 订单详情 |
| GET | `/orders/:id/renewal-options` | 查询供应商当前允许的续期或重新启用选项及报价 |
| POST | `/orders/:id/renew` | 按所选选项续期或重新启用号码；必须携带 16–128 字符的 `Idempotency-Key` 请求头 |
| POST | `/orders/:id/complete` | 完成并结算 |
| POST | `/orders/:id/cancel` | 取消号码 |

取消能力以订单接口返回的 `canCancel`、`cancelAvailableAt`、`cancelWaitSeconds` 和 `cancelUnavailableReason` 为准。HeroSMS 标准号码购买满 2 分钟后可取消；HeroSMS 长期号码还须处于购买后 20 分钟内。SMSBower 根据当前实际接口行为允许购买后立即尝试取消，SMSPool 的短暂锁定时长由供应商动态裁决；两者若被上游暂时拒绝，接口会返回可重试提示。任何平台在已收到短信、订单终结或号码过期后均关闭取消入口。
| GET | `/users` | 用户列表 |
| POST | `/users` | 创建用户 |
| PUT | `/users/:id` | 更新用户 |

供应商规范代码为 `herosms`、`smsbower`、`smspool`。完整 DTO 位于 `src/types/api.ts`。
购买页统一先选服务再选国家。HeroSMS 的购买时长、价格和库存由接口按当前服务与国家动态返回，不在前端维护固定时长列表；标准短时激活仍展示当前账号有权限且有库存的多价格档位，长租则使用所选时长绑定的唯一价格与库存。未选择长租时，购买请求省略 `duration`，保持供应商默认短时激活语义。SMSBower 会合并 `bronze`、`silver`、`gold` 三个等级的可用资源，并在价格选项中展示等级；选择价格时会同时确定下单等级，无需单独选择。切换供应商时，页面会分别保留各供应商当前的服务、国家、HeroSMS 时长和价格选择；切换到其他管理页面再返回时，会恢复供应商、服务、国家和 HeroSMS 时长，并重新加载实时库存与报价，标准短时价格需要重新选择。供应商卡片会定时刷新实时余额；停用或未配置的供应商仍会显示，但不允许用于采购，余额显示为不可查询状态。

## 号码续期与重新启用

- 订单页仅在 `/orders/:id/renewal-options` 返回可用选项时显示操作按钮；资格以供应商实时 options/history 为准，不需要等到号码到期后再判断。
- HeroSMS 可按 prolong/options 返回的选项与报价续期，也可重新启用符合条件的 `completed` 订单；提交后以 history 返回的实际记录结算。
- SMSPool 仅支持重新启用美国 Foxtrot、未收到短信且已取消或退款的订单；选项价格来自 history 报价，提交后以 active 返回的实际价格结算。重新启用成功后订单不可退款。
- 每次提交都生成并复用 `Idempotency-Key`。上游返回超时或结果未知时，后端进入只读对账并暂时关闭重复提交，确认实际结果后再更新订单，避免重复扣费。

## 持续收码规则

- Webhook 或第三方轮询都只在后端运行，浏览器只查询本平台数据库。
- `pending`、`active`、`receiving` 等非终态订单每 5 秒从本平台 API 自动刷新；页面重新可见时立即刷新。供应商返回 `expiresAt` 时，页面按秒在本地刷新倒计时，不额外增加接口请求。
- 本地倒计时归零后显示“状态确认中”，等待服务端确认订单终态；`expiresAt` 缺失或无效时显示“等待平台同步”，由后续轮询补充供应商时效。
- 收到第一条验证码后继续刷新，多条短信按服务端消息 ID 分别展示，不覆盖历史记录。
- `settled`、`completed`、`cancelled`、`expired`、`failed` 为终态。
- 号码和每条验证码、每条短信全文均支持独立复制。
- API Key 和 Webhook Token 只写不回显；空值代表保留服务端已有密钥。
