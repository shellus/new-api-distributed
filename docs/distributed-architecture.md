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
- 配额租约：master 提前划给指定 edge、用户和令牌的一小部分可用额度。
- 控制变化：令牌撤销、策略更新、节点停用和配置版本变化。

edge 不接收明文 API token、中心数据库、其他节点数据或 CPA OAuth 凭证。CPA 凭证由节点本机管理。

用户第一次访问某个 edge 时，鉴权仍在本地完成，不需要查询 master。若该用户在此节点尚无可用租约，edge 才向 master 申请小额初始租约；master 不可用时，该用户的请求在访问 CPA 前被拒绝。

签名计费策略明确免费时，edge 仍申请与用户、token 和快照绑定的零额度 lease。该 lease 不预留钱包或订阅额度，但保留完整 admission、usage event、结算序列和审计语义。

## 2. Edge 向 Master 上报的内容

主要包括：

- 使用结算区块：批量上报已经完成的请求消费，由 master 生成正式账单和统计。
- 节点声明与运行状态：公开访问地址、健康度、负载、延迟、可用模型、CPA 状态和程序版本。
- 同步状态：当前策略版本、租约余量、上报进度和是否需要补充额度。

结算数据先可靠保存在 edge 本地，再异步上报。请求完成后先持久化精确 staged settlement，再原子更新 lease、完成 reservation 并写入 usage event/outbox。网络中断后继续积压，恢复连接后从已确认位置续传；重复发送同一批数据不会重复计费。

master 在权威结算事务中写入 usage event 和 consume-log outbox。日志与 `quota_data` 使用同一全局 billing event key 做 exactly-once 投影，因此 outbox 重试、master 崩溃和重复区块都不能重复登记消费。

## 3. Master 与 Edge 的通信时间点

1. edge 首次启动或重新注册时，获取完整鉴权索引、策略快照和初始运行配置。
2. 正常运行期间，edge 定期获取增量变化；没有变化时只交换少量版本信息。
3. 某个用户首次在该节点使用但没有租约时，edge 申请小额初始租约。
4. 已有租约降到补充阈值时，edge 提前申请续租，不等待完全耗尽。
5. 消费记录达到时间或数量阈值时，edge 组成结算区块批量上报。
6. edge 定期发送心跳，用于展示节点健康度和负载。
7. 网络恢复时，edge 先续传未确认结算，再同步策略和补充租约。
8. edge 正常关闭时，尽量上报剩余结算并归还未使用租约。

用户请求通常不触发 master 通信。唯一例外是本地鉴权成功但该用户没有可用租约；此时只申请租约，不把 AI 请求转发给 master。

## 4. Edge 数据面 readiness

edge 的 `/healthz` 只表示进程存活；`/readyz` 同时要求：

- 应用仍接受新请求。
- 已验证签名的快照未过期。
- 至少一个本地 CPA 的 `HEAD /healthz` 成功且快照声明了可用模型。
- 本地 accounting 可写，且没有待恢复的 staged settlement。

应用新快照时，edge 先关闭数据面，在策略写锁内原子替换投影并探测已配置的本地上游，随后才恢复 readiness。请求完成 lease reservation 后固定快照和价格版本，重试不能跨快照采用新的路由或价格。

已完成上游请求但本地结算失败时，accounting readiness 立即关闭。维护循环只从 durable staged payload 恢复；已 staged 的 reservation 不能退款。启动时发现 active 但未 staged 的孤儿 reservation 也会永久关闭 readiness，保留现场等待人工核查，重启不自动退款或解除阻断。正常关闭会先停止 admission、等待在途 handler 完成，再停止后台循环和关闭数据库。

## 边界原则

- master 不代理 edge 的用户请求。
- edge 不自行实现 AI 协议、流式转发或 OAuth 凭证调度。
- New API 继续负责鉴权、权限、渠道策略和计费。
- CPA 继续负责 OAuth 凭证池、上游调度、重试和执行。
- master 失联时，edge 在已有有效快照和租约范围内继续服务。
- edge 使用自己的节点凭证声明公开访问地址，master 直接接受该可信声明。master 的主动探测只形成可达性和延迟状态，不承担地址审批。
- 当前 v1 master 不以公网主动探测决定 edge readiness；readiness 由 edge 本地签名快照、CPA 和 accounting 状态决定。

主从节点配置的事实来源、公开地址和 CPA 内部地址约定见 [distributed-configuration.md](./distributed-configuration.md)。
