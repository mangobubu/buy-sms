# 短信号码购买聚合平台

这是一个聚合 Hero-SMS、SMSBower 与 SMSPool 的短信号码购买平台。后端使用 Go、Gin 与 PostgreSQL，管理端使用 Vue 3 与 Element Plus。生产镜像会把前端静态资源嵌入 Go 可执行文件，因此只运行一个应用进程；PostgreSQL 作为独立持久化服务运行。

## 核心能力

- 聚合三个供应商的服务、国家、价格、库存、下单、取消与结算能力；SMSBower 支持 Gold、Silver、Bronze 号码等级。
- 供应商支持 Webhook 时优先接收推送，不支持时降级为服务端轮询。
- 号码在结算或取消前持续接收多条验证码，号码、短信和状态变更都持久化到 PostgreSQL。
- 登录鉴权与不区分大小写的图形验证码；除登录和验证码签发外，业务接口统一鉴权。
- 后台入口通过 `ADMIN_ENTRY_PATH` 随机化，未知 API、页面和静态资源均返回 404，不回退到首页。
- 号码和验证码支持一键复制，复制行为不影响服务端持续接码。

## 平台接收策略

- HeroSMS 使用原生 REST API，Webhook 为主，并通过完整 OTP 列表进行低频补偿对账。
- SMSBower 使用账户中配置的 Webhook，兼容接口轮询兜底；收到验证码后自动请求继续接收下一条。
- SMSPool 支持 Custom Webhook，同时保留轮询对账；需要下一条短信时先检查 resend 能力再请求 resend，供应商返回的追加费用会计入订单金额。
- 管理员需要在供应商后台填入本系统生成的 HTTPS 回调地址，再在本系统供应商配置页显式开启 Webhook。仅生成回调 Token 不代表上游已完成配置。
- 所有活动订单都以 PostgreSQL 中的状态为准。收到第一条验证码不会自动结算，只有完成、取消或上游明确过期后才停止接收。

## 目录约定

```text
cmd/server/       Go 服务入口
internal/         后端业务代码
frontend/         Vue 管理端源码
web/dist/         前端构建产物（构建时生成并由 Go 嵌入）
Dockerfile        前端 + 后端多阶段生产构建
```

生产构建顺序为：Vue 构建 → 产物复制到 `web/dist` → Go 编译并嵌入资源 → 仅复制单一二进制到运行镜像。

## 本地开发

需要 Go 1.24、Node.js 22、npm，以及一个独立的 PostgreSQL 实例。请使用项目专用数据库和端口，不要复用宿主机上其他服务的 Redis 或 PostgreSQL。

```bash
cp .env.example .env
make setup
make dev-api
```

另开终端启动前端开发服务器：

```bash
make dev-web
```

构建单一可执行文件：

```bash
make build
```

Windows PowerShell 可使用 `Copy-Item .env.example .env` 创建配置；没有 `make` 时，可直接执行 Makefile 中对应的 Go/npm 命令。

## 安全配置

首次部署前复制 `.env.example` 为 `.env`，并至少替换以下配置：

- `POSTGRES_PASSWORD`：建议 `openssl rand -hex 24`，使用十六进制可避免连接 URL 转义问题。
- `JWT_SECRET`：建议 `openssl rand -base64 48`，作为服务端会话令牌哈希与校验的独立秘密值；会话本身以不透明令牌和哈希形式存入数据库。
- `DATA_ENCRYPTION_KEY`：建议 `openssl rand -base64 32`，必须解码为恰好 32 字节，用于加密供应商密钥等敏感字段。
- `ADMIN_ENTRY_PATH`：例如 `/manage-` 加 `openssl rand -hex 16` 的结果，不要使用 `/admin` 等固定值。
- `ADMIN_PASSWORD`：使用密码管理器生成独立长密码。
- `PUBLIC_BASE_URL`：填写供应商可访问的外部真实 HTTPS 地址，Webhook 回调地址由此生成；生产环境不接受 HTTP 地址。

`.env` 已加入忽略名单。不要把真实密钥、数据库备份或生产日志提交到仓库。反向代理部署时，仅把代理地址加入 `TRUSTED_PROXIES`，并在代理层启用 HTTPS、请求体限制与访问日志脱敏。

首次登录后，请在“供应商配置”页录入 Hero-SMS、SMSBower 与 SMSPool 的 API 地址和密钥。供应商凭证只以加密形式写入 PostgreSQL，不通过浏览器存储或容器环境变量维护；修改 `DATA_ENCRYPTION_KEY` 前必须按应用的数据密钥轮换流程重新加密已有凭证。

购买接口要求 `Idempotency-Key` 请求头。管理端会为一次购买尝试生成并复用该键；当上游响应超时、结果未知时，同键重试不会再次购买，避免重复扣费。
SMSBower 的 `tier` 可取 `gold`、`silver`、`bronze`；报价与购买使用同一等级，等级也是幂等请求身份的一部分。

## Docker 部署

Compose 不会把 PostgreSQL 端口映射到宿主机，数据库只存在于项目内部网络。应用默认只绑定宿主机 `127.0.0.1:18080`，可通过 `APP_BIND_ADDR` 和 `APP_PORT` 修改；公网部署建议保持本机绑定并由 Nginx、Caddy 或负载均衡器终止 TLS。

PostgreSQL 数据使用固定的外部卷。生产 `.env` 必须用不加引号、不带行尾注释的单行 `POSTGRES_VOLUME_NAME` 指定卷名，默认示例为 `buy-sms_postgres-data`。仅在确认是首次部署时显式创建：

```bash
make docker-volume-create
```

`make docker-up` 与 `make docker-up-cn` 会先只读检查该卷；卷缺失时部署会中止，已上线环境应恢复原卷而不是新建空卷。由于该卷声明为 external，`docker compose down -v` 也不会删除它。

国际网络环境：

```bash
make docker-config
make docker-up
```

中国大陆网络环境（叠加镜像与依赖源配置）：

```bash
make docker-config-cn
make docker-up-cn
```

仅检查仓库内示例配置的结构时，可执行 `docker compose --env-file .env.example config --quiet`；该命令只解析配置、不启动服务，示例密钥不得用于实际部署。

大陆覆盖文件默认使用可替换的 Docker 镜像代理、npmmirror、goproxy.cn 与阿里云 Alpine/Debian 软件源。企业环境可在 `.env` 中覆盖所有 `CN_*` 变量，指向内部可信镜像仓库。国际与大陆构建都保留 Go 校验数据库，不通过关闭 `GOSUMDB` 来换取下载成功。

查看状态与日志：

```bash
docker compose ps
docker compose logs -f app
```

停止服务并保留数据库卷：

```bash
docker compose down
```

显式执行 `docker compose down -v` 会删除 PostgreSQL 数据卷，仅应在确认不需要数据后使用。上线前应为 `postgres-data` 建立定期备份和恢复演练。

## 健康检查与运行约束

- 应用健康检查地址为 `/healthz`；该接口只报告进程与数据库就绪状态，不返回敏感信息。
- 应用容器以 UID 10001 的非 root 用户运行，根文件系统只读，并移除 Linux capabilities。
- PostgreSQL 使用具名卷持久化，不依赖客户端缓存保存订单、号码、验证码或轮询状态。
- 结束本地验证后应停止开发服务；Compose 验证结束可执行 `docker compose down`，数据库卷默认保留。
