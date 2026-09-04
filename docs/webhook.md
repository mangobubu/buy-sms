# Webhook 使用说明

本平台为 HeroSMS、SMSBower 和 SMSPool 分别生成独立的 Webhook 回调地址。供应商推送到达后，平台会校验地址中的 Token，将短信写入对应订单，并继续保留服务端轮询用于补偿对账。

收到第一条短信不会自动结算订单；活动订单可以继续接收多条短信，直到手动完成、取消或上游明确标记为过期。

## 部署前提

1. 在 `.env` 中把 `PUBLIC_BASE_URL` 设置为供应商可从公网访问的 HTTPS 地址，例如：

   ```dotenv
   PUBLIC_BASE_URL=https://sms.example.com
   ```

2. 重新部署应用，使新配置生效。生产环境要求该值是有效的 HTTPS 绝对地址。
3. 确保反向代理、防火墙和 WAF 允许供应商向 `/api/webhooks/` 下的地址发送 `POST` 请求。该路径不受 `ADMIN_ENTRY_PATH` 影响。
4. 反向代理必须把请求体原样转发给应用。单次回调请求体上限为 2 MiB。

完整回调地址类似：

```text
https://sms.example.com/api/webhooks/herosms/<token>
```

三个供应商对应的路径标识如下：

| 供应商 | 路径标识 |
| --- | --- |
| HeroSMS | `herosms` |
| SMSBower | `smsbower` |
| SMSPool | `smspool` |

回调地址中的 Token 等同于凭证。不要把完整地址提交到仓库、粘贴到工单或写入公开日志；同时应在反向代理访问日志中对 `/api/webhooks/<provider>/<token>` 的最后一段进行脱敏。

## 配置步骤

1. 使用管理员账号进入后台的“供应商配置”页面。
2. 先为目标供应商填写 API 地址和 API Key，并启用该供应商。
3. 保存后，复制页面显示的“Webhook 回调地址”。平台初始化供应商时会自动生成随机 Token，Token 原文不会单独回显。
4. 在供应商后台填写完整回调地址：

   - HeroSMS：进入账号个人信息（Personal information），可填写最多 3 个 HTTPS Webhook 地址。
   - SMSBower：填写到个人资料（Profile）的 Webhook URL 配置中。
   - SMSPool：进入 Settings，在 Your webhook 中选择 Custom webhook 并填写地址。

5. 在供应商后台保存并完成其提供的回调测试；如供应商没有测试功能，可按下文使用测试订单验证。
6. 确认上游配置完成后，回到“供应商配置”页面，打开“已在供应商后台配置 Webhook”，设置兜底轮询间隔并保存。

仅看到或生成回调地址，不代表供应商后台已经开始推送。Webhook 开启后轮询对账仍会保留，用于补偿延迟、漏推或推送失败。

如果手动填写 Webhook Token，长度至少为 24 个字符。新 Token 保存后旧回调地址会立即失效，应同步更新供应商后台，避免出现推送空窗期。一般情况下直接使用平台自动生成的 Token 即可。

## HeroSMS 专项说明

HeroSMS 支持原生 Webhook。其官方 API 规范说明，可以在账号个人信息中同时配置最多 3 个 HTTPS 回调地址，每个地址会独立收到推送。

配置 HeroSMS 时：

1. 在本平台“供应商配置”的 HeroSMS 卡片中复制完整回调地址，地址路径应包含 `/api/webhooks/herosms/`。
2. 进入 HeroSMS 账号个人信息，将地址填入 Webhook 配置并保存。
3. 如果服务器使用来源 IP 白名单，HeroSMS 官方当前公布的推送地址为 `84.32.223.53` 和 `185.138.88.87`；上线前应在其最新 API 文档中再次核对。
4. 创建一个 HeroSMS 测试订单，等待真实短信或按下文发送测试请求，确认订单详情出现短信。
5. 验证成功后，在本平台打开“已在供应商后台配置 Webhook”并保存。

HeroSMS 使用 `POST` 和 `application/json` 推送，主要字段包括 `activationId`、`phoneFrom`、`service`、`text`、`code`、`country` 和 `receivedAt`。本平台使用 `activationId` 关联上游订单，`phoneFrom` 不参与订单匹配。

HeroSMS 等待回调响应的时间为 3 秒。未收到 `200` 时，官方规范说明会至少重试 7 次，重试间隔为 20～30 秒，总尝试时间不少于 3 分钟。因此回调入口应尽快返回 `200`，重复推送由本平台负责幂等处理。

## 回调请求格式

供应商应向页面生成的完整地址发送 JSON 格式的 `POST` 请求。使用供应商原生 Webhook 时保持其原始 JSON；自定义接入或排查时可参考下面的规范化示例：

```http
POST /api/webhooks/herosms/<token> HTTP/1.1
Content-Type: application/json

{
  "activationId": "UPSTREAM_ORDER_ID",
  "code": "123456",
  "text": "Your verification code is 123456",
  "service": "tg",
  "country": "2",
  "receivedAt": "2026-09-04T12:30:00+08:00"
}
```

字段名不区分大小写，平台兼容以下字段：

| 含义 | 是否必填 | 支持的字段名 | 说明 |
| --- | --- | --- | --- |
| 上游订单 ID | 是 | `activationId`、`activation_id`、`orderid`、`orderId`、`id` | 必须与平台订单保存的上游订单 ID 一致 |
| 验证码 | 与短信正文至少一个 | `code`、`sms`、`smsCode` | 字符串或数字均可 |
| 短信正文 | 与验证码至少一个 | `text`、`full_sms`、`fullSms`、`smsText` | 用于保存完整短信内容 |
| 服务代码 | 否 | `service` | 提供时必须与订单服务代码一致 |
| 国家代码 | 否 | `country` | 提供时必须与订单国家代码一致 |
| 接收时间 | 否 | `receivedAt`、`received_at`、`timestamp` | 缺省时使用平台收到请求的时间 |

接收时间支持 RFC 3339、Unix 秒级时间戳、Unix 毫秒级时间戳、`YYYY-MM-DD HH:mm:ss`，以及 SMSPool 使用的 `YYYY-MM-DD hh:mm:ssam/pm` 格式。建议始终发送带时区的 RFC 3339 时间，这样同一订单在不同时间收到相同验证码时仍能被识别为不同短信。

## 响应与重试

| HTTP 状态码 | 含义 | 处理建议 |
| --- | --- | --- |
| `200` | 请求已处理，或重复/无需写入的事件已被幂等确认 | 停止重试 |
| `400` | JSON 无效，或缺少订单 ID、验证码/短信正文 | 修正请求内容后再发送 |
| `404` | 供应商标识或 Token 不匹配 | 核对完整回调地址，尤其是 Token 是否已轮换 |
| `413` | 请求体超过 2 MiB | 缩小请求体后再发送 |
| `500` | 平台暂时处理失败 | 使用退避策略重试 |

相同 JSON 即使字段顺序不同，也只会保存一次。供应商可安全重试同一个事件；平台会返回成功响应，避免重复短信和无意义的持续重试。

`200` 表示平台已经确认该事件，不一定表示新增了一条短信。例如，找不到对应活动订单、订单已经结束、服务或国家不匹配、事件重复，或者相同短信已由轮询先行写入时，平台会记录或忽略事件并返回成功。

## 验证配置

请使用专门的测试订单，避免向真实业务订单写入测试短信。

1. 通过平台创建一个活动订单，记下供应商返回的上游订单 ID。
2. 优先使用供应商后台的 Webhook 测试功能；也可以用以下请求替换回调地址、上游订单 ID、服务代码和国家代码后测试：

   ```bash
   curl -i -X POST 'https://sms.example.com/api/webhooks/herosms/<token>' \
     -H 'Content-Type: application/json' \
     --data '{"activationId":"UPSTREAM_ORDER_ID","code":"123456","text":"Webhook test 123456","service":"tg","country":"2","receivedAt":"2026-09-04T04:30:00Z"}'
   ```

3. 确认响应为 `200` 且内容为 `{"ok":true}`。
4. 在订单详情中确认测试短信已出现。
5. 原样重复发送一次，确认仍返回 `200`，并且订单中没有新增重复短信。
6. 测试完成后结束该测试订单。

## 常见问题

### 供应商请求不到回调地址

- 检查 `PUBLIC_BASE_URL` 是否为公网可解析、证书有效的 HTTPS 地址。
- 检查反向代理是否把 `/api/webhooks/` 转发到应用，而不是转到后台随机入口。
- 检查防火墙、WAF、CDN 和代理的请求体限制及 `POST` 方法限制。
- 查看脱敏后的反向代理访问日志，确认请求是否到达以及返回的状态码。
- SMSPool 连续请求失败达到其限制后会停用 Webhook；修复地址后，需要回到 Settings 重新执行 Update Webhook。

### 返回 404

- 从“供应商配置”页面重新复制完整地址，避免手工拼写供应商标识或 Token。
- 如果最近轮换过 Token，确认供应商后台没有继续使用旧地址。
- 确认代理没有改写路径、删除路径段或自动追加斜杠。

### 返回 200，但订单里没有新短信

- 确认回调中的上游订单 ID 与平台保存的上游订单 ID 完全一致，并且订单仍处于活动状态。
- 如果请求包含 `service` 或 `country`，确认它们与订单一致。
- 检查该事件是否已经推送过，或相同短信是否已由轮询先行写入。
- 确认查看的是该供应商对应的订单，而不是另一个供应商生成的订单。

### 更换域名或 Token

修改 `PUBLIC_BASE_URL` 并重新部署，或保存新的 Token 后，页面会生成新的完整回调地址。必须逐个更新对应供应商后台的地址并重新验证；旧 Token 在保存新 Token 后立即失效。

## 供应商官方参考

供应商可能调整后台入口、来源 IP 或重试策略，部署时应同时核对其最新说明：

- [HeroSMS API 文档](https://hero-sms.com/api)
- [SMSBower API 文档（Notification via Webhook）](https://smsbower.com/api/?page=client)
- [SMSPool Webhook 配置说明](https://www.smspool.net/article/how-to-setup-webhooks-for-smspool-ec19b80ade92)
