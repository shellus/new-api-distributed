# 分布式边缘开发实施计划

## 目标

基于官方 New API 主分支维护一个派生仓库，在同一 Go module 中构建 master 和 edge。master 保持 New API 默认行为并增加控制面；edge 复用相同的 relay、鉴权、渠道、计费和数据访问代码，只增加边缘运行模式、同步、租约、可靠 outbox 和心跳业务。

实施前必须先阅读 [distributed-design-context.md](./distributed-design-context.md) 和已接受的 [架构决策记录](./adr/)，避免为局部实现便利重新引入已否定方案。

基线提交：`a63364d156cf2a64f1c3d1ee4923d73d5f3222a1`。

## 架构决定

- 只维护一个 Git 仓库，不复制 master 和 edge 的 `.go` 文件。
- 上游默认 `main.go` 继续构建 master，减少后续合并冲突。
- 新增 `cmd/newapi-edge` 作为 edge 编译入口。
- 公共启动流程提取为可复用 application package，两个入口通过运行模式选择路由和后台任务。
- edge 首期通过运行模式关闭无关能力，不大量删除上游源码；稳定后再评估编译级裁剪。
- 通信概览以 [distributed-architecture.md](./distributed-architecture.md) 为准。
- 主从配置事实来源与节点地址语义以 [distributed-configuration.md](./distributed-configuration.md) 为准。

## 当前实现状态

第一至第五阶段已经落地并完成隔离链路验收：

- 默认 master 入口和 `cmd/newapi-edge` 共用 `internal/app`，分布式 master 路由受 `EDGE_DISTRIBUTED_ENABLED` 控制。
- v1 控制契约、Ed25519 请求认证、持久化防重放 receipt、签名快照、节点管理、lease、heartbeat 和 settlement 已实现。
- edge 使用独立 SQLite 完成本地 token 指纹鉴权、模型与渠道投影、请求 admission、lease reservation、计费和 outbox。
- CPA 探测、快照切换读写锁、快照与价格版本固定、graceful shutdown、最终 drain 和后台循环取消已接入 application 生命周期。
- 请求完成后先持久化 staged settlement，再原子完成 lease、usage event 和 edge outbox；结算失败会关闭 accounting readiness，并由维护循环恢复。
- master 通过权威结算事务和持久化 outbox，把事件 exactly-once 投影到 consume log；启用统计时，`quota_data` 也使用独立事件标记避免重复累加。
- 签名免费策略使用零额度 lease，零余额用户仍必须经过 lease、usage event 和结算序列。

真实 CPA 进程、节点凭证和本地凭证加载链路已经验证；测试凭证对应的上游账户在验收时返回用量上限错误，因此成功响应、流式结束和精确 usage 使用同一 CPA HTTP 契约的隔离确定性实例完成。该外部账户状态不影响 master、edge、CPA 边界和账务一致性的验收结论。

## 复用目标

目标是至少 80% 复用现有实现，新增代码只表达分布式业务。

| 现有能力 | 复用方式 |
|------|------|
| relay 与 provider adaptor | master 和 edge 直接 import 同一 package |
| OpenAI、Responses、Claude、Gemini 和流式处理 | 不在 edge package 中重新实现 |
| token 鉴权、分组和模型权限 | edge 改为使用本地同步快照，保持现有上下文语义 |
| 渠道选择、模型映射、参数覆盖和系统提示词 | 继续使用现有 channel/relay 代码 |
| `billingexpr`、倍率和额度安全边界 | master 与 edge 使用同一 package 和同一价格版本 |
| `FundingSource`、`BillingSession` | 新增 `EdgeLeaseFunding`，不复制预扣、结算和退款逻辑 |
| GORM、迁移和 SQLite | edge 本地状态继续使用现有数据库基础设施 |
| 消费日志和统计 | master 结算后通过 durable outbox 和 billing event key 写入现有日志与 `quota_data` |
| CPA | 保留 OAuth 凭证池、凭证调度、重试和上游执行职责 |

## 仓库与编译入口

计划结构如下：

```text
main.go                         # 上游默认入口，构建 master
cmd/newapi-edge/main.go         # 新增 edge 编译入口
internal/app/                   # 两个入口复用的初始化、HTTP 生命周期和关闭流程
service/edge/                   # 快照、租约、结算、outbox、心跳业务
model/edge_*.go                 # master/edge 所需的持久化模型
dto/edge_*.go                   # 共享通信协议结构
```

前端静态资源继续由默认入口持有。edge 入口不需要管理前端资源，只注册数据面、健康检查和必要的只读路由。

## Master 增量业务

- 管理节点身份、状态和协议版本。
- 接受已认证 edge 声明的公开访问地址；公网主动探测作为后续独立观测能力，不能修改声明或参与 v1 readiness。
- 编译并签发令牌鉴权索引和业务策略快照。
- 从钱包或订阅中事务型预留配额并签发 lease。
- 对签名免费策略签发不占用钱包或订阅的零额度 lease，但保留完整审计与结算语义。
- 接收、复核并幂等写入 usage block。
- 通过 durable outbox 和全局 billing event key，在现有消费日志和 `quota_data` 统计链路中 exactly-once 登记实际使用。
- 正常关闭 lease 后退还已确认未使用额度。

master 不实现用户请求转发、AI 协议解析或 CPA 调度。

## Edge 增量业务

- 使用本地 SQLite 保存鉴权快照、策略、lease、reservation 和 outbox。
- 本地完成所有令牌鉴权；首次请求不会为了鉴权访问 master。
- 使用 `EdgeLeaseFunding` 接入现有 `BillingSession`。
- 在访问 CPA 前原子预占额度，避免并发请求超出 lease。
- 请求完成后先持久化精确 staged settlement，再原子更新 lease 并写入 usage event/outbox。
- 本地结算失败时立即关闭 accounting readiness；只从 durable staged payload 恢复，不允许把已 staged 的请求退款。
- 后台完成增量同步、租约补充、结算区块上报和心跳。
- 所有 edge 后台循环由 application 生命周期持有可取消的 context，不能使用无法停止的永久 goroutine。
- 以本地部署配置为事实来源声明公开访问地址；地址变化后自动随控制通信更新 master。
- master 失联时，在快照和 lease 有效范围内继续处理请求。
- 至少一个 CPA 健康且声明了可用模型时数据面才 ready；CPA 全部不可用时在鉴权和 relay 前 fail closed，恢复后原子重新启用渠道。

edge 不实现协议适配、流式转发、OAuth 凭证调度或第二套计费引擎。

## 实施阶段

### 第一阶段：共享运行入口（已实现）

1. 把 `main.go` 中可复用的初始化和 HTTP 生命周期提取到 application package。
2. 保持默认 master 构建和启动行为不变。
3. 增加 edge 编译入口和运行模式，暂不接入真实同步业务。
4. 验证两个入口可以独立编译，master 原有测试不回归。

### 第二阶段：共享通信契约（已实现）

1. 定义快照、lease、结算区块和确认游标的版本化 DTO。
2. 定义节点身份认证、签名、幂等和错误语义。
3. 增加 master/edge 共同使用的协议兼容测试。

### 第三阶段：Master 控制面（已实现）

1. 增加节点、策略版本、lease 和结算记录模型。
2. 实现 bootstrap、增量同步、租约签发、结算和心跳接口。
3. 复用钱包、订阅和 `billingexpr` 完成预留与复核。
4. 验证并发节点不能重复使用同一余额。
5. 使用 durable consume-log outbox，把权威 usage exactly-once 投影到日志和 `quota_data`。

### 第四阶段：Edge 本地数据面（已实现）

1. 增加 SQLite 快照、lease、reservation 和 outbox 模型。
2. 接入完整令牌鉴权索引和本地权限判断。
3. 实现 `EdgeLeaseFunding` 并接入 `BillingSession`。
4. 实现 staged settlement、原子结算、断点续传、租约补充和心跳。
5. 限制 edge 暴露的路由和后台任务。
6. 实现有序关闭：停止接收新请求和创建 reservation，等待在途请求，尽力发送 outbox 与关闭 lease，停止后台循环，最后关闭本地数据库；HTTP 优雅关闭超时后必须强制关闭连接，不能让活跃 handler 与数据库关闭并发发生。
7. 把 CPA 健康与 accounting readiness 纳入 `/readyz`，快照切换、CPA 故障和结算异常期间统一 fail closed。

### 第五阶段：真实链路验证（已完成）

1. 启动隔离的 master、edge 和 CPA，确认只放行 Chat/Responses 文本请求，并覆盖非流式和流式响应。
2. 验证 master 增加控制面延迟不会进入已有 lease 的用户请求路径。
3. 验证 master 断开时，已有有效快照和 lease 继续服务；需要新 lease 的请求必须在访问 CPA 前失败。
4. 验证正额度 lease 耗尽、零额度免费 lease、实际用量高于预扣和租约续租边界。
5. 验证 CPA 全部不可用时 `/readyz` 失败且渠道被禁用，CPA 恢复后 readiness 与渠道原子恢复。
6. 验证 staged settlement 在进程重启后恢复，恢复完成前 accounting readiness 保持关闭。
7. 验证重复、乱序和重放区块不会重复扣费、重复写 consume log 或重复累加 `quota_data`。
8. 验证优雅关闭停止 admission、等待在途请求、上传剩余 outbox、关闭可关闭 lease，并在 drain 失败时保留 SQLite 状态。

2026-07-16 隔离验收记录：

- Chat Completions 和 Responses 的非流式、流式请求均通过 edge 到达 CPA；流式响应正常结束并携带 usage。
- CPA 停止后 `/readyz` 和用户请求同步 fail closed，且失败请求没有到达 CPA；CPA 恢复后 readiness 和渠道恢复。
- master 停止后，edge 在已有快照和 lease 内继续完成 10 个请求；额度不足且无法补租时，下一个请求在访问 CPA 前返回 503。master 恢复后待发送事件全部结算，并可继续取得新 lease。
- edge 重启后从原 SQLite 恢复并继续服务；优雅关闭后全部 lease 均关闭，全部 reservation 均为 settled，edge outbox 全部 acked。
- 最终共 16 个 usage event，权威扣费 2208，prompt/completion token 分别为 80/48；master consume log、`quota_data` 事件和请求统计均精确为 16，`quota_data` 累计额度为 2208。
- 同一结算区块重放得到原确认结果；usage、用户/令牌额度、consume log、`quota_data` 和请求统计均未重复变化。
- 快照截断 lease 的维护循环连续观察期间没有重复 acquire/close；CPA 故障、master 离线、结算重放、edge 重启和优雅关闭均保留账务守恒。
- 零额度免费 lease、实际用量高于预扣、staged settlement 重启恢复、乱序/冲突区块回滚和孤儿 reservation fail-closed 由确定性数据库回归测试覆盖。
- 真实 CPA 使用隔离凭证完成加载、模型发现和上游请求转发；上游账户返回 429 用量上限错误。成功响应的四种协议模式由隔离确定性 CPA 实例验证，不在仓库中保存凭证、地址或本机路径。

## 首期边界

- 图片、视频、异步任务和 Realtime 在完成独立计费验证前不开放。
- Chat/Responses 中的图片、音频、视频、截图、内置搜索等无法由 v1 usage 与价格策略完整表达的输入同样在访问 CPA 前拒绝。
- CPA 只允许 edge 通过本机或容器私网访问。
- token 撤销延迟由增量同步频率、快照有效期和剩余 lease 共同限制。
- 无本地 lease 且 master 不可用时，请求 fail closed。
- 无健康且声明了可用模型的 CPA，或 accounting readiness 关闭时，请求 fail closed。
- 节点由同一运营方控制；签名和认证主要防止伪造、重放和配置错误，不尝试防御恶意节点运营者。
- master 不审批 edge 的公开访问地址。节点凭证是信任边界；未来的公网地址探测只形成观测，不能修改声明或参与 edge 本地 readiness。
- edge v1 只执行倍率和固定价格；tiered expression 模型在进入 relay 前拒绝。

## 风险控制

| 风险 | 控制措施 |
|------|------|
| 形成第二套网关 | 协议、relay、计费和鉴权上下文直接复用同仓库 package |
| 上游合并困难 | 默认入口保持兼容，新增业务集中在 edge package 和少量启动分支 |
| 多节点超卖 | master 签发 lease 时事务型预留额度 |
| edge 本地结算失败丢账 | 精确 usage 先 staged；lease、usage event 和 outbox 再原子提交；失败时 accounting fail closed 并从 staged payload 恢复 |
| edge 硬中断留下不确定 reservation | 不自动退款或关闭相关 lease；保留 SQLite 供恢复，并把对应故障窗口纳入真实 E2E |
| 重复结算 | 节点代次、连续序号、持久化请求 receipt 和 master 唯一约束 |
| 日志或统计重复投影 | durable master outbox、全局 billing event key、consume log 唯一键和 `quota_data` 事件标记 |
| 首次请求依赖 master 鉴权 | 全量同步安全令牌指纹，鉴权始终本地完成 |
| master 失联停服 | 用户请求不访问 master，已有 lease 内继续工作 |
| CPA 故障仍接流量 | `HEAD /healthz` 探测、渠道原子禁用和 `/readyz` fail closed |
| 快照切换与在途请求混用策略 | 数据面策略读写锁；reservation 固定 lease、快照和价格版本，跨快照重试被拒绝 |

## 验证

所有合并候选至少执行：

```bash
git diff --check
go mod tidy -diff
go test ./...
go test -race ./model ./service/edge ./service ./middleware ./controller ./router ./internal/app
go vet ./...
go build .
go build ./cmd/newapi-edge
(cd web/default && bun run build)
(cd web/classic && bun run build)
```

真实链路验收必须使用隔离数据库、临时环境变量和仅供测试的 CPA 凭证。结果至少记录 HTTP 状态、流式是否正常结束、CPA 是否被实际访问、edge lease/reservation/outbox 状态、master usage/lease/outbox 状态，以及 consume log 和 `quota_data` 的前后计数。凭证、真实地址和本机路径不进入项目文档或测试输出。

通过条件：

- 默认 master 行为与上游基线一致。
- master 和 edge 共用协议、计费与 relay package，没有复制实现。
- 首次请求可以使用本地 token 指纹完成鉴权。
- 无 lease 的请求不会访问 CPA。
- master 延迟或暂时失联不进入正常用户请求路径。
- 免费请求使用零额度 lease，不因用户钱包为零而偏离 master 计费语义。
- CPA 全部不可用、快照过期或 accounting 未恢复时，`/readyz` 和 admission 同时 fail closed。
- 已 staged 的请求不能退款；恢复后只生成一条 usage event 和一条 edge outbox。
- 重复结算最多产生一次权威扣费、一次 consume log 和一次 `quota_data` 累加。
- edge 关闭期间不再创建 reservation，已完成请求的 usage 在数据库关闭前进入 durable outbox。

当前状态：五个阶段均已完成。真实链路、故障边界、重放幂等、重启恢复和账务数据库核对结果已形成上述验收记录；后续变更仍须重新执行本节合并门和与变更相关的故障注入测试。

## 回滚

默认 master 入口必须始终能够在关闭分布式功能后按上游方式运行。edge 停止接收新请求后先发送剩余 outbox 并关闭 lease；master 未确认前不删除 edge 本地状态或释放未结算额度。
