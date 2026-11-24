# ToughRADIUS vs FreeRADIUS 功能与模块对比

> 基于 toughradius @ `dev-freeradius-compare` 分支源码分析（commit 2cb8a42）与 FreeRADIUS 3.2.x / 4.0 官方文档（截至 2026-11）。
> 用途：为后续二开（二次开发）提供架构定位与选型依据。

## 一、项目定位对比

| 维度 | ToughRADIUS | FreeRADIUS |
|---|---|---|
| 语言/形态 | Go 单体二进制（前端 embed 内嵌），Docker 三阶段构建 | C，多平台（Unix-like），无原生 Windows |
| 许可证 | 项目 LICENSE | GPLv2 |
| 目标用户 | ISP / 企业网络 / 运营商，开箱即用 | 超大规模 ISP、eduroam、Fortune 500，通用引擎 |
| 管理界面 | 内置 React 19 + react-admin 5 + MUI 7 Web 管理台（`web/`，/api/v1） | **无官方 GUI**，纯文件配置（radiusd.conf / sites-enabled / mods-enabled）；第三方 daloRADIUS（停滞）、RADIUSdesk、OpenWISP RADIUS |
| 配置方式 | toughradius.yml + `TOUGHRADIUS_*` 环境变量 + 数据库运行时设置（SysConfig 热更） | 多文件分层配置 + unlang 策略语言，灵活但学习曲线陡峭 |
| 扩展模型 | 编译期注册的插件 Registry（PasswordValidator / PolicyChecker / ResponseEnhancer / Guard / AccountingHandler / EAP Handler） | 运行时模块机制（rlm_* 数十个模块，mods-enabled 软链启用）+ unlang + rlm_perl/python/lua/mruby/exec/rest |
| 版本状态 | v9 主线，活跃 CI | 生产推荐 3.2.x（最新 3.2.10）；4.0.0 截至本文仍未正式发布（预计 2026 年底 beta 含 RadSec） |

## 二、协议能力对比

| 能力 | ToughRADIUS | FreeRADIUS |
|---|---|---|
| RADIUS/UDP (RFC 2865/2866) | ✅ 1812/1813（`internal/radiusd/server.go`） | ✅ |
| RADIUS/TCP 明文 | ❌ 未开放监听 | ✅ |
| RadSec (RFC 6614, TCP/TLS 2083) | ✅ 自研实现（`radsec_server.go`，worker 池默认 100） | ✅（3.x 经 home_server tls 配置；4.x 重做） |
| DTLS | ❌ | ✅ |
| CoA / Disconnect (RFC 5176) | ✅ 内置（`coa_service.go`，端口 3799，管理台会话操作 + 审计表 `RadiusSessionActionAudit`） | ✅ 含动态客户端 |
| 代理转发 / realm 路由 / 负载均衡 | ❌ 无 username@realm 解析转发（`Domain` 字段是下发 NAS 的业务域，非路由） | ✅ proxy realm + load-balance / redundant 池（4.x 最少连接 + 确定性哈希） |
| BlastRADIUS (CVE-2024-3596) 防护 | ✅ 强制 Message-Authenticator 校验，Reject 也带 MA | ✅ 3.2.5+ require_message_authenticator |
| 动态 VLAN | ✅ 经厂商 Enhancer / VLAN 绑定校验（`plugins/auth/checkers/vlan_bind_checker.go`） | ✅ Tunnel-Private-Group-ID 等属性，来源任意（SQL/LDAP/unlang） |
| 限速下发 | ✅ 产品化：Profile 上下行速率 → 各厂商 VSA（Huawei Input/Output PeakRate、Mikrotik Rate-Limit、H3C） | ✅ 手写属性映射（unlang + VSA） |
| IPv6 属性 | ✅ Framed/Delegated IPv6 Prefix + Pool（RFC 3162/4818/6911） | ✅ |

## 三、认证方式（EAP）对比

| 方法 | ToughRADIUS | FreeRADIUS |
|---|---|---|
| PAP | ✅ `plugins/auth/validators/pap_validator.go` | ✅ rlm_pap |
| CHAP | ✅ `chap_validator.go` | ✅ rlm_chap |
| MS-CHAPv2 | ✅ `mschap_validator.go` | ✅ rlm_mschap（可对接 winbind/AD） |
| EAP-MD5 | ✅ Type 4 | ✅ |
| EAP-TLS | ✅ 自研 TLS 引擎（`plugins/eap/tlsengine/`），证书↔用户映射、分片重组 | ✅ RFC 5216，最成熟的实现 |
| PEAPv0/MSCHAPv2 | ✅（`peap_handler.go`，含 MPPE 密钥推导） | ✅ |
| EAP-TTLS (PAP/MSCHAPv2 内层) | ✅ RFC 5281（`ttls_*.go`） | ✅ |
| EAP-PWD / FAST / TEAP / SIM / AKA / GTC | ❌ | ✅（SIM/AKA 为 3GPP 场景，ToughRADIUS 无对应） |
| EAP-OTP | 代码存在但**未注册**（无真实后端） | ✅ rlm_totp 等 |
| LDAP/AD 认证 | ✅ Bind 验证（`internal/ldapauth/`，仅 PAP 与 EAP-TTLS 内层 PAP 可用） | ✅ rlm_ldap，方式更多样 |

## 四、数据与运维对比

| 维度 | ToughRADIUS | FreeRADIUS |
|---|---|---|
| 数据库 | PostgreSQL（pgx）+ SQLite（纯 Go 无 CGO），GORM 双后端 | rlm_sql：mysql/postgresql/sqlite/oracle/db2/firebird/mssql/unixodbc 等 10+ 驱动 |
| 计费存储 | `RadiusAccounting` 表，幂等写入（Acct-Session-Id），90 天自动清理 | radacct 表 / rlm_detail 平面文件 / radutmp（4.x 移除） |
| 会话管理 | `RadiusOnline` 表 + 僵死会话 cron 清理 + 并发数限制 | 无内置业务会话管理（依赖 SQL/detail 自行处理） |
| 监控指标 | 自研内存 Counter/Gauge（`pkg/metrics/`），dashboard 聚合；**非 Prometheus 客户端库** | statistics 模块（4.x 初期缺失）；常配合 SNMP |
| 工具链 | `cmd/radtest`（测试客户端）、`cmd/benchmark`（压测）、`cmd/certgen`（CA/证书）、`cmd/reset-password`、demo-seed | radtest/radclient、rlm_detail、debug 模式（-X） |
| 测试 | ~30 个单元测试 + 14 个集成测试（PG + OpenLDAP 容器化）+ EAP 验收报告 | 长期积累，文档/社区成熟 |
| 性能量级 | Go 并发模型（ants 协程池默认 1024），单二进制 | 普通硬件 PAP 数万次/秒；EAP-TLS 全握手 100–300 次/s/核（SSL CPU 瓶颈，二者共性） |

## 五、模块机制本质差异（二开视角）

- **FreeRADIUS**：运行时模块 + unlang，改行为不需要改 C 代码，但复杂策略 = 复杂配置文件；深度定制仍需写 C 模块。
- **ToughRADIUS**：全部扩展点为编译期注册接口（`internal/radiusd/registry/registry.go` + `plugins/init.go`）：
  - 加一个认证方式 → 实现 EAP Handler 接口并注册（按 EAP Type 索引）；
  - 加一个厂商属性下发 → 实现并注册 ResponseEnhancer（现覆盖 Huawei/Cisco/H3C/ZTE/Mikrotik/Aruba/Ikuai 等 14 厂商 VSA）；
  - 加一条业务校验规则 → 实现 PolicyChecker（现有：状态/到期/MAC 绑定/VLAN 绑定/在线数）；
  - 加计费处理逻辑 → 实现 AccountingHandler（Start/Update/Stop/NasState）。
  - 优点：类型安全、单二进制部署、无运行时模块装配出错风险；代价：每次扩展需重新编译。

## 六、结论与二开建议

1. **选型定位**：ToughRADIUS ≈ "RADIUS 引擎 + 运营管理台" 产品；FreeRADIUS ≈ "协议引擎 + 模块生态" 平台。做 ISP 运营（套餐/并发/MAC 绑定/限速/会话管理），ToughRADIUS 开箱即用且二开门槛（Go/React）低于 C + 文件配置。
2. **ToughRADIUS 短板（可列为二开候选）**：
   - 无 RADIUS 代理转发 / realm 路由 / 负载均衡（对接上游漫游、多级运营商场景缺失）；
   - 无 DTLS、无明文 TCP 监听；
   - EAP 覆盖少于 FreeRADIUS（无 EAP-PWD/FAST/SIM/AKA），OTP 未接入真实后端；
   - 无多租户（仅 NodeId 分区 + 三级操作员权限）；
   - metrics 未标准化为 Prometheus 暴露。
3. **不建议绕开的模块**：认证管线 8 个 Stage（`auth_pipeline.go`/`auth_stages.go`）与 Registry 注册点，遵循现有插件接口扩展即可保持可维护性。
