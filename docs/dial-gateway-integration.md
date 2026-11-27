# 代拨网关对接契约（RADIUS / CoA）

> 状态：**架构边界约定 + 对接契约**。本文只描述本项目（校园认证计费系统）如何与外部代拨网关对接，
> 不实现 PPPoE 拨号本身。代拨聚合网关是**独立开发/独立部署**的系统，其 PPPoE 发现、
> LCP/PAP/CHAP、会话态转发与内核驱动不属于本项目范围（2026-11-27 用户确认，已删除误置的
> `internal/pppoe` 包）。
>
> 契约参考：用户提供的 Java 实现 `fanchaoping37/XuJing-Radius` 中
> `radius-authentication/.../radius/dial/DialProxy.java`（`sendToDialProxyAccessRequest` /
> `sendToDialProxyAccounting`），以及其调用点 `RadiusServerImpl.java:210 / :575`。
> 源码克隆位置为本地临时目录，非本项目依赖；本文只提炼协议契约，不复制其实现。

---

## 1. 职责边界（冻结）

| 角色 | 归属 | 负责 | 不负责 |
|---|---|---|---|
| **代拨聚合网关** | 外部独立项目 | PPPoE Discovery（PADI/PADO/PADR/PADS/PADT）、LCP/IPCP、PAP/CHAP 拨号、PPPoE 会话与内核转发、把拨号凭据封装成 RADIUS 请求 | 用户/套餐/账务/计费策略、管理台、多租户 |
| **校园认证计费系统（本项目）** | 本仓库 | 作为 RADIUS Server 受理代拨网关的 Access-Request / Accounting-Request；本地认证与计费；必要时通过代理转发把校园账号转换成运营商账号送到上游 AAA；通过 CoA/Disconnect 控制代拨网关上的会话 | PPPoE 线格式、拨号状态机、AF_PACKET、内核转发 |
| **上游运营商 AAA / 代拨设备** | 外部 | 真正的运营商账号认证、计费结算，或接受 CoA/DM 执行下线/改速 | 校园用户与套餐管理 |

关键结论：**代拨网关在本项目眼中就是一台 NAS（RADIUS Client）**。本项目的
`net_nas` 表登记它的 IP 与共享密钥，认证/计费管线按标准 RADIUS 处理；对其下发的
CoA/DM 走 `CoAService`（RFC 5176，默认 3799）。此前"本项目自己实现 PPPoE"的理解是错误的，已纠正。

---

## 2. 对接链路总览

```
  拨号用户 ──PPPoE──> [代拨聚合网关(外部)] ──RADIUS Access-Request──> [本项目 AAA:1812]
                              │                                          │
                              │                       ┌──────────────────┴──────────────┐
                              │                       │ 本地认证 / 计费 / 账务           │
                              │                       │ 或 proxy_dispatch 转发上游 AAA   │
                              │                       └──────────────────┬──────────────┘
                              │                                          │
                              │<──Access-Accept/Reject(+VSA限速/VLAN)──────┘
                              │
                              │──Accounting-Request(Start/Interim/Stop)──> [本项目 AAA:1813]
                              │                                          │
                              │<────────CoA-Request / Disconnect-Request── [本项目 :3799]
                              │          (管理端踢线 / 改速 / 到期断网)
```

- **认证方向**：代拨网关 → 本项目 1812。
- **计费方向**：代拨网关 → 本项目 1813。
- **控制方向**：本项目 → 代拨网关 3799（CoA/DM）。反向 DAS（代拨网关/上游 → 本项目）
  由 `internal/radiusd/dynauth_server.go` 受理后中继到 NAS。

---

## 3. 认证对接契约（Access）

### 3.1 入站请求（代拨网关 → 本项目）

代拨网关发送的 `Access-Request` 除标准属性外，按 DialProxy 的透传清单携带：

| 属性 | 编号 | 用途 / 来源 |
|---|---|---|
| User-Name | 1 | 代拨场景通常是 MAC 形式或校园账号；Java 侧 MAC 认证用户名与设备 MAC 一致性校验 |
| User-Password / CHAP-Password | 2 / 3 | 拨号凭据（PAP/CHAP） |
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

### 3.2 可选：校园账号 → 运营商账号转换后转发上游

当校园账号本身不负责真实拨号，而要用运营商账号到上游 AAA 认证时，走本项目的
`proxy_dispatch` 管线阶段（`internal/radiusd/proxy/`），**不需要**在项目中实现 PPPoE：

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

---

## 5. CoA / Disconnect 对接契约

| 场景 | 方向 | 实现 |
|---|---|---|
| 管理端/策略到期踢代拨网关上的用户 | 本项目 → 代拨网关 3799 | `CoAService.Disconnect/CoA`，`radius_coa_task` 先落库后发送，固定 Identifier 重投 |
| 代拨网关或运营商发来 CoA/DM | 外部 → 本项目 3799 | `dynauth_server.go` 校验来源 Secret + MA，中继到 NAS，回 ACK/NAK |
| PANABIT Stop 触发下线（Java 特例） | AAA → 上游 3799 | `dial_vendor=panabit` 且 Stop 时复用选中上游，向其 CoA 端口发 Disconnect；失败仅记录日志/指标 |

冻结语义（继承基线第 9 节）：上游 Secret 不得用于 NAS 出站动态授权；上游 Disconnect 要真正下线，
必须用该会话 NAS 自己的 Secret。

---

## 6. 已具备能力 vs 待补适配

**已具备（已接线）**
- 代拨网关作为 NAS 的注册/密钥绑定、认证/计费受理、本地用户与套餐策略。
- 校园账号→运营商账号的 PAP 凭据转换与 realm 路由转发（`proxy_dispatch`）。
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

---

## 7. 本项目的后续开发规则

1. 任何"PPPoE"字样若再次出现在 `internal/`，必须确认是**对接契约/属性映射**，而非拨号实现。
2. 新增上游运营商适配时，优先复用 `radius_upstream` / `radius_realm_route` 配置，不新增拨号包。
3. 每次改动后按 `docs/ARCHITECTURE-BASELINE.md` 第 1.3 节更新唯一有效基线。
