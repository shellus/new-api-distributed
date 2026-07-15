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
| 消费日志和统计 | master 结算后写入现有日志和统计链路 |
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
- 接受已认证 edge 声明的公开访问地址，并单独记录主动探测得到的可达性和延迟。
- 编译并签发令牌鉴权索引和业务策略快照。
- 从钱包或订阅中事务型预留配额并签发 lease。
- 接收、复核并幂等写入 usage block。
- 在现有消费日志和统计链路中登记实际使用。
- 正常关闭 lease 后退还已确认未使用额度。

master 不实现用户请求转发、AI 协议解析或 CPA 调度。

## Edge 增量业务

- 使用本地 SQLite 保存鉴权快照、策略、lease、reservation 和 outbox。
- 本地完成所有令牌鉴权；首次请求不会为了鉴权访问 master。
- 使用 `EdgeLeaseFunding` 接入现有 `BillingSession`。
- 在访问 CPA 前原子预占额度，避免并发请求超出 lease。
- 在请求结算时原子更新 lease 并写入 usage event/outbox。
- 后台完成增量同步、租约补充、结算区块上报和心跳。
- 以本地部署配置为事实来源声明公开访问地址；地址变化后自动随控制通信更新 master。
- master 失联时，在快照和 lease 有效范围内继续处理请求。

edge 不实现协议适配、流式转发、OAuth 凭证调度或第二套计费引擎。

## 实施阶段

### 第一阶段：共享运行入口

1. 把 `main.go` 中可复用的初始化和 HTTP 生命周期提取到 application package。
2. 保持默认 master 构建和启动行为不变。
3. 增加 edge 编译入口和运行模式，暂不接入真实同步业务。
4. 验证两个入口可以独立编译，master 原有测试不回归。

### 第二阶段：共享通信契约

1. 定义快照、lease、结算区块和确认游标的版本化 DTO。
2. 定义节点身份认证、签名、幂等和错误语义。
3. 增加 master/edge 共同使用的协议兼容测试。

### 第三阶段：Master 控制面

1. 增加节点、策略版本、lease 和结算记录模型。
2. 实现 bootstrap、增量同步、租约签发、结算和心跳接口。
3. 复用钱包、订阅和 `billingexpr` 完成预留与复核。
4. 验证并发节点不能重复使用同一余额。

### 第四阶段：Edge 本地数据面

1. 增加 SQLite 快照、lease、reservation 和 outbox 模型。
2. 接入完整令牌鉴权索引和本地权限判断。
3. 实现 `EdgeLeaseFunding` 并接入 `BillingSession`。
4. 实现原子结算、断点续传、租约补充和心跳。
5. 限制 edge 暴露的路由和后台任务。

### 第五阶段：真实链路验证

1. 只放行当前 Pro20x 实际使用的 Chat/Responses 文本及流式请求。
2. 验证 master 200ms 延迟不会增加用户请求延迟。
3. 验证 master 断开至少 10 分钟时已有 lease 继续服务。
4. 验证 edge 崩溃重启后 usage 不丢失并能继续上报。
5. 验证重复、乱序和重放区块不会重复计费。
6. 验证 lease 耗尽时请求在访问 CPA 前失败。

## 首期边界

- 图片、视频、异步任务和 Realtime 在完成独立计费验证前不开放。
- CPA 只允许 edge 通过本机或容器私网访问。
- token 撤销延迟由增量同步频率、快照有效期和剩余 lease 共同限制。
- 无本地 lease 且 master 不可用时，请求 fail closed。
- 节点由同一运营方控制；签名和认证主要防止伪造、重放和配置错误，不尝试防御恶意节点运营者。
- master 不审批 edge 的公开访问地址。节点凭证是信任边界；地址探测失败只影响健康状态和节点选择。

## 风险控制

| 风险 | 控制措施 |
|------|------|
| 形成第二套网关 | 协议、relay、计费和鉴权上下文直接复用同仓库 package |
| 上游合并困难 | 默认入口保持兼容，新增业务集中在 edge package 和少量启动分支 |
| 多节点超卖 | master 签发 lease 时事务型预留额度 |
| edge 崩溃丢账 | lease 结算与 outbox 在同一 SQLite 事务中写入 |
| 重复结算 | 节点代次、连续序号和 master 唯一约束 |
| 首次请求依赖 master 鉴权 | 全量同步安全令牌指纹，鉴权始终本地完成 |
| master 失联停服 | 用户请求不访问 master，已有 lease 内继续工作 |

## 验证

```bash
go test ./model ./service/... ./middleware/... ./router/...
go test ./pkg/billingexpr/...
go test ./relay/...
go test ./...
go build .
go build ./cmd/newapi-edge
```

通过条件：

- 默认 master 行为与上游基线一致。
- master 和 edge 共用协议、计费与 relay package，没有复制实现。
- 首次请求可以使用本地 token 指纹完成鉴权。
- 无 lease 的请求不会访问 CPA。
- master 延迟或暂时失联不进入正常用户请求路径。

## 回滚

默认 master 入口必须始终能够在关闭分布式功能后按上游方式运行。edge 停止接收新请求后先发送剩余 outbox 并关闭 lease；master 未确认前不删除 edge 本地状态或释放未结算额度。
