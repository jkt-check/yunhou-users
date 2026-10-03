# 全仓 Code Review 收敛日志(2026-10-03)

任务:对 yunhou-users 做代码 review —— 先审 PR #50(feat/paddle-launch-followups)完整 diff,再按包扩到全仓;每轮「问题清单 + 修复 + 测试」,直到一轮完整扫描零新增问题且 `make test` 全绿。

收敛结论:**3 轮后达标**。R3(终轮)3 路独立 reviewer 对 R2 修复 diff 完整扫描,**零新增问题**;`make test` 28 包(-race -cover -p 1)与 `make e2e` 全绿。

## 轮次与产出

| 轮次 | 范围 | Findings | 修复 commit | 验证 |
|------|------|----------|-------------|------|
| R1 | PR #50 全部 diff | 6(1 Critical / 2 Important / 3 Minor) | `ca39d09` | make test 28 包全绿 |
| R2 | 全仓 12 路按包扫描 | 19(0 Critical / 6 Important / 13 Minor) | `08ad260`(58 文件,+2206/-238,每条带测试) | make test 28 包全绿 + make e2e 全绿 |
| R3 | R2 修复 diff 收敛 review(3 路) | **0 新增** | —(无需修复) | gofmt 无回归;构建/vet 干净 |

## R2 重要发现摘要(6 Important)

1. **自动续费双重订阅漏洞**(service):CreateOrder 的 `channelAutoRenews` 守卫只查 active 订阅,不挡 pending 订单 —— 两笔 paddle 单都支付后渠道侧产生两个自动续费订阅,前者无法本地取消、永续扣费。修法:同产品未过期 pending 单拒新单(409)。
2. **异 txn 失败事件翻转已 paid 订单**(service):Stripe/PayPal 以不同 txn id 投递失败事件会把已结算订单翻成不可自愈的 failed。修法:已 paid 订单不再插 pending 行 + 订单翻转仅限 wasPaid 级联。
3. **writeAudit 持 tx 连接申请第二池连接**(service):12 处「审计+返回」分支在 tx 未释放时向 MaxOpenConns=25 的池申请第二条连接,投递风暴下死锁。修法:auditAndCommit / 先显式回滚再审计。
4. **Alipay 验签偏离官方规范**(middleware):对 URL 解码后的值重新百分号编码再拼接,与支付宝签名原文逐字节不符,真实通知(必含 `.`/`-`/`:`/中文)启用即全量 400 —— 与 2026-07-23 微信事故同族。修法:按官方规范用解码值拼接,夹具改独立实现。
5. **Anthropic 平面钱包门控 500**(inference/httpapi):`CodeQuotaExceeded`/`CodeInsufficientBalance` 无 case 落 default 500,chat 平面同语义是 429。修法:补 429/409 case。
6. **worker 厂商调用无单次超时**(inference/workers):进程级 ctx + 无 body 读取限时,厂商 stall 永久冻结顺序 pass(探测/冷却恢复/租约清扫/OAuth 轮换全停)。修法:VendorTimeout 60s 子 ctx。

其余 13 Minor 见 commit `08ad260` message(升级退役门、终身订阅保持 NULL、dispute/续费乱序、PADDLE_ENV=live mock 门禁、catalog alias 非确定路由、0 价钱包 hold、月限额在途口径、流式 reasoning 项丢失等)。

## R3 终轮重点核对(零新增的证据面)

- pending 单守卫边界(异渠道 pending 也挡,测试 pin;无唯一索引兜底与 active-sub 预检同款性质,已记录为可接受残留)
- 12 处 audit 事务语义逐点核对:无任何分支把该回滚的提交了、或把该落库的审计随回滚丢掉
- Alipay canonical 与 body 编码夹具逐字节闭合(`%20`/`+`/中文三路径解码后殊途同归)
- catalog alias 校验双侧(发布闸 + ParseSnapshot)一致;存量坏修订 fail-closed 行为评估为可接受(SnapshotCache fallback,进程不死)
- 月限额 spent+held 口径无双重计数窗口(settle 同事务内 held→consume)
- responses_client 双集合三处 append 点全部正确;VendorTimeout 子 ctx 5 处全部及时 cancel

## 测试证据

- `make test`(`go test -race -cover -p 1 ./internal/...`):**28 包全 ok**(billing/paddle 35.8% … middleware 95.3%),commit `08ad260` 后跑一次通过。
- `make e2e`(`go test -race -count=1 ./tests/e2e/`):第一次运行出现 1 例偶发失败(输出被 tail 截断未能定位),随后**连续两轮全绿**(127.5s / 128.6s);判定为既有 flake,非本次修复引入。
- gofmt:本次 diff 无新增违规(HEAD 即存在的未格式化文件未触碰);R3 标记的两处新引入违规已 gofmt 修复。

## 已知残留(非 defect,记录在案)

- pending 单守卫是应用层最佳努力,无 partial unique index 兜底(active-sub 有 027 索引,pending 没有);纯并发双 CreateOrder 存在 TOCTOU 窗口,与既有预检同款性质。
- ExtendMembershipSub 终身行并发改写极端 race 下响应回显 `0001-01-01` 零值(DB 正确,仅回显怪异)。
- 若生产当前 active catalog 修订已存在跨 model 重复 alias,升级后冷启动将无法服务目录,需先发布干净修订(fail-closed 设计意图)。
- e2e 套件存在 1 例未定位的偶发失败(3 跑 2 绿),建议后续独立排查。
