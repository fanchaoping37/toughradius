# ToughRADIUS 协议规范与业务流程手册

> **当前唯一有效的协议规范与业务流程参考文档**
> 更新日期：2026-11-27
> 仓库：`fanchaoping37/toughradius`（上游 `talkincode/toughradius`） · 分支：`dev-freeradius-compare`
> 代码基准提交：`ab2175ba77bedf153d4f032064ce57a6fd1b1eea`（与 `docs/ARCHITECTURE-BASELINE.md` 同源）
> 参考范式：用户提供的 Java 版《Radius-Vue-Plus 协议规范与业务流程手册》（Radius-Vue-Plus 商业版，Java 21）
>
> **定位（与架构基线并列，维度不同）**
> - `docs/ARCHITECTURE-BASELINE.md`：**唯一有效**的架构决策、模块状态矩阵、验收口径与变更记录；每次开发前对照、开发后必须更新。
> - 本文：**唯一有效**的线上协议结构、字段语义、交互顺序、安全校验、失败处理与业务事实边界；开发时用于查证"协议应该怎么做 / 现在做到哪 / 哪些不能做"。
> - 本文不是架构基线的 v2，不替代它，也不参与"基准提交"的滚动更新。
> - **禁止**创建 `PROTOCOL-SPEC-v2.md`、`协议规范-2026xxxx.md` 等并存版本。规范演进直接修订本文；历史状态只保留在架构基线第 20 节变更记录与各专项文档中。
>
> **本轮性质**：纯文档新增，**未改动任何代码**，未运行编译、测试、应用启动、数据库迁移、UDP 联调、压测或真实 NAS 验收。
> 代码事实以 `dev-freeradius-compare` 分支源码为准；文中所有"现状"均按架构基线第 1.2 节状态术语标注。

---

## 0. 效力、证据等级与阅读方式

### 0.1 冲突解释顺序

沿用架构基线第 1.1 节，并补充本文的适用方式：

1. 当前分支代码、`config/config.go`、`internal/domain/tables.go`；
2. 用户在项目讨论中明确确认的业务规则（含 2026-11-27 冻结的 7 条澄清）；
3. `docs/ARCHITECTURE-BASELINE.md` 的状态矩阵与已冻结架构决策；
4. 本文的规范性条文（用于判断**实现是否合规**）；
5. RFC / IANA / FreeRADIUS 3.2.x / 厂商正式资料 / 用户提供的参考手册；
6. 历史专项文档与批处理说明（仅作证据，不作当前口径）。

> 关键：**本文的规范条文不用于判定"功能是否已完成"**。一个字段写在 RFC 里、写在 Java 参考手册里，
> 或本项目有一个同名端口常量，都不等于 ToughRADIUS 已实现该能力。完成度一律以架构基线状态矩阵和源码为准。

### 0.2 证据等级

在参考范式的五级基础上，把 PROJECT 拆成两级——这是本文最容易被误读的地方：

| 标记 | 来源 | 用途 | 约束 |
|---|---|---|---|
| NORMATIVE | IETF RFC、IANA | RADIUS / EAP / CoA 线上规范 | 与实现冲突时优先修正实现，或明确写出兼容策略 |
| FR-3.2 | FreeRADIUS 3.2.x 官方文档与源码 | 请求阶段、重复请求、EAP、Proxy、Accounting 参照 | 不把 FR 内部实现当作 RFC 强制要求 |
| VENDOR | H3C / Huawei / 运营商 / 控制器正式资料 | Portal、MAC-Trigger、VSA 与设备行为 | 必须绑定 Vendor / Model / Version / Profile |
| PROJECT-JAVA | 用户提供的 Java 版 Radius-Vue-Plus 手册与其源码索引 | 参考范式中的产品实现约束（Netty、Request Journal、Grant Gate、Profile 版本化、MySQL/PostgreSQL schema） | **只代表 Java 参考产品，不代表本仓库**；不得照抄为 ToughRADIUS 能力 |
| PROJECT-GO | 本仓库 Go 源码、配置与业务决策 | ToughRADIUS 当前实现事实 | 只代表当前分支；必须带状态标注 |
| CAPTURE | 经确认的设备抓包 / 互通记录 | 补充未公开字段差异 | 记录型号、软件版本、方向、原始报文与验证结论 |

### 0.3 状态术语（与基线 1.2 一致）

`已实现（静态代码）` / `已接线` / `部分实现` / `库已实现未接线` / `未实现` / `待验证` / `暂缓` / `边界外`。
另对厂商协议使用 `UNKNOWN` / `UNVERIFIED`：厂商字段不确定时，**不猜**偏移、字符集、长度、错误码、摘要算法或重试语义。

### 0.4 每个业务流程的统一读法

第 4、5、6 节每节按三段式组织，避免把"规范"读成"已完成"：

- **规范要求**：NORMATIVE / FR-3.2 / VENDOR 的应然描述；
- **ToughRADIUS 现状**：PROJECT-GO，指向具体代码文件与状态；
- **冻结约束 / 待对齐**：用户已确认的业务规则，以及登记在第 8 节的偏差编号。

---

## 1. 目的、范围与重要边界

本手册把开发所需的协议结构、字段语义、交互顺序、安全校验、失败处理和业务边界固定在仓库内。
后续开发先查本文、架构基线和源码；遇到新 RFC、FreeRADIUS 行为变化、新 NAS 型号或抓包证据时再扩展核验。

**四条不可混淆的边界：**

1. RADIUS 是标准 AAA 协议；**NAS Portal Client（UDP 2000）**、**Portal MAC-Trigger（UDP 50100）**、
   **华为/CMCC Portal V2** 是独立的 NAS 厂商协议族，不是 RADIUS，也不是 FreeRADIUS 定义的标准协议。
2. FreeRADIUS 可承载 RADIUS AAA、EAP、Accounting、Proxy 和 Dynamic Authorization；
   它**不定义**本项目的 Portal Web、NAS Portal Client、MAC-Trigger、套餐、钱包或财务协议。
3. 本文是项目摘要与本地索引，**不替代** RFC、IANA 和厂商原始资料。RFC 全文归档于 `docs/rfcs/`，
   精选、面向实现的索引见 `docs-site/src/zh/rfc-index.md` 与 `docs-site/src/en/rfc-index.md`。
4. 参考范式是 Java 产品。其 Netty UDP Listener、Request Journal、Grant Gate、Profile 版本化配置、
   全量安装 SQL 等属于 PROJECT-JAVA；本文只提炼可移植的**协议契约与业务事实边界**，不复制其实现。

---

## 2. 协议族与端口辨析（新增设备支持前先读本节）

端口相同不代表协议相同。三族厂商协议与 RADIUS 共用或相邻端口号，是本项目最容易被误接的地方。

| 协议族 | 端口 | 方向 | 本项目角色 | 状态 | 依据 |
|---|---|---|---|---|---|
| RADIUS Authentication | UDP 1812（历史 1645） | NAS → 本项目 | RADIUS Server | 已接线 | NORMATIVE / RFC 2865 |
| RADIUS Accounting | UDP 1813（历史 1646） | NAS → 本项目 | RADIUS Server | 已接线 | NORMATIVE / RFC 2866 |
| Dynamic Authorization | UDP 3799 | 双向 | CoA/DM Server 与 Client | 已接线（入站默认关闭） | NORMATIVE / RFC 5176 |
| 华为/CMCC Portal V2 | 本项目监听 50100；发往 NAS 2000 | 双向 | Portal Server（UDP 协议侧） | 已接线，待设备验证 | VENDOR / 本项目实现 + 澄清 5 |
| NAS Portal Client | UDP 2000（NAS 侧） | Portal 应用 → NAS | **未实现该协议族** | UNVERIFIED / 边界外 | PROJECT-JAVA（参考范式 4.x） |
| Portal MAC-Trigger | UDP 50100（Server 侧） | NAS → Server | **未实现该协议族** | UNVERIFIED / 边界外 | PROJECT-JAVA（参考范式 5.x） |

### 2.1 四个高危混淆点

| 混淆点 | 澄清 |
|---|---|
| 同一个 **50100** | 本项目是**华为/CMCC Portal V2 Server** 监听端口，收 NAS 的 ACK_CHALLENGE / ACK_AUTH / NTF_LOGOUT / 心跳；Java 参考手册中是**MAC-Trigger Server** 监听端口，收 NAS 的 Type 48 / 52 查询与通知。两者报文头、Type、字段、摘要完全不同，不得按端口号选解析器。 |
| 同一个 **2000** | 本项目是 **NAS 侧 Portal V2 端口**（本项目把 REQ_CHALLENGE / REQ_AUTH / REQ_LOGOUT 发往 NAS 2000）；Java 参考手册中是 **NAS Portal Client** 端口（Portal 应用主动发 REQ_AUTH）。语义接近但 Profile 不同，不得假定字段与错误码兼容。 |
| 同一个 **Type 52** | RADIUS Attribute 52 = `Acct-Input-Gigawords`（RFC 2869）；EAP Method Type 52 = `EAP-pwd`（RFC 5931，第四批新增）；MAC-Trigger Type 52 = `NTF_MACUSER_LOGOUT`（厂商私有）；Portal V2 属性编号又是另一命名空间。四者协议入口、报文头与处理器完全不同。 |
| 同一个 **Type 80** | RADIUS `Message-Authenticator`（HMAC-MD5）；Portal 报文的验证字是独立的 MD5 摘要，**不是** RADIUS MA，也不是 RADIUS Response Authenticator。都叫 Authenticator、都用 MD5 不代表两套协议兼容。 |

**结论**：新增设备支持前必须绑定 `Vendor / Model / Version / Profile` 与**端口方向**，并按架构基线第 1.3 节走开发前对照；
不得按端口号猜协议，不得把 Java 参考手册的 Profile 定义直接套到本项目 Portal V2 实现上。

---

## 3. RADIUS 基础报文规范

### 3.1 传输和端口

- Authentication/Authorization：UDP 1812；历史兼容 1645。Accounting：UDP 1813；历史兼容 1646。
  Dynamic Authorization（CoA/Disconnect）：UDP 3799（RFC 5176）。
- **端口号不能替代 NAS 身份验证**；必须结合本地监听端点、UDP 源 IP、`net_nas` 配置和 Secret 选择。
- ToughRADIUS 现状：`internal/radiusd/server.go` 按 `radiusd.enabled` 启停；`config/config.go` 管端口；
  NAS 身份与密钥由 `net_nas` 解析。入站 DAS（3799）默认关闭。

### 3.2 通用报文结构

RADIUS 报文由 20-octet 固定头和零个或多个 Attribute 组成，整数使用网络字节序：

| 偏移 | 长度 | 字段 | 语义 |
|---:|---:|---|---|
| 0 | 1 | Code | 报文类型 |
| 1 | 1 | Identifier | 请求/响应关联标识，8 bit |
| 2 | 2 | Length | 整个报文长度，含 20 字节头 |
| 4 | 16 | Authenticator | 请求或响应认证器 |
| 20 | 可变 | Attributes | Type-Length-Value 属性序列 |

RFC 2865 传统 RADIUS UDP 报文最大长度 4096 octets。解析器必须检查实际 Datagram 长度、Length 下限/上限、
截断、尾随字节、Attribute 长度至少为 2 且不越过报文边界；**不得信任未验证的 Length 或数组偏移**。

- ToughRADIUS 现状：报文解析由 `layeh.com/radius` 的 fork 承担；`internal/radiusd/plugins/accounting/packet.go`
  实现 `DedupKey` / `EventTime` / `SanitizedAttributes` / `GigawordOctets` 等边界处理。
  逐项边界用例（尾随字节、超长属性、Length 与 Datagram 不一致）**待验证**。

### 3.3 常用 Code

| Code | 名称 | 方向 / 用途 |
|---:|---|---|
| 1 | Access-Request | NAS → RADIUS |
| 2 | Access-Accept | RADIUS → NAS，认证与授权成功 |
| 3 | Access-Reject | RADIUS → NAS，拒绝 |
| 4 | Accounting-Request | NAS → RADIUS |
| 5 | Accounting-Response | RADIUS → NAS |
| 11 | Access-Challenge | RADIUS → NAS，多轮认证继续 |
| 12 / 13 | Status-Server / Status-Client | 状态探测，需按配置支持 |
| 40 / 41 / 42 | Disconnect-Request / ACK / NAK | RFC 5176 |
| 43 / 44 / 45 | CoA-Request / ACK / NAK | RFC 5176 |

未知 Code 可保留为未知值，但**不能自动进入业务处理**；未支持的 Code 按入口策略拒绝或丢弃。
ToughRADIUS 现状：Status-Server（12/13）未确认有独立业务处理，标 `待验证`。

### 3.4 Attribute 编码与常用属性

传统 Attribute 为 `Type(1) + Length(1) + Value(Length-2)`。Type 26 为 Vendor-Specific，
内部 Vendor-Id 是 IANA Private Enterprise Number，厂商子属性按对应厂商定义解析。

| Type | 名称 | 本项目主要用途 |
|---:|---|---|
| 1 | User-Name | 用户身份；代理/漫游时按 realm 重写 |
| 2 | User-Password | PAP 隐藏后的密码字段 |
| 3 | CHAP-Password | CHAP 标识符与摘要（入站可出现；出站约束见 4.3） |
| 4 / 32 | NAS-IP-Address / NAS-Identifier | NAS 身份线索，**不作为 Secret 选择依据** |
| 5 | NAS-Port | 端口号 |
| 6 | Service-Type | 服务类型 |
| 8 | Framed-IP-Address | IPv4 授权地址 |
| 11 | Filter-Id | 代拨厂商策略引用（见 `dial-gateway-integration.md`） |
| 18 | Reply-Message | 响应文本；上游拒绝原因缓存读取此属性 |
| 24 | State | 多轮认证不透明载体 |
| 25 | Class | 服务端不透明分类数据，按 RFC 语义往返；抄送副本附加 `Class=profile=…` |
| 26 | Vendor-Specific | 厂商扩展 |
| 27 / 28 | Session-Timeout / Idle-Timeout | 会话时长限制 |
| 30 / 31 | Called-Station-Id / Calling-Station-Id | AP / 终端上下文 |
| 33 | Proxy-State | Proxy 链路状态，遵循顺序与回送语义 |
| 40 | Acct-Status-Type | Start=1 / Stop=2 / Interim=3 / Accounting-On=7 / Off=8 |
| 41 | Acct-Delay-Time | NAS 发送延迟 |
| 42 / 43 | Acct-Input-Octets / Acct-Output-Octets | 32 位流量低位计数 |
| 44 | Acct-Session-Id | NAS 会话标识 |
| 46 | Acct-Session-Time | 会话时长 |
| 49 | Acct-Terminate-Cause | 结束原因 |
| 52 / 53 | Acct-Input-Gigawords / Acct-Output-Gigawords | 对应 Octets 的 2^32 回绕计数 |
| 55 | Event-Timestamp | NAS 事件时间（RFC 2869） |
| 61 | NAS-Port-Type | 接入媒介类型 |
| 64 / 65 / 81 等 | Tunnel 属性 | VLAN / 隧道授权，按 RFC 与 NAS Profile |
| 79 | EAP-Message | 承载 EAP 数据 |
| 80 | Message-Authenticator | HMAC-MD5 完整性校验属性 |
| 85 | Acct-Interim-Interval | Interim 周期 |
| 87 | NAS-Port-Id | 端口标识 |
| 89 | Chargeable-User-Identity | CUI |
| 95 / 96 / 97 | NAS-IPv6-Address / Framed-Interface-Id / Framed-IPv6-Prefix | IPv6 身份与授权 |

> 属性编号以 RFC + IANA + 厂商字典为准（仓库 `docs/rfcs/` 收录 50 个 RFC 原文，`share/` 字典来自 FreeRADIUS）。
> 本项目 VSA 解析在 `internal/radiusd/plugins/vendorparsers/`；华为 2011 / H3C 25506 / 锐捷 4881 / Cisco 9 / 中兴 3902。
> 配置驱动字典尚未完成，属 `部分实现`。

### 3.5 Authenticator、Message-Authenticator 与 Secret

必须区分三种认证器：

1. **Request Authenticator**：Access-Request 的 16 字节随机值，用于 PAP User-Password 隐藏等算法输入；
   它本身不是对整个 Access-Request 的通用签名。
2. **Response Authenticator**：响应摘要绑定原请求 Authenticator、Code、Identifier、Length、响应属性和
   Shared Secret；必须使用与请求对应的 Secret 验证。
3. **Message-Authenticator（Type 80）**：RFC 2869 的 HMAC-MD5 完整性保护；
   计算时按规范把该属性 Value 置零后参与 HMAC。严格校验长度、重复属性策略与报文类型要求。

Accounting-Request 的 Request Authenticator 用 RFC 2866 规则，**不能套用** Access-Request 的随机值规则；
CoA / Disconnect 用 RFC 5176 对应规则。响应必须以原始请求上下文校验。

安全要求：

- Secret 由可信 NAS source identity 解析；**不能只用报文内 NAS-IP-Address / NAS-Identifier 选择 Secret**。
- NAS RADIUS Secret、上游 RADIUS Secret、Portal NAS Secret、MAC-Trigger Secret、Web JWT Secret 各自独立。
  ToughRADIUS 现状：架构基线 4.3 已冻结三类凭据分离（NAS / 上游 / 运营商账号密码），另列 Portal NAS Secret
  与 Web JWT Secret；**MAC-Trigger Secret 未建模**（该协议族未实现）。生产 KMS/HSM、密文完整性验证与
  轮换审计未完成 → 见偏差 D9。
- Authenticator 使用常量时间比较；日志不得输出 Secret、密码、完整认证器或可重放敏感报文。
- 不支持或未确认的认证器 / 属性组合 **fail closed**。

ToughRADIUS 现状：`internal/radiusd/message_authenticator.go` 提供 `disabled / warn / enforce` 三模式，
配置键 `radius.RequireMessageAuthenticator`，**默认 `warn`**；已做 BlastRADIUS 防护与计费认证器置零计算、EAP 请求不代理。
默认 `warn` 与"不明组合 fail closed"的规范目标存在差距 → 见偏差 D5。

---

## 4. RADIUS AAA 业务流程

### 4.1 Access-Request

**规范要求（NORMATIVE / FR-3.2）**

1. NAS 发 Access-Request 到 RADIUS。
2. 服务端根据 UDP source IP、接入 endpoint 与可信 NAS Inventory 定位 NAS。
3. 读取有效 NAS Secret 版本，验证报文边界、Request Authenticator、Message-Authenticator（存在时及规范要求时）与属性语义。
4. 执行**重复请求检测**；相同请求命中原处理状态 / 响应缓存，不能并发执行两次外部副作用。
5. 归一化用户、终端 MAC、NAS、端口、SSID、VLAN、IP、接入类型等上下文。
6. 选择 PAP / CHAP / MS-CHAPv2 / MAC / EAP 或明确配置的外部认证源。
7. 执行认证与授权策略：用户状态、终端绑定、订阅、在线限制、套餐额度、位置与 NAS 能力。
8. 生成授权快照与 Grant；只有 Grant Gate 允许时才生成 Access-Accept。
9. 用匹配 NAS Secret 生成响应认证器，持久化重复请求所需的精确响应材料，再经原 UDP endpoint 回包。

**ToughRADIUS 现状（PROJECT-GO）**

- 9 阶段管线（`internal/radiusd/auth_stages.go`）：
  `request_metadata → nas_lookup → message_authenticator → auth_rate_limit → proxy_dispatch → vendor_parsing → load_user → eap_dispatch → plugin_auth`。
  限流在代理之前，保护后端；EAP 请求跳过代理，落本地方法引擎。
- 入口：`internal/radiusd/radius_auth.go`；MA 校验见 3.5；VSA 解析见 `internal/radiusd/plugins/vendorparsers/`；
  响应属性增强（Session-Timeout / Filter-Id / Class / 限速 / VLAN）由响应增强器下发。
- 认证与计费扣费**异步解耦**：认证结果不等待账务事务（架构基线 5.2）。
- 计费侧有 `DedupKey` 去重（`plugins/accounting/packet.go`）；**认证侧重复请求的精确响应缓存与
  外部副作用一次性语义未验证** → 见偏差 D6。
- Java 参考手册中的 Grant Gate、授权快照、Identity Normalization 属于 PROJECT-JAVA，本项目无同名组件，
  不得据其名称推断本项目已实现。

**冻结约束**：上游出站统一 PAP（澄清 2，见 4.3）；全接入类型 IPoE / PPPoE / EAP / Portal 均可转换送上游（澄清 7，见 4.4）。

### 4.2 Access-Challenge / State / EAP

- Access-Challenge 是**继续交互，不是认证成功**。NAS 后续必须原样回送 State；服务端把 State 视为不透明值。
- 每轮 State 关联当前认证对话，按项目策略 single-use 消费 / 轮换；旧 State 不可无限重放。
- EAP-Message 可拆成多个 Attribute，按 RFC 3579 顺序拼接为一个 EAP Packet；
  **不能把多个独立 EAP Packet 塞进同一 RADIUS 报文**。EAP 报文需要 MA 完整性保护。
- **EAP 本地终止**：本项目 PEAP / EAP 本地终止；如本地 EAP 成功后还需上游认证，则另发**普通上游
  Access-Request，不透传 EAP-Message**（与澄清 7 一致：入站 EAP 可落地，出站按 PAP 转换）。
- 跨节点 State Owner 路由必须使用独立安全代理；不能把 TLS Engine、Secret Lease、Request Context
  或本机 UDP 回包资源序列化到 Redis / 网络 envelope。
- ToughRADIUS 现状：`internal/radiusd/plugins/eap/` 方法矩阵见架构基线第 12 节；
  EAP-pwd（Type 52 / RFC 5931）为第四批新增、待验证；多实例 State 共享**未实现**。

### 4.3 PAP / CHAP / MS-CHAP

- PAP：NAS 按 RFC 2865 User-Password 隐藏算法处理密码字段；它不是 TLS，也不是端到端加密。
- CHAP：使用 CHAP-Challenge（或协议规定的替代 challenge）、CHAP-Identifier 与摘要；
  CHAP-Password 不是明文密码。MS-CHAPv2：使用 Microsoft 定义的 challenge/response 与错误响应语义，
  **不能用普通 CHAP 或 PAP 比对替代**。
- 凭据存储格式必须与认证方法兼容；不可从不可逆散列反推出 PAP / CHAP 所需明文或 NT hash。
- 内部原因需区分认证失败、暂时不可用、凭据缺失与配置错误；**对 NAS 的响应不能泄漏用户是否存在或内部细节**。

**入站 / 出站边界（本项目关键约束）**

| 链路 | 允许的认证方式 | 依据 |
|---|---|---|
| NAS → 本项目（Access-Request） | PAP / CHAP / MS-CHAPv2 / MAC / EAP（按 NAS 能力） | 澄清 7 要求全接入类型入站；架构基线第 12 节 |
| 本项目 → 上游 RADIUS / 代拨 / 运营商 AAA | **统一 PAP，不使用 CHAP / MSCHAPv2** | 澄清 2（架构基线 2.1 / 5.2 / 7.2 / 11.2） |
| 本项目 Portal Server → NAS（Portal V2） | Portal 协议自有 CHAP/PAP 属性，与上游 RADIUS 无关 | `docs/portal-v2-integration.md` |

> PAP-only 是**出站代理/代拨链路**的冻结约束，**不限制 NAS 入站**；也不改变 Portal V2 段的认证模式。
> Java 参考手册 3.4「EAP 本地终止后做普通 PAP/CHAP 二次认证时，出站请求不带 EAP-Message」与本项目一致，
> 但本项目进一步收紧为**出站仅 PAP**。

**现状与偏差**：`internal/radiusd/proxy/client.go` 的 `buildUpstreamPacket` 目前只跳过
`User-Password` / `Message-Authenticator` / `User-Name` 三类属性，**其余属性逐条原样复制**，
包括 `CHAP-Password` 及 MS-CHAP 相关属性；凭据转换仅在检测到 PAP `User-Password` 时生效。
即当前代码未按"统一 PAP"收紧 → 见偏差 D2（架构基线 7.2 已如实标注"待开发"）。

### 4.4 Proxy / Realm / 上游二次认证

FreeRADIUS 3.2.x Proxy 可参考：`authorize` 识别 realm / 目标 → `pre-proxy` 调整请求 → 发往上游并等待 →
收到响应后 `post-proxy` → 决定本地响应。

本项目约束：

- 上游 RADIUS 是独立 Server 配置与 Secret，**不是 NAS Secret**。
- 上游请求有独立 Identifier、源端口、目标、超时、重试、响应认证与 Proxy-State 管理。
- 同步上游二次认证必须**等待合法 Access-Accept / Reject / Challenge**；UDP 已发送不等于成功。
- EAP 本地终止后做普通上游二次认证时，出站请求不带 EAP-Message。
- **Accounting copy / forward 与认证代理不同**：可按策略异步发送，不阻塞 NAS ACK；
  每个目的地有独立 Outbox、幂等键与重试策略。

ToughRADIUS 现状：

- `internal/radiusd/proxy/service.go`：`MatchRoute(username)` / `SplitUsername` / `SelectUpstreams`
  （single / round_robin / weight）/ `ReplicateTargets`；上游与路由表 30 秒 TTL 缓存，无主动健康检查。
- `internal/radiusd/auth_proxy_stage.go`：`proxy_dispatch` 阶段命中路由即同步转发，
  Accept / Reject 原样回 NAS（重签 NAS 密钥与 MA），上游拒绝原因按用户名缓存 5 分钟仅影响 Reply-Message。
- `internal/radiusd/proxy/client.go`：`ForwardAuth` / `ForwardAcct` / `Replicate`；
  `internal/radiusd/proxy/dial.go`：`ApplyDialProxyProfile`（Filter-Id 厂商默认值、DRCOM 属性裁剪、
  配置驱动的原始 VSA 注入，不硬编码运营商 IP）。
- **一次 Access-Request 中 `role=replicate` 异步抄送副本**的完整语义见架构基线第 7.3 节（澄清 1）：
  `internal/radiusd/radius_auth.go` 在管线返回后无条件触发（通过 NAS 校验、非 EAP 即可），
  与本地/代理分支、Accept/Reject 结果无关；逐属性复制 + 镜像上游 Secret 重签 + 可加 `Class=profile=套餐名`，
  2 秒 fire-and-forget 发给所有 enabled 的 `role=replicate` 上游；**不等待响应、不参与认证决策、不产生计费**。
- Proxy-State 精确回送语义、独立 Outbox 与逐目的地重试策略**待核 / 待验证** → 见偏差 D6。

### 4.5 Accounting

**规范要求（NORMATIVE / FR-3.2）**

1. NAS 发 Accounting-Request（Start / Interim-Update / Stop / Accounting-On / Off）。
2. 校验 NAS 来源、Secret、Request Authenticator、属性边界。
3. 按 RFC 2866 / 2869 生成 Accounting-Response；**response-first 策略必须记录 ACK 与持久化之间的崩溃窗口**。
4. 写入 Inbox / 可靠缓冲并异步处理；未完成可靠持久化前不得宣称"ACK 后绝不丢失"。
5. 只有与成功 Access-Accept / Grant 精确关联的 Accounting Start 才建立计费 Session。
6. 通过 tenant + NAS + Acct-Session-Id + generation 关联 Start / Interim / Stop，
   处理重复、乱序、延迟、NAS 重启与 Session-ID 复用。
7. Usage 只从可信计数器快照生成；不完整方向、计数器重置或不确定回绕进入基线 / 复核状态。
8. **缺失 Stop 只能标记 STOP_MISSING / 离线候选，不合成 Stop；真实 Stop 才关闭 Session**。

计数器注意事项：

- `Acct-Input/Output-Octets` 是 32 位低位；对应 Gigawords 是高位回绕计数。
  **只有同方向低位与高位都存在时**才可组合完整 64 位值。
- 计数器下降不自动等于回绕；缺乏连续 Gigawords 证据时**不得推算流量**。
- RADIUS Type 52 / 53 与 MAC-Trigger Type 52 / 53 无关（见 2.1）。

**ToughRADIUS 现状（PROJECT-GO）**

- `internal/radiusd/radius_acct.go` + `acct_plugin_runner.go` + `plugins/accounting/handlers/`：
  Start / Interim / Stop / Accounting-On(7) / Off(8) 分流；RFC 5080 去重、脱敏原始属性落
  `radius_acct_record`；Event-Timestamp(55) 事件时间；Gigawords 合并；原始报文成功后置 `consumed=true`。
- 会话对账（`internal/billing/reconciliation.go`）：Stop-Missing、`DetectOutOfOrder`、
  `HandleNASReboot`（依据 Accounting-On 事件时间）、`RestoreDeadLetters`；
  5 分钟周期 Worker + 运营 API（架构基线 6.2）。
- **按会话增量计费为必需能力（澄清 3，架构基线 6.3）**：`internal/billing/engine.go` 的
  `ChargeSession` 按累计计数器的 delta 扣费、幂等、赠品优先、乐观锁，引擎已有单测；
  但**正常 Accounting 路径尚未接线**，目前仅 `RestoreDeadLetters` 调用 → 见偏差 D3。

**已知偏差**：`closeMissingStop` 会写一条 synthetic Stop 标记并把 `radius_accounting` 行 finalize 为 Stop，
即**本项目当前会合成 Stop**，与本节第 8 条规范及 Java 参考手册 3.5 冲突 → 见偏差 D1。
本轮仅登记，不改代码。

### 4.6 CoA / Disconnect

RFC 5176 定义 Dynamic Authorization：

- CoA-Request(43) 动态改变已授权会话属性；响应 CoA-ACK(44) / CoA-NAK(45)。
- Disconnect-Request(40) 终止会话；响应 Disconnect-ACK(41) / Disconnect-NAK(42)。
- 请求必须精确定位目标 Session；NAS 按能力应用属性并返回结果。
- 校验 source、Secret、Request Authenticator、Message-Authenticator 与目标 Session；
  **不支持属性或不明确 Session 不能假装成功**。
- NAS 来源与上游来源必须分别绑定来源身份与 Secret。
- 上游 Disconnect 需要转发 NAS 时，**只有收到并验证 NAS ACK/NAK 后才能回复上游**；
  超时 / 未知结果不能伪造 ACK。

ToughRADIUS 现状：

- `internal/radiusd/coa_service.go` + `coa_task_manager.go`：任务先落库后发送、固定 Identifier 重投；
  `internal/radiusd/dynauth_server.go`：3799 入站 DAS，按来源 IP 匹配上游 Secret 校验 MA 后中继到 NAS（默认关闭）。
- `internal/adminapi/proxy_relay.go` 的 `relayDisconnectUpstream`：管理端踢线时按转换后用户名向上游发 Disconnect；
  `internal/radiusd/acct_proxy.go`：PANABIT Stop 向上游 CoA 端口发 Disconnect。
- **冻结策略（澄清 4，架构基线 9.3）**：给上游的强制下线必须按上游对接模式选择
  ——上游支持 CoA/DM → 发 **Disconnect-Request**；上游只支持 RADIUS（无 3799）→ 发
  **Accounting-Request with Acct-Status-Type=Stop(2)**。当前仅有 DM 路径，**模式选择与 Stop 路径未实现** → 见偏差 D4。
- 冻结语义：上游 Secret 不得用于 NAS 出站动态授权；上游 Disconnect 要真正下线，
  必须用该会话 NAS 自己的 Secret。handoff 后结果未知不得自动重发 Disconnect。

---

## 5. Portal 协议族（厂商协议，非 RADIUS）

### 5.1 华为/CMCC Portal V2（本项目已实现，UDP 协议侧）

完整报文算法、启用配置与 API 见 `docs/portal-v2-integration.md`；本节只固化边界与易错点。

- **链路**：终端 → NAS（引流 + Portal Web 登录页 URL 配置）→ Portal V2（本项目 50100 与 NAS 2000）→
  NAS → RADIUS 1812 → ToughRADIUS。RADIUS Access-Request **由 NAS 发出**，本模块不代发。
- **端口方向（澄清 5）**：本项目 Portal Server 监听 UDP 50100 收 NAS 的 ACK/NTF；
  REQ_CHALLENGE / REQ_AUTH / REQ_LOGOUT / ACK **发往 NAS UDP 2000**（`portal.nas_port=2000`、`portal_nas.port=2000`）。
- **报文**：V2 32 字节头、全大端；请求验证字 `MD5(包头(Auth 清零) || 属性 || Secret)`，
  响应验证字 `MD5(包头(Auth 替换为请求 Auth) || 属性 || Secret)`；
  CHAP 密码 `MD5(ReqID 低 8 位 || 密码 || 16 字节 Challenge)` 放属性 0x04。
- **状态机**：Challenge / Auth / Logout / Info / 心跳、SerialNo 会话状态机、ACK 匹配、重传、NTF_LOGOUT；
  `auth_ticket` 绑定用户名 / IP / MAC / Challenge，2 分钟一次性消费；HTTP bridge 供外部登录页调用。
- **边界**：不托管登录页、不做 HTTP 302 / 用户引流、不做短信/微信/券码开户。
  DHCP/DHCPv6 Option（含 Portal URL）、DNS 重定向、Captive 302 由 **NAS 给用户推送**，
  **Portal Web 登录页 URL 在 NAS 上配置**（澄清 6，架构基线 11.1）。
- **未验证 / 未实现**：V3（48 字节头 / SHA256）、H3C iMC 私有方言、MAC-Trigger Type 48/52 业务闭环、
  真实设备抓包复核（Portal 逐字节格式无厂商公开权威全文，重点复核 NAS 侧 2000 端口方向）、
  多实例会话状态共享（当前仅进程内存）。

> **Profile 差异警告**：Java 参考手册 4.2 描述的 Portal Client 固定头为 **16 字节** +
> Profile 定义扩展；本项目 Portal V2 实现为 **32 字节头**。两者不是同一 Profile。
> 不得按 16/32 字节的任一方去推断另一方的字段偏移、AttrNum 是否存在、Authenticator 长度或摘要输入顺序
> → 见偏差 D7。

### 5.2 NAS Portal Client 协议（UDP 2000，PROJECT-JAVA 参考语义）

> 本节是**参考范式摘要，不是 ToughRADIUS 实现说明**。本项目未实现该协议族（见 2. 表）。
> 状态：`UNVERIFIED / 边界外`。启用前必须补齐 Profile 与抓包证据。

- 定位：Portal Client 是 NAS/AC/BRAS 与 Portal 认证服务之间的厂商协议。Portal Web 不直接向
  RADIUS UDP 1812 发认证包，也不冒充 NAS Portal Client。
  链路：终端 → NAS Captive Portal → Portal Web(HTTPS) → Portal 应用 → NAS Portal Client(UDP 2000) → NAS → RADIUS(1812)。
- 报文模型：16 字节固定头（Version / Type / AuthType / Reserved / SerialNo / RequestID / UserIP /
  UserPort / ErrCode）+ Profile 定义的可选扩展（如 16 字节 Authenticator、AttrNum）+ 可变 TLV 属性。
  **AttrNum 是否存在、Authenticator 长度、属性编码与摘要输入顺序由已验证 Profile 决定；未知布局禁止猜测。**
- 常见 Type：1/2 REQ/ACK_CHALLENGE、3/4 REQ/ACK_AUTH、5/6 REQ/ACK_LOGOUT、7 AFF_ACK_AUTH、
  8 NTF_LOGOUT（NAS → Server）、9/10 REQ/ACK_INFO、11/12 确认类、13–20 心跳/挑战/通知类、
  33/34 NTF_AUTH、35/36 查询状态类。方向表只用于识别已知语义；消息是否启用、属性与应答关系由锁定
  Message Profile 决定。**未知 Type 不得自动应答**。
- Authenticator（Legacy Portal V2，经 Profile 确认的 MD5 摘要）概念输入顺序：
  固定头 Version 至 ErrCode 前 15 字节 → Profile 指定时加入 AttrNum → 16 字节 Authenticator 输入
  （请求全零、响应用对应请求 Authenticator）→ 按线上顺序编码的 Attribute TLV → Shared Secret。
  只允许 `verified=true` 且算法、字段长度、AttrNum 策略均确认的 Profile。
  **不得把 RADIUS Response Authenticator 或 Message-Authenticator 公式用于 Portal**。
- 主动认证流程：创建短时一次性 Ticket（绑定 tenant / NAS / 终端 IP/MAC / Portal Session / 过期时间）
  → 从可信 NAS Inventory 解析地址、端口、Vendor Profile、Message Profile、Secret 引用与版本
  → 选 Type、分配 SerialNo/RequestID、编码属性并计算请求 Authenticator
  → 持久化请求关联后发 UDP → 以来源 endpoint / NAS / Version / SerialNo / RequestID / Type / Profile 校验响应
  → 校验 Authenticator 与属性后解释 ACK/NAK/ErrCode → **只有明确成功响应才记为 NAS 认证成功**。
- Logout 流程：创建稳定 operation UUID 并持久化下线意图与固定 Profile/Secret 版本引用 → 发 REQ_LOGOUT
  → 等待精确关联的 ACK/NAK → **ACK 才是 NAS 已确认下线的证据**；
  超时 / 进程崩溃 / 结果未知进入 `UNKNOWN` 或 `RECOVERY_REQUIRED`，禁止盲目重发可能造成重复副作用的
  UDP 命令，也不能伪造 NAS ACK。人工核验是追加证据与受控 reconciliation，不改写原始 NAS outcome。
- 会话事实：Portal Session、RADIUS Grant、RADIUS Accounting Session **分别建模**，通过明确 Link 关联。

### 5.3 Portal MAC-Trigger 协议（UDP 50100，PROJECT-JAVA 参考语义）

> 本节同为**参考范式摘要，不是 ToughRADIUS 实现说明**。本项目未实现 MAC-Trigger
> （架构基线 10.3 明列为未实现）。状态：`UNVERIFIED / 边界外`。

- 定位：NAS 发往 MAC 绑定/认证服务的私有 UDP 协议，**不是 RADIUS Access，也不是 Portal Client UDP 2000**。
  NAS 维护 MAC-trigger 条目，流量达到免认证阈值后查询 MAC 与 Portal 账号绑定，命中时由 NAS 发起传统 Portal 认证。
- 端到端链路：终端接入 → NAS 记录 MAC/端口/上下文 → NAS 按阈值向 UDP 50100 查询 →
  服务端验证 NAS 身份 / Profile / 请求认证器与属性 → 查询本租户 MAC Binding 并规范化 MAC →
  命中后通过 **Portal Client UDP 2000** 向同一 NAS 发起传统 Portal Authenticate →
  NAS 向 RADIUS 1812 发起**独立认证** → MAC-Trigger 服务返回 Type 49；Type 52 通知用于下线关联。
- Type 48/49（MAC 绑定查询）：48 为 NAS → Server 查询，49 为 Server → NAS 结果响应。
  Type 48 处理顺序要点：按 UDP source IP + 监听 endpoint 解析唯一可信 NAS（**不能仅凭报文内 NAS-ID/BAS-IP**）
  → 加载固定版本 Vendor/Message/Attribute/ErrorCode Profile 与 MAC-Trigger Secret
  → 解码校验版本、方向、Type、长度、Authenticator 与属性约束
  → 计算原始请求 fingerprint 查 Request Journal，已完成请求**直接重放原始签名 Type 49**，不再次执行 Portal 登录副作用
  → Profile 映射 Session-ID / BAS-IP / NAS-ID / MAC / IP，MAC 在边界规范化为小写冒号格式并保留原始值用于审计，
  畸形或歧义 MAC 拒绝 → Portal 登录前固化身份快照 → 命中且用户可用才通过 UDP 2000 执行登录
  → **NAS 登录成功必须由 Portal Client 明确成功响应证明**，成功后建立 Portal Session 与 PENDING Link
  → UDP 超时 / 崩溃 / 结果无法确认记 `RECOVERY_REQUIRED`，不能自动重发登录
  → 构造 Type 49（关联原 Version/AuthType/SerialNo/RequestID/用户 IP/端口并按 Profile 生成响应 Authenticator）
  → **Type 49 响应先持久化再发送**，相同请求重传精确重放原始响应字节。
  **Type 48 不代表用户已认证，Type 49 不是 Access-Accept。**
- Type 52/53（MAC 用户下线）：52 为 NAS → Server 通知，53 为 Server → NAS 响应。
  处理要点：验证可信 NAS / 固定 Profile / Type 与方向 / Authenticator / 属性
  → 从 Profile 读 MAC 与 NAS 签发的 Portal Session-ID（**不能仅按用户名或 IP 猜 Session**）
  → 按 tenant + NAS + canonical MAC + Portal Session-ID 精确关联 Portal Session/Link
  → 同一事务关闭精确 Session/Link 并追加唯一离线通知事件
  → 同一请求 fingerprint 幂等，持久化结果与 Type 53 响应后再发送
  → 若同一 MAC/Session-ID 有历史 CLOSED Session 且协议无 generation/事件时间可区分迟到通知，
  标记 `REVIEW`，**不关闭当前 Session**
  → **Type 52 不生成 RADIUS Accounting-Stop，不关闭 RADIUS Accounting Session，不生成 Usage**；
  RADIUS Session 只由真实 Accounting 与 Grant 控制。
- 其他消息：枚举可识别 Type 50/51（MAC 用户上线通知及 ACK）、Type 54（请求 MAC 用户离线）。
  **不能因枚举存在就视为所有厂商已启用**；支持状态、方向、属性、响应 Type、认证器与重试语义必须逐条由
  Profile 配置并有正式资料/抓包证据。H3C 不同文档版本可能使用 `REQ_MACBIND` / `ACK_MACBIND` /
  `NTF_MTUSER_LOGON` / `NTF_MTUSER_LOGOUT` 等名称，CMCC/iMC 服务器类型也影响 Profile；
  名称相似不等于 Type、字段或摘要算法相同。
- 安全与可靠性：Secret 类型固定为 `PORTAL_MAC_TRIGGER`，**不能使用 NAS RADIUS Secret 或 Portal Client Secret**；
  只按源地址 + 监听 endpoint 找 NAS，未知或歧义来源丢弃；请求 fingerprint 绑定原始报文字节与可信
  NAS/endpoint 语境，不能只用 MAC 作幂等键；Request Journal 保存摘要、Profile/Secret 版本引用、状态、
  响应原始字节与恢复信息，Secret 明文不落库；会触发 Portal 登录/下线的操作必须先持久化意图并定义
  UNKNOWN 恢复路径；只有能证明幂等或有协议级重放保证时才自动重试；日志脱敏。

### 5.4 启用新 Profile 的证据门槛

启用任一厂商 Portal / MAC-Trigger Profile 前，必须同时具备：厂商正式资料或可信抓包、
设备型号与软件版本、字段定义（字节序 / 字符集 / 长度）、请求响应样本、Authenticator 输入顺序、
Secret 来源与类型、错误码映射、重试与幂等语义。

**证据不完整则保持 disabled / unverified**，不得通过"枚举里有这个 Type""两边都叫 Portal"
或"端口一样"推断兼容。新增支持时按架构基线第 1.3 节走开发前对照与开发后基线更新。

---

## 6. 协议与业务事实边界

| 事实 / 协议 | 权威产生方 | 不可替代的事实 |
|---|---|---|
| Portal Web Ticket | Portal 应用 | 浏览器访问票据 |
| Portal Client ACK_AUTH | NAS | NAS 接受 / 拒绝 Portal 登录 |
| MAC-Trigger Type 49 | MAC-Trigger 服务 | MAC 查询请求响应 |
| RADIUS Access-Accept | RADIUS 服务 | AAA 授权并签发 Grant |
| Accounting Start / Interim / Stop | NAS | NAS 报告的计费会话事实 |
| CoA / Disconnect ACK / NAK | NAS | NAS 对动态授权 / 下线命令的结果 |
| Portal Session | Portal 域 | Portal 层会话 |
| RADIUS Session | Session / Accounting 域 | 与 Grant 关联的 RADIUS 接入会话 |
| Usage Fact | Accounting / Usage 域 | 可验证的用量事实 |

**禁止：**

- 用 Portal 登录成功替代 RADIUS Access-Accept；
- 用 MAC-Trigger Type 49 替代 Portal ACK_AUTH；
- 用 Portal Logout ACK 替代 RADIUS Accounting-Stop；
- 用 MAC-Trigger Type 52 替代 Accounting-Stop；
- 用 Disconnect-ACK 推断 Portal Web Session、ISP Dial Session 或所有网络链路均已关闭；
- **用对账生成的 synthetic Stop 替代 NAS 真实 Accounting-Stop**（本项目当前偏差，见 D1）；
- 混用上游 RADIUS、NAS RADIUS、Portal Client、MAC-Trigger 与 Web JWT Secret。

---

## 7. FreeRADIUS 3.2.x 流程对照（Go 侧映射）

| FreeRADIUS 概念 / 阶段 | 作用 | ToughRADIUS 对应边界 |
|---|---|---|
| listen | 接收 RADIUS 并选虚拟服务器 | `internal/radiusd/server.go` + `net_nas` 源 IP / Secret 解析 |
| recv Access-Request | 初始校验、属性解析 | `radius_auth.go` + `message_authenticator.go` + fork 解析器 |
| authorize | 识别用户 / Realm、加载属性、策略前置 | `request_metadata` / `nas_lookup` / `vendor_parsing` / `load_user` + `proxy_dispatch`（realm） |
| authenticate | PAP / CHAP / MS-CHAP / EAP | `plugin_auth` + `eap_dispatch`（`internal/radiusd/plugins/eap/`） |
| post-auth | 最终策略与响应属性 | 响应增强器（`vendorparsers`）+ Class / Session-Timeout / Filter-Id / 限速 VLAN |
| pre-proxy / post-proxy | 上游请求与响应处理 | `internal/radiusd/proxy/client.go`（`ForwardAuth` / `ForwardAcct` / `Replicate`）、独立 Secret 与响应验证 |
| preacct / accounting | Accounting 预处理与计费事件 | `radius_acct.go` + `acct_plugin_runner.go` + `plugins/accounting/handlers/` + `radius_acct_record` |
| CoA / Disconnect virtual server | Dynamic Authorization | `dynauth_server.go`（3799 双来源 ingress）+ `coa_service.go` / `coa_task_manager.go` |

FreeRADIUS 使用模块化阶段与 request/reply/control 属性列表组织处理。项目借鉴阶段职责与协议行为，
但**不复制** unlang、模块执行器或 C 内存模型。
**Portal UDP 2000、MAC-Trigger UDP 50100、华为/CMCC Portal V2 都不映射为 FreeRADIUS 虚拟服务器阶段**。

> 参照版本锁定 FreeRADIUS 3.2.x（以 3.2.9 文档与 v3.2.x stable 分支为主）。
> **不得用 4.0.x 新行为未经核验地反推 3.2.x。**

---

## 8. 已知偏差与待对齐清单（开发前必读）

本表把"规范应然 / 冻结约束"与"代码实然"的差距显式登记，供后续开发排期。
**本轮仅登记，不改代码**。状态与验收口径以架构基线第 18/19 节为准。

| 编号 | 偏差 | 规范 / 冻结依据 | 代码现状（PROJECT-GO） | 建议处理 | 基线章节 |
|---|---|---|---|---|---|
| D1 | 对账会**合成 Stop** | Java 参考手册 3.5、本文 4.5：缺失 Stop 只标 STOP_MISSING / 离线候选，真实 Stop 才关闭 Session | `internal/billing/reconciliation.go` 的 `closeMissingStop` 写 synthetic Stop 标记并把 `radius_accounting` finalize 为 Stop | 明确区分"对账标记"与"NAS 真实计费事实"；保留原流量值但不得把补偿记录冒充真实 Stop；重新定义会话关闭条件 | 6.2 / 6.3 |
| D2 | 出站未收紧为 PAP | 澄清 2：本项目 → 上游 / 代拨 / 运营商 AAA 统一 PAP，不用 CHAP / MSCHAPv2 | `proxy/client.go` 的 `buildUpstreamPacket` 逐属性复制，仅跳过 User-Password / MA / User-Name；CHAP / MS-CHAP 属性原样转发 | 出站前剥离所有挑战式凭据属性，仅重建 PAP User-Password；非 PAP 入站请求按策略拒绝或明确不支持 | 2.1 / 5.2 / 7.2 / 11.2 |
| D3 | 会话增量计费未接线 | 澄清 3：Accounting 按会话增量计费**必需** | `billing/engine.go` 的 `ChargeSession` 引擎已实现且有单测，但正常 Accounting 路径（start / update / stop handler → `radius_acct.go`）未调用，仅 `RestoreDeadLetters` 使用 | 在 Interim / Stop 处理链按 delta 调用 `ChargeSession`，复用幂等键，补齐正常路径测试 | 6.3 / 8.1 |
| D4 | 上游下线模式选择未实现 | 澄清 4：上游用 CoA → 发 DM；上游仅 RADIUS → 发 Accounting-Stop(2) | 仅 `adminapi/proxy_relay.go` 的 DM 路径与 PANABIT Stop 的 DM 联动；无 `disconnect_mode` 配置与 Stop 路径 | 在 `radius_upstream` / 路由增加对接模式，按模式分发；失败只记日志/指标，不阻断本地计费 | 9.3 / 11.2 |
| D5 | MA 默认非 fail-closed | 本文 3.5：不支持 / 未确认组合 fail closed | `message_authenticator.go` 默认 `warn` | 生产默认 `enforce`；EAP / DAS / 代理响应逐路径核验 MA 必填策略 | 4.2 |
| D6 | 重复请求与可靠投递语义待验证 | 本文 4.1 / 4.4 / 4.5：精确响应缓存、外部副作用一次性、Inbox、Proxy-State、Outbox 与逐目的地重试 | 计费侧 `DedupKey` 去重；认证侧精确响应缓存、Proxy-State 回送、独立 Outbox 与崩溃窗口未验证；会话状态多为进程内 | 补重复请求检测与响应材料持久化；多实例引入共享状态时不得序列化本机 UDP 资源 | 5.1 / 6.1 / 7.2 |
| D7 | Portal 头长度 / Profile 未锁定 | 本文 5.1 / 5.2：16 字节头 + Profile 扩展 vs 本项目 32 字节头，字段布局不得互推 | `internal/portal/protocol.go` 固定 32 字节头；无 Profile 版本化配置 | 按 Vendor/Model/Version 引入 Profile；新设备必须有抓包样本与 Authenticator 输入顺序证据 | 10.2 / 10.3 |
| D8 | MAC-Trigger / NAS Portal Client 未实现 | 本文 5.2 / 5.3、架构基线 10.3 | 仓库无对应实现；`internal/portal` 仅为华为/CMCC Portal V2 Server | 保持 `UNVERIFIED / 边界外`；如立项，另建独立协议包并按 5.4 证据门槛启用 | 10.3 / 11.1 |
| D9 | Secret 生命周期与分类未完备 | 本文 3.5 / 6：五类 Secret 独立、KMS/HSM、密文完整性、轮换审计 | 架构基线 4.3 冻结三类分离 + Portal/Web 两类；MAC-Trigger Secret 未建模；生产密钥提供器未完成 | 补 Secret 类型枚举与轮换审计；无生产密钥提供器时失败关闭 | 4.3 |

---

## 9. 开发审查清单（Go 侧）

新增协议字段、消息或设备支持时，必须同时更新：

1. 本文对应章节与来源 / 证据等级链接；
2. 协议模型、编解码器与 Validator（含边界校验、脱敏与常量时间比较）；
3. Vendor Profile / Message Profile / Attribute Profile / ErrorCode Profile 的**版本化配置**（若启用）；
4. 安全校验、重复请求检测、响应签名与失败 / UNKNOWN 结果处理；
5. 应用流程、Journal / 幂等键、恢复流程与审计字段；
6. `internal/domain/tables.go` 迁移清单、迁移脚本与全量安装 SQL（若持久化结构变化）；
7. **`docs/ARCHITECTURE-BASELINE.md`**：按第 1.3 节强制规则——开发前对照第 2/18/19 节，
   开发后更新基准提交、相关功能域状态、模块职责表 / 数据模型表，并在第 20 节追加变更记录；
   **禁止创建并存版本，禁止夸大，未验证能力标"待验证"**；
8. 本文第 8 节偏差表：若某条偏差被修复，更新状态并在架构基线变更记录中同步说明。

启用新 Profile 前必须有厂商资料或可信抓包、设备型号与软件版本、字段定义 / 字节序 / 字符集 / 长度、
请求响应样本、Authenticator 输入顺序、Secret 来源与类型、错误码映射、重试与幂等语义。
证据不完整则保持 `disabled / unverified`。

**本手册不代表代码已通过互通测试。** 用户未授权前，不运行编译、测试、应用启动、数据库迁移、
UDP 联调、压测或真实 NAS 验收。本轮严格遵守：仅新增文档，代码零改动。

---

## 10. 权威资料与源码索引

### 10.1 IETF / IANA（NORMATIVE）

- [RFC 2865 RADIUS](https://www.rfc-editor.org/rfc/rfc2865) · [RFC 2866 Accounting](https://www.rfc-editor.org/rfc/rfc2866)
- [RFC 2869 Extensions（MA、Gigawords、Event-Timestamp）](https://www.rfc-editor.org/rfc/rfc2869)
- [RFC 3579 RADIUS Support for EAP](https://www.rfc-editor.org/rfc/rfc3579) · [RFC 3748 EAP](https://www.rfc-editor.org/rfc/rfc3748)
- [RFC 5080 Common RADIUS Implementation Issues](https://www.rfc-editor.org/rfc/rfc5080)
- [RFC 5176 Dynamic Authorization (CoA/Disconnect)](https://www.rfc-editor.org/rfc/rfc5176)
- [RFC 6929 RADIUS Extensions](https://www.rfc-editor.org/rfc/rfc6929) · [RFC 5216 EAP-TLS](https://www.rfc-editor.org/rfc/rfc5216) · [RFC 5281 EAP-TTLS](https://www.rfc-editor.org/rfc/rfc5281)
- [IANA RADIUS Types](https://www.iana.org/assignments/radius-types/radius-types.xhtml) · [IANA Private Enterprise Numbers](https://www.iana.org/assignments/enterprise-numbers/enterprise-numbers)
- 本地归档：`docs/rfcs/`（50 个 RFC 原文）；精选索引：`docs-site/src/zh/rfc-index.md`、`docs-site/src/en/rfc-index.md`。

### 10.2 FreeRADIUS 官方资料（锁定 3.2.x）

- [FreeRADIUS Documentation Portal](https://www.freeradius.org/documentation/)
- [3.2.9 RADIUS Protocol](https://www.freeradius.org/documentation/freeradius-server/3.2.9/concepts/protocol/authproto.html)
- [3.2.9 Request Processing](https://www.freeradius.org/documentation/freeradius-server/3.2.9/concepts/session/processing.html)
- [3.2.6 AAA request handling](https://www.freeradius.org/documentation/freeradius-server/3.2.6/concepts/aaa.html)
- [v3.2.x stable source](https://github.com/FreeRADIUS/freeradius-server/tree/v3.2.x)

### 10.3 厂商 Portal / MAC-Trigger 资料（VENDOR）

- [H3C Portal 命令参考：MAC-Trigger Server 信息](https://www.h3c.com/cn/d_202111/1486910_30005_0.htm)
- [H3C Portal 配置：MAC 绑定服务器及默认 UDP 端口](https://www.h3c.com/en/d_202304/1822598_294551_0.htm)
- [H3C MAC 快速 Portal 认证流程说明](https://www.h3c.com/en/d_202202/1553314_294551_0.htm)
- [H3C Portal 命令参考：MAC binding 消息名称](https://www.h3c.com/en/d_202208/1657749_294551_0.htm)

这些资料证明 H3C MAC binding / MAC-trigger 的**产品功能与部分消息语义**；
不自动证明 CMCC Profile、其他厂商或本项目所有设备字段完全一致。具体部署仍需对应型号 / 版本资料与抓包确认。

### 10.4 本项目源码入口（PROJECT-GO）

| 领域 | 入口 |
|---|---|
| RADIUS 认证 / 管线 | `internal/radiusd/radius_auth.go`、`auth_stages.go`、`message_authenticator.go` |
| RADIUS 计费 / 话单 | `internal/radiusd/radius_acct.go`、`acct_plugin_runner.go`、`plugins/accounting/` |
| 代理 / 抄送 / 漫游 / 代拨适配 | `internal/radiusd/auth_proxy_stage.go`、`proxy/`、`acct_proxy.go`、`dynauth_server.go` |
| Portal V2 协议侧 | `internal/portal/`（`protocol.go` / `server.go` / `service.go` / `ticket.go`） |
| CoA / Disconnect | `internal/radiusd/coa_service.go`、`coa_task_manager.go`、`internal/adminapi/proxy_relay.go` |
| 计费 / 对账 / 账本 | `internal/billing/`（`engine.go` / `reconciliation.go` / `ledger.go` / `orders.go`） |
| 数据模型与迁移 | `internal/domain/tables.go`、`internal/domain/*.go` |
| 配置与端口 | `config/config.go`、`.env.example` |
| 单一对接契约 | `docs/portal-v2-integration.md`、`docs/dial-gateway-integration.md` |
| 状态与验收 | `docs/ARCHITECTURE-BASELINE.md` |

> 源码是当前实现事实，本文是开发协议摘要。二者冲突时先确认 RFC / 厂商资料，再修正代码与文档，
> **不得通过改文档掩盖实现偏差**。

### 10.5 参考范式的关系

用户提供的 Java 版《Radius-Vue-Plus 协议规范与业务流程手册》是 **PROJECT-JAVA** 参考范式。
本文保留其证据等级体系、业务事实边界表与 FreeRADIUS 阶段对照的可移植部分，但：

- 不复制 Netty Listener、Request Journal、Grant Gate、Profile 版本化配置、MySQL/PostgreSQL schema 等 Java 实现；
- 不把 Java 手册里的"项目约束"写成 ToughRADIUS 现状；
- 所有 ToughRADIUS 结论必须能落到本仓库源码或架构基线状态矩阵。

---

> **维护规则**：本文是唯一有效的协议规范手册。规范演进直接修订本文，并在
> `docs/ARCHITECTURE-BASELINE.md` 第 20 节变更记录中登记；不新建并存版本。
