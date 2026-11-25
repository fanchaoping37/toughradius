# 校园网认证计费软件二开（第三批）实现说明

分支：`dev-freeradius-compare`。本文对应 `校园网认证计费软件.pdf`（SRS，共 68 页）中缺失的核心运营能力，按 RFC / IANA / FreeRADIUS 权威资料实现，而非照抄 SRS 中的错误描述。

## 1. 协议依据与 SRS 纠错

所有协议细节均核对了权威来源；以下修正已落实到代码常量与注释：

| 项目 | SRS 原描述 | 权威结论（已实现） | 来源 |
|---|---|---|---|
| User-Password 混淆 | 含 Identifier | `b1=MD5(Secret‖RequestAuth)`，**不含 Identifier** | https://www.rfc-editor.org/rfc/rfc2865.txt §5.2 |
| Acct-Status-Type | Start=1 / Interim=2 / Stop=3 | **Start=1 / Stop=2 / Interim-Update=3** | https://www.rfc-editor.org/rfc/rfc2866.txt §5.1；https://www.iana.org/assignments/radius-types/ |
| Event-Timestamp(55) | 归于 RFC 2866 | 出自 **RFC 2869 §5.3** | https://www.rfc-editor.org/rfc/rfc2869.txt |
| Gigawords(52/53) | 只写“合并” | `total = gigawords*2^32 + octets`；FreeRADIUS `acct_counters64` 同义 | https://www.rfc-editor.org/rfc/rfc2869.txt §5.1/§5.2；https://github.com/freeradius/freeradius-server/blob/v3.0.x/raddb/sites-available/default |
| CoA Error-Cause | 403=Invalid-Attribute-Value、503=Session-Context-Removed | 403=NAS Identification Mismatch，407=Invalid Attribute Value，503=Session Context Not Found | https://www.rfc-editor.org/rfc/rfc5176.txt §3.5；https://www.iana.org/assignments/radius-types/ |
| 计费去重 | 仅“幂等键” | RFC 5080 §2.2.2：按（源 IP、源端口、Identifier）缓存并重发原响应，5–30 秒清理 | https://www.rfc-editor.org/rfc/rfc5080.txt §2.2.2 |
| VSA Vendor-ID | 华为 2011 / H3C 25506 / 锐捷 4881 / Cisco 9 / ZTE 3902 / Juniper 2636 | 与 IANA PEN 注册表一致 | https://www.iana.org/assignments/enterprise-numbers/ |
| Portal V2 CHAP | 文档未给出权威公式 | 华为官方确认 Portal 2.0 兼容中国移动 Portal 2.0、优先 CHAP；**逐字节报文头/摘要公式无权威公开全文**，保持第二批实现并标记需设备实测复核 | https://support.huawei.com/enterprise/en/doc/DOC1000060368/f894b506/EN-US_TOPIC_0000001256387161 |

参考 Java 仓库 `https://github.com/fanchaoping37/XuJing-Radius` 在该账号下返回 GitHub 404（API `GET /repos/fanchaoping37/XuJing-Radius` 亦为 404），因此未作为实现依据；编码以 RFC、IANA、FreeRADIUS 源码为准。

## 2. 本批交付模块

### 2.1 协议合规与话单完整性（SRS §8/§12.5）
- `internal/radiusd/plugins/accounting/packet.go`：Event-Timestamp 解析、计费开始时间 `Event - SessionTime - Delay`、Gigawords 64 位合并、RFC 5080 去重键、属性快照脱敏（密码掩码、EAP 截断）。
- `internal/domain/accounting_record.go`：`radius_acct_record`（逐包话单）、`radius_acct_dedup`（30 秒响应缓存）。
- 话单模型新增 `event_timestamp / acct_delay_time / acct_status_type / dedup_key / raw_attrs`；Start/Interim/Stop 与仓储层全部写入，Stop 以 NAS 事件时间作为结算时间。
- 去重命中时仍发送 Accounting-Response，但跳过插件处理与代理抄送，避免重复计费/重复转发。

### 2.2 计费与账务核心（SRS 模块 3/8、§10、§12.4）
- `internal/domain/billing.go`：账户多资产（资金/时长/流量，含赠送额度）、资产流水（只追加 + 唯一幂等键）、套餐、价格规则（阶梯/优惠时段 JSON）、订购、会话用量、账单、订单、支付、卡批次/卡。
- `internal/billing/engine.go`：
  - 乐观锁 `WHERE version=?` + CAS 更新，冲突重试 5 次；
  - 赠送流量优先扣减，写独立 gift 流水；
  - 按 Acct-Session-Id 维护累计用量，Interim-Update 只计增量，乱序/重传不重复扣费；
  - 计费模式：包月/包天（订购时一次性）、计时长（向上取整计费单位）、流量、阶梯（左闭右开）、封顶、组合；优惠时段按 Event-Timestamp 归属。
- `internal/billing/orders.go`：订单幂等、支付回调渠道流水号唯一 + HMAC 验签后入账、卡兑换、账单幂等开具。
- `internal/adminapi/billing.go`：账户/套餐/订购/订单/账单 API，充值必须带 `Idempotency-Key`。

### 2.3 CoA 任务持久化（SRS 模块 16.5、§12.5）
- `internal/domain/coa_task.go` + `internal/radiusd/coa_task_manager.go`：任务先落库再发送；状态 pending→sending→ack/nak/timeout/failed；保存 Identifier、Attempts、Error-Cause、RTT。
- `CoAService` 新增 `WithCoAIdentifier`，进程重启重投时复用同一 Identifier（RFC 5176 §3）。
- 管理端 Disconnect/CoA 同步落任务；`/api/v1/coa-tasks` 查询、`/coa-tasks/retry` 重投。

### 2.4 Portal auth_ticket（SRS §9.2）
- `internal/portal/ticket.go`：HMAC-SHA256 签名、2 分钟 TTL、绑定用户名 + IP + MAC + Challenge SerialNo、一次性消费、签名/重放/篡改/过期校验。
- `Service.Challenge` 签发票据，`Service.Authenticate` 校验并消费；启动时由 `main.go` 启用强制票据。HTTP bridge 的 `auth_ticket` 字段自动兼容。

### 2.5 开放平台 API（SRS §11）
- `config.OpenAPIConfig`：默认关闭（fail-closed），AppKey→AppSecret 映射、QPS、Nonce TTL、幂等 TTL；环境变量 `TOUGHRADIUS_OPENAPI_ENABLED / CREDENTIALS / QPS / NONCE_TTL / IDEMPOTENCY_TTL`（凭据格式 `key1:secret1,key2:secret2`）。
- 签名：`HMAC-SHA256(secret, METHOD\nPATH\nTimestamp\nNonce\nSHA256(body))`；5 分钟时间窗 + Nonce 防重放 + 令牌桶限流（429 + Retry-After）+ `SysOprLog` 审计 + `traceId`。
- 写操作强制 `Idempotency-Key`，24 小时内重放返回首次响应。
- 端点：`/api/v1/open/accounts`（POST）、`/open/accounts/:id/status`（PUT）、`/open/accounts/:id/quota`（GET）、`/open/sessions/online`（GET）、`/open/sessions/:id/disconnect`（POST）、`/open/plans`（GET）、`/open/orders`（POST）。该组跳过 JWT，只由 HMAC 中间件保护。

## 3. 验证

- `go build ./...` 通过。
- `go test -short -count=1 ./internal/...` 全绿。
- 本次新增测试：协议合规 5 个、计费引擎 8 个、计费 API 2 个、CoA 任务 3 个、Portal 票据 4 个、开放 API 3 个。
- 既有 `config` 包 3 个 Windows 路径分隔符测试在干净 main 分支上同样失败，与本次无关。

## 4. 未覆盖的 SRS 范围（需后续迭代）

以下属于网络层/第三方集成/前端范畴，本批不声称完成：DHCP 地址池与 Option 43/82、DNS 劫持与白名单视图、内置 Portal 登录页与 302 重定向、802.1X 新增 EAP 方法、PPPoE/IPoE 真实拨号网关、微信/支付宝/一卡通 SDK 对接、Web 前端与小程序、短信/邮件推送、ClickHouse/Redis 集群、IPv6 单栈溯源、等保测评材料与运营大屏。

Portal V2 的逐字节报文格式缺少厂商权威公开全文，若现场设备验证不通过，应以设备厂商正式协议文档为准调整 `internal/portal/protocol.go`。
