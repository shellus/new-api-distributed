# 分布式主从节点配置与运行约定

## 目标

分布式配置遵循一个原则：业务配置由 master 管理，节点运行配置由 edge 自己管理，edge 数据库是自动生成的本地投影，不是人工配置入口。真实地址、端口、账号和密钥只进入 `.env`、编排系统或密钥管理系统；本文只记录稳定的变量语义、默认值和占位符。

配置所有权的形成背景和相关否定方案见 [distributed-design-context.md](./distributed-design-context.md) 与 [ADR 0003](./adr/0003-edge-owned-runtime-configuration.md)。

有效节点凭证代表 master 对 edge 的完整信任。edge 有权声明自己的公开访问地址，master 不增加人工审批状态；master 保留吊销节点凭证和整体禁用节点的能力。

## 运行入口与功能开关

- 默认根入口继续构建 master。`EDGE_DISTRIBUTED_ENABLED` 默认为 `false`；关闭时不注册 edge 控制面和管理接口，也不启动快照编译与消费日志 outbox。
- `cmd/newapi-edge` 构建独立 edge 入口。edge 只暴露 `/healthz`、`/readyz`、`/v1/chat/completions` 和 `/v1/responses`，不承载管理后台。
- master 与 edge 使用同一 Go module、relay、协议适配、计费换算和 `BillingSession`；edge 不复制这些实现。
- CPA 是 edge 的本地上游执行引擎。CPA 不得作为绕过 edge 鉴权、租约和计费的公网入口。

## 配置事实来源

| 配置范围 | 事实来源 | 说明 |
|------|------|------|
| 用户、令牌、分组、模型、渠道和计费 | master | 统一编辑并下发版本化快照 |
| 节点身份、启停和资源限制 | master | 控制节点是否可以参与系统 |
| 节点公开访问地址 | edge | 使用节点凭证声明，master 直接记录 |
| CPA OAuth 凭证和本地数据目录 | edge | 属于节点本地运行环境，不从 master 下发 |
| CPA 内部服务地址 | 标准部署约定 | 所有节点使用一致的 Docker 服务别名和内部端口 |
| 健康度、负载和延迟 | 运行时观测 | 当前由 edge 心跳和本地 CPA 探测形成；master 公网主动探测属于可扩展观测，不作为配置真值 |

## Master 管理的全局业务配置

master 继续使用现有 New API 后台管理：

- 用户和 token 状态。
- 分组和模型权限。
- 渠道、模型映射、参数覆盖和系统提示词。
- 计费规则和价格版本。
- token 撤销和渠道启停。
- 配额租约的上限和补充策略。

这些配置由 master 编译为 edge 所需的最小快照。edge 不复制中心数据库，也不提供本地编辑入口。

## Master 管理的节点状态

master 负责维护：

- 节点身份和凭证状态。
- 节点启用、禁用和吊销状态。
- 节点允许承载的业务范围。
- 节点租约和风险限制。
- edge 最近一次声明的公开访问地址。
- edge 上报和 master 探测得到的运行状态。

公开访问地址属于 edge 声明，而不是 master 配置。master 管理员可以禁用整个节点，但不需要批准或重写该地址。

## 节点凭证与快照签名

控制面使用两组相互独立的 Ed25519 密钥：

1. master 快照签名密钥签署不可变快照清单和数据集。edge 只接受控制面下发且处于有效期内的公钥，并在原子应用快照前校验摘要、签名、页序和数据集完整性。
2. 节点凭证签署 edge 到 master 的控制请求。创建或轮换节点凭证时，master 只保存公钥，私钥只在响应中返回一次，随后必须写入 edge 的密钥管理系统。

每个控制请求同时携带节点 ID、节点代次、凭证 ID、时间戳、nonce 和幂等键。master 使用持久化 receipt 防止 nonce 重放和同一幂等键承载不同请求。节点代次只有在放弃或重建该节点全部持久化账务状态时才允许变化。

## Master 环境变量

启用分布式控制面时，master 至少需要设置 `EDGE_DISTRIBUTED_ENABLED=true`、`EDGE_SNAPSHOT_SIGNING_KEY_ID` 和 `EDGE_SNAPSHOT_SIGNING_PRIVATE_KEY`。签名私钥不得与节点凭证私钥复用。

| 变量 | 默认值或范围 | 作用 |
|------|------|------|
| `EDGE_DISTRIBUTED_ENABLED` | `false` | 注册控制面与 edge 管理接口，启动快照编译和 master 消费日志 outbox |
| `EDGE_SNAPSHOT_SIGNING_KEY_ID` | 启用时必填 | 当前快照签名公钥的稳定标识 |
| `EDGE_SNAPSHOT_SIGNING_PRIVATE_KEY` | 启用时必填 | Ed25519 快照签名私钥，只保存在 master 密钥环境 |
| `EDGE_SNAPSHOT_SIGNING_NOT_BEFORE_UNIX` | 可选；Unix 秒 | 限制签名密钥生效时间；省略时使用内置长期兼容下界 |
| `EDGE_SNAPSHOT_SIGNING_EXPIRES_AT_UNIX` | 可选；Unix 秒 | 限制签名密钥失效时间；必须晚于生效时间和当前时间 |
| `EDGE_SNAPSHOT_COMPILE_INTERVAL_SECONDS` | `900`，最小 `60` | 周期性重新编译业务快照；启动时也会先编译一次 |
| `EDGE_SNAPSHOT_TTL_SECONDS` | `3600`，范围 `60..86400` | 已签名快照有效期；不能超过签名密钥剩余有效期 |
| `EDGE_SNAPSHOT_PAGE_LIMIT` | `500`，范围 `1..10000` | 编译分页大小，同时作为控制面允许的页面大小 |
| `EDGE_HEARTBEAT_INTERVAL_SECONDS` | `30`，范围 `1..3600` | master 下发给 edge 的心跳周期 |
| `EDGE_SNAPSHOT_POLL_INTERVAL_SECONDS` | `60`，范围 `1..86400` | master 下发给 edge 的快照轮询周期 |
| `EDGE_SETTLEMENT_MAX_EVENTS` | `500`，范围 `1..10000` | 单个结算区块最多包含的事件数 |
| `EDGE_SETTLEMENT_MAX_DELAY_SECONDS` | `10`，范围 `1..3600` | edge 定时形成并上传结算区块的最大等待时间 |
| `EDGE_CONTROL_CLOCK_SKEW_TOLERANCE_SECONDS` | `120`，范围 `1..900` | 控制请求和结算事件允许的时钟偏差 |
| `EDGE_MAX_INFLIGHT_REQUEST_SECONDS` | `3600`，范围 `1..86400` | master 接受单个 usage event 的最大持续时间 |
| `EDGE_CONTROL_RECEIPT_TTL_SECONDS` | `86400`，范围 `1..604800` | 控制请求 receipt 与重放保护的保留时间 |
| `EDGE_LEASE_TTL_SECONDS` | `900`，有效范围 `60..86400` | master 签发租约的有效期 |
| `EDGE_LEASE_MAX_QUOTA` | `500000`，上限为 `common.MaxQuota` | 单次租约最多授予的额度 |
| `EDGE_LEASE_RENEW_DIVISOR` | `4`，范围 `2..1000` | 以 `granted_quota / divisor` 计算续租阈值 |
| `EDGE_CONSUME_LOG_OUTBOX_INTERVAL_SECONDS` | `2`，最小 `1` | master 投影消费日志 outbox 的轮询周期 |
| `EDGE_CONSUME_LOG_OUTBOX_BATCH_SIZE` | `100`，范围 `1..1000` | 每轮消费日志 outbox 的最大处理量 |

`EDGE_SNAPSHOT_COMPILE_INTERVAL_SECONDS` 必须明显短于 `EDGE_SNAPSHOT_TTL_SECONDS`，建议不超过 TTL 的一半，为编译失败重试和 edge 拉取预留时间。默认值 `900/3600` 满足该约束；缩短 TTL 时必须同步缩短编译间隔，否则旧快照会按设计过期并使 edge fail closed。

控制面下发的心跳、轮询、分页、结算和时钟参数会通过 v1 DTO 再次校验。超出协议范围的配置不会被 edge 静默接受。

## Edge 本地部署配置

edge 的人工配置只保留部署必需信息：

- master 控制面地址。
- 节点身份和已配置的节点凭证。
- 节点公开访问地址。
- 节点名称和区域等展示信息。
- 本地 SQLite 数据目录。
- CPA OAuth 凭证目录。
- 反向代理、证书和容器网络等运行环境。

这些内容保存在环境变量或节点部署配置中，不写入 edge SQLite 作为人工维护数据。

### Edge 身份与控制连接

| 变量 | 默认值或范围 | 作用 |
|------|------|------|
| `EDGE_MASTER_URL` | 必填 | master 根地址；生产必须使用 HTTPS，HTTP 只允许 localhost 回环测试，不允许路径前缀、查询或片段 |
| `EDGE_NODE_ID` | 必填 | master 创建节点时指定的稳定节点 ID |
| `EDGE_NODE_GENERATION` | 必填，正整数 | 节点持久化状态代次，必须与 master 一致 |
| `EDGE_CREDENTIAL_KEY_ID` | 必填 | master 返回一次的节点凭证 ID |
| `EDGE_CREDENTIAL_PRIVATE_KEY` | 必填 | 对应 Ed25519 私钥，只保存在 edge 密钥环境 |
| `EDGE_PUBLIC_URL` | 必填 | edge 签名声明的用户入口；生产必须使用 HTTPS，允许部署所需路径前缀，不允许查询、片段或内嵌账号 |
| `EDGE_NODE_NAME` | 必填 | 心跳和节点列表使用的展示名称 |
| `EDGE_REGION` | 可选 | 不参与鉴权的展示区域 |
| `EDGE_CONTROL_REQUEST_TIMEOUT_SECONDS` | `15`，范围 `1..300` | 单次 edge 到 master 控制请求超时 |
| `EDGE_CONTROL_MAX_RESPONSE_BYTES` | `16777216`，范围 `1..67108864` | 控制响应最大字节数，防止异常快照响应耗尽内存 |

### Edge 本地状态与生命周期

| 变量 | 默认值或范围 | 作用 |
|------|------|------|
| `EDGE_SQLITE_PATH` | 必填 | 独立 edge SQLite 文件；不能复用 master 的 `SQL_DSN` 或日志库 |
| `EDGE_LEASE_REQUEST_QUOTA` | `100000`，范围 `1..common.MaxQuota` | 非免费请求首次申请或续租的目标额度 |
| `EDGE_LEASE_MINIMUM_QUOTA` | `1000`，范围 `0..common.MaxQuota` | 非免费请求可接受的最小授予额度；实际请求所需额度会提高该下界 |
| `EDGE_LEASE_MAINTENANCE_INTERVAL_SECONDS` | `15`，有效范围 `1..300` | staged settlement 恢复、主动续租和旧租约关闭周期 |
| `EDGE_LEASE_RENEW_BEFORE_SECONDS` | `60`，有效范围 `1..3600` | 租约接近到期时提前续租的窗口 |
| `EDGE_CPA_HEALTH_TIMEOUT_SECONDS` | `3`，有效范围 `1..30` | 单个 CPA `HEAD /healthz` 探测超时 |
| `SHUTDOWN_TIMEOUT_SECONDS` | `120` | HTTP 优雅关闭时间；超时后强制关闭连接，但仍等待 handler 完成账务收尾 |
| `EDGE_DRAIN_TIMEOUT_SECONDS` | `30` | 停止后台循环后，最终上传结算并关闭可关闭租约的时间预算 |

`PORT` 仍控制进程监听端口。部署值属于环境信息，只写入私有 `.env` 或编排配置，不写入项目文档。


## 公开访问地址

公开访问地址是用户访问 New API Edge 的入口，例如：

```text
https://<edge-public-host>
```

处理规则如下：

1. edge 从本地部署配置读取公开访问地址。
2. edge 在注册、重连和地址变化时使用节点身份签名声明该地址。
3. master 完成协议格式和节点凭证校验后直接保存，不进入人工审批流程。
4. 当前 v1 控制面持久化该声明和 edge 心跳，不使用 master 公网探测决定 edge readiness。
5. 后续增加公网主动探测时，只能记录可达性和延迟；探测失败不能否定地址声明。
6. 地址变化后由 edge 自动覆盖旧声明，不要求管理员同步修改 master。

节点凭证是公开地址的信任边界。如果节点不再可信，处理方式是吊销凭证或禁用节点，而不是为每次地址变化增加审批。

Master 观察到的来源 IP 只能作为辅助信息，不能替代公开访问地址。节点可能经过 Nginx、CDN、NAT、非标准端口或路径前缀。

## CPA 内部地址

CPA 地址不是节点公开地址。它只存在于 edge 的本地容器网络中。

CPA 的 OpenAI 兼容 base URL 必须指向服务根路径，不能附加 `/v1`。当前默认内部端口为 `8317`。Docker 部署可使用逻辑服务别名：

```text
http://<cpa-pro20x4-service>:8317
http://<cpa-pro20x5-service>:8317
http://<cpa-pro20x6-service>:8317
```

单进程本地验证可以使用 `http://localhost:8317`。edge 会自行拼接 relay 路径，并使用 `HEAD /healthz` 探测 CPA；base URL 包含路径、查询、片段、内嵌账号或端口 `0` 时会拒绝加载。

| 逻辑服务 | Base URL 变量 | API key 变量 | 未设置 Base URL 时的容器默认值 |
|------|------|------|------|
| `cpa-pro20x4` | `EDGE_CPA_PRO20X4_BASE_URL` | `EDGE_CPA_PRO20X4_API_KEY` | `http://cpa-pro20x4:8317` |
| `cpa-pro20x5` | `EDGE_CPA_PRO20X5_BASE_URL` | `EDGE_CPA_PRO20X5_API_KEY` | `http://cpa-pro20x5:8317` |
| `cpa-pro20x6` | `EDGE_CPA_PRO20X6_BASE_URL` | `EDGE_CPA_PRO20X6_API_KEY` | `http://cpa-pro20x6:8317` |

API key 和 CPA OAuth 凭证都是 edge 本地秘密，不从 master 下发。某个 API key 为空时，对应渠道在本地投影中保持禁用；节点只部署部分 CPA 时，其余 key 应留空。

master 快照编译要求存在名称精确为 `cpa-pro20x4`、`cpa-pro20x5` 和 `cpa-pro20x6` 的三个 OpenAI 类型渠道。渠道 ID、模型、分组、优先级和权重来自 master；本地 URL 与 API key 按逻辑服务名绑定，因此不维护 channel ID 到地址的人工映射。无法安全表达的代理、凭证或任意覆盖配置会使快照编译失败，而不是被静默下发。

edge 在快照安装后立即探测 CPA，并在每次心跳时重新探测。至少一个 CPA 必须同时满足健康检查成功且签名快照声明了可用模型；否则 `/readyz` 返回 `503`，新请求在鉴权和访问 CPA 前 fail closed。恢复健康后，渠道投影与 readiness 原子恢复。

## Edge 自动生成的数据

edge SQLite 保存：

- master 下发的令牌鉴权索引和业务快照。
- 本地 lease、reservation 和结算状态。
- staged settlement、usage event、outbox 和同步游标。
- 渠道的本地运行投影。

这些数据只由同步和运行逻辑修改。edge 后台不提供用户、渠道、分组、计费或余额编辑入口，运维过程也不直接修改 SQLite。

在没有未上报账务数据时，SQLite 可以删除并通过 master 重新生成；存在未确认 outbox 或 lease 时必须先完成恢复或结算。

## 租约与免费模型

所有请求都必须先获得与当前签名快照、用户和 token 绑定的本地 lease，随后才能访问 CPA。非免费请求使用正额度 lease；master 在签发时从钱包或订阅中事务型预留额度，并受节点最大未结额度限制。

签名计费策略明确为免费时，edge 申请 `requested_quota=0`、`minimum_acceptable_quota=0` 的零额度 lease。master 不预留钱包或订阅额度，但仍签发可审计、可结算、与快照版本绑定的 lease。零额度 lease 不参与正额度自动续租；免费请求仍不能绕过本地鉴权、lease 校验、usage event 和结算序列。

当前 v1 数据面只执行倍率计费和固定价格计费。tiered expression 可以存在于与当前请求无关的快照策略中，但使用 tiered 计费的模型不会在 edge v1 上执行，master 也不会接受伪造为 v1 可结算事件的动态计费结果。

## Durable settlement 与账务 fail closed

请求完成后，edge 按以下顺序落盘：

1. 先把精确 usage event 写入 active reservation 的 staged settlement 字段。
2. 再在同一 SQLite 事务中调整 lease、完成 reservation、写入不可变 usage event 和本地 outbox。
3. 后台按连续序列组成持久化结算区块；同一区块重试和进程重启复用完全相同的请求内容。

一旦上游请求已经完成而本地结算失败，edge 会关闭账务 readiness。已有 staged settlement 由租约维护循环确定性重试；在 staged 事件全部完成前，`/readyz` 保持 `503` 且不接受新请求。若失败发生在精确事件成功 staged 之前，或启动扫描发现 active 但未 staged 的孤儿 reservation，账务门会保持锁定并要求人工核查和处置，不能猜测请求结果后自动退款；重启不会自动清除该阻断。正常关闭会先停止 admission、等待在途 handler 完成账务收尾，再停止后台循环并执行最终 drain。

## Master exact-once 投影

master 在接收结算区块的权威事务中校验节点代次、连续事件序列、lease、快照和价格，并写入 usage event 与消费日志 outbox。重复区块、重复事件和重放请求返回幂等结果，不重复扣费。

消费日志 worker 使用全局 billing event key 投影正式 consume log；启用数据统计时，`quota_data` 使用独立事件标记在主数据库中原子累加。SQL 日志库、独立日志库和 ClickHouse 投影都必须以同一 billing event key 保持重试幂等。投影成功前 outbox 不会被删除；失败会退避重试，master 崩溃后可继续领取过期 claim。

## 健康检查语义

- `/healthz` 只表示 edge 进程存活，不代表可以接收用户请求。
- `/readyz` 只有在 admission 开启、签名快照仍有效、至少一个 CPA 健康且声明了模型、账务状态可写且没有待恢复 staged settlement 时返回 `200`。
- 应用新快照时先关闭数据面，在持有策略写锁的情况下原子替换投影并探测新 CPA，再恢复 readiness。请求完成本地 lease 预占后会固定快照与价格版本，重试不能跨快照使用新的价格或路由策略。

## 配置与状态流向

```text
Master 管理后台
  -> 用户、令牌、渠道、模型、计费和节点策略
  -> Edge 本地自动投影

Edge 本地部署配置
  -> 节点身份、公开地址、CPA 凭证和运行环境
  -> Master 节点状态与公开节点列表

运行时观测
  -> Edge 上报负载和 CPA 状态
  -> Master 记录节点心跳和可用能力
  -> 可选公网探测只补充可达性和延迟
```

## 节点生命周期

### 首次部署

1. 在 master 配置快照签名密钥、三个逻辑 CPA 渠道及可安全投影的模型和计费策略。
2. 通过受 root 权限保护的 edge 管理接口创建节点；立即安全保存只返回一次的凭证 ID 和私钥。
3. 准备 edge 与 CPA 编排，把节点身份、master 地址、公开访问地址、SQLite 路径和 CPA 凭证写入私有部署配置。
4. 启动 CPA，确认根 base URL 下的 `HEAD /healthz` 可用；随后启动 edge。
5. edge 使用节点凭证 bootstrap，原子生成 SQLite 投影、验证签名快照、探测 CPA 并开始心跳。

### 重装或迁移

1. 保留或安全迁移节点身份。
2. 未确认 usage 和 lease 必须先结算，或恢复原 edge SQLite。
3. 在新环境设置新的公开访问地址和 CPA 凭证。
4. edge 重连后自动同步业务快照并更新公开地址声明。

### 地址变化

1. 修改 edge 本地公开访问地址。
2. edge 在下一次控制通信中签名上报。
3. master 直接更新地址并重新开始可达性探测。

### 节点下线

1. edge 停止接受新请求并上报剩余结算。
2. 等待在途 handler 完成 durable settlement，上传 outbox，并关闭或归还可关闭的 lease。
3. master 禁用节点或吊销凭证。
4. 节点公开地址从可选节点列表中移除。

## 部署前检查

- master、edge 和 CPA 的时间必须由可靠时钟同步；允许偏差不能超过 `EDGE_CONTROL_CLOCK_SKEW_TOLERANCE_SECONDS`。
- `EDGE_MASTER_URL` 和三个 CPA base URL 必须为根 URL；CPA base URL 不能带 `/v1`。
- master 快照签名私钥与 edge 节点私钥必须属于不同密钥，并且都不能进入数据库、日志或版本库。
- edge SQLite 必须位于持久化存储；未确认 usage、staged settlement、outbox 或 active lease 存在时不能删除或替换。
- 生产探针应使用 `/readyz` 决定是否接流量，`/healthz` 只用于判断进程是否需要重启。
- 只有 `/v1/chat/completions` 和 `/v1/responses` 的文本及流式请求属于 v1 边界；图片、音频、视频、Realtime、异步任务和无法表达的内置工具在访问 CPA 前拒绝。

## 运维边界

- 不在 edge 数据库中手工创建或编辑渠道。
- 不在每个节点维护 channel ID 到 CPA 地址的映射。
- 不从 master 下发 CPA OAuth 凭证。
- 不根据公网探测结果修改 edge 声明的公开地址。
- 不允许 CPA 通过公网绕过 edge 鉴权和计费。
- 节点失去信任时吊销节点凭证，不增加地址审批流程。
- 不把没有真实执行记录的 master/edge/CPA E2E 标记为已通过。
