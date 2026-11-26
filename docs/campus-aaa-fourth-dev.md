# 校园网 AAA 第四批二开：接入网络、认证增强与合规

本批实现第三批明确列为"未覆盖"的九项。所有协议实现均以 RFC/IANA/厂商官方文档为依据；对无权威公开标准的部分明确标注为"事实标准"或"私有约定"，不冒充 RFC。

## 0. 权威依据与重要事实更正

调研中发现规划文档/常见资料的多处错误，本实现按权威资料纠正：

| 项 | 常见错误说法 | 权威结论 |
|---|---|---|
| DHCP Portal URL | Option 243 是标准 | IANA BOOTP-DHCP 注册表：224–254 为私有保留。**标准方案是 RFC 7710 §2.1 的 Option 160**。实现同时发 160（标准）+ 243（兼容旧 CPE）。 |
| Option 82 | 出自 RFC 2132 | 出自 **RFC 3046**（含子选项 1 Circuit-ID / 2 Remote-ID，回复须原样回显且置于最后）。 |
| EAP-PWD Type | 103 | IANA EAP 注册表：**Type 52**（RFC 5931）。 |
| EAP-TTLS / SIM / AKA / FAST / TEAP | 9/32/50/52/53 | 正确为 **21 / 18 / 23 / 43 / 55**（IANA eap-numbers）。 |
| EAP-MSCHAPv2 | 26 | IANA 为 29；本代码历史值 26 保留兼容，新增 29 常量，服务端两者均识别。 |
| DHCPv6 消息 | Renew=6 / Decline=7 | RFC 3315 §5.3：**Renew=5 / Rebind=6 / Reply=7 / Decline=9**。 |
| RADIUS CoA/DM | RFC 3579、端口 3812 | **RFC 5176**（obsoletes RFC 3576）、**UDP 3799**。 |
| Acct-Status-Type | 2=Interim | RFC 2866：**1=Start / 2=Stop / 3=Interim-Update**。 |
| Event-Timestamp | RFC 2866 | **RFC 2869 §5.13**（属性 55）。 |

来源（均已逐条访问核实）：
- DHCP: https://www.rfc-editor.org/rfc/rfc2131.txt , https://www.rfc-editor.org/rfc/rfc2132.txt , https://www.rfc-editor.org/rfc/rfc3046.txt , https://www.rfc-editor.org/rfc/rfc7710.txt , https://www.iana.org/assignments/bootp-dhcp-parameters/bootp-dhcp-parameters.xhtml
- DNS: https://www.rfc-editor.org/rfc/rfc1035.txt
- DHCPv6: https://www.rfc-editor.org/rfc/rfc3315.txt , https://www.rfc-editor.org/rfc/rfc9915.txt , https://www.iana.org/assignments/dhcpv6-parameters/dhcpv6-parameters.xhtml
- PPPoE: https://www.rfc-editor.org/rfc/rfc2516.txt , https://www.rfc-editor.org/rfc/rfc1661.txt , https://www.rfc-editor.org/rfc/rfc1334.txt , https://www.iana.org/assignments/ppp-numbers/ppp-numbers.xhtml
- EAP: https://www.rfc-editor.org/rfc/rfc3748.txt , https://www.rfc-editor.org/rfc/rfc3579.txt , https://www.rfc-editor.org/rfc/rfc5931.txt , https://www.rfc-editor.org/rfc/rfc4851.txt , https://www.rfc-editor.org/rfc/rfc4186.txt , https://www.rfc-editor.org/rfc/rfc4187.txt , https://www.rfc-editor.org/rfc/rfc7170.txt , https://www.iana.org/assignments/eap-numbers/eap-numbers.xhtml , https://github.com/freeradius/freeradius-server/tree/master/src/modules/rlm_eap/types
- 支付: https://pay.weixin.qq.com/wiki/doc/apiv3/apis/chapter3_5_5.shtml , https://opendocs.alipay.com/common/02kkv7
- RADIUS: https://www.rfc-editor.org/rfc/rfc2865.txt , https://www.rfc-editor.org/rfc/rfc2866.txt , https://www.rfc-editor.org/rfc/rfc2869.txt , https://www.rfc-editor.org/rfc/rfc5176.txt

> 参考的 Java 项目 `fanchaoping37/XuJing-Radius` 在 GitHub 返回 404（API 同样 404），无法作为依据；本批全部基于上述权威公开资料。

## 1. DHCP / DNS 引流

**代码**：`internal/dhcp/`、`internal/dnsintercept/`、`internal/accessnet/`

- DHCPv4 服务：UDP 67，236 字节 BOOTP 头 + 0x63825363 Cookie + TLV；支持 DISCOVER/OFFER/REQUEST/ACK/NAK/RELEASE/DECLINE/INFORM；地址池、租期、Option 3 网关 / 6 DNS / 51 租期 / 54 服务器标识；**Option 82 原样回显**（RFC 3046 §2.2）；**Option 160（RFC 7710）** 下发 Portal URL，附兼容用 Option 243。
- RFC 2131 §4.1 回送寻址：giaddr→中继 67；ciaddr→单播 68；B 标志→广播；NAK 无中继时强制广播。
- DNS 引流：UDP 53，RFC 1035 报文解析与 A 记录伪造（QR/AA/RA 置位，Answer NAME 用压缩指针 0xC00C）；未认证用户返回 Portal IP；白名单域名/已认证 IP 走上游递归；**DNS 劫持是部署技术而非 RFC 规定**，代码中已明确注明。
- `internal/accessnet/bootstrap.go` 统一接线，全部默认关闭（opt-in）。

**启用**：
```yaml
dhcp:
  enabled: true
  bind_addr: ":67"
  server_ip: 10.0.0.1
  network: 10.0.0.0/24
  gateway: 10.0.0.1
  dns: "10.0.0.1,114.114.114.114"
  start: 10.0.0.10
  end: 10.0.0.200
  lease: 3600
  portal_url: http://10.0.0.1/portal/login
dns_redirect:
  enabled: true
  bind_addr: ":53"
  portal_ip: 10.0.0.1
  upstream: 114.114.114.114:53
  whitelist: ["*.example.edu"]
```
等价环境变量：`TOUGHRADIUS_DHCP_ENABLED` / `_NETWORK` / `_START` / `_END` / `_PORTAL_URL` / `TOUGHRADIUS_DNSREDIRECT_ENABLED` / `_PORTAL_IP` 等。

## 2. 内置登录页与 302

**代码**：`internal/captive/`、`internal/webserver/captive.go`、`internal/adminapi/captive.go`

- 未认证 HTTP 请求 302 到登录页，return 参数用 **HMAC-SHA256 签名**防开放重定向；内置登录页模板完整实现 Challenge → auth_ticket → Auth 流程（与第三批的一次性票据联动）。
- 白名单网段/主机、API 路径免重定向；授权带 TTL，支持 Revoke。
- **边界说明**：上游项目铁律"不托管登录页"（`docs-site/src/zh/portal-hotspot-boundary.md`）。本模块默认关闭，需显式 `captive.enabled=true`，属于明确的二开扩展。
- 认证成功后登录页调用 `/api/v1/captive/authorize` 放行客户端 IP。

**启用**：`captive.enabled=true`、`bind_addr`/`login_path`/`portal_url`/`whitelist_cidr`/`authorized_ttl`。

## 3. EAP 新增方法：EAP-pwd（Type 52）

**代码**：`internal/radiusd/plugins/eap/handlers/pwd_crypto.go`、`pwd_handler.go`

- RFC 5931 完整三阶段：ID（Ciphersuite + token + Server-ID + Prep）→ Commit（Scalar/Element）→ Confirm；仅实现强制套件 group 19（NIST P-256）、PRF/RF=HMAC-SHA256、Prep=none。
- 密码学原语全部标准库实现：`H(x)=HMAC-SHA256([0]^32,x)`、SP800-108 KDF、hunting-and-pecking PWE（P-256 上 `sqrt=y^((p+1)/4)`）、Commit/共享密钥/Confirm/MSK+EMSK 派生。
- 注册到 EAP registry（Type 52）与 coordinator 的 `eap-pwd` 配置；不支持分片（L/M 标志明确拒绝，RADIUS EAP-Message 拼接已覆盖常规 MTU）。
- **安全提示**：Go 标准库 P-256 非常量时间，高保障场景应替换为常量时间曲线实现（代码注释已标注）。
- **SIM/AKA 未实现**：RFC 4187 §4/§12.3 明确服务端需从 AuC 取得认证向量 (RAND, AUTN, RES, CK, IK)，无法纯软件模拟；需先有 AuC/HLR 对接再立项。FAST/TEAP 依赖完整 TLS 隧道与 PAC 基础设施，列为后续。

## 4. PPPoE / IPoE

**代码**：`internal/pppoe/`

- RFC 2516 纯协议层：EtherType 0x8863/0x8864、PADI(0x09)/PADO(0x07)/PADR(0x19)/PADS(0x65)/PADT(0xa7)、Tag TLV（Service-Name/AC-Name/Host-Uniq/AC-Cookie 等）、PPPoE 头 6 字节；RFC 1661 LCP、RFC 1334 PAP（Peer-ID→User-Name、Password→User-Password，对接 RFC 2865 §2.2）、IANA IPCP/IPv6CP 协议号。
- AC 侧 Discovery 状态机（PADI→PADO→PADR→PADS、PADT 终止）可单测。
- Linux `AF_PACKET` raw socket 层（build tag `linux`）需 CAP_NET_RAW；非 Linux 平台编译为不支持（协议层仍可用）。**生产环境的会话态 PPP 转发通常由内核 pppoe 驱动处理**，本模块提供发现阶段网关与 RADIUS 联动基础。
- **IPoE 不是独立 RFC**：是"DHCP Option 82 定位用户 + Portal 认证 + RADIUS CoA 授权"的事实标准组合，复用本批 DHCP/DNS/Captive 与第三批 CoA 实现。

## 5. 微信/支付宝/一卡通 SDK

**代码**：`internal/payment/`

- 微信支付 v3：回调头 `Wechatpay-Timestamp/Nonce/Signature`，签名串 `timestamp\nnonce\nbody\n`，**RSA-SHA256** 用平台证书验签；resource 用 **AES-256-GCM** 解密（APIv3 key 32 字节、AAD=associated_data）；5 分钟时间窗；附 v2 MD5 兼容。
- 支付宝：RSA2 验签，参数排序拼 `k=v&...`（排除 sign/sign_type），校验 app_id 防跨应用重放；trade_status=SUCCESS/FINISHED 判定支付。
- 一卡通：厂商中立的 HMAC-SHA256 适配器（签串 method/card_no/amount/timestamp/nonce），明确标注为部署约定而非公开标准。
- 统一 `Provider` 接口与 `Result` 归一化；凭据全部走 `payment.Config`，不硬编码、不落库、不入日志。

## 6. 前端与小程序

- `web/src/resources/proxy.tsx`：RADIUS 上游、域路由、Portal NAS、计费账户/订单、CoA 任务 6 类 react-admin 资源（List/Edit/Create/Show），已注册到 `App.tsx`。
- `miniprogram/`：微信小程序登录页（Challenge→auth_ticket→Auth→下线），API 层明确要求**后端签名**（小程序包公开，绝不能内嵌 AppSecret）。

## 7. Redis / ClickHouse

- `internal/cache/`：统一 Store 接口（Get/Set/Delete/SetNX），go-redis v9 实现 + 内存降级；未启用 Redis 时行为与之前完全一致（fail-open 降级到本地内存）。SetNX 可用于幂等与 RFC 5080 去重。
- `internal/analytics/`：ClickHouse 归档层（clickhouse-go v2），MergeTree 按 `toYYYYMM(account_date)` 分区；字段对齐 RFC 2866/2869（Status-Type 1/2/3、Event-Timestamp、Gigawords、dedup_key）；批量插入 + 定时 Flush + 失败回滚重放；未配置 DSN 时全部 no-op。

**启用**：预留 `TOUGHRADIUS_REDIS_ADDR/_PASSWORD`、`TOUGHRADIUS_CLICKHOUSE_DSN` 环境变量后，在 main 中构造 Store/Archive 注入即可；本批已提供完整实现与测试，配置接线为可选增强。

## 8. IPv6 单栈溯源

**代码**：`internal/dhcpv6/`

- RFC 3315/9915 DHCPv6：UDP 547，Solicit/Advertise/Request/Renew/Rebind/Reply/Information-Request/Release/Decline；DUID（LLT/EN/LL/UUID，按字节精确比较）；IA_NA(3)/IAADDR(5)、DNS servers(23)、**RFC 7710 Captive Portal URI option 103**。
- 地址绑定表：IPv6 → DUID/IAID/MAC/用户名，支持按地址反查与 `SetUsername` 关联 Portal/RADIUS 认证身份，实现单栈溯源。
- 启用：`ipv6.enabled=true`、`prefix`/`dns`/`portal_url`/`lease`。

## 9. 等保测评材料

见 `docs/compliance/README.md`：等保 2.0 三级技术指标与本系统各模块的对应关系、配置基线、审计与日志要求、测评取证清单模板。

## 10. 验证状态

- `go build ./...` 通过（Windows 与 `GOOS=linux` 交叉编译均通过）。
- `go test -short ./internal/...` 全绿（config 包 3 个 Windows 路径分隔符用例为 main 分支既有失败，与本批无关）。
- 本批新增测试：DHCP 6 + DNS 5 + Captive 7 + EAP-pwd 5 + PPPoE 5 + 支付 6 + 缓存 4 + ClickHouse 2 + DHCPv6 5 + Captive API 3，共 48 个。

## 11. 明确未覆盖 / 需外部条件

1. **PPPoE 会话阶段内核转发**：需 Linux pppoe 驱动 + CAP_NET_RAW，本模块提供发现阶段与协议基础，不替代内核拨号网关。
2. **EAP-SIM/AKA**：需 AuC/HLR 认证向量接口；无硬件/接口无法纯软件实现。
3. **EAP-FAST/TEAP**：需在既有 TLS 隧道基础设施上增量开发。
4. **微信/支付宝真实商户凭证**：需用户在官方平台申请 AppID/密钥/证书；代码只做验签与归一化，不含商户密钥。
5. **一卡通协议**：厂商私有，当前为可配置的 HMAC 适配器，具体厂商需按其文档调整字段。
6. **Redis/ClickHouse 配置接线**：实现与测试已完成，启用需在部署时提供 DSN 并注入。
7. **等保测评**：本文提供技术措施说明与取证清单模板，**不等同于测评报告**，正式测评需第三方机构出具。
