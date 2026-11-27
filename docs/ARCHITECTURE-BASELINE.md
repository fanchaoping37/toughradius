# ToughRADIUS 架构与开发基线

> **当前唯一有效的架构与开发状态文档**
> 更新日期：2026-11-27
> 仓库：`fanchaoping37/toughradius`（上游 `talkincode/toughradius`）
> 开发分支：`dev-freeradius-compare`
> 基准提交：`3e61b9de74d80b10f906e1260ade93ead10eb6b1`（第四批二开完成）
> 参考范式：用户提供的 Java 版《Radius-Vue-Plus 架构与开发基线》。本文合并此前四批二开的设计说明，
> 历史审查证据见 `docs/freeradius-comparison.md`、`docs/campus-aaa-second-dev.md`、
> `docs/campus-aaa-fourth-dev.md`、`docs/portal-v2-integration.md`。

---

## 1. 文档效力与状态定义

### 1.1 效力优先级

本文是当前架构与开发状态的唯一权威文档。发生冲突时按以下顺序判断：

1. 当前 `dev-freeradius-compare` 分支代码、`config/config.go`、`internal/domain/tables.go`；
2. 用户在项目讨论中明确确认的业务规则；
3. 本文的状态矩阵与已冻结架构决策；
4. RFC / IANA / FreeRADIUS 3.2.x / 厂商协议与用户提供的参考资料；
5. 历史专项文档与批处理说明（仅作证据，不与本文冲突解释）。

### 1.2 状态定义

| 状态 | 含义 |
|---|---|
| **已实现（静态代码）** | 代码存在对应实现且有单元测试；不代表真实设备验收通过 |
| **已接线** | 除实现外，已被 `main.go` / 管线 / 路由实际调用，进入运行路径 |
| **部分实现** | 模型、协议原语或部分业务链存在，但闭环不完整 |
| **库已实现未接线** | 包可编译、有测试，但生产入口无引用（孤儿包），不能视为产品能力 |
| **未实现** | 当前分支未发现可用实现，或仅有常量 / 接口契约 |
| **待验证** | 代码存在，但未完成编译、自动化测试、数据库迁移、压力测试或真实设备联调 |
| **暂缓** | 用户已要求暂不实现，未经重新确认不得擅自推进 |

### 1.3 基线更新规则（强制）

> 这是后续所有开发的对照基准。必须遵守：
>
> 1. **开发前对照**：任何新功能开始前，先读本文第 2、18、19 节，确认目标能力现有状态，禁止重复建设。
> 2. **开发后更新**：每次提交代码后，必须同步更新本文——① 更新头部"基准提交"；② 修改相关功能域状态；
>    ③ 在文末"基线变更记录"追加一行；④ 如新增模块，补入第 2.2 模块职责表与第 14 数据模型表。
> 3. **旧的删除**：本文是唯一有效基线。**禁止**创建 `ARCHITECTURE-BASELINE-v2.md`、
>    `架构基线-2026xxxx.md` 等并存版本；历史状态只保留在文末变更记录中，旧文件一律删除。
> 4. **禁止夸大**：静态源码、接口契约、数据库表、监听端口或设计文档都不能单独证明业务闭环或生产就绪。
>    未执行验证的能力必须标记"待验证"。

---

## 2. 产品定位与总体架构

### 2.1 定位

ToughRADIUS 的最终目标是**可私有化部署、可多租户运营、可扩展集成的商业化 AAA 认证计费平台**，
不是单一 RADIUS daemon，也不把 DHCP/DNS/NAT/DPI/PPPoE 全部塞进 RADIUS 实时核心。
当前代码是在开源 Go RADIUS 服务器（ants 协程池 + GORM + Echo）基础上，经四批二开扩展而来。

### 2.2 控制面与数据面

- **管理控制面**：`internal/adminapi` + `internal/webserver` + `web/`——用户、租户节点、NAS、身份凭据、
  套餐订阅、权限、审计、配置发布与运维管理。
- **AAA 数据面**：`internal/radiusd`——RADIUS UDP 接入、协议解码、安全验证、认证/授权、Accounting 接收、
  动态授权、RadSec、代理转发。
- 两者在**同一 Go 单二进制进程**中，通过 `main.go` 的 `errgroup` 并行启动；不拆微服务。
- 数据库是配置权威源；`internal/app` 提供动态配置与 TTL 缓存刷新。Redis 仅作可选缓存，
  不得成为唯一可靠事件源或财务真相源。

### 2.3 模块职责

| 包 | 职责 | 接线状态 |
|---|---|---|
| `internal/radiusd` | 认证/计费管线、CoA、DynAuth、RadSec、registry、repository、代理 | 已接线 |
| `internal/radiusd/proxy` | realm 匹配、上游池、凭据转换、转发/抄送客户端 | 已接线 |
| `internal/radiusd/plugins/eap` | EAP 方法引擎（MD5/MSCHAPv2/TLS/PEAP/TTLS/PWD） | 已接线 |
| `internal/radiusd/plugins/accounting` | Start/Interim/Stop/NAS-On/Off handler、报文合规 | 已接线 |
| `internal/domain` | 27 个 GORM 实体与迁移清单 | 已接线 |
| `internal/app` | 应用装配、迁移、动态配置、缓存刷新、cron、metrics | 已接线 |
| `internal/adminapi` | `/api/v1` 管理 REST + 开放 API HMAC 鉴权 | 已接线 |
| `internal/webserver` | Echo HTTP/HTTPS、嵌入式 React UI、JWT、captive 中间件 | 已接线 |
| `internal/portal` | 华为/CMCC Portal V2 UDP 协议侧、auth_ticket | 已接线 |
| `internal/accessnet` | DHCP/DNS/DHCPv6/Captive 统一装配 | 已接线（默认关闭） |
| `internal/dhcp` / `dnsintercept` / `dhcpv6` | RFC 2131 / 1035 / 3315 接入网服务 | 已接线（默认关闭） |
| `internal/captive` | HTTP 302 与内置登录页（二开突破项目"不托管登录页"铁律） | 已接线（默认关闭） |
| `internal/billing` | 计费引擎：整数分/秒/字节、赠品优先、乐观锁、幂等 | 已接线 |
| `internal/pppoe` | PPPoE Discovery/PPP 线格式 + Linux AF_PACKET | **未接线**（仅线格式层） |
| `internal/payment` | 微信 v3 / 支付宝 RSA2 / 一卡通 HMAC 验签 | **未接线**（孤儿包） |
| `internal/cache` | Redis TTL K/V + 内存实现 | **未接线**（孤儿包） |
| `internal/analytics` | ClickHouse 话单归档 | **未接线**（孤儿包） |
| `internal/ldapauth` | LDAP/AD Bind 口令校验（仅 PAP 族） | 已接线 |
| `internal/benchmark` | RADIUS 压测客户端 | 命令行工具 |
| `pkg/` | certgen / common / excel / metrics / timeutil / validator / validutil / web | 公共库 |
| `cmd/` | benchmark / certgen / config-tool / demo-seed / radtest / reset-password / testdata | 命令行工具 |

### 2.4 技术基线

| 技术 | 当前代码基线 |
|---|---|
| Go | 1.26.0（`go.mod`） |
| RADIUS 协议库 | `layeh.com/radius` → `replace` 到自维护 fork `github.com/talkincode/radius v0.1.0` |
| Web 框架 | Echo v4.16.0 + echo-jwt v4.4.0 + golang-jwt v5 |
| ORM / DB | GORM v1.31.2 + postgres v1.6.3 + `glebarez/sqlite` v1.11.0（纯 Go） |
| 并发池 | `panjf2000/ants/v2` v2.12.1 |
| 缓存 / 分析 | `redis/go-redis/v9` v9.24.0、`ClickHouse/clickhouse-go/v2` v2.51.0（均未接线） |
| 前端 | React 19.3 + react-admin 5.15 + MUI 7（overrides）+ Vite 8 + TypeScript 7 |
| 小程序 | `miniprogram/` 原生微信小程序（登录页 + 后端签名 API 工具） |
| 日志 / 调度 | zap v1.28、robfig/cron v3 |
| 部署 | 单二进制；Dockerfile / docker-compose；`-initdb` 初始化 |

---

## 3. RADIUS 端点与网络边界

| 服务 | 协议 / 端口 | 配置字段 | 默认 |
|---|---|---|---|
| Access | UDP 1812 | `radiusd.auth_port` | 启用 |
| Accounting | UDP 1813 | `radiusd.acct_port` | 启用 |
| RadSec | TCP/TLS 2083 | `radiusd.radsec_port` | 启用（证书缺失时安全拒绝） |
| DynAuth / CoA / DM | UDP 3799 | `radiusd.dynauth_port` | 关闭（`dynauth_enabled=false`） |
| Portal V2 | UDP 50100（NAS 侧 2000） | `portal.bind_port` / `nas_port` | 关闭 |
| Web 管理 | HTTP 1816 / HTTPS 1817 | `web.port` / `web.tls_port` | 启用 |
| DHCPv4 / DNS / DHCPv6 / Captive | 67 / 53 / 547 / 80 | 各段 `bind_addr` | 全部关闭 |

- UDP 总开关 `radiusd.enabled`；关闭时不监听任何 RADIUS 端点。
- 二开接入网服务全部**默认关闭**，需显式设置 `TOUGHRADIUS_DHCP_ENABLED` 等开关。
- 生产启动前必须设置 `web.secret` / `TOUGHRADIUS_WEB_SECRET`；生产模式拒绝占位或空 JWT 密钥。

---

## 4. RADIUS 协议与安全基线

### 4.1 报文与字典

- 属性编号 / 枚举以 RFC + IANA + 厂商字典为准（仓库 `docs/rfcs/` 收录 60+ RFC 原文，`share/` 字典来自 FreeRADIUS）。
- Vendor-Id 使用 IANA PEN（华为 2011、H3C 25506、锐捷 4881、Cisco 9、中兴 3902）。
- 厂商解析在 `internal/radiusd/plugins/vendorparsers/`，8 个 VSA 解析器 + 8 个响应增强器（华为/H3C/中兴/
  MikroTik/爱快/Aruba/Cisco/default）。VSA 目前以代码为主，配置驱动字典尚未完成。

### 4.2 Authenticator 与 Message-Authenticator

- Access / Accounting / CoA/Disconnect 及响应按各自 RFC 规则校验 Authenticator。
- `message_authenticator.go` 三种模式：`disabled` / `warn` / `enforce`，配置键 `radius.RequireMessageAuthenticator`，默认 `warn`；
  含 `verifyMessageAuthenticator` / `verifyResponseMessageAuthenticator` / `addResponseMessageAuthenticator`。
- EAP-Message 报文必须遵守 RFC 3579 的 MA 要求。
- 已做 BlastRADIUS 防护（MA 校验）；已纠正计费认证器计算（置零）、EAP 请求不代理。

### 4.3 Secret 分类与生命周期（冻结决策）

以下三类凭据必须分离，各自独立加密与审计：

1. NAS RADIUS Shared Secret（`net_nas.secret`）；
2. 上游 RADIUS Server Shared Secret（`radius_upstream.secret`）；
3. 用户运营商账号密码（`radius_realm_route.mapped_password`）。

另有 Portal NAS Secret（`portal_nas.secret`）与 Web JWT Secret（`web.secret`），不得混用。
当前 Secret 以字段加密存储为主；生产 KMS/HSM、密文完整性验证与轮换审计未完成。无生产密钥提供器时应失败关闭。

---

## 5. Access 认证与授权

### 5.1 处理阶段（9 阶段管线，`auth_stages.go`）

| # | Stage | 职责 |
|---|---|---|
| 1 | `request_metadata` | 请求元数据、远程 IP |
| 2 | `nas_lookup` | NAS 查找与 Secret 绑定 |
| 3 | `message_authenticator` | MA 校验（warn/enforce） |
| 4 | `auth_rate_limit` | 认证限流 |
| 5 | `proxy_dispatch` | **二开**：realm 匹配，命中即转发上游（EAP 跳过） |
| 6 | `vendor_parsing` | 厂商 VSA 解析 |
| 7 | `load_user` | 本地用户加载 |
| 8 | `eap_dispatch` | EAP 方法分发 |
| 9 | `plugin_auth` | 密码校验 / 策略检查 / 响应增强 / 认证守卫 |

管线支持 `Use/InsertBefore/InsertAfter/Replace/Remove`，`AuthPipelineContext.Stop` 可短路。

### 5.2 当前实现

- PAP/CHAP 本地认证、用户组/套餐策略、并发数、MAC/VLAN 绑定、有效期检查已实现。
- 授权采用 fail-closed：未匹配策略或凭据缺失不得隐式放行。
- 重复请求去重：认证侧有 TTL 缓存基础；计费侧已实现 RFC 5080 去重（见第 6 节）。
- Access-Accept 的限速/VLAN/Class 属性通过响应增强器下发。
- 未实现：财务扣费与认证结果的事务联动（当前认证与计费扣费异步解耦）。

---

## 6. Accounting、Session 与话单

### 6.1 可靠接收与合规（第三批修正）

- 入口 `AcctService.ServeRADIUS` → `HandleAccountingWithPlugins` → Start/Interim/Stop/NAS-On/Off handler。
- **Acct-Status-Type**：1=Start、2=Stop、3=Interim-Update（纠正早期文档错误）。
- **Event-Timestamp(55)** 属 RFC 2869，优先于本地时间归属事件。
- **Acct-Delay-Time(41)** 落库；**Gigawords(52/53)** 按 `gigawords*2^32 + octets` 合并为 64 位。
- **RFC 5080 去重**：`DedupKey` = 源 IP + 源端口 + Identifier，缓存窗口内重发原响应；落 `radius_acct_dedup`。
- 原始属性脱敏后落 `radius_acct_record.raw_attrs`，供审计与对账。
- 用户属性 Class 携带 `profile=<套餐名>` 供下游抄送识别。

### 6.2 Session 与对账

- `RadiusOnline` 为在线会话，`RadiusAccounting` 为话单；Start/Stop 由 handler 写入。
- `RadiusOnline` 已加 `ProxyUpstream` / `ProxyRealm` 列（**未写值**，预留会话溯源）。
- Stop-Missing、乱序、NAS 重启的自动对账 Worker 未实现。
- Usage 是计量事实，不是扣费指令；计费扣减见第 8 节。

---

## 7. 代理、抄送、漫游与负载均衡

### 7.1 数据模型

- `radius_upstream`：`Addr/AuthPort/AcctPort/CoaPort/Secret/Role/Weight/Status`；
  `Role = proxy | replicate`。
- `radius_realm_route`：`Realm/Strategy/UpstreamIds/UserPrefix/UserSuffix/MappedPassword/AcctMode/Status`；
  `Strategy = single | round_robin | weight`；`AcctMode = both | forward | local`。

### 7.2 能力

| 能力 | 实现 | 状态 |
|---|---|---|
| 抄送（replicate） | 认证/计费副本异步转发所有 `role=replicate` 上游，携带用户信息与 `Class=profile=…` | 已接线 |
| 负载均衡 | realm=`*` 兜底路由 + round_robin / weight 池，横向分发到后端 RADIUS 节点 | 已接线 |
| 代理转发 | `proxy_dispatch` 阶段命中路由即转发，Accept/Reject 原样回 NAS（重签密钥与 MA） | 已接线 |
| 凭据转换 | `UserPrefix+本地名+UserSuffix` + `MappedPassword`（仅 PAP）；CHAP/MSCHAPv2 透传 | 已接线 |
| 漫游 | `user@realm` 按域匹配（大小写不敏感）到对应上游池 | 已接线 |
| CoA 方式对接上游 | 管理端踢线时按转换后用户名向上游发 Disconnect | 已接线 |
| RADIUS 协议方式中继 | RFC 5176 DAS 入站（3799）校验密钥与 MA 后中继到 NAS | 已接线（默认关闭） |
| EAP 代理 | 有状态 EAP 中继未实现，请求落本地管线 | 未实现 |

上游/路由表 30 秒 TTL 缓存；心跳依赖 `radius.Client` 重传与超时，无主动健康检查。

---

## 8. 计费与账务

### 8.1 已实现（`internal/billing`）

- 多资产账户：`CashFen / TimeSeconds / GiftTimeSeconds / BytesQuota / GiftBytes / FrozenFen` + `Version` 乐观锁。
- 扣费：赠品优先、5 次乐观锁重试、`IdempotentKey` 唯一键幂等、按会话累计增量计费。
- 规则：包月 `monthly`、包天 `daily`、时长 `time`、流量 `traffic`、阶梯 `tiered`、封顶 `capped`、组合 `combo`；
  支持优惠时段与累计阶梯（左闭右开、末档延用）。
- 订单 / 支付回调 / 充值卡兑换 / 账单：`BillingOrder`、`BillingPayment`、`BillingCard`、`BillingCardBatch`。
- 管理 API：`/api/v1/billing/*`（账户、套餐、订阅、流水、订单、支付回调）。

### 8.2 未完成

- 财务双分录账本、预付/后付账期结算、退款/冲正、发票、欠费控制与对账。
- ACADEMIC_TERM 学期周期需学校日历配置，不臆造起止时间。
- `payment` 包的三个渠道 Provider 未接线；当前 `PaymentNotify` 仅用 `X-Signature` + `web.secret` 做裸 HMAC，
  不走渠道验签，不能视为微信/支付宝生产回调能力。

---

## 9. CoA / Disconnect / 动态授权

### 9.1 已实现

- `CoAService`：RFC 5176 报文、Message-Authenticator、`Disconnect/CoA`、`WithSessionTimeout/WithFilterID`、
  按 `User-Name / NAS-IP / NAS-Port / Calling-Station-Id / Acct-Session-Id` 定位会话。
- **持久化任务**：`radius_coa_task` 表，`CoATaskManager.Create/MarkSending/Complete/Pending/Execute/RetryPending`，
  先落库后发送；固定 Identifier 支持重启后同 ID 重投。
- 管理 API：`/api/v1/coa-tasks`（列表 / 重试）；会话动作 `POST /sessions/:id/coa`、`/disconnect` 同步落任务与审计。
- DynAuth 入站：上游 CoA/DM 按来源 IP 匹配上游 Secret，校验 MA 后中继到 NAS，回 ACK/NAK（含 Error-Cause 503）。

### 9.2 冻结语义与缺口

- 上游 Secret 绝不用于 NAS 出站动态授权；上游 Disconnect 如需 NAS 实际下线，应使用 NAS 自己的 Secret。
- 当前 CoA 任务为简化版 outbox：无租约 fencing、`UNKNOWN` 状态、审计导出与多实例并发验收。
- 属性白名单未建立；不接受任意厂商 VSA 透传变更。
- handoff 后结果未知不得自动重发 Disconnect（Java 基线已实现此约束，Go 侧待补）。

---

## 10. Portal 与内置登录页

### 10.1 角色边界（冻结）

Portal、NAS、RADIUS 是三个独立角色：Portal 经 NAS 触发认证，NAS 再向 RADIUS 发 Access-Request；
**Portal 不直接作为 NAS 调用 RADIUS**。Portal 认证成功不等于用户上线或计费开始。

### 10.2 已实现（`internal/portal`）

- 华为/CMCC Portal V2：32 字节头、MD5 验证字、CHAP `MD5(ReqID低8位‖密码‖Challenge)` 与 PAP；
  Challenge / Auth / Logout / Info / 心跳、SerialNo 会话状态机、ACK 匹配、重传、NTF_LOGOUT。
- `auth_ticket`：HMAC-SHA256 签名、2 分钟 TTL、绑定用户名/IP/MAC/Challenge、一次性消费、防重放。
- HTTP bridge：`/api/v1/portal/challenge|auth|logout|session`，供外部登录页调用；`RequireTicket=true`。
- 内置登录页与 302（第四批）：`internal/captive` 签名 return URL 防开放重定向，走 auth_ticket 流程。
  **默认关闭**，需显式突破项目"不托管登录页"铁律。

### 10.3 待验证 / 未实现

- V3（48 字节头 / SHA256）、H3C 私有方言、MAC-Trigger Type 48/52 业务闭环。
- 真实设备互通与抓包复核（Portal 逐字节格式无厂商公开权威全文）。
- Portal 会话状态仅在进程内存，多实例需 sticky 路由。

---

## 11. 接入网：DHCP / DNS / DHCPv6 / PPPoE

| 模块 | 依据 | 实现 | 状态 |
|---|---|---|---|
| DHCPv4 | RFC 2131/2132、3046、7710 | 地址池、Option 53/3/6/51/54、Option 82 回显、Option 160 Portal URL（兼容 243） | 已接线（默认关闭） |
| DNS 引流 | RFC 1035 | 未认证用户 A 记录劫持到 Portal IP，白名单域名透传上游 | 已接线（默认关闭） |
| DHCPv6 | RFC 3315/8415、7710 | Solicit/Advertise/Request/Reply/InfoReq、DUID、IA_NA/IAADDR、Option 103、地址→用户绑定反查 | 已接线（默认关闭） |
| Captive 302 | 工程实践 | 未认证浏览器 302 到登录页，授权后放行 | 已接线（默认关闭） |
| PPPoE/IPoE | RFC 2516、1661/1334 | Discovery 状态机、LCP/PAP、Tag TLV、Linux AF_PACKET | **未接线**：无服务端进程，`main.go` 无引用 |

IPoE 属于事实标准组合（DHCP Option 82 定位 + Portal 认证 + RADIUS CoA 授权），非独立 RFC。
PPPoE 会话态内核转发仍需 pppoe 驱动 + CAP_NET_RAW，Windows 仅为协议层编译。

---

## 12. EAP 方法矩阵

| 方法 | Type | 实现 | 状态 |
|---|---|---|---|
| EAP-MD5 | 4 | 挑战响应校验 | 已实现 |
| EAP-MSCHAPv2 | 26（历史）/ 29（IANA） | RFC 2759 NT-Response | 已实现；29 类型请求落 unsupported |
| EAP-TLS | 13 | 完整 TLS 握手、CA 链校验、身份映射、分片、MPPE | 已实现 |
| PEAPv0 | 25 | TLS 隧道 + 内层 EAP-MSCHAPv2 + MPPE | 已实现（注释滞后称未实现） |
| EAP-TTLS | 21 | TLS 隧道 + 内层 PAP / MS-CHAPv2 | 已实现；其他内层方法未实现 |
| EAP-pwd | 52（**非 103**，RFC 5931） | P-256 PWE/Commit/Confirm、MSK/EMSK | 已实现 |
| EAP-OTP | 5 | `HandleResponse` 恒返回 `ErrOTPNotConfigured` | 未实现（未注册，避免绕过） |
| EAP-SIM / AKA / AKA' / FAST / TEAP | 18/23/50/43/55 | 仅常量 | 未实现（SIM/AKA 需 AuC） |

`radius.EapMethod` 选择首选方法，`radius.EapEnabledHandlers` 白名单控制启停（`*` 全开）。

---

## 13. 管理面、开放 API 与前端

- 19 组路由注册函数，统一前缀 `/api/v1`；角色 `LevelSuper / LevelAdmin / LevelOperator`。
- JWT：HS256，`web.secret` 签名，12h；跳过前缀含 `/auth/login`、`/auth/refresh`、`/open`。
- 开放 API `POST /open/*`：AppKey + Timestamp + Nonce + HMAC-SHA256，5 分钟窗口、Nonce 防重放、
  QPS 限流、Idempotency-Key 24h、`SysOprLog` 审计、traceId；**默认关闭（fail-closed）**。
- 前端：react-admin 资源页 14 个（含二开 `proxy.tsx` 的代理/域路由/Portal NAS/账户/订单/CoA 任务）。
- 小程序：`miniprogram/pages/login` 走 Portal auth_ticket；API 工具由后端签名，不内嵌 AppSecret。

---

## 14. 数据模型（27 张表）

| 分组 | 表 |
|---|---|
| 系统 | `sys_config`、`sys_opr`、`sys_opr_log`、`sys_cert` |
| 网络 | `net_node`、`net_nas` |
| RADIUS 核心 | `radius_profile`、`radius_user`、`radius_online`、`radius_accounting`、`radius_session_action_audit` |
| 代理 / Portal | `radius_upstream`、`radius_realm_route`、`portal_nas` |
| 话单合规 | `radius_acct_record`、`radius_acct_dedup` |
| CoA | `radius_coa_task` |
| 计费 | `billing_account`、`billing_asset_flow`、`billing_plan`、`billing_subscription`、
  `billing_session_usage`、`billing_charge`、`billing_order`、`billing_payment`、`billing_card`、`billing_card_batch` |

迁移清单 `internal/domain/tables.go` 与实体一一对应；测试夹具 `internal/adminapi/test_helpers_test.go` 单独列出迁移表。

---

## 15. 配置基线

`AppConfig` 11 个顶层段：`System / Web / Database / Radiusd / Portal / Captive / DHCP / DNSRedirect / IPv6 / OpenAPI / Logger`。
`radiusd` 内嵌 `proxy_*` / `dynauth_*` 字段。环境变量统一前缀 `TOUGHRADIUS_`，覆盖顺序：
`-c` 指定 YAML → `./toughradius.yml` → `/etc/toughradius.yml` → 默认值 → 环境变量覆盖。

全部 `Enabled` 开关：`system.debug`、`web.tls_enabled`、`database.debug`、`radiusd.enabled/debug`、
`radiusd.proxy_enabled`、`radiusd.dynauth_enabled`、`portal.enabled`、`captive.enabled`、`dhcp.enabled`、
`dns_redirect.enabled`、`ipv6.enabled`、`openapi.enabled`、`logger.file_enable`。
二开环境变量见 `.env.example`（**尚未覆盖二开段，待补**）。

---

## 16. FreeRADIUS 3.2.x 功能对照

FreeRADIUS 是协议与 AAA 成熟度参照，不要求复制其 C 模块架构或 unlang。

| FreeRADIUS 能力 | 项目对应 | 状态 |
|---|---|---|
| PAP / CHAP | 认证管线 validator | 已实现，待设备验收 |
| EAP-MD5/TLS/TTLS/PEAP/PWD | EAP Method Engine | 已实现，待设备验收 |
| MS-CHAPv2 | handler + LDAP/AD | 已实现 |
| EAP-SIM/AKA/FAST/TEAP | 常量占位 | 未实现 |
| authorize/authenticate/post-auth | 9 阶段管线 | 已实现 |
| SQL / LDAP 后端 | GORM + `ldapauth` | 已实现（PostgreSQL/SQLite） |
| detail / SQL 话单 | `radius_accounting` + raw_attrs | 部分实现，对账待补 |
| SQL Counter / Quota | `billing` 引擎 | 部分实现，财务闭环待补 |
| Proxy / Home Server / Realm | `radiusd/proxy` | 已实现（四批二开） |
| Dynamic Authorization | CoAService + DynAuth + task | 部分实现，租约/审计待补 |
| Virtual Servers / unlang | 版本化策略 + registry | 项目采用类型安全注册，非复制 unlang |
| HA / Load Balance / Observability | 代理池 + `pkg/metrics` 内存占位 | 部分实现，Prometheus/H A 待补 |
| DHCP / DNS / PPPoE 等非 RADIUS 模块 | `accessnet` / `pppoe` | 部分实现，PPPoE 未接线 |

权威参考：[FreeRADIUS Features](https://wiki.freeradius.org/features/) ·
[3.2 AAA 流程](https://www.freeradius.org/documentation/freeradius-server/3.2.11/concepts/aaa.html) ·
[源码模块](https://github.com/FreeRADIUS/freeradius-server/tree/v3.2.x/src/modules)。
协议权威依据见 `docs/rfcs/` 与 `docs/campus-aaa-fourth-dev.md`。

---

## 17. 商业化交付目标与非目标

**必须纳入交付规划**：多租户隔离（当前仅 `node_id` 弱隔离）、策略发布/回滚、会话对账、财务账务闭环、
CoA 租约与审计导出、HA/灾备、License、安装升级回滚、数据迁移、API/Webhook、运维诊断与正式验收。

**不属于 RADIUS 实时核心**：DHCP/DNS/NAT/DPI/防私接/IPAM/设备配置下发——当前以独立可关闭模块或外部集成存在。
**需单独立项**：RadSec 生产 PKI、TACACS+、Diameter、云 PKI、跨区域无损会话切换、EAP-SIM/AKA（需 AuC）。

---

## 18. 当前状态矩阵（2026-11-27，基准 3e61b9d）

| 功能域 | 状态 | 已有实现 | 缺失 / 待验证 |
|---|---|---|---|
| RADIUS Access | 已实现，待验收 | 9 阶段管线、PAP/CHAP、策略、限流、MA | 设备互通、属性矩阵、并发压测 |
| Accounting / Session | 部分实现 | Start/Interim/Stop、RFC 合规、去重、话单 | Stop-Missing 对账、乱序/NAS 重启、死信恢复 |
| 代理 / 抄送 / 漫游 | 已实现，待验收 | realm 路由、三种池、凭据转换、DAS 中继 | 主动健康检查、EAP 代理、多实例会话 |
| EAP | 部分实现 | MD5/MSCHAPv2/TLS/PEAP/TTLS/PWD | SIM/AKA/FAST/TEAP、OTP、真实客户端互通 |
| 计费 / 账务 | 部分实现 | 多资产、赠品优先、幂等、订单/卡 | 双分录账本、账期结算、退款冲正、发票 |
| CoA / Disconnect | 部分实现 | RFC 5176、任务持久化、固定 ID 重投、DAS 入站 | 租约 fencing、UNKNOWN 状态、审计导出、白名单 |
| Portal V2 | 部分实现 | 协议侧、auth_ticket、HTTP bridge、内置登录页 | V3/H3C、MAC-Trigger、设备互通、多实例状态 |
| 接入网 DHCP/DNS/DHCPv6 | 已实现，待验证 | 协议与服务、Portal URL 下发、溯源 | 真实设备联调、地址池持久化 |
| PPPoE / IPoE | 部分实现 | Discovery/PPP 线格式、Linux raw socket | 服务端进程未接线、内核转发、CAP_NET_RAW |
| 支付渠道 | 库已实现未接线 | 微信 v3 / 支付宝 RSA2 / 一卡通验签 | 生产入口接线、真实商户凭证、回调幂等 |
| Redis / ClickHouse | 库已实现未接线 | Store 接口 + Redis、MergeTree 归档 | 生产接线、DSN 注入、故障降级（Redis 不自动降级） |
| 管理面 / 开放 API | 已实现 | 19 组路由、JWT、HMAC、幂等、审计 | 全量前端页、配置发布审批、License |
| 前端 / 小程序 | 部分实现 | 14 个资源页、小程序登录 | 依赖未安装验证、二开页面设备联调 |
| 等保材料 | 文档模板 | `docs/compliance/README.md` | 正式测评报告、制度落地证据 |

---

## 19. 验收原则

任何能力只有在**代码、数据库、权限、安全、审计、文档与必要验证**均有证据后才能标记"完成"。
静态源码、接口契约、数据库表、监听端口或设计文档都不能单独证明业务闭环或生产就绪。

**已知失败（非本次引入，需在后续验收中修复）**：

1. `config` 包 14 个断言失败：`GetXxxDir` 用 `filepath.Join` 在 Windows 产生反斜杠，与期望 POSIX 路径不符。
2. EAP sqlite 集成测试（`TestEAPMD5IntegrationSuccess` 等）：`internal/app/database.go` 用 `path.IsAbs`
   而非 `filepath.IsAbs` 判断 Windows 盘符路径，导致 sqlite 打开失败；`-short` 模式跳过。
3. `coverage.out` 为陈旧产物，仅覆盖 radiusd 子集且引用已移动文件，不可当全项目覆盖率。

`go test -short ./internal/...` 当前 56 个包 ok、1 个包（config）失败；`go build ./...` 与
`GOOS=linux GOARCH=amd64 go build ./...` 均通过。

---

## 20. 基线变更记录

| 日期 | 基准提交 | 变更摘要 |
|---|---|---|
| 2026-11-27 | `3e61b9d` | 建立唯一有效基线。汇总四批二开：`064eeec` 代理子系统核心、`db452b7` DynAuth、`21db012` 管理 API、`094b575` Portal V2、`f118a52` 校园 AAA 核心、`3e61b9d` 九大接入网/认证增强模块。 |
| 2026-11-27 | `3e61b9d` | 明确"开发前对照、开发后更新、旧版删除"规则；标注孤儿包（payment/cache/analytics/pppoe）与已知失败。 |
