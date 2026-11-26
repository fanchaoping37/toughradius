# 等保 2.0 三级测评材料：技术措施对应说明

> 本文件是**技术措施说明与测评取证清单模板**，不是测评报告。正式定级备案与测评须由具备资质的第三方机构出具。

## 1. 系统边界与定级建议

- 系统：校园网认证计费软件（ToughRADIUS + 二开接入网络模块）
- 建议定级：三级（S3A3G3）——承载校园网用户身份、计费资金与上网行为数据。
- 边界：RADIUS 1812/1813/2083(RadSec)/3799(CoA)、Portal UDP 50100、DHCP 67、DNS 53、DHCPv6 547、管理 HTTPS、开放 API `/api/v1/open/*`。

## 2. 安全物理环境 / 安全通信网络

| 测评项 | 本系统措施 | 取证位置 |
|---|---|---|
| 网络架构 | 管理面（Web/API）与认证面（RADIUS/UDP）分离监听；RadSec 走 TCP/TLS 2083 | `config/config.go` RadiusdConfig，`main.go` 各 Listen* |
| 通信加密 | RadSec TLS（RFC 6614）；管理面 HTTPS（`web.tls_enabled`）；开放 API HMAC-SHA256 签名 | `internal/radiusd/*radsec*`，`internal/adminapi/openapi.go` |
| 可信网络接入 | 未认证用户经 DHCP/DNS/HTTP 302 引流到 Portal；认证成功后 CoA 放行 | `internal/dhcp/`、`internal/dnsintercept/`、`internal/captive/` |

## 3. 安全区域边界

| 测评项 | 措施 | 证据 |
|---|---|---|
| 边界防护 | DHCP 仅下发受限地址与 Portal URL（RFC 7710 Opt160）；DNS 对未认证用户返回 Portal IP | `internal/dhcp/server.go`、`internal/dnsintercept/server.go` |
| 访问控制 | 管理 API 按 admin/super 角色鉴权（`requireAdmin()`）；开放 API 默认关闭（fail-closed） | `internal/adminapi/authz.go`、`openapi.go` |
| 入侵防范 | 报文校验：RADIUS Message-Authenticator（RFC 3579）、DHCP 畸形 Option 拒绝、DNS 非法标签拒绝、PPPoE 版本/Tag 校验、EAP-pwd 标量范围/曲线点校验 | 各协议包 `Parse*`/`Verify*` 与单测 |
| 恶意代码防范 | 依赖固定版本（go.mod + go.sum），无运行时代码加载 | `go.mod`、`go.sum` |

## 4. 安全计算环境

### 4.1 身份鉴别
- RADIUS 共享密钥 + Message-Authenticator（防 BlastRADIUS）；EAP-TLS/TTLS/PEAP/PWD 支持。
- 管理面 JWT（HS256，签名密钥 `web.secret`）；开放 API AppKey+Timestamp+Nonce+HMAC-SHA256，Nonce 5 分钟防重放。
- 口令不明文落库：支付凭据不入库；EAP-pwd 由明文口令派生 PWE（RFC 5931），不传输口令本身。

### 4.2 访问控制
- 用户—角色—权限：`internal/adminapi/authz.go`。
- Portal 一次性 auth_ticket（HMAC 签名、2 分钟 TTL、绑定 user_ip/MAC/Challenge、一次性消费）防越权。
- 客户端授权 IP 白名单与 TTL（`captive.Authorize/Revoke`）。

### 4.3 安全审计
- `SysOprLog` 审计表：管理操作与开放 API 调用均记录（含 traceId）。
- 计费话单全量落库：`radius_accounting` + `radius_acct_record`（含脱敏后的原始属性 `raw_attrs`）。
- CoA 任务表 `radius_coa_task`：记录 identifier/status/retry/error-cause，可追溯每次强制下线。
- 可选 ClickHouse 归档 `radius_accounting_archive`（MergeTree 按月份分区）。

### 4.4 数据完整性与保密性
- 传输：RadSec/管理 HTTPS/开放 API 签名。
- 存储：支付回调原始报文 `raw_notify` + `sign_verified` 标志；RADIUS 去重键 `dedup_key`（RFC 5080）防重放导致计费错乱。
- 完整性：EAP-pwd Confirm 交换与 MSK/EMSK 派生；DNS 答案 ID 回显与签名 return URL。

### 4.5 剩余信息保护 / 入侵防范
- 登录页 `Cache-Control: no-store`；密钥不出现在响应体或日志。
- 畸形报文一律丢弃且不响应（DHCP/DNS/PPPoE/EAP 解析错误路径）。

## 5. 安全管理制度（模板，需按机构实际补充）

1. 账号与权限：三权分立（系统管理员/安全管理员/审计管理员），最小权限，离职回收。
2. 密钥管理：RADIUS 共享密钥、APIv3 key、支付证书、JWT secret 分类存储，定期轮换，禁止入库/入代码。
3. 变更管理：所有二开走 dev 分支 + code review + 测试通过再合并；保留 commit 记录。
4. 日志留存：审计与话单日志≥180 天（等保三级要求），ClickHouse 分区归档便于长期留存。
5. 应急预案：RADIUS 集群（上游池 round_robin/weight）+ 代理转发，单点故障切换。

## 6. 测评取证清单模板

| 序号 | 测评项 | 取证方式 | 预期证据 | 状态 |
|---|---|---|---|---|
| 1 | 身份鉴别 | 抓包/配置核查 | RadSec TLS 握手、JWT 鉴权失败返回 401 | 待测评 |
| 2 | 访问控制 | 角色矩阵测试 | 普通用户访问 admin API 返回 403 | 待测评 |
| 3 | 审计 | 查询审计表 | 操作记录含操作者/时间/来源/IP/traceId | 待测评 |
| 4 | 数据完整性 | 重放测试 | 重复 Nonce/Idempotency-Key 被拒 | 待测评 |
| 5 | 入侵防范 | 畸形报文注入 | 畸形 DHCP/DNS/PPPoE/EAP 包被丢弃无响应 | 待测评 |
| 6 | 数据保密性 | 配置核查 | 管理 HTTPS 开启、密钥不落库 | 待测评 |
| 7 | 日志留存 | 数据库查询 | 审计/话单留存 ≥180 天 | 待测评 |
| 8 | 备份恢复 | 演练 | 双库（PostgreSQL/SQLite）+ ClickHouse 归档可恢复 | 待测评 |

## 7. 需人工补充的证明材料

- 定级备案回执、测评机构资质、安全管理制度正式发文。
- 机房/物理环境测评（本软件不涉及）。
- 第三方渗透测试报告与整改闭环记录。
- 真实商户支付凭证的密钥托管证明（本项目代码不含凭证）。
