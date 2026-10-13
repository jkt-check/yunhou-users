# Built-in Model Multimodal (Image Input) Design

日期：2026-10-13。状态：已经 kaya 侧评审一轮（7 点意见），本版为修正稿。

## 背景与问题

kaya 客户端已支持多模态输入，但内置模型入口（平台代理的 `/chat`）整条链路是纯文本设计：

- `model.ChatMessage.Content` 是 `string`（`internal/model/chat.go:11`），多模态数组在 JSON 绑定阶段就 400；
- 上行构造只产生纯文本；
- 模型目录有 `input_modalities` 字段（`migrations/024_inference_catalog.sql`），但所有模型实际值都是 `{text}`，且 `GET /chat/models` 不向客户端透出模态信息。

客户端自定义模型直连用户自己的上游、不经本服务，因此天然支持图片。本次目标：让内置模型入口在保持向后兼容的前提下支持图片输入。

## 范围

**做：** `/chat` kaya 链路端到端图片输入（legacy `ChatService` 与网关 `ChatGatewayFacade` 两条实现都改）；模型能力门控；`GET /chat/models` 透出 `input_modalities`。

**不做（本期明确排除）：** `/v1/*` 对外协议面（保持现有显式拒绝）；音频/文件等其他模态；服务器侧图片抓取/存储（`image_url` 原样透传，http(s) URL 由上游自己抓取，本服务不产生 SSRF 面）；网关 `openai_responses` 协议适配器的图片转换（见下文「上行 builder 清单」——它显式报错而非丢图）。

## 上行 builder 清单（穷举，评审意见 1）

消费 `[]model.ChatMessage` 并构造上行 payload 的 builder 共 **5 个**（已用 `grep '\[\]model.ChatMessage'` 全仓核实）：

| Builder | 文件 | 本期处理 |
|---|---|---|
| legacy OpenAI | `internal/llm/openai.go` `BuildOpenAIPayload` | 透传 parts 数组 |
| legacy Anthropic | `internal/llm/anthropic.go` `BuildAnthropicPayload` | 转换为 image block |
| 网关 openai_chat | `internal/inference/providers/openai_chat.go` `BuildPayload` | 透传 parts 数组 |
| 网关 anthropic | `internal/inference/providers/anthropic.go` `BuildPayload` | 转换为 image block |
| 网关 openai_responses | `internal/inference/providers/openai_responses.go` `BuildPayload` | **显式报错**（CodeInvalidInput），不转换——Responses 协议面本期不开放图片，报错响亮而非静默丢图 |

**防静默丢图机制**（核心安全属性）：

1. `ContentParts` 用 `json:"-"`、不加自定义 MarshalJSON，意味着任何未改造 builder 会把图片静默丢掉——为消除这个风险，实施分两步走：
   - **批次①**：5 个 builder 全部先加显式拒绝——检测到 `MessagesHaveImages` 即返回错误（llm 两个返回 `fmt.Errorf`；providers 三个返回 `domain.CodeInvalidInput`）。此时全链路 fail-closed：图片请求在任何一层都不可能被静默降级为纯文本。
   - **批次②**：4 个 chat builder 把显式拒绝替换为真正的透传/转换；openai_responses 永久保留显式拒绝（本期）。
2. **守门测试**：网关 adapter 注册表从 `cmd/server/main.go:419` 提取为 `providers.RegisteredAdapters()`（main.go 与测试共用同一来源）；守门测试遍历注册表里**每一个** adapter，喂含图请求，断言「payload 含图片数据」或「显式 CodeInvalidInput 报错」，二者必居其一。将来新增 adapter 注册进表而未处理图片时，测试自动失败。legacy 侧同款测试覆盖两个 llm builder。

## 消息表示

`model.ChatMessage` 增加 `ContentParts []ContentPart`，**`json:"-"`**：

- 自定义 `UnmarshalJSON`：`content` 为字符串 → 行为与今天完全一致；为数组 → 解析为 `[]ContentPart`（仅接受 `text` / `image_url`，其他 type 报错 → 400，不静默丢字段），同时把 text parts 拼接进 `Content` 字符串；`null` → 空串（assistant tool_calls 轮的合法形态）；
- **不实现自定义 `MarshalJSON`**：默认序列化只输出 string content，审计日志天然不含 base64 图片数据；尺寸校验与字节统计继续基于 `Content` 工作；
- 上行 payload 由 builder 显式调用共享转换函数（`internal/model` 提供，`llm` 与 `providers` 都依赖它，不重复实现）：
  - `ChatMessage.OpenAIWireContent() any`：无 parts 返回 string，否则返回 parts 数组；
  - `model.AnthropicImageBlock(url string) map[string]any`：data URI → base64 source（`image/jpg` 归一为 `image/jpeg`），http(s) URL → url source；
  - `model.ParseDataImageURI(u string) (mediaType, data string, ok bool)`；
  - `model.IsAllowedImageMediaType(mt string) bool`：`image/png|jpeg|jpg|gif|webp`（SVG 排除——可脚本化标记，不是模型眼里的图片）；
  - `model.SniffImageMediaType(prefix []byte) string`：magic bytes 嗅探（见下文）。

## 入口校验（handler/validateChatMessages，评审意见 2、6）

**限额口径：一律按 wire 字节计**（即请求里 `image_url` 值的 `len()`——data URI 的 base64 字符数或 URL 字符数）。这就是body上限看到的字节、也是校验点 `len()` 直接拿到的值，不存在"解码后膨胀超限"的窗口：base64 膨胀发生在客户端编码时，服务端限额本来就施加在膨胀后的形式上。

| 常量 | 值 | 含义 |
|---|---|---|
| `ChatMaxImages` | 4 | 每请求图片数上限 |
| `ChatMaxImageBytes` | 1 MiB | 单个 image_url 值的 wire 字节上限（base64 形态 ≈768 KiB 解码后） |
| `ChatMaxTotalImageBytes` | **3 MiB** | 每请求图片 wire 字节合计上限 |

合计定为 3 MiB（而非初稿 4 MiB）的原因：文本预算 4 MiB + 图片 3 MiB = 7 MiB，加 JSON 开销与 reasoning_content 余量后仍低于 8 MiB body 上限（`chatMaxBodyBytes`，且低于 nginx 10m），保证**校验产生的精确 400 永远先于 MaxBytesReader 的笼统 "invalid request body" 触发**。

规则：

- 图片 part 仅允许 `role=user`（OpenAI/Anthropic 的交集；system/tool 带图上游行为不一，入口统一拒绝好过歧义）；
- data URI 必须 `data:image/<允许类型>;base64,<非空数据>`；否则必须是合法 http(s) URL；
- **magic bytes 校验**（评审意见 6）：对 data URI 解码 base64 前 24 个字符（→18 字节，覆盖 PNG/JPEG/GIF/WEBP 全部签名，不解码整图、开销可忽略），用 `SniffImageMediaType` 嗅探；要求声明类型在白名单内**且**与嗅探结果一致（`jpg`≡`jpeg`），否则 400 `invalid image data`。http(s) URL 无法嗅探（本服务不抓取），依赖上游校验；
- `content` 必填判定放宽为 `Content == "" && len(ContentParts) == 0 && role != tool && 无 tool_calls`（纯图片消息合法）。

**纯图消息下游排查结论**（评审意见 6，已逐处核实）：`Content == ""` 的唯一拒绝点在 `validateChatMessages`（本次放宽）；网关 `gateway.Service.run` 对消息无空 content 检查（能力校验在边缘层，见 `service.go:333` 注释）；Anthropic builder 跳过空 content 的逻辑只在 system/assistant 分支，user 分支走 parts 不受影响；审计日志无"空 content 跳过"逻辑（`truncateChatInput`/`logAccess` 对空串正常记录）。

## 能力门控与缺省语义（评审意见 4）

**缺省一律 fail-closed：缺失/空 `input_modalities` = 纯文本模型。**

- legacy：`LLM_PROVIDERS_JSON` 模型条目新增可选 `input_modalities`，不配 = `["text"]`，`AcceptsImages()` 为 false。视觉模型必须由运营显式加字段才放行——失败方向永远是"视觉模型暂时拒绝图片"（安全），绝不会"文本模型误放行"。env JSON 是配置不是数据，**无需迁移/回填**；
- 网关：DB schema 默认 `'{text}'` 已就位，存量行天然 text-only；运营通过既有管理 API（`PUT /admin/inference/models`，`admin_models.go:159` 已支持该字段）设置后发布 catalog revision，**无需新迁移**；
- 检查点三处：
  1. legacy `ChatService.StreamChat`：Resolve + 订阅检查之后、构造上行 payload 之前 → `ErrChatModelNoVision`（新哨兵，400）；
  2. facade `streamChatModel`：快照在手，查 `snap.Model(modelID).InputModalities` → 同一哨兵（400 文案对齐 legacy）；
  3. 网关 `gateway.Service.run` 在 `modelSpeaks` 检查旁加防御性检查 → `CodeInvalidInput`（兜底所有现有/未来网关入口面，含 /v1 若将来放行数组）。facade 层拦截在前，正常路径客户端拿到的是 400 而非 502。
- handler `chatErrorMapping` 增加 `ErrChatModelNoVision` → 400。

## `/v1/*` 协议面不受影响（评审意见 5，已核实）

`/v1/*` 三个面**不绑定 `model.ChatRequest`**——各自有独立 wire 结构体：`chat_completions.go:76` 用 `json.RawMessage` 接 content 并在 `:172-176` 显式拒绝数组（"content arrays (multimodal parts) are not supported"）；`messages_client.go:349` 显式拒绝 image/document 块；`responses_client.go` 显式拒绝 input_image/input_file。`ChatMessage.UnmarshalJSON` 的改动对它们零影响。全仓唯一绑定 `model.ChatRequest` 的入口是 `handler/chat.go:102`。plan 仍加一条回归 pin 测试：`/v1/chat/completions` 对 content 数组继续返回该显式 400（而非绑定错误）。

## 上行转换

- OpenAI 系：无 parts 的消息按原 struct 序列化（逐字节不变，保护现有 e2e pin 测试）；有 parts 的改 map 形式，`content` 用 `OpenAIWireContent()`；
- Anthropic 系：user 分支改 parts 遍历（text → text block，image_url → `AnthropicImageBlock`）；相邻 user 轮合并、tool_result 前置等现有行为不变。

## 计费估算（评审意见 3）

`EstimateInputTokens` 的实际用途（已核实 `gateway/service.go:397/500` 与 `estimateUsage:1244`）：

1. **准入预扣**：作为 `quota.Admit` 的 `EstimatedInputTokens` 预占额度，真实 usage chunk 到达后按实结算、预占修正——这是主路径（适配器按设计 §7.1 永远显式请求 usage）；
2. **兜底估算**：仅当上游完全没给 usage 时，`estimateUsage` 用准入上界折算入账——罕见路径。

结论：它**不是直接扣配额**，预扣会在结算时修正。固定每图 +1105 token（OpenAI high-detail 上界）保留：按 Anthropic (w×h)/750 估算需要解码解析每张图片的尺寸，为准入预扣付出 CPU 与格式处理不成比例；且该估算的设计原则就是"安全上界"（高估在结算时被修正，方向安全）。兜底路径对带图请求偏高估——与现有文本 bytes/2 启发式同属保守方向，接受该偏差并在此记录。

## 模型列表透出

`ChatModelInfo`（legacy）与 `kayaModelEntry`（网关）都增加 `input_modalities []string`，空值归一 `["text"]`。kaya 据此对非视觉模型禁用图片入口。

## 错误语义

| 场景 | 状态码 | 文案 |
|---|---|---|
| content 数组含未知 part type | 400 | invalid request body（绑定失败） |
| 图片在非 user 消息 / 超数量 / 超大小 | 400 | 对应固定英文短语 |
| 非法 URL / MIME 不在白名单 / magic bytes 不一致 | 400 | invalid image_url / invalid image data |
| 模型不支持图片 | 400 | the selected model does not support image input |
| openai_responses 部署收到图片（防御层） | 502 | 上游协议不支持（正常不可达：gate 应先拦） |

## 实施节奏（评审意见 7）

分 3 批，每批结束跑 `go test ./...` 全量：

- **批次① 结构体 + 入口 + 门控（全链路 fail-closed）**：model 核心类型、handler 校验、5 个 builder 显式拒绝、三处门控。部署中间态安全：所有模型缺省 text-only，图片请求在门控被拒；即使绕过门控，builder 也显式报错——任何一层都不丢图。
- **批次② 上行透传 + 守门测试**：4 个 chat builder 实现透传/转换，openai_responses 保留显式拒绝，`RegisteredAdapters` 守门测试落地。
- **批次③ 透出 + 计费 + e2e + 文档**：模型列表透出、准入估算、e2e、运营文档。批次②完成后功能是暗的（目录未配模态），由运营配置点亮——干净的发布顺序。

## 测试策略

- 每个改动点配包内单测；守门测试防未来新增 adapter 漏处理；
- e2e：多模态请求透传断言 + 文本-only payload 逐字节回归 pin + `/v1` 显式拒绝回归 pin；
- 现有全部测试必须通过。
