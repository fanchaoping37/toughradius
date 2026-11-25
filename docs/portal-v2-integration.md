# Portal V2 协议侧对接指南

## 边界

本模块**只实现华为/CMCC Portal V2 的 UDP 协议侧**（与 NAS 交互），不托管登录页、不做
Web 重定向、不做短信/微信/券码开户。认证链路仍然是：

```text
终端 → NAS（内置 Portal 页面）→ Portal UDP 50100↔2000 → NAS → RADIUS 1812 → ToughRADIUS
```

RADIUS Access-Request 由 NAS 发出，本模块不代发；管理员踢线仍然走 RADIUS CoA/DM（3799）。

## 启用

```yaml
portal:
  enabled: true
  bind_host: 0.0.0.0
  bind_port: 50100      # Portal 服务器监听端口（收 NAS 的 ACK/NTF）
  nas_port: 2000        # 向 NAS 发送报文的默认端口
  timeout: 5            # 每次等待响应秒数
  retry: 2              # 重传次数
  session_ttl: 600      # 会话缓存秒数
```

环境变量：`TOUGHRADIUS_PORTAL_ENABLED / BIND_HOST / BIND_PORT / NAS_PORT / TIMEOUT / RETRY / SESSION_TTL`。

在 `portal_nas` 表登记设备（管理 API `/api/v1/portal-nases`）：

```json
POST /api/v1/portal-nases
{"name":"AC-1","addr":"10.0.0.1","port":2000,"secret":"nas-side-portal-secret"}
```

设备侧配置：Portal Server IP 指向本机，端口 50100，共享密钥与上面一致；NAS 上 RADIUS
认证服务器指向本机 UDP 1812。

## 外部登录页调用流程

登录页（由独立 Portal 产品提供）通过管理 API 驱动协议交互：

```text
1. POST /api/v1/portal/challenge
   {"nas_addr":"10.0.0.1","username":"alice@campus","user_ip":"192.168.1.100"}
   → {"data":{"serial":1234,"req_id":4660,"message":"challenge accepted"}}

2. NAS 侧自行向 ToughRADIUS UDP 1812 发 RADIUS Access-Request。
   （这一步由 NAS 完成，ToughRADIUS 的 RADIUS 管线照常处理用户/套餐/鉴权。）

3. POST /api/v1/portal/auth
   {"nas_addr":"10.0.0.1","username":"alice@campus","password":"***",
    "user_ip":"192.168.1.100","auth_mode":"chap"}
   → {"data":{"code":"ACCEPT","err_code":0,"authenticated":true,...}}
   或 {"data":{"code":"REJECT","err_code":1,"message":"rejected","nas_info":"..."}}

4. 用户下线：POST /api/v1/portal/logout {"nas_addr":"...","user_ip":"..."}
   查询会话：GET /api/v1/portal/session?user_ip=...
```

`auth_mode` 支持 `chap`（默认）与 `pap`；当前实现 Challenge 阶段仍会执行（用于取得
SerialNo/ReqID），PAP 的密码以明文属性 0x02 发送。

## 报文与算法

- 版本：V2（32 字节头，全大端）；请求验证字 = `MD5(包头(Auth 清零) || 属性 || Secret)`，
  响应验证字 = `MD5(包头(Auth 替换为请求 Auth) || 属性 || Secret)`。
- CHAP 密码 = `MD5(ReqID 低 8 位 || 密码 || 16 字节 Challenge)`，放在属性 0x04。
- 消息类型：`0x01` REQ_CHALLENGE / `0x02` ACK_CHALLENGE / `0x03` REQ_AUTH /
  `0x04` ACK_AUTH / `0x05` REQ_LOGOUT / `0x06` ACK_LOGOUT / `0x07` AFF_ACK_AUTH /
  `0x08` NTF_LOGOUT / `0x09` REQ_INFO / `0x0a` ACK_INFO / `0x81` STATUS_NOTIFY。
- 下线：用户主动下线走 Portal REQ_LOGOUT；NAS 强制下线发 NTF_LOGOUT（本模块回
  `0x0e` ACK_NTF_LOGOUT 并清理会话）；管理员在 ToughRADIUS 踢线走 RADIUS CoA/DM。

## 已知限制

- 暂不支持 V3（SHA256 验证字）、H3C iMC 私有方言、IPv6 用户属性 0xf1、USER_SYN 同步。
- 会话状态在进程内存中，多实例部署需 sticky 路由或后续引入共享缓存。
- 不做登录页与 HTTP 302 重定向，这部分必须由外部 Portal 产品提供。
