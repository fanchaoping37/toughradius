# 代拨网关对接契约（RADIUS / CoA）

> 状态：**架构边界约定 + 对接契约**。本文只描述本项目（校园认证计费系统）如何与外部代拨网关 /
> 上游运营商 AAA 对接，**不实现 PPPoE 拨号本身**。代拨聚合网关是**独立开发/独立部署的外部系统**，
> 其 PPPoE 发现、LCP、会话态转发与内核驱动不属于本项目范围（2026-11-27 用户确认，
> 已删除误置的 `internal/pppoe` 包）。
>
> 契约参考：用户提供的 Java 实现 `fanchaoping37/XuJing-Radius` 中
> `radius-authentication/.../radius/dial/DialProxy.java`（`sendToDialProxyAccessRequest` /
> `sendToDialProxyAccounting`），以及其调用点 `RadiusServerImpl.java:210 / :575`。
> 源码克隆位置为本地临时目录，非本项目依赖；本文只提炼协议契约，不复制其实现。

**2026-11-27 业务澄清（用户确认，本轮仅更新文档、暂不开发）**：

1. **链路**：`NAS → 本项目 RADIUS → 外部代拨网关 / 上游运营商 AAA（外部系统，非本项目实现）`。
2. **不是只有拨号用户才代拨**：所有内网用户经 NAS 以 **IPoE / PPPoE / EAP / Portal Web** 等方式
   送到本项目 RADIUS 的认证，**均可**转换为运营商账号转发上游；账号转换与接入方式解耦。
3. **出站统一 PAP**：本项目 → 上游 RADIUS / 代拨网关 / 运营商 AAA 一律 `User-Name + User-Password`，
   **不使用 CHAP / MS-CHAPv2**；EAP 在本地终结后以 PAP 重构上游请求。Java 侧的 CHAP 路径
   只作为厂商差异事实记录，**不被本项目采用**。
4. **对接协议**：CoA/DM（3799）或 RADIUS（1812/1813）；给上游的强制下线按模式选择
   DM 或 Accounting-Stop（见第 5 节）。
5. **接入网引流（DHCP/DNS/DHCPv6/Captive）由 NAS 给用户推送**，Portal Web 登录页 URL 在 NAS 配置；
   本项目不提供这些 Server。
6. 本轮冻结但未实现的能力，一律按"待开发"标注，不得计入已实现（详见唯一有效基线
   `docs/ARCHITECTURE-BASELINE.md` 第 6.3 / 7.2 / 9.3 / 11 节）。

---

## 1. 职责边界（冻结）

| 角色 | 归属 | 负责 | 不负责 |
|---|---|---|---|
| **NAS / BRAS（接入层）** | 校园网络设备 | 内网用户的 **IPoE / PPPoE / EAP / Portal Web** 接入；DHCP/DHCPv6 地址推送与 Option（含 Portal URL）、DNS 重定向、Captive 302；**Portal Web 登录页 URL 在 NAS 配置**；向本项目发 RADIUS 1812/1813 | 用户/套餐/账务/计费策略、运营商账号转换 |
| **校园认证计费系统（本项目）** | 本仓库 | 作为 RADIUS Server 统一受理 NAS 送来的 Access-Request / Accounting-Request；本地认证与计费；按需在 `proxy_dispatch` 把校园账号转换成运营商账号送上游 AAA；通过 CoA/DM 控制 NAS 上的会话 | PPPoE 线格式、拨号状态机、AF_PACKET、内核转发、接入网引流 |
| **外部代拨网关 / 上游运营商 AAA** | **外部独立系统** | 真正的运营商账号认证与计费结算；接受 RADIUS（认证/计费/Stop）或 CoA/DM 执行下线/改速；作为 RADIUS Client 时也可把请求送本项目 | 校园用户与套餐管理 |
| ~~本项目内的 PPPoE 实现~~ | 已删除 | 不存在（`internal/pppoe` 已移除） | — |

**关键结论**：此前"本项目自己实现 PPPoE"的理解是错误的，已纠正；此前"只有拨号用户才代拨"
的理解同样错误，已按用户澄清修正为**全接入类型统一转发**。代拨网关与本项目有两种部署形态：
作为 RADIUS Client 时登记于 `net_nas`（它送请求给本项目），作为上游 AAA 时登记于
`radius_upstream`（本项目转发给它）；两种形态都只走 RADIUS/CoA，且出站统一 PAP。
对其下发的 CoA/DM 走 `CoAService`（RFC 5176，默认 3799）。

---

## 2. 对接链路总览

```text
  内网用户 ──IPoE / PPPoE / EAP / Portal Web──> [ NAS / BRAS（接入层 + 引流 + Portal URL 配置） ]
                                                       │
                                                       │ RADIUS Access-Request (1812)
                                                       v
                                             [ 本项目 AAA :1812 ]
                                                       │
                    ┌──────────────────────────────────┴──────────────────────────────────┐
                    │ 本地认证 / 计费 / 账务  ·  或 proxy_dispatch 校园账号→运营商账号转换 │
                    │ 出站统一 PAP（User-Name + User-Password），不使用 CHAP/MSCHAPv2      │
                    └──────────────────────────────────┬──────────────────────────────────┘
                                                       │ RADIUS(1812/1813) 或 CoA/DM(3799)
                                                       v
                    [ 外部代拨网关 / 上游运营商 AAA（外部系统，非本项目实现） ]

  NAS <──Access-Accept / Reject(+VSA 限速/VLAN)──────── 本项目 AAA
  NAS ──Accounting-Request(Start/Interim/Stop)─────────> 本项目 AAA:1813
  NAS <──CoA-Request / Disconnect-Request(3799)───────── 本项目 AAA（到期 / 踢线 / 改速）
```

- **接入方向**：内网用户 → NAS（**全接入类型**）→ 本项目 1812/1813；账号转换与接入方式解耦，
  **不是只有 PPPoE 拨号用户才代拨**。
- **转发方向**：本项目 → 外部代拨网关 / 上游运营商 AAA，使用 **RADIUS（1812/1813，统一 PAP）**
  或 **CoA/DM（3799）**；代拨网关作为 RADIUS Client 时也可反向把请求送到本项目。
- **控制方向**：本项目 → NAS 3799 下发 CoA/DM；**给上游的强制下线按上游对接模式选择
  DM 或 Accounting-Stop**（第 5 节，冻结策略、待开发）。反向 DAS（网关/上游 → 本项目）
  由 `internal/radiusd/dynauth_server.go` 受理后中继到 NAS。
- **接入网引流**（DHCP/DNS/DHCPv6/Captive 302）由 NAS 给用户推送，**Portal Web 登录页 URL 在 NAS 配置**；
  本项目不提供这些 Server，Portal 模块只做 UDP 50100 ↔ NAS 2000 协议侧。
- **按会话增量计费为必需能力**：引擎已实现但正常 Accounting 路径待接线（基线 6.3）；
  本轮**暂不开发**。

---

## 3. 认证对接契约（Access）

### 3.1 入站请求（NAS / 代拨网关 → 本项目，全接入类型）

NAS 或代拨网关发送的 `Access-Request` 除标准属性外，按 DialProxy 的透传清单携带。
**入站不限接入方式**（IPoE / PPPoE / EAP / Portal Web 经 NAS 均送达），但**出站到上游统一 PAP**：

| 属性 | 编号 | 用途 / 来源 |
|---|---|---|
| User-Name | 1 | 校园账号（或 MAC 形式）；全接入类型一致，Java 侧另有 MAC 一致性校验 |
| User-Password / CHAP-Password | 2 / 3 | 入站凭据；**本项目转发上游时只使用 PAP（2），不采用 CHAP/MSCHAPv2** |
| NAS-IP-Address | 4 | 网关地址，用于 `net_nas` 查找与 Secret 绑定 |
| Framed-IP-Address | 8 | 用户 IP（转发上游时透传） |
| Called-Station-Id | 30 | 被叫号码 / 接入标识 |
| Calling-Station-Id | 31 | 用户 MAC（`userIp` / `mac` 校验依据） |
| Acct-Session-Id | 44 | 会话标识（认证与计费关联键） |
| NAS-Port-Id | 87 | 端口/VLAN 描述，如 `slot=0;subslot=0;port=0;vlanid=1128` |
| NAS-Port-Type | 61 | 以太网/WLAN；Java 侧转发上游固定写 `15` |

项目现有管线已覆盖：NAS 查找、Message-Authenticator 校验（warn/enforce）、限流、
VSA 解析（含锐捷 4881、华为 2011、H3C 25506、中兴 3902）、用户加载、PAP/CHAP 校验、
响应增强下发 Session-Timeout(27) / Filter-Id(11) / Class(25)。

> **冻结约束（澄清 2）与代码现状**：本地入站可含 CHAP/EAP；但**本项目 → 上游 RADIUS / 代拨网关 /
> 运营商 AAA 的转发一律 PAP**（`User-Name + User-Password`，`MappedPassword` 替换密码），
> **不使用 CHAP / MS-CHAPv2**，也不做有状态 EAP 中继。`buildUpstreamPacket` 当前仅在原包携带
> `User-Password` 时改写凭据，非 PAP 属性仍会被原样复制转发，**待收紧**（本轮暂不开发，
> 详见基线 7.2）。上表与 3.3 节中的 CHAP 描述仅是 Java 侧 DialProxy 的厂商事实，不是本项目采用的路径。

### 3.2 校园账号 → 运营商账号转换后转发上游（不限接入类型）

**所有**内网用户经 NAS 以 IPoE / PPPoE / EAP / Portal Web 等方式送到本项目 RADIUS 的认证，
只要命中 realm 路由、需要用运营商账号到上游 AAA 认证，就走 `proxy_dispatch` 管线阶段
（`internal/radiusd/proxy/`），把校园账号转换成运营商账号转发给**外部代拨网关 / 上游运营商 AAA**。
这与接入方式无关，**不是只有拨号用户才代拨**；**不需要**在项目中实现 PPPoE。
出站统一 PAP，不使用 CHAP / MS-CHAPv2：

| DialProxy 行为 | 本项目对应能力 | 配置位置 |
|---|---|---|
| `radiusUser.getDname()/getDword()` 替换用户名密码 | `UserPrefix + local-part + UserSuffix` 重写用户名，`MappedPassword` 替换 PAP 密码 | `radius_realm_route` 表 |
| 按 `serverType` 选 `DialConfig`（vendorId/ip/secret/port） | realm 匹配选 `radius_upstream` 池，`strategy=single/round_robin/weight` | `radius_realm_route.upstream_ids` + `radius_upstream` |
| 锐捷 VSA `4881/156 = "CMCC"` | 上游请求侧可按 `dial_vendor_attrs_json` 注入原始 VSA 字节；具体值由路由配置提供，不硬编码 | `radius_realm_route` + `internal/radiusd/proxy/dial.go` |
| 上游 Accept/Reject 合成回网关 | `ForwardAuth` 取上游响应并重签 NAS 密钥与 MA 后回包 | `internal/radiusd/proxy/client.go` |
| 上游失败返回合成 Reject + `Reply-Message="upstream failure"` | fail-closed 拒绝；上游 Reply-Message 按用户名缓存 5 分钟，仅回显文案不改变认证结果 | `auth_proxy_stage.go` + `internal/radiusd/dial_reject_cache.go` |

### 3.3 厂商差异（DialProxy 事实，供适配参考）

| `vendorId` | 认证方式 | 上游端口 | 计费/下线特点 |
|---|---|---|---|
| `4881`（锐捷） | Access-Request + CHAP + VSA 4881/156=CMCC | `dialConfig.serverPort` | 计费 1813，携带 4881/155 |
| `VENDOR_DRCOM` | PAP，移除属性 3/60 | `serverPort` | 计费 1813 |
| `VENDOR_PANABIT` | **CoA-Request**（带 Filter-Id `__PAIPPXY__`、属性 77/88） | `serverPort` | Stop 时发 Disconnect-Request 到 **3799** |
| `CTCC` / `CMCC` / `CUCC` | CHAP，固定 NAS-IP-Address（电信/移动/联通 AAA IP），端口 1645 | 1645 | 计费 1646，另抄送 DNS 服务器 1813（`Class`=运营商标识） |
| `VENDOR_XUJING` | PAP，Filter-Id=`__DIALPROXY__` | `serverPort` | 计费 1813 |

> 注：Java 侧写死的运营商 IP（`222.89.246.21` 等）与共享密钥属于其部署环境私有值，
> **不得**复制到本项目。本项目一律通过 `radius_upstream` 配置注入，密钥独立加密。
>
> **PAP 冻结约束（澄清 2）**：表中锐捷 `4881` 与 `CTCC / CMCC / CUCC` 的 **CHAP** 行是
> Java 侧 DialProxy 的历史厂商事实，仅供差异参考；本项目按用户确认**统一采用 PAP**，
> **不实现、不启用这些 CHAP 路径**，也不复制其固定运营商 IP / 密钥。
> DRCOM「移除属性 3/60」的裁剪能力保留（去掉 CHAP 相关属性与统一 PAP 方向一致）。

---

## 4. 计费对接契约（Accounting）

| 属性 | 编号 | DialProxy 透传 | 本项目状态 |
|---|---|---|---|
| Acct-Status-Type | 40 | Start=1 / Stop=2 / Interim-Update=3 | 已实现，且已纠正历史文档错误 |
| Acct-Delay-Time | 41 | 透传 | 已落库 |
| Acct-Input/Output-Octets | 42 / 43 | 透传 | 已实现 |
| Acct-Session-Id | 44 | 透传（关联键） | 已实现 |
| Acct-Session-Time | 46 | 透传 | 已实现 |
| Acct-Input/Output-Gigawords | 52 / 53 | 透传 | 已合并为 64 位 |
| Event-Timestamp | 55 | 透传 | 已优先用于事件归属 |
| Acct-Terminate-Cause | 49 | Stop 时缺省补 `1` | 已解析落库（缺省补全未实现） |
| Called/Calling-Station-Id | 30 / 31 | 透传 | 已实现 |
| NAS-Port-Id / NAS-Port-Type | 87 / 61 | 透传 | 已实现 |

转发模式由 `radius_realm_route.acct_mode` 控制：`both`（本地+上游，默认）/ `forward` / `local`。
RFC 5080 去重、脱敏原始属性落 `radius_acct_record` 已实现。

**按会话增量计费为必需能力（澄清 3，非可选）**：Interim-Update / Stop 的累计计数器必须以
delta 方式扣费、按 `Acct-Session-Id` 幂等，流量用 Gigawords 合并后的 64 位值。
`billing.ChargeSession` 引擎已实现并有单元测试，但**正常 Accounting 处理路径尚未接线**
（当前仅死信重放调用），属待开发项；本轮**暂不开发**，详见基线第 6.3 节。

---

## 5. CoA / Disconnect 对接契约

| 场景 | 方向 | 实现 |
|---|---|---|
| 管理端/策略到期踢代拨网关上的用户 | 本项目 → 代拨网关 3799 | `CoAService.Disconnect/CoA`，`radius_coa_task` 先落库后发送，固定 Identifier 重投 |
| 代拨网关或运营商发来 CoA/DM | 外部 → 本项目 3799 | `dynauth_server.go` 校验来源 Secret + MA，中继到 NAS，回 ACK/NAK |
| PANABIT Stop 触发下线（Java 特例） | AAA → 上游 3799 | `dial_vendor=panabit` 且 Stop 时复用选中上游，向其 CoA 端口发 Disconnect；失败仅记录日志/指标 |

冻结语义（继承基线第 9 节）：上游 Secret 不得用于 NAS 出站动态授权；上游 Disconnect 要真正下线，
必须用该会话 NAS 自己的 Secret。

### 5.1 给上游的强制下线按对接模式选择（澄清 4，冻结策略 + 待开发）

CoA / 强制下线**不只是对本地 NAS 下发**，还**必须给上游发一份**。发什么由**上游使用的对接模式**决定：

| 上游对接模式 | 本项目 → 上游的下线报文 | 端口 | 状态 |
|---|---|---|---|
| **CoA / DM（RFC 5176）** | `Disconnect-Request`（DM） | 上游 `coa_port`（默认 3799） | 已有 DM 路径（`proxy_relay.go` / PANABIT 联动） |
| **仅 RADIUS（认证/计费，无 DAS）** | `Accounting-Request`，`Acct-Status-Type = Stop(2)` | 上游 `acct_port`（默认 1813） | **待开发**：当前无 Stop 模式路径 |

- 用户名按 realm 路由转换为运营商账号，属性带 `Acct-Session-Id` / `NAS-IP-Address` /
  `Calling-Station-Id` 等，用**上游自己的 Secret** 签名；本地 NAS 下线仍用 NAS 自己的 Secret。
- 模式判定来自 `radius_upstream` 配置，不运行时猜测；无可用模式 fail-closed。
- **尚未实现**：按上游模式在 DM 与 Accounting-Stop 之间选择；上游下线未纳入 `radius_coa_task`
  持久化与重投。本轮**暂不开发**，详见基线第 9.3 节。

---

## 6. 已具备能力 vs 待补适配

**已具备（已接线）**
- NAS / 代拨网关作为 RADIUS Client 的注册与密钥绑定、**全接入类型**认证/计费受理、本地用户与套餐策略。
- 校园账号→运营商账号的 PAP 凭据转换与 realm 路由转发（`proxy_dispatch`），与接入方式解耦。
- 计费转发/抄送、RFC 合规话单、CoA/DM 双向中继与任务持久化。
- **上游请求侧属性注入**：`dial_vendor` 选择厂商 profile，`dial_filter_id` 显式覆盖，
  PANABIT/XuJing 有默认 Filter-Id，DRCOM 移除 type 3/60；`dial_vendor_attrs_json` 用
  `[{"type":26,"value_hex":"..."}]` 注入原始 VSA。配置在管理 API 与运行时双重校验，非法值 fail-closed。
- **上游拒绝原因缓存**：按用户名在进程内缓存 5 分钟 Reply-Message；Accept 时清除；
  只影响 NAS 收到的文案，不改变 Access-Accept/Reject。多实例不共享。
- **PANABIT Stop 联动**：`dial_vendor=panabit` 的 Stop 计费在转发同时向上游 CoA 端口下发 Disconnect，
  使用上游 Secret；这不等于对本地 NAS 下发，遵守“上游 Secret 不用于 NAS 出站动态授权”的冻结语义。

**待补适配（明确未实现，不得声称完成）**
1. **运营商固定 NAS-IP/端口模板**：CTCC/CMCC/CUCC 的 `1645/1646` + 固定 NAS-IP 模板化
   （当前由 `radius_upstream.auth_port/acct_port` 和显式 type-4 属性配置，可用但无一键模板；
   不复制 Java 部署环境的私有 IP）。
2. **DNS 服务器抄送**：计费抄送到 DNS 服务器带 `Class` 运营商标识，本项目抄送机制可复用，
   但专用 `sendDns` 语义未实现。
3. **真实设备联调**：所有字段映射需与选定代拨网关抓包复核。
4. **跨实例拒绝原因共享**：当前缓存为进程内；若需要全局一致，应改用已接线的 Redis Store。
5. **出站统一 PAP 收紧（澄清 2，冻结约束）**：转发前校验出站凭据为 PAP，非 PAP 请求 fail-closed
   （落本地或拒绝），不得把 CHAP / MS-CHAPv2 属性透传到运营商 AAA；当前 `buildUpstreamPacket`
   仍原样复制非 PAP 属性（基线 7.2）。
6. **按会话增量计费正常路径接线（澄清 3，商业化必需）**：在 Interim-Update / Stop handler 后
   调用 `ChargeSession`，当前仅死信重放调用（基线 6.3）。
7. **给上游的强制下线按模式选择 DM / Accounting-Stop（澄清 4）**：见 5.1 节（基线 9.3）。
8. **全接入类型转发的设备级验证**：IPoE / PPPoE / EAP / Portal Web 各接入方式需分别抓包复核
   realm 路由命中与账号转换结果一致（澄清 7）。

> 以上 5–8 项为 2026-11-27 用户澄清后新冻结的需求，本轮**仅更新文档、暂不开发**。

---

## 7. 本项目的后续开发规则

1. 任何"PPPoE"字样若再次出现在 `internal/`，必须确认是**对接契约/属性映射**，而非拨号实现。
2. 新增上游运营商适配时，优先复用 `radius_upstream` / `radius_realm_route` 配置，不新增拨号包。
3. 每次改动后按 `docs/ARCHITECTURE-BASELINE.md` 第 1.3 节更新唯一有效基线。
4. **出站统一 PAP**：本项目 → 上游 RADIUS / 代拨网关 / 运营商 AAA 只用 `User-Name + User-Password`，
   不使用 CHAP / MS-CHAPv2；厂商文档中的 CHAP 事实仅供差异参考，不得实现为本项目路径。
5. **代拨不限接入类型**：IPoE / PPPoE / EAP / Portal Web 经 NAS 送来的认证走同一 realm 路由与
   账号转换；禁止把"代拨"窄化为 PPPoE 拨号用户。
6. **接入网引流属 NAS**：DHCP/DNS/DHCPv6/Captive 与 Portal Web 登录页 URL 由 NAS 承担/配置，
   本项目不提供这些 Server；仓库内参考实现默认关闭、不纳入交付面。
7. **本轮（2026-11-27）仅更新文档、暂不开发**：所有冻结但未实现的能力必须在基线中标为待开发，
   不得计入已完成或已验证。
