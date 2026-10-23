# 用量统计接口契约 Review（users 侧 / 实现方）

- 日期：2026-10-22
- 对象：yunhou-terminal `docs/api/usage-stats.md`（v1 草案，746 行，未提交）
- 立场：Users（云端实现方）+ 消费方复核
- 核对方式：通读契约全文；对照 kaya 仓库源码逐条验证其"反推"字段；对照本仓库（yunhou-users）既有服务端惯例评估落地方式。**未修改 yunhou-terminal 任何文件。**

## 结论

**可以进入实现，但先修订契约：6 个阻塞项必须先定死（多数是文档补一两句话、服务端加一行过滤的活），其中「删除后重传复活」和「bucket_size 无下发通道」是真阻塞。** 其余设计（归一化、幂等覆盖、桶模型、隐私边界）扎实，且关键假设经源码核对基本属实。

---

## A. 阻塞项（不定死就会算错数 / 无法实现）

### A1. bucket_size 没有下发通道，"客户端必须与服务端配置一致"无法达成

契约 §3.1 要求客户端 bucket_size 与服务端部署配置一致，不一致返回 `bucket_size_mismatch`。但**契约里没有任何让客户端发现该配置的接口**。客户端只能 hardcode `hour`；一旦部署改成 `day`，所有在网客户端永久 400，且无自恢复路径。

- 修法（任选其一）：`GET /user/usage/tokens/coverage` 响应加 `bucket_size` / `retention_days` / `server_time`；或独立 `GET /user/usage/config`。上传器启动时拉取，不匹配就暂停上传而不是硬发。
- 附带收益：`server_time` 同时解决客户端时钟偏移导致的 `future_bucket` 误判；`retention_days` 让 backfill 的 `max_days` 有依据。

### A2. §6.1 的防双计论证不成立——bucket_size 进唯一键**本身**防不住混存双计

契约 §6.1 写「唯一键包含 `device_id` 与 `bucket_size`，不会因 … hour/day 混存而双计」。这只对了一半：唯一键防的是**同一行被重复插入**，而混存双计发生在**查询侧**——summary/timeseries 的 `SUM()` 对同一窗口的 hour 行和 day 行会各算一遍。唯一键对此毫无作用。

真正防双计的是另外三件事，契约必须把它们从"隐含"升格为 MUST：

1. 摄入侧硬拒绝不匹配配置（`bucket_size_mismatch`，已有，保持为硬错误）；
2. **所有读查询加 `WHERE bucket_size = <当前部署配置>`**（防御 DB 直写、迁移残留、配置回翻）；
3. 配置变更（hour↔day）必须配套一次性清桶/迁移 runbook（§11.4 已提，保留）。

结论：bucket_size 进唯一键的决定**保留**（它让混存可检测、可清理，且无害），但文档论证要改写，否则实现者会以为光靠唯一键就够了。

### A3. 删除后数据会"复活"——DELETE 与重传之间没有防线

`DELETE /user/usage/tokens`（全量/按窗口/按设备）删掉云端桶后，客户端本地 `~/.kaya/usage/` 的 180 天原始事件还在，下一轮同步会把删除窗口内的桶**原样重新上传**（累计值覆盖语义下这是"正确"行为）。用户视角：点了删除，数据明天又回来了。

- 设备撤销 + `purge=true` 是安全的（撤销后上传返回 `device_revoked`），**问题只在全量删除与窗口删除**。
- 修法：服务端在 `usage_devices` 加 `cleared_before TIMESTAMPTZ`（或 user 级 watermark 表），DELETE 时推进水位；摄入时 `bucket_start <= cleared_before` 的桶静默丢弃并在响应 `rejected` 里计数。契约需补这条语义。
- 次优（不推荐）：契约声明"删除只删云端，客户端可能重传"——体验上等于删除无效。

### A4. 撤销设备的查询口径未定（文档自己标注"需在文档固定"）

§4.8 写「查询默认排除其数据（**或按产品选择保留历史只读，需在文档固定**）」。这不是细节，它直接改变每条查询 SQL（要不要 join `usage_devices` 过滤 `revoked_at`）和 summary 里 `device_count` 的口径。

- 建议 v1 定死：**查询一律排除已撤销设备**（与 §4.2 `device_count` = "未撤销设备数"的现有口径自洽），历史要找回就引导用户撤销撤销操作或重新添加设备。

### A5. batch 部分失败语义自相矛盾

§3.2 成功响应带 `rejected: []` 数组（暗示逐桶部分接受），§8 错误表却把单桶非法定义为整请求 400 `invalid_bucket`（带 `data.index`）。客户端无法据此写重试逻辑。

- 建议定死 **all-or-nothing**：整批校验全过才落库（一个事务），任一非法整批 400 并给 `data.index`。理由：幂等覆盖语义下整批重发零成本，部分接受只会让"哪些桶活着"难以追踪。响应里的 `rejected` 字段要么删掉，要么留给 A3 的水位丢弃计数用。

### A6. bucket_size=day 部署下，timeseries 的 `tz` 参数会算错日界

§4.4 允许 `tz=<IANA>` 影响日/周/月切分。hour 桶下这可行（`date_trunc` + `AT TIME ZONE`）；但 day 桶按 UTC 00:00 对齐落库，**非 UTC 时区的日界横跨两个 UTC 日桶，从 day 桶算不出正确的本地日数字**——不是报错，是静默错账。

- 修法：契约写明 `bucket_size=day` 部署下 `tz` 仅接受 `UTC`，其余返回 400；或 v1 直接把 `tz` 标记为"仅 hour 部署可用"。
- 顺带：§4.1 的 `from/to` 恒按 UTC 日期解释，与 `tz` 并存时边缘日的口径要在文档写一句（建议 from/to 也按 `tz` 解释，实现上只是窗口平移）。

---

## B. 字段缺口与多余（控制台展示视角）

### 缺口

1. **config discovery**：`bucket_size` / `retention_days` / `server_time`（见 A1，最优先）。
2. **导出 CSV 缺 `device_id` 列**：§4.6 的列清单没有设备列，而查询过滤支持 `device_id`——多设备用户导出后无法按设备拆分。加一列零成本。
3. **breakdown 建议直接带 `display_name`**：契约已把"preset 目录映射"定为 Users 侧职责（§2.4），那就让 `breakdown?dimension=provider|provider_model` 每项带 `display_name`（目录命中给名，缺失回退 key 原文），免得 WebSite 再维护一份映射、两边漂移。
4. **`usage_devices` 缺 `consent_version`**：§1.6 要求记录 consent 版本，但 §5 的表只有 `consented_at`。隐私审计时"用户在哪个政策版本下同意的"答不上来。加一列 `consent_version TEXT`。
5. **撤销语义相关字段**（见 A4）：查询排除撤销设备后，coverage 里的 `revoked` 标记保留即可。

### 多余 / 可不做的

- `summary.first_bucket / last_bucket` 与 coverage 的 `earliest/latest_bucket` 重复，保留无妨（一次聚合查询顺手出），不算负担。
- `total_tokens` 不能省：上游 total 可能 ≠ prompt+completion（且契约规则是优先信上游），它不是派生字段。
- `batch_id` 不作幂等键是对的，留着做日志追踪成本为零。
- `source` 维度与 `agent` 强相关（chat→chat/cli，gateway→编程 agent），作为独立维度信息量少，但作为过滤器（"只看网关流量"）有用，保留。

---

## C. 反推风险：假设字段的源码核对结果与防御设计

先把结论摆正：**文档的事件字段不是纯猜测，我逐条对过源码，核心形状属实**——但有几处文档没写的真实行为，会影响"数据完整性"预期。

### C1. 已核实属实（源码证据）

| 契约断言 | 核对结果 |
|---|---|
| `UsageEvent` 七字段（ts/source/agent/provider_id/model_id + 三个可选 token） | ✅ `crates/kaya-core/src/usage_stats.rs:60-73`，逐字段一致 |
| 可选 token 字段缺失时**省略**而非 `null` | ✅ `skip_serializing_if = "Option::is_none"`（同文件 :67-72） |
| 上游不给 usage 就不写事件 | ✅ `record_usage` 三字段全 None 直接 return（:151-153）；网关侧同口径（server.rs:638-640） |
| `total` 缺失或为 0 按 prompt+completion 兜底 | ✅ 聚合侧 `filter(|t| *t > 0)`（:294-298），有单测锁 |
| 桌面三路径身份：`p_xxx` / `builtin` / `custom` | ✅ `src-tauri/src/llm_commands.rs:600-624`（`usage_identity`）——**`custom` 哨兵真实存在** |
| CLI 恒 `provider_id="cli"`、`agent="cli"` | ✅ `crates/kaya-cli/src/chat_driver.rs:434-444` |
| 网关 `agent` 来自占位 token 解析，`provider_id:model_id` 来自 route_entry | ✅ `crates/kaya-core/src/gateway/server.rs:632-672` |
| 网关只记 Content 路由 | ✅ server.rs:1391-1394（Metadata/Unknown 不记） |
| IM 渠道不记账 | ✅ `crates/kaya-core/src/channels/inbound.rs:880` 注释明示 |
| `p_` id = `p_` + 8 位小写 hex，每设备随机 | ✅ 形状由 `is_valid_provider_id` 强约束（model_library.rs:2165-2172） |

### C2. 文档没写、但真实存在的风险点

1. **`ts` 实际后缀是 `+00:00` 且可能带小数秒**：写入用 `Utc::now().to_rfc3339()`，chrono 该方法是 `use_z=false` + 自适应精度（0/3/6/9 位小数）。即真实事件**永远不会是 `Z` 后缀**，且小数位数不定。上传器/服务端解析必须容忍「`+00:00` + 任意小数位」，不能只认示例形态。
2. **网关用量可能整块缺失且不可逆（进程级）**：网关为拿流式 usage 会注入 `stream_options.include_usage`；上游若因此 400，该 route 进入**进程内不可逆黑名单**，从此该路由零用量事件直到进程重启（server.rs:1341-1347，注释明说"永久性统计降级"）。加上"上游不给 usage 就不记"的总闸，**控制台数字预期低于供应商账单的原因不只是 §6.8 的辅助调用**。契约 §6 的口径清单应补这一条；coverage/控制台文案不能把这些数据当对账依据。
3. **网关路径可能产出空 `model_id`**：`route_entry.split_once(':')` 在无冒号时给出 `(route_entry, "")`（server.rs:651）。空串经 §2.4 归一为 `default`——但它与 builtin 的「内置·默认」语义不同。文档的特例规则（只保护 `builtin`+`default`）已经挡住误合并，可接受；上传器注意空串必须归一，不能原样上送（服务端 `model_key` 要求 1–200 字符，空串会整批 400，见 A5 全或无语义下的连带拒收）。
4. **归一化表的兜底方向**：§2.4 只列了 `p_xxx` / `builtin` / `cli` / `custom` 四种 provider_id。若真机出现第五种（未来新哨兵、空串、坏数据），上传器行为未定义。**必须 fail-closed：任何不识别的 provider_id 一律归一为 `custom`，绝不原样上传**——否则自由文本（甚至误填的 base_url）会直接上云，这正是契约花一整节防的泄露面。
5. **本地滚动删除用本地日期，文件名用 UTC 日期**：`rotate` 以 `Local::now().date_naive()` 计算 age（usage_stats.rs:190），日文件按 ts 的 UTC 日命名。时区边缘上，某个 UTC 日文件可能比"180 天"提前或延后约一天被删。影响：backfill `max_days=180` 贴边时本地文件可能已没了。无害但要在预期内。
6. **u32 饱和**：网关侧把上游 u64 token 数饱和钳到 `u32::MAX`（server.rs:659-665）。单次请求超 42 亿 token 不现实，无实际影响；服务端用 BIGINT 是对的。
7. **演进兼容**：当前 serde 结构无 `deny_unknown_fields`，未来 kaya 版本大概率加字段（cache_read 之类）。上传器必须**忽略未知字段、容忍 `null` 当缺失**；新增指标维度走 `schema_version` 升级，服务端表结构预留加列空间即可。

### C3. 文档小勘误（不阻塞）

- §2.2 写"写入点 `:176-182`"，那是 `append_event` 落盘函数；语义写入点是 `UsageMeter.record_usage`（:151-165）与网关 `record_gateway_usage`（server.rs:632）。
- `model_library.rs:1514` 的行号与现状有漂移（形状校验在 :2165 附近），不影响结论。

---

## D. 服务端设计意见（落在 yunhou-users 现有架构上）

### D1. 接口路径与鉴权

- 路径直接可用：`/user/usage/tokens/*`、`/user/devices/*` 与本仓既有 `/user/usage/heartbeat`（JWT 组）同族，本仓无 `/api` 前缀，不要加。注册进 `internal/router/router.go` 的 JWT 组。
- 鉴权复用 `middleware.JWTAuth`，handler 内 `c.GetString(middleware.ContextUserID)`；请求体带 `user_id` 一律忽略（与 router.go 现有注释口径一致）。
- **不开 admin 变体**：本仓 `/admin/*` 走 `InternalAppAuth`（X-App-ID/Secret），与 JWT 用户面是两套。契约明确不做 admin/团队看板，那就任何面都不开这个口；运营要看聚合另起产品决策，不在这条契约里留缝。

### D2. 表结构与索引

- 新 migration 按本仓惯例：`migrations/047_usage_token_buckets.sql` 起（编号按当时最大值顺延），幂等 DDL（`IF NOT EXISTS`），头部注释写清口径——**必须与既有 `usage_events`（心跳）、`llm_usage_events`（/chat 计量）、inference 计量表划清界限**，本仓已有三套"用量"，第四套不写清楚会给后来者挖坑。
- 契约的 schema 基本可直接用（BIGSERIAL + `UNIQUE(...)` + FK `ON DELETE CASCADE`，与本仓 event 表先例一致；账号注销级联自动满足）。
- **索引修正：删掉 `idx_usage_token_user_device_time`**。`UNIQUE (user_id, device_id, bucket_start, ...)` 的最左前缀已经覆盖 (user_id, device_id, bucket_start) 查询，额外索引纯浪费写放大。保留 `(user_id, bucket_start)` 即可。
- `usage_devices` 加两列：`cleared_before TIMESTAMPTZ`（A3 水位）、`consent_version TEXT`（B4）。
- **RLS 建议 v1 不上**：本仓零 RLS 先例，sqlx + 连接池下要每请求 `SET LOCAL` + 强制事务，复杂度与收益不成比例。靠"handler 强制 WHERE user_id + IDOR 越权测试"（契约 §10 已列测试）兜住；真要上 RLS 另起专项。

### D3. 聚合查询写法

- 所有读查询两个固定过滤：`user_id = $jwt_sub` + `bucket_size = $server_config`（A2 防线），撤销设备按 A4 口径排除。
- 窗口查询用半开区间：`bucket_start >= $from_utc AND bucket_start < $to_utc + interval '1 day'`，别用 `bucket_start::date = ...` 走不上索引。
- summary：单条 SQL 出 `SUM×3 + COUNT(DISTINCT device_id) + COUNT(DISTINCT bucket_start::date) + MIN/MAX(bucket_start)`。
- timeseries（hour 部署）：`date_trunc($interval, bucket_start AT TIME ZONE $tz)` 分组；`tz` 用 `time.LoadLocation` 白名单校验；day 部署按 A6 拒 `tz≠UTC`。
- 量级判断同意契约：单用户 180 天 × 小时桶 × 维度基数，行数很小，直查桶表即可，物化视图是以后的事。

### D4. 摄入

- 契约的 `INSERT ... ON CONFLICT ... DO UPDATE` 直接可用（Postgres 唯一索引支撑）。
- 校验顺序：schema_version → bucket_size（配置匹配）→ 逐桶字段（对齐、非负、`requests>=1`、长度/字符集）→ 全过才一个事务写入（A5 全或无）。
- 水位过滤：`bucket_start <= usage_devices.cleared_before` 的桶不写库，计入响应 `rejected`（A3）。
- 设备 upsert 与撤销检查同事务：revoked 设备 → `device_revoked`；否则 `ON CONFLICT (user_id, device_id) DO UPDATE last_seen_at`。
- body 上限 256 KiB 用 `http.MaxBytesReader`；500 桶上限在 decode 后校验。

### D5. 限流与保留期

- 限流复用 `middleware.RateLimitWithKey`（per-user，JWT 之后取 sub 作 key，本仓 heartbeat 接口已有同款先例）。契约建议值映射：上传 120/min ≈ 2 rps + burst 20；回填 10/min；查询 60/min；导出 5/min。`Retry-After` 中间件已带。
- 保留期清理：本仓没有 TTL 先例，但有成熟的 sweeper 模式（`service.OrderSweeper`：构造函数 + `Start(ctx)` ticker + `Stop()`，`main.go` 装配）。照抄一个 `UsageRetentionSweeper`，每日 `DELETE WHERE bucket_start < now() - interval '180 days'`，天数进 config。
- 导出：CSV 防公式注入（`= + - @` 前导加 `'`），`model_key`/`agent_key` 是半自由文本必须处理；流式写出 + 行数上限。

### D6. 测试清单（在契约 §10 基础上补）

- hour/day 混存时查询不双计（A2 防线生效）；
- 删除后重传被水位拦截（A3）；
- 撤销设备后上传 403、查询排除（A4）；
- 非法桶整批 400 且零写入（A5）；
- day 部署 `tz≠UTC` 拒绝（A6）；
- 跨账号 IDOR（契约已有）。

---

## 附：建议回给 yunhou-terminal 的契约修订点（按优先级）

1. 加 config discovery（`bucket_size`/`retention_days`/`server_time`，放 coverage 即可）——解 A1。
2. 改写 §6.1 论证：唯一键不防查询双计；补上"读查询按当前 bucket_size 过滤"与"配置变更需迁移"两条 MUST——解 A2。
3. 定义删除水位语义（`cleared_before` + 摄入丢弃）——解 A3。
4. 定死撤销设备查询口径（建议：一律排除）——解 A4。
5. 定死 batch 全或无语义，收敛 `rejected` 字段用途——解 A5。
6. 写明 day 部署下 `tz` 限制 + from/to 与 tz 的交互——解 A6。
7. §6 口径清单补"网关 usage 注入黑名单导致的路由级缺失"——解 C2.2 的预期管理。
8. §2.4 归一化表加 fail-closed 兜底行（未知 provider_id → `custom`）——解 C2.4。
9. 导出 CSV 加 `device_id` 列；`usage_devices` 加 `consent_version`；breakdown 带 `display_name`——B 类增强。
