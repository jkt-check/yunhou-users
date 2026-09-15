# yunhou-users relay 模块实施规格(kaya 远程控制配套)

> 日期:2026-09-15 · 版本:v1.0(定稿)· 读者:yunhou-users 服务端团队
>
> 本文是**自包含**的服务端实施规格,不依赖 kaya 仓库上下文。
> 目标:在 yunhou-users(Go 服务)内新增一个轻量 WebSocket relay 模块,
> 支撑 kaya 桌面端的「浏览器 / 手机 APP 远程控制」功能。
>
> 背景一句话:kaya 是跑在用户自己电脑上的桌面 app(Mac/Windows)。
> 用户想在浏览器/手机上远程查看并指挥 kaya 里的 AI coding agent。
> 双方(kaya 桌面端、浏览器端)都**主动外连** relay,relay 按用户隔离房间、
> 双向透传 JSON 帧。relay 是**哑管道**:只做鉴权、路由、presence,
> 不解析业务载荷。

---

## 1. 交付范围总览

| 交付物 | 说明 |
|---|---|
| `POST /relay/ticket` | 用 access_token 换短期 relay ticket(HMAC JWT) |
| `GET /relay/ws` | WSS 端点:hello 认证、房间路由、presence、帧透传 |
| 连接生命周期管理 | hello 超时、ticket 续期/宽限、同设备踢旧连、优雅停机 |
| 限额与防滥用 | 帧大小/速率/连接数/缓冲上限、ticket 签发限流、Origin 校验 |
| metrics 与日志 | 连接/房间/帧计数;日志绝不包含业务载荷 |
| 服务端测试 | §10 验收标准列出的全部场景 |

非目标(v1 不做):业务载荷的解析或持久化、多实例横向扩展(§11)、E2E 加密
(协议已预留,后续版本)、消息历史/离线存储。

---

## 2. 术语

| 术语 | 含义 |
|---|---|
| device | kaya 桌面端连接(角色 `device`),如用户的 Mac。一个用户可有多个 device |
| client | 浏览器/PWA 连接(角色 `client`),同一用户可同时多个 client |
| room | 按 `user_id` 隔离的路由域。device 与 client 都在自己用户的 room 里 |
| relay ticket | 短期 HMAC JWT,`aud="relay"`,WS 连接的唯一凭证 |
| envelope | relay 解析的帧外层(v/type/路由字段);`payload` 是不透明的业务内容 |

---

## 3. 端点规格

### 3.1 `POST /relay/ticket` —— 签发 relay ticket

**请求**

```http
POST /relay/ticket
Authorization: Bearer <access_token>      # 与 /chat 完全相同的鉴权
Content-Type: application/json

{}                                         # 无参数;body 可空
```

**处理**

1. 校验 access_token(与 /chat 同一套 JWT 校验逻辑);
2. **entitlement 校验**:远程功能是付费功能,等价于 `require_paid_access("remote")`
   —— 复用现有订阅/trial 判定;trial 是否放行由产品决定(开放问题 §13.1);
3. 签发 relay ticket(见 §4)。

**响应**(沿用现有 envelope 惯例,payload 在 `data` 层)

```json
{
  "code": 0,
  "data": {
    "ticket": "<HMAC JWT>",
    "expires_in": 300,
    "ws_url": "wss://<host>/relay/ws"
  },
  "message": "ok"
}
```

**错误**

| HTTP | 场景 | 口径 |
|---|---|---|
| 401 | access_token 无效/过期 | 与 /chat 401 一致,客户端触发 refresh |
| 403 | entitlement 不足 | 固定文案,不泄露订阅细节,如 `remote access requires paid plan` |
| 429 | 签发限流 | 每 user ≤ **30 次/min**(正常用量 = 每 5 min 续期 1 次 + 偶尔重连);超限带 `Retry-After` |

**说明**:kaya 桌面端与浏览器端(SPA 持 website 会话的 access_token)调的是
同一个端点、拿到同一种 ticket。website 与 kaya 共享 `app_id=yunhou-website`
的 token family,无需任何额外对接。

### 3.2 `GET /relay/ws` —— WebSocket 端点

**Upgrade 握手**

- 标准 WS Upgrade;**不在 query string 传 ticket**(防日志泄露),ticket 在
  首帧 `hello` 里携带。
- **Origin 校验(必须)**:浏览器 WS 不生效同源策略,防 CSWSH(跨站 WebSocket
  劫持):
  - 请求带 `Origin` 头(= 浏览器 client)→ 必须在白名单内
    (prod:`https://www.yunhouai.com` / `https://www.yunhou.ai`;
    staging 各自域名;从现有 CORS 配置派生);
  - 不带 `Origin` 头(= 非浏览器 device)→ 不校验。
- Origin 不匹配 → 握手阶段直接 403,不升级。

**nginx 注意**:透传 `Upgrade`/`Connection` 头;`proxy_read_timeout 120s`
(> §6 的 30s ping 周期);帧上限对应 `client_max_body_size` 不影响 WS,
但建议在 WS 层实现 256 KiB 帧限制(§8)。

---

## 4. relay ticket 格式

- 算法:**HMAC-SHA256 JWS**(与现有 JWT 体系同构,secret 独立);
- secret:每环境独立(cn/intl × prod/staging),KMS/环境变量注入,支持轮换
  (校验时接受当前 + 上一个 secret);
- claims:

```json
{
  "iss": "yunhou-users",
  "aud": "relay",
  "sub": "<user_id>",
  "iat": 1726000000,
  "exp": 1726000300,
  "jti": "<随机>"
}
```

- TTL:**300s**;校验时钟偏移宽限 30s;
- relay 校验 ticket 是**纯本地 HMAC 验签**,不查库、无状态。

---

## 5. 房间模型与 presence

### 5.1 房间

- room key = ticket 的 `sub`(user_id)。连接方**不可指定**房间;
- room 是内存结构:`{ devices: map[device_id]conn, clients: map[client_id]conn }`;
- 单实例内一个全局 `map[user_id]*Room` + RWMutex 即可。

### 5.2 角色注册

- `device` 角色:hello 带 `device_id`(kaya 侧持久化的随机 ULID)、
  `device_name`、`app_version`。**同一 device_id 重复注册 = 新连踢旧连**:
  旧连发 `closed reason=replaced` 后关闭;
- `client` 角色:hello 带 `client_id`(SPA 生成的会话级随机 id)、
  可选 `client_name`(如 "iPhone Safari");同 client_id 重复注册同样踢旧连。

### 5.3 presence(LWT 语义)

- device 连接建立 → 向 room 内全部 client 广播
  `presence { device_id, online: true, meta }`;
- device 断开(**任何原因**,含超时/踢出/停机)→ 广播
  `presence { device_id, online: false }`;
- client 加入时,`hello_ok` 的 `room_devices` 给出当前全量在线 device 表;
- client 的连接/断开**不**广播(v1 不需要,device 不感知 client 在线数)。

---

## 6. 信封协议(relay 唯一需要理解的层)

全部 JSON 文本帧,字段 snake_case。`v` 为协议版本,当前 `1`;
不认识的 `v` → `closed reason=protocol`。

### 6.1 连接方 → relay

```jsonc
// 认证(必须为首帧,10s 内,超时关闭)
{ "v": 1, "type": "hello",
  "ticket": "<relay ticket>",
  "role": "device",                       // 或 "client"
  "device_id": "…", "device_name": "MacBook Pro", "app_version": "2.4.0",  // device 必填
  "client_id": "…", "client_name": "…" }                                   // client 必填 client_id

// ticket 续期(收到 ticket_expiring 后发送)
{ "v": 1, "type": "renew", "ticket": "<新 relay ticket>" }

// 业务帧:client → 指定 device
{ "v": 1, "type": "app", "target_device_id": "…", "payload": { … } }

// 业务帧:device → 广播给 room 内全部 client
{ "v": 1, "type": "app", "payload": { … } }
```

### 6.2 relay → 连接方

```jsonc
// hello 接受
{ "v": 1, "type": "hello_ok",
  "room_devices": [ { "device_id": "…", "device_name": "…",
                      "app_version": "…", "connected_at": 1726000000 } ],
  "server_time": 1726000000 }

// device 上下线(广播给 room 内全部 client;client 角色不触发)
{ "v": 1, "type": "presence", "device_id": "…", "online": true,
  "meta": { "device_name": "…", "app_version": "…" } }

// ticket 即将过期:exp 前 60s 发送一次
{ "v": 1, "type": "ticket_expiring", "retry_after_ms": 30000 }

// 业务帧转发(原样透传 payload;补充来源信息)
{ "v": 1, "type": "app", "from_device_id": "…", "payload": { … } }   // device→client
{ "v": 1, "type": "app", "from_client_id": "…", "payload": { … } }   // client→device

// client 指定的 device 不在线(仅回给发送方 client)
{ "v": 1, "type": "undeliverable", "target_device_id": "…" }

// 连接即将被服务端关闭(关闭前发送,能发尽发)
{ "v": 1, "type": "closed",
  "reason": "auth" }   // auth | entitlement | replaced | shutdown |
                       // slow_consumer | protocol | idle_timeout
```

### 6.3 keepalive

- relay 每 **30s** 向每个连接发 WS 协议层 ping;读侧 **90s** 内未收到任何帧
  (含 pong)判死,触发断开(`closed reason=idle_timeout` 能发尽发);
- 应用层不需要自带心跳。

---

## 7. 连接生命周期(状态机)

```
[Upgrade + Origin 校验]
      │ 失败 → HTTP 403
      ▼
[等 hello](10s 超时 → close 1008,不发帧)
      │ hello 到达
      ▼
[校验 ticket:HMAC + aud + exp(±30s 偏移)]
      │ 失败 → 发 closed auth → close
      ▼
[注册入房:device 同 id 踢旧连(closed replaced)]
      ▼
[发 hello_ok]──── device 角色另广播 presence online ────▶ [服务中]
                                                              │
      ┌───────────────┬───────────────┬───────────────────────┤
      ▼               ▼               ▼                       ▼
[ticket 剩 60s]   [收到 renew]    [收到 app]            [读超时 90s]
  发 ticket_        校验新票        按 §5 路由              判死断开
  expiring          ├ 失败 →        (离线 →
      │             │  closed       undeliverable)
      │             │  auth/        │
      ▼             │  entitlement  ▼
[60s 内未 renew]   └ 成功 →       [出站缓冲溢出]
  发 closed auth    更新 exp        发 closed slow_consumer
  → close                          → close
```

**优雅停机**:收到 SIGTERM → 停止接受新连接 → 全部连接发
`closed reason=shutdown` → 等待 ≤5s → 强制关闭。(客户端有指数退避重连,
雪崩防护在客户端侧,relay 无需特殊处理。)

**renew 的 entitlement 语义**:`/relay/ticket` 每次签发都过 entitlement;
renew 失败(403)= 付费丢失 → relay 回 `closed reason=entitlement`。
付费丢失最长 6 min(5 min TTL + 60s 宽限)收敛断连,relay 无需轮询用户库。

---

## 8. 限额与防滥用

| 项 | 值 | 说明 |
|---|---|---|
| 帧大小 | ≤ **256 KiB** | 超限 → `closed reason=protocol`;与 /chat body 限额同量级 |
| 帧速率 | ≤ **100 帧/s/连接** | 滑动窗口;超限 → `closed reason=protocol` |
| 出站缓冲 | ≤ **256 帧/连接** | 溢出 → `closed reason=slow_consumer` 断开慢消费者,不影响同 room 其他连接 |
| 每 user 连接数 | device ≤ **10**,client ≤ **20** | 超限 → 拒绝 hello(`closed reason=protocol`) |
| hello 超时 | **10s** | Upgrade 后 10s 内无合法 hello → close |
| hello 失败限流 | **5 次/min/IP** | 连续认证失败临时拒连(防 ticket 爆破;HMAC 本身不可伪造,此为兜底) |
| ticket 签发限流 | **30 次/min/user** | 见 §3.1 |
| payload 内容 | **不校验、不解析** | 仅信封字段参与路由 |

---

## 9. 日志与 metrics

**日志(连接级元数据,绝不记 payload / user_text / ticket 本体)**:

- 每连接一行建立日志:`user_hash`(user_id 的 SHA-256 截断,不对齐明文)、
  `role`、`device_id/client_id`、`app_version`、`origin`(仅是否通过,不记值);
- 每连接一行关闭日志:reason、存活时长、双向帧计数;
- 异常分支(hello 失败、限流命中、slow_consumer)warn 级,节流
  (同 key 1/min)。

**metrics**(Prometheus 风格,沿用现有暴露方式):

```
relay_connections{role, env}                  gauge
relay_rooms_active{env}                       gauge
relay_frames_total{direction, role, env}      counter
relay_undeliverable_total{env}                counter
relay_hello_failures_total{reason, env}       counter
relay_renew_failures_total{env}               counter
relay_slow_consumer_closes_total{env}         counter
relay_connection_duration_seconds{role, env}  histogram
```

**隐私承诺(写进代码评审 checklist)**:relay 模块任何位置不得
持久化帧内容、不得将 payload 写入日志/错误响应/追踪系统。
错误响应用固定文案(与 /chat 的 `invalid request body` 先例一致)。

---

## 10. 测试与验收标准

### 10.1 单元/集成测试(必备场景)

1. **ticket 签发**:有效 access_token → 200 + ticket;过期 token → 401;
   无付费 → 403;限流 → 429 + Retry-After;ticket HMAC 校验(伪造/篡改/
   过期/错误 aud/错误 secret 均拒)。
2. **hello**:10s 超时关闭;Origin 白名单通过/拒绝;坏 JSON / 缺字段 /
   `v` 不支持 → closed protocol。
3. **路由**:device→多 client 广播(两个 client 都收到,内容逐字节一致);
   client→指定 device;`target_device_id` 不存在 → undeliverable 只回发送方;
   跨 room 不可达(A 用户的 client 指定 B 用户的 device_id → undeliverable,
   不泄露存在性)。
4. **presence**:device 上线/正常断开/读超时判死 → 各路径 client 均收到
   online true/false;client 中途加入 → hello_ok 携带全量在线 device。
5. **续期**:exp 前 60s 收到 ticket_expiring;合法 renew → exp 更新;
   伪造 renew → closed auth;过期 + 60s 宽限未 renew → closed auth。
6. **替换**:同 device_id 双连 → 旧连收 closed replaced 且旧连不再收帧。
7. **背压**:慢 client(不读)→ 缓冲满 → 该 client 收 closed slow_consumer,
   device 与其他 client 不受影响。
8. **限额**:257 KiB 帧拒绝;帧速率超限断开;第 11 个 device 拒绝。
9. **优雅停机**:SIGTERM → 所有连接收 closed shutdown;新连接拒绝。

### 10.2 联调验收(kaya 团队配合)

- staging 环境:kaya 桌面端启用远程访问 → website 远程页看到设备在线 →
  发起 chat → 浏览器看到流式输出 → 授权卡在浏览器批准 → kaya 侧生效;
- 杀 kaya 进程 → 浏览器 90s 内看到设备离线;
- 付费态翻转(测试账号)→ ≤6 min relay 断连。

---

## 11. 部署形态与扩容路径

- **v1:单实例**。房间路由是有状态的(连接表在内存),单实例无一致性问题;
  预期量级(付费用户子集 × 人均 1 device + 1~2 client)单实例 Go 服务
  轻松承载(万级长连是 Go WS 的常规水平)。
- **扩容路径(不在 v1 范围,仅备案)**:多实例时需要跨节点扇出层
  (NATS/Redis pub-sub 做 room 广播的内部总线),或按 user_id 一致性哈希
   sticky 路由。届时再评估,协议无需变动。
- **环境**:沿用 cn/intl × prod/staging 四象限;ticket HMAC secret
  每环境独立。

## 12. 实现建议(非约束)

- WS 库:`coder/websocket`(最小、惯用、活跃)或 gorilla/websocket(已恢复
  维护);二选一即可,团队熟悉度优先。
- 房间与连接表:`sync.RWMutex` + map 足够,无需引入外部依赖。
- 与 /chat 共享:JWT 校验中间件、entitlement 判定、envelope 响应格式、
  限流器、固定错误文案模式。

## 13. 开放问题(需产品/双方确认)

1. **trial 用户是否放行远程功能?**(影响 /relay/ticket 的 entitlement 判定分支)
2. **ticket TTL 300s + 宽限 60s 是否可接受?**(更短 = entitlement 收敛更快、
   签发更频繁;更长 = 反之)
3. **website 远程入口页的域名**加入 Origin 白名单的确切值(staging/prod 各一)。

---

*本规格与 kaya 侧设计文档 `2026-09-15-remote-access-design.md`(kaya 仓库
`docs/superpowers/specs/`)配套;如两份文档冲突,以本规格的服务端行为描述为准,
并请联系 kaya 团队同步修订。*
