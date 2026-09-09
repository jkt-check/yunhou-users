# Kaya Coding Plan — 上游连接器评估（Task 12）

日期：2026-09-09。执行：Task 12 实现者。关联：设计 §8、计划 Task 12。

## 0. 术语工作假设（待业务方最终确认）

计划与设计中的 **"OOS / SUM TO API"** 术语无法在本环境向业务方求证
（基线报告 §6 遗留项）。本任务按控制者裁决以以下**工作假设**推进：

> **OOS = 经 OAuth 授权接入的上游服务**（OAuth/Sub2API 语义）：独立于
> 社交登录的 OAuth 授权流接入供应商侧账号，形成可调度上游账号池；
> 授权撤销/过期进入 reauth_required；账号池用量仅作运营与调度信号。

若业务方最终确认实为 **OSS 自托管模型**：按设计 §1 声明，自托管走同一
上游部署接口（Provider.access_type='self_hosted' + auth_type='service'
静态凭据），不影响模型目录与账本；本任务交付的独立接入适配器
（`internal/inference/providers/connector/service_auth.go`）已覆盖该
路径，OAuth 流不会强加给自托管模型。

**实现中未发现与该假设冲突的证据。**

## 1. 候选版本固定（2026-09-09 观测）

| 候选 | 固定版本 | 许可证 | 来源 |
|---|---|---|---|
| Sub2API | `0.2.3`（docker `weishaw/sub2api:0.2.3`，releases/latest） | **LGPL-3.0(-or-later)** | [repo](https://github.com/Wei-Shaw/sub2api)、[LICENSE](https://raw.githubusercontent.com/Wei-Shaw/sub2api/main/LICENSE) |
| CLIProxyAPI | `v7.2.155`（releases/latest，changelog `v7.2.154...v7.2.155`） | **MIT** | [repo](https://github.com/router-for-me/CLIProxyAPI) |

评估仅基于公开仓库文档/已有笔记（控制者裁决：**不 clone、不运行**）。
设计 §8 同时声明：正式接入前重新核对选定版本、许可证和接口，不把
README 能力陈述视为集成验收——下表"文档声称"均待真实环境复验。

## 2. 能力核对表（文档口径）

| 维度 | Sub2API 0.2.3 | CLIProxyAPI v7.2.155 |
|---|---|---|
| 许可证 | LGPL-3.0：作为**外置进程**使用合规边界清晰；**内嵌/链接进 Go 二进制**会引入 LGPL 合规义务（修改与组合作品条款） | MIT：内嵌 SDK 无许可证障碍 |
| 管理认证 | 管理端 Web + Admin 账号（README "Creating the Admin Account"）；服务端管理面 | Management API（MANAGEMENT_API.md）；管理面随部署 |
| 模型发现 | 管理端模型/分组配置；composite groups 解析模型→provider | 客户端协议入口透传各上游模型；账号池按 provider 分组 |
| 协议字段 | OpenAI 兼容 + Anthropic Messages + Responses 路径（0.2.3 changelog 修过 Messages/Responses 输出上限与鉴权） | OpenAI（含 Responses）/Gemini/Claude 兼容入口，流式/非流式/WebSocket |
| 流式终止 | 透传上游 SSE（未见独立终止语义文档） | 透传上游流式；含 translate/executors SDK |
| 用量来源 | **自带 token 级计量与成本计算 + 客户 Key/订阅/支付系统**（一套完整客户计费账本） | **v6.10.0 起不再内置用量统计**；用量由外置 CPA Usage Keeper / CPA-Manager-Plus 另存 SQLite |
| 账号切换 | 多账号（OAuth、API Key）+ 智能调度 + **sticky sessions** + 每用户/每账号并发上限 | 多账号 round-robin（Gemini/OpenAI/Claude/Grok）+ 每账号 5h/7d 配额探测生态 |
| 状态 API | 管理面板监控 | Management API（账号池批量体检/配额检测由 CPA-Manager 生态补充） |

## 3. 选型结论：**不外置、不内嵌 —— 按连接器接口自实现**（SDK 复用仅限协议参考）

决定：首期**不引入 Sub2API/CLIProxyAPI 作为外置连接器，也不内嵌其
SDK**；按设计 §8 连接器接口在本仓库自实现
（`internal/inference/providers/connector/`），两项目仅作协议与账号接
入参考。理由：

1. **扣费权威唯一（硬约束）**：Sub2API 自带客户 Key/订阅/支付/账本一整
   套客户计费系统；引入它必然形成第二扣费权威。控制者裁决与计划均禁
   止复制其客户系统/账本——客户计费只经过 Yunhou 账本
   （`inference_ledger_entries`，Task 6/9）。剥掉它的账本只剩协议转发，
   而我们已有 Task 8 适配器。
2. **用量来源不匹配**：CLIProxyAPI 自 v6.10.0 移除内置用量统计（外置
   SQLite 生态），与本产品"用量事实落统一计量表、账本可重建核对"
   （Task 6/9/11）不兼容；Sub2API 的用量是其自身账本口径，同样不能作
   为扣费输入。
3. **账号池语义已被覆盖**：轮询/冷却（Task 7/8）、跨实例并发租约
   （Task 7）、会话绑定与额度观测（本任务）已按设计 §8 实现；两项目
   的 sticky/round-robin 形状仅作参考。
4. **许可证**：CLIProxyAPI MIT 内嵌无障碍但其价值（协议翻译/账号池）
   与现有适配器重叠；Sub2API LGPL-3.0 内嵌引入合规义务而外置又带来
   第 1、2 条冲突。
5. **运维面**：外置连接器 = 多一个有状态服务（PG/Redis）+ 管理认证面
   与网络信任边界；设计 §8 要求"连接器管理 API 只允许服务端调用"，
   自实现零新增攻击面。

### 差异记录（若未来重新评估）

- 若首个 OAuth 供应商的授权/刷新细节与 CLIProxyAPI 的实现有出入，
  以其 SDK 源码为**协议参考**（MIT 允许），但仍不复制其账号池/用量
  存储。
- 若业务确需 Sub2API 的"订阅配额分发"产品形态，应作为**独立业务决
  策**评估，且其客户系统/账本永远不作为本产品扣费依据。

## 4. 本任务落地映射

| 设计 §8 连接器接口 | 实现 |
|---|---|
| 授权开始/回调 | `credentials/oauth.go`（state+PKCE 一次性回调，独立于社交登录） |
| 刷新 | `credentials/refresh.go`（pg_advisory_xact_lock 跨实例互斥 + generation CAS；旧 token 晚返回不覆盖新值） |
| 可用模型发现 | `connector.Client.Models`（vendor 无发现端点 = 目录保持运营管理） |
| 调用 | 复用 Task 8 协议适配器（凭据 auth_type='oauth' 的 bundle 解出 access token 即 Bearer） |
| 限额读取 | `connector.Client.Quota` → `quota_observed_at/source/reset_at` 缓存；不可得保持未知 |
| 健康状态 | `workers/upstream_health.go`（401→即时刷新/reauth；可重试→冷却→复测恢复） |
| 自托管服务认证 | `connector.ServiceAuth`（静态凭据探测；不强迫 OAuth） |
| 管理 API 仅服务端 | `httpapi/admin_oauth.go` 挂 credentials:manage 授权组（JWT+app 双重身份） |

## 5. 真实上游验收状态

**待业务方提供测试账号后补做。** 本环境没有任何真实上游 OAuth 测试账
号，全部验收场景（授权过期、双实例同时刷新、旧 token 晚于新 token、
限额耗尽、撤销、绑定失效、连接器不可用）以 mock/仿真 + 真实 PostgreSQL
覆盖（见 task-12-report.md）。真实验收时必须记录：供应商、账号数、
授权→刷新→撤销全链路、产生的上游消耗量、以及 README 声称能力与实
测的差异（按设计 §8 不把文档陈述当作验收）。
