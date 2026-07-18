# 分布式边缘架构概览

## 目标

中心 New API 负责用户、令牌、策略和账务真值，边缘 New API 负责就近处理用户请求。本架构不复制中心数据库，也不要求用户请求实时访问中心。

本架构的设计动机、被否定方案和不可变共识见 [distributed-design-context.md](./distributed-design-context.md)。

master 与 edge 使用同一代码仓库和同一套 New API 业务代码。master 使用上游默认入口，edge 使用额外的轻量编译入口；CPA 继续作为 edge 的本地上游执行引擎。

## 1. Edge 从 Master 获取的内容

edge 获取的是“能够在本地完成请求所需的最小业务快照”，不是中心数据库副本。

主要包括：

- 完整的令牌鉴权索引：覆盖允许访问 edge 的有效令牌安全指纹，以及鉴权所需的用户、分组、模型权限和启停状态。
- 业务策略：模型、分组、渠道行为和计费规则的版本化快照。
- 余额向量：钱包、订阅和 token 的权威余额副本，以及每节点 revision 与结算回冲水位。
- 控制变化：令牌撤销、策略更新、节点停用和配置版本变化。

edge 不接收明文 API token、中心数据库、其他节点数据或 CPA OAuth 凭证。CPA 凭证由节点本机管理。

用户第一次访问某个 edge 时，鉴权和余额 admission 都在本地完成，不需要查询 master。第一次 `edge-control.v2` heartbeat 必须完成余额 full 初始化；之后 heartbeat 只下发相对已确认 revision 的 delta，断档时重新 full。master 不可用时，edge 继续在最后一份已验证策略、confirmed balance 和本地负余额下限内服务。

签名计费策略明确免费时，edge 仍创建零额度 reservation 和 usage event，保留完整结算序列与审计语义，但不扣减有限账户。

## 2. Edge 向 Master 上报的内容

主要包括：

- 使用结算区块：批量上报已经完成的请求消费，由 master 生成正式账单和统计。
- 节点声明与运行状态：公开访问地址、健康度、负载、延迟、可用模型、CPA 状态和程序版本。
- 同步状态：当前策略版本、余额 revision、结算水位和 circuit epoch。

结算数据先可靠保存在 edge 本地，再异步上报。请求完成后先持久化精确 staged settlement，再原子更新余额 overlay、完成 reservation 并写入 usage event/outbox。网络中断后继续积压，恢复连接后从已确认位置续传；重复发送同一批数据不会重复计费。

master 在权威结算事务中重新计算 charge，按事件携带的资金来源 exactly-once 扣减钱包或订阅及有限 token，再写入 usage event 和 consume-log outbox。日志与 `quota_data` 使用同一全局 billing event key 做 exactly-once 投影，因此 outbox 重试、master 崩溃和重复区块都不能重复登记消费。

master 还按节点代次和 usage event 完成时间检查滑动窗口。超限时整个区块拒收，只提交节点 settlement circuit 标记与拒绝 receipt；账务、usage、水位和日志 outbox 都不提交。edge 收到 circuit 后停止新 admission、继续 heartbeat 并保留 durable outbox。管理员核账并清除 circuit 后，edge 使用新的 HTTP request ID 重试同一 block，链式摘要和事件内容不变。

## 3. Master 与 Edge 的通信时间点

1. edge 首次启动或重新注册时，获取完整鉴权索引、策略快照、余额 full dataset 和初始运行配置。
2. 正常运行期间，edge 定期获取策略变化，并在 heartbeat 中确认或接收余额 delta；没有变化时只交换版本和水位。
3. 消费记录达到时间或数量阈值时，edge 组成结算区块批量上报。
4. edge 定期发送心跳，用于余额复制、circuit 状态和节点运行观测。
5. 网络恢复时，edge 续传未确认结算并恢复策略、余额 revision 收敛。
6. edge 正常关闭时，停止 admission、等待在途账务完成并尽量上报剩余结算。

用户请求不触发 master 通信。余额未初始化、本地有限账户达到负下限或 settlement circuit 打开时，请求在访问 CPA 前失败。

## 4. Edge 数据面 readiness

edge 的 `/healthz` 只表示进程存活；`/readyz` 同时要求：

- 应用仍接受新请求。
- 已存在最后一份验证并应用的策略快照；TTL 过期只冻结策略变化。
- 余额副本已经完成 full 初始化。
- 本地 accounting 可写，没有待恢复的 staged settlement，且 settlement circuit 未打开。

应用新快照时，edge 先关闭数据面，在策略写锁内原子替换投影，随后才恢复 readiness。请求完成余额 reservation 后固定资金来源、快照、价格和 balance revision，重试不能跨快照采用新的路由或价格。

已完成上游请求但本地结算失败时，accounting readiness 立即关闭。维护循环只从 durable staged payload 恢复；已 staged 的 reservation 不能退款。启动时发现 active 但未 staged 的孤儿 reservation 也会永久关闭 readiness，保留现场等待人工核查，重启不自动退款或解除阻断。正常关闭会先停止 admission、等待在途 handler 完成，再停止后台循环和关闭数据库。

## 边界原则

- master 不代理 edge 的用户请求。
- edge 不自行实现 AI 协议、流式转发或 OAuth 凭证调度。
- New API 继续负责鉴权、权限、渠道策略和计费。
- CPA 继续负责 OAuth 凭证池、上游调度、重试和执行。
- master 失联时，edge 在最后一份已验证策略、余额副本和本地负余额下限内继续服务。
- edge 使用自己的节点凭证声明公开访问地址，master 直接接受该可信声明。master 的主动探测只形成可达性和延迟状态，不承担地址审批。
- master 不以公网主动探测决定 edge readiness；readiness 由 edge 本地策略、余额、accounting 和 settlement circuit 状态决定。

主从节点配置的事实来源、公开地址和 CPA 内部地址约定见 [distributed-configuration.md](./distributed-configuration.md)。
