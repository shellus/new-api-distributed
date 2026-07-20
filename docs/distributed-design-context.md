# 分布式边缘设计背景与共识

## 为什么建设这套能力

当前主要流量集中在少量 Pro20x 渠道，上游由可重复部署的 CPA 和 GPT OAuth 凭证提供。中心服务器的上下行带宽成为扩展瓶颈，因此需要把用户请求流量分散到任意数量的轻量节点，同时继续由中心 New API 维护计费和统计。

节点可能跨国家部署，master 与 edge 之间存在约 100–200ms 延迟，也可能间歇性失联。正常用户请求不能依赖一次或多次跨国控制面调用，否则节点延迟和可用性将重新受 master 限制。

本项目的目标是交付分布式业务，不是重新开发 AI 网关、协议转换器、OAuth 调度器或分布式数据库。设计质量的重要判断标准是至少 80% 复用成熟的 New API 与 CPA 能力。

## 已确认的系统边界

- New API 继续负责用户鉴权、权限、渠道策略、请求处理和计费语义。
- CPA 继续负责 OAuth 凭证、凭证调度、重试和上游执行。
- master 是用户、token、策略、配额和账务的唯一权威。
- edge 是由同一运营方控制的可信数据面，不是低信任第三方节点。
- 用户请求通常不访问 master；控制面延迟不得叠加到正常 AI 请求。
- edge 在本地持久化未确认账务，网络恢复后继续上报。
- master 不与 edge 共享 PostgreSQL、MySQL、SQLite 或 Redis。

## 方案选择过程

### 否定现有 New API slave/共享数据库集群

共享数据库或 Redis 的 slave 更适合同机房集群。跨国家部署时，每个请求会产生远程状态访问；master 短暂失联也可能让 edge 无法服务。这与离线工作和低延迟目标冲突。

### 否定自写独立 AI 网关

透明代理最初看似简单，但权限、渠道策略、系统提示词、模型映射、计费、流式错误和协议边界会逐步形成一套新的 New API。长期维护成本和语义漂移风险过高。

### 否定由 CPA 插件独立承担 edge 账务

CPA 能够处理协议、OAuth 调度和 usage 观察，但其插件 usage 链路不是财务级持久账本。把完整 edge 业务放入插件还会重复实现 New API 的用户、策略和计费语义。CPA 插件可以用于观测，不作为唯一账务来源。

### 选择同仓库的 New API Master 与 Edge

master 与 edge 基于同一个官方 New API 派生仓库。上游默认入口继续构建 master，edge 只增加编译入口和少量边缘业务。协议、relay、鉴权上下文、渠道逻辑、计费表达式和 BillingSession 直接复用同一 Go package。

### 选择余额复制、本地预留与异步结算

master 向 edge 复制带 revision 的钱包、订阅和 token 余额。edge 在本地余额 overlay 上原子预留，完成请求后把 durable usage event 组成 settlement block 异步上报；新请求必须在预占后保持有限账户非负，已执行请求的实际费用超过预扣时使用独立 Settlement Floor 完成落账。多节点窗口期风险继续由余额 revision 和 settlement circuit 约束。该决定见 ADR 0004 与 ADR 0006。

### 选择完整共享数据面

master 与 edge 使用同一套路由注册、relay、provider adaptor 和计费计算器。edge 不维护请求体形状白名单，也不在遇到新格式时回源 master；Responses 压缩、图片、音频、embeddings、rerank、Claude、Gemini、Realtime、视频和异步任务都在 edge 本地执行。该决定见 ADR 0005。

## 共识结论列表

1. 只维护一个 Git 仓库和一个 Go module，不复制 master/edge 代码。
2. 上游默认 `main.go` 保持 master 行为；edge 是附加编译入口。
3. edge 通过运行模式关闭管理后台等控制面能力，但共享 master 已实现的完整用户数据面。
4. 正常用户请求不进行同步 master 调用。
5. edge 必须拥有完整的安全令牌鉴权索引，首次请求鉴权在本地完成。
6. 用户请求只使用本地余额投影和 reservation；master 不可用时不得为单个请求同步申请额度。
7. master 使用每节点余额 revision、零余额 admission 和 settlement circuit 控制多节点离线消费风险；Settlement Floor 只处理已经发生的真实消费。
8. edge 使用 New API 的原生计费逻辑计算本地使用，master 按相同价格版本和 Billing Receipt 复核。
9. usage event、reservation 结算和 outbox 必须使用可靠本地事务，不能依赖进程内队列。
10. edge SQLite 是可恢复的自动投影和账务状态，不是人工配置入口。
11. 渠道由 master 创建并保持全局 channel ID；edge 自动生成同一业务渠道的本地投影。
12. master 默认下发所有逻辑渠道；edge 按渠道名合并本地 YAML 中的 URL、凭证、代理和请求覆盖，不维护逐节点 channel ID 映射。
13. CPA 不暴露为可绕过 edge 鉴权和计费的公网入口。
14. edge 的公开访问地址由 edge 本地配置并使用节点凭证声明。
15. master 直接信任已认证 edge 的地址声明，不增加人工审批。
16. master 主动探测公开地址只产生可达性和延迟观测，不改变地址真值。
17. master 主要维护节点身份、名称、凭证、启停和额度风险；地址、能力和运行状态由 edge 上报。
18. edge 执行能够由 master 快照表达并形成 Billing Receipt 的完整用户数据面；未配置本地凭证的渠道在该节点保持禁用。

## 明确的非目标

- 不建设跨地域共享数据库集群。
- 不让 master 转发 edge 的用户流量。
- 不重新实现 AI 协议和 CPA 凭证调度。
- 不同步 CPA OAuth 凭证到 master。
- 不在 edge 后台提供用户、渠道、计费或余额编辑能力。
- 不以审计服务或 CPA usage 插件作为唯一账本。
- 不为 edge 复制第二套路由、协议转换或计费实现。

## 有意留给实现阶段决定的内容

以下事项尚未形成不可变共识，实施者应通过测试和最小设计确定，不应把个人偏好写成既定架构：

- 通信 DTO 的具体字段、编码和分页形式。
- 节点身份使用的具体密钥算法和注册流程。
- token 安全指纹的具体算法与索引结构。
- Settlement Floor、结算滑动窗口和 circuit 恢复阈值。
- 快照同步、心跳和 settlement block 的具体时间或数量阈值。
- edge 本地事务的表结构和清理周期。
- master 节点列表的具体 UI 和客户端选点方式。

这些实现决定必须服从已确认边界：不增加逐请求 master 依赖、不复制成熟业务逻辑、不削弱幂等和账务持久性。

## 文档阅读顺序

新实现会话按以下顺序建立上下文：

1. [领域词汇表](../CONTEXT.md)
2. 本设计背景与共识
3. [架构决策记录](./adr/)
4. [分布式边缘架构概览](./distributed-architecture.md)
5. [主从节点配置设计](./distributed-configuration.md)
6. [开发实施计划](./distributed-implementation-plan.md)
