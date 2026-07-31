# 分布式主从节点配置与运行约定

## 目标

分布式配置遵循一个原则：业务配置由 master 管理，节点运行配置由 edge 自己管理，edge 数据库是自动生成的本地投影，不是人工配置入口。真实地址、端口、账号和密钥只进入 `.env`、编排系统或密钥管理系统；本文只记录稳定的变量语义、默认值和占位符。

配置所有权的形成背景和相关否定方案见 [distributed-design-context.md](./distributed-design-context.md) 与 [ADR 0003](./adr/0003-edge-owned-runtime-configuration.md)。

有效节点凭证代表 master 对 edge 的完整信任。edge 有权声明自己的公开访问地址，master 不增加人工审批状态；master 保留吊销节点凭证和整体禁用节点的能力。

## 运行入口与功能开关

- 默认根入口继续构建 master。`EDGE_DISTRIBUTED_ENABLED` 默认为 `false`；关闭时不注册 edge 控制面和管理接口，也不启动快照编译与消费日志 outbox。
- `cmd/newapi-edge` 构建独立 edge 入口。edge 只暴露 `/healthz`、`/readyz`、`/v1/chat/completions` 和 `/v1/responses`，不承载管理后台。
- master 与 edge 使用同一 Go module、relay、协议适配、计费换算和 `BillingSession`；edge 不复制这些实现。
- CPA 是 edge 的本地上游执行引擎。CPA 不得作为绕过 edge 鉴权、本地余额 admission 和计费的公网入口。

## 配置事实来源

| 配置范围 | 事实来源 | 说明 |
|------|------|------|
| 用户、令牌、分组、模型、渠道和计费 | master | 统一编辑并下发版本化快照 |
| 节点身份、启停和资源限制 | master | 控制节点是否可以参与系统 |
| 节点公开访问地址 | edge | 使用节点凭证声明，master 直接记录 |
| CPA OAuth 凭证和本地数据目录 | edge | 属于节点本地运行环境，不从 master 下发 |
| CPA 内部服务地址 | 标准部署约定 | 所有节点使用一致的 Docker 服务别名和内部端口 |
| 健康度、负载和延迟 | 运行时观测 | 当前由 edge 心跳和真实请求结果形成；心跳中的 CPA 字段仅为空值兼容保留，master 公网主动探测属于可扩展观测，不作为配置真值 |

## Master 管理的全局业务配置

master 继续使用现有 New API 后台管理：

- 用户和 token 状态。
- 分组和模型权限。
- 渠道、模型映射、参数覆盖和系统提示词。
- 计费规则和价格版本。
- token 撤销和渠道启停。
- 用户计费偏好、余额复制和节点结算风险参数。

这些配置由 master 编译为 edge 所需的最小快照。edge 不复制中心数据库，也不提供本地编辑入口。

## Master 管理的节点状态

master 负责维护：

- 节点身份和凭证状态。
- 节点启用、禁用和吊销状态。
- 节点允许承载的业务范围。
- 节点结算滑动窗口、熔断状态和人工恢复代次。
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
| `EDGE_NODE_SETTLEMENT_WINDOW_SECONDS` | `300`，范围 `10..86400` | 每节点按 usage event 完成时间计算的结算滑动窗口长度 |
| `EDGE_NODE_SETTLEMENT_WINDOW_QUOTA` | `50000000`，范围 `1..common.MaxQuota` | 同一节点代次在任一事件时间窗口内允许受理的最大 charge |
| `EDGE_CONSUME_LOG_OUTBOX_INTERVAL_SECONDS` | `2`，最小 `1` | master 投影消费日志 outbox 的轮询周期 |
| `EDGE_CONSUME_LOG_OUTBOX_BATCH_SIZE` | `100`，范围 `1..1000` | 每轮消费日志 outbox 的最大处理量 |
| `EDGE_CONSUME_LOG_SNAPSHOT_FIELDS_ENABLED` | `false` | 向 edge 快照下发消费日志一致性所需的 token 名称、IP 记录策略和特殊分组倍率标记；仅在全部 edge 已升级到支持这些可选字段的版本后启用 |
| `EDGE_PRE_CONSUMED_QUOTA_SNAPSHOT_ENABLED` | `false` | 向签名价格策略下发 master 当前 `PreConsumedQuota`；仅在全部 edge 已支持可选 `pre_consumed_quota` 字段后启用 |
| `EDGE_PROMPT_SAFETY_SNAPSHOT_ENABLED` | `false` | 向签名业务快照下发 master 当前提示词敏感检查开关和指纹词表；仅在全部 edge 已支持可选 `prompt_safety` 字段后启用 |

`EDGE_SNAPSHOT_COMPILE_INTERVAL_SECONDS` 应明显短于 `EDGE_SNAPSHOT_TTL_SECONDS`，建议不超过 TTL 的一半，为编译失败重试和 edge 拉取预留时间。快照 TTL 仍限制新快照的发布和应用，但最后一份已经验证并应用的策略过期后只冻结策略变化，不再关闭数据面。

`EDGE_CONSUME_LOG_SNAPSHOT_FIELDS_ENABLED` 用于 master-first 滚动升级。新版 master 首次部署时保持 `false`，随后升级全部 edge；确认 edge 健康且没有待上传结算区块后，再将变量设为 `true` 并重启 master，使下一份编译快照携带新增字段。旧版 edge 使用严格 JSON 解码，混合版本期间提前启用会使其拒绝新快照。

`EDGE_PRE_CONSUMED_QUOTA_SNAPSHOT_ENABLED` 使用相同的兼容顺序。关闭时新版 edge 继续使用自身默认值，只用于混合版本过渡；全部 edge 升级后必须启用，使 master 后台修改 `PreConsumedQuota` 时下一份签名价格策略同步携带该值。

`EDGE_PROMPT_SAFETY_SNAPSHOT_ENABLED` 也使用相同的 master-first 顺序。关闭时新版 edge 对缺失字段安装编译默认的 Prompt Safety Policy；全部 edge 升级后必须启用，使 `CheckSensitiveEnabled`、`CheckSensitiveOnPromptEnabled`、`StopOnSensitiveEnabled` 和 `SensitiveWords` 的后台变更在下一份签名快照中同步生效。协议最多接受 10000 个词条、单项 4096 字节、词条内容合计 1 MiB；超限配置不会发布新快照。旧版 edge 仍运行时提前启用会因未知 `prompt_safety` 字段拒绝快照。

控制面下发的心跳、轮询、分页、结算、circuit 和时钟参数会通过版本化 DTO 再次校验。超出协议范围的配置不会被 edge 静默接受。

控制面连接失败、超时、EOF，以及 HTTP `408`、`425`、`429`、`5xx`（包括反向代理返回的非 JSON 错误页）属于暂时不可用。edge 保留活动控制客户端和最后一份已验证策略，按 bootstrap 或既有 heartbeat、snapshot、settlement 周期继续重试，master 恢复后无需重启 edge。TLS 身份校验、签名或摘要错误，2xx 成功响应的非 JSON/严格 DTO 错误，以及身份、代次和结算完整性冲突仍按 fail closed 处理并关闭 readiness。

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
| `EDGE_CHANNEL_CONFIG_DIR` | `/config/channels` | edge 本地渠道 YAML 目录；只读取顶层 `.yaml`/`.yml` 文件 |
| `EDGE_BALANCE_SETTLEMENT_FLOOR_QUOTA` | `-10000000`，范围 `-common.MaxQuota..0` | 已执行请求的实际 charge 超过 reservation 时，有限资金账户和有限 token 账户仍可完成结算的最低余额；不参与新请求 admission |
| `EDGE_BALANCE_NEGATIVE_FLOOR_QUOTA` | 兼容别名 | 仅在未设置新变量时作为 Settlement Floor 读取；后续部署应迁移到新变量名 |
| `EDGE_LOCAL_ACCOUNTING_RETENTION_EVENTS` | `10000`，范围 `1000..1000000` | 保留最近多少条已经完成 master 确认和本地余额回冲的账务序列；更老的本地重复审计副本由维护循环分批清理 |
| `EDGE_LOCAL_ACCOUNTING_PRUNE_BATCH_SIZE` | `100`，范围 `1..5000` | 单轮从 reservation、usage、outbox 和 settlement block 各清理的最大行数；保守默认值用于限制后台清理持有 SQLite 写锁的时间 |
| `EDGE_CPA_HEALTH_TIMEOUT_SECONDS` | 兼容保留 | 当前 edge 不执行本地上游合成探测，也不读取此变量 |
| `SHUTDOWN_TIMEOUT_SECONDS` | `120` | HTTP 优雅关闭时间；超时后强制关闭连接，但仍等待 handler 完成账务收尾 |
| `EDGE_DRAIN_TIMEOUT_SECONDS` | `30` | 停止后台循环后，最终上传 durable settlement 的时间预算 |

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
4. 当前控制面持久化该声明和 edge 心跳，不使用 master 公网探测决定 edge readiness。
5. 后续增加公网主动探测时，只能记录可达性和延迟；探测失败不能否定地址声明。
6. 地址变化后由 edge 自动覆盖旧声明，不要求管理员同步修改 master。

节点凭证是公开地址的信任边界。如果节点不再可信，处理方式是吊销凭证或禁用节点，而不是为每次地址变化增加审批。

Master 观察到的来源 IP 只能作为辅助信息，不能替代公开访问地址。节点可能经过 Nginx、CDN、NAT、非标准端口或路径前缀。

## Edge 本地上游

上游地址、代理和凭证只存在于 edge 本地渠道 YAML。master 默认下发所有逻辑渠道和可结算模型，但不下发 URL、API key、OAuth/JSON 凭证、代理或任意请求覆盖。

edge 使用渠道 `name` 与 master 快照匹配。本地文件采用与部署仓库 `config/channels/*.yaml` 相同的字段语义，但允许只写本节点需要覆盖的字段：

```yaml
name: mistral
type: mistral
base_url: https://api.mistral.ai
auth: |
  key-one
  key-two
channel_setting:
  proxy: socks5h://edge-proxy:1080
```

CPA 等本地服务同样使用渠道 YAML：

```yaml
name: cpa-vip
type: openai
base_url: http://cpa-vip:8317
auth: local-cpa-api-key
```

修改 `config/edge/channels/*.yaml` 后、重启前执行渠道配置校验：

```bash
docker compose -f docker-compose.edge.yml run --rm new-api-edge --validate-channels
```

支持的本地实现字段包括 `enabled`、`status`、`type`、`base_url`、`auth`、`auth_data`、`auth_files`、`openai_organization`、`other`、`models`、`groups`、`model_mapping`、`channel_setting`、`settings`、`priority`、`weight`、`multi_key_mode`、`param_override` 和 `header_override`。`auth`、`auth_data`、`auth_files` 互斥；多行 `auth` 默认使用随机多 Key 模式。

合并规则如下：

- master 是渠道 ID、类型、全局启停、模型、分组和计费上限的事实来源。
- edge 本地 YAML 提供物理 URL、凭证、代理和节点特有请求行为。
- 本地 `models`、`groups` 只能与 master 下发值取交集，不能扩张权限。
- 本地 YAML 缺失、禁用或没有凭证时，对应渠道在该 edge 保持禁用；请求只匹配到该渠道时返回无可用渠道错误。
- 没有可复核价格的模型不进入 edge 模型投影，不影响 master 启动和其他渠道。

edge 不对本地上游发起合成探测，也不根据瞬时探测结果禁用渠道或关闭整机 readiness。渠道可用性由真实请求结果决定；真实密钥错误、网络错误和上游业务错误沿用 master 共用的 relay 与可选 auto-ban 处理。

## Edge 自动生成的数据

edge SQLite 保存：

- master 下发的令牌鉴权索引和业务快照。
- 本地余额账户、reservation、overlay 和结算状态。
- staged settlement、usage event、outbox 和同步游标。
- 渠道的本地运行投影。

这些数据只由同步和运行逻辑修改。edge 后台不提供用户、渠道、分组、计费或余额编辑入口，运维过程也不直接修改 SQLite。

在没有未上报账务数据时，SQLite 可以删除并通过 master 重新生成；存在 active/staged reservation、未确认 usage/outbox 或 pending settlement block 时必须先完成恢复或结算。

## 余额副本、零余额准入与结算容忍

`edge-control.v2` 通过 heartbeat 下发钱包、订阅和 token 的版本化余额向量。edge 把 master confirmed balance 与本地 active reservation、unsettled usage overlay 合并后完成 admission；普通用户请求不为了余额访问 master。第一次 v2 heartbeat 必须 full 初始化，鉴权索引和余额副本未同时就绪时不访问 CPA。

每次 reservation 在同一 SQLite 事务中固定资金来源、订阅 ID、token quota 模式、策略 revision、balance revision 和 Settlement Floor。钱包或订阅与有限 token 分别检查；任一有限账户预占后低于零时，请求在访问 CPA 前返回额度不足。免费请求 charge 为零，仍保留本地鉴权、reservation、usage event、连续结算序列和审计语义。

请求完成后的实际 charge 可以高于 reservation。该差额只允许在 `EDGE_BALANCE_SETTLEMENT_FLOOR_QUOTA` 内完成本地 durable settlement；账户因此变为负数后，后续正额度 reservation 会立即被零余额 admission 拒绝。该变量不能作为断网期间的固定信用额度使用。

倍率计费的最低预估值使用 master Option `PreConsumedQuota`。启用 `EDGE_PRE_CONSUMED_QUOTA_SNAPSHOT_ENABLED` 后，该值固定在每个签名价格策略中；edge 使用策略值计算本次预扣，不读取本地 Option 或独立编译默认值。

当前 v2 数据面只执行倍率计费和固定价格计费。tiered expression 可以存在于与当前请求无关的快照策略中，但使用 tiered 计费的模型不会在 edge v2 上执行，master 也不会接受伪造为 v2 可结算事件的动态计费结果。

## Durable settlement 与账务 fail closed

请求完成后，edge 按以下顺序落盘：

1. 先把精确 usage event 写入 active reservation 的 staged settlement 字段。
2. 再在同一 SQLite 事务中调整余额 overlay、完成 reservation、写入不可变 usage event 和本地 outbox。
3. 后台按连续序列组成持久化结算区块；同一区块重试和进程重启复用完全相同的请求内容。

账务维护循环只清理同时满足两个水位的历史：master 已确认的 settlement sequence，以及后续余额复制已经回冲的 settlement sequence。清理保留最近的审计尾部；异步任务 owner reservation 当前全部保留作为崩溃恢复证据，pending、in-block、未回冲 usage、staged settlement 和 active reservation 也不进入清理范围。在线清理只释放 SQLite 可复用页，不执行会长时间独占数据库的 `VACUUM`。

一旦上游请求已经完成而本地结算失败，已有 staged settlement 会关闭全局账务 readiness，并由账务维护循环确定性重试；在 staged 事件全部完成前，`/readyz` 保持 `503` 且不接受新请求。若失败发生在精确事件成功 staged 之前，或启动扫描发现无 owner、active 且未 staged 的孤儿 reservation，edge 保留 reservation 和预占现场并隔离其用户与 token；相关 subject 的新请求在访问 CPA 前失败，其他 subject 继续服务。该隔离要求人工核查和处置，不能猜测请求结果后自动退款；重启会从 reservation 重建隔离。数据库不可用、reservation 身份不完整或账务结构损坏仍关闭全局 readiness。正常关闭会先停止 admission、等待在途 handler 完成账务收尾，再停止后台循环并执行最终 drain。

## Master exact-once 投影

master 在接收结算区块的权威事务中校验节点代次、连续事件序列、资金来源、快照、价格和事件时间窗口，随后按事件保存点 exactly-once 扣减钱包或订阅及有限 token，并写入 usage event 与消费日志 outbox。当前用户、token 或订阅已经无法安全扣账时，该事件写入持久化 Settlement Skip，区块仍按总事件数确认；渠道和累计统计缺失只跳过辅助投影。重复区块、重复事件和重放请求返回幂等结果，不重复扣费或重复登记 skip。

PostgreSQL 在两个 edge 并发结算且锁顺序交叉时可能返回 `SQLSTATE 40P01`。master 当前会回滚整个事务并返回可重试的 `5xx`，edge 复用同一 durable block 重试，因此不会产生部分扣费或事件序号缺口；若该错误频率升高，应在 master 增加有界的事务级 deadlock retry，并持续监控 `40P01`。

结算事件固定引用 admission 时的快照 ID 和订阅 ID，因此 master 必须保留所有已发布/退役快照，并应保留已取消/过期的订阅账务行。快照 TTL 只限制新应用，不授权删除结算依据；管理端删除订阅也只取消其新请求资格，不物理删除历史账务行。若历史维护已经物理删除当前业务引用，可信事件形成 Settlement Skip，不再永久阻塞结算链。只有从未发布的陈旧 draft 可以自动清理。

当任一节点事件时间窗口的 charge 超过阈值时，master 使用受限的 committed rejection：只提交节点 circuit 标记和拒绝 receipt，不提交区块、usage、余额扣减、事件水位或日志 outbox。edge 在后续 heartbeat 收到 circuit 后停止新 admission，但继续心跳并保留本地 outbox。熔断不自动恢复；管理员核账后调用 `POST /api/edge/nodes/:id/settlement-circuit/clear`，清除 circuit 并递增 epoch，edge 随后用新的 HTTP request ID 重试同一 durable block，block ID、事件、序号和 digest 不变。

消费日志 worker 使用全局 billing event key 投影正式 consume log；启用数据统计时，`quota_data` 使用独立事件标记在主数据库中原子累加。SQL 日志库、独立日志库和 ClickHouse 投影都必须以同一 billing event key 保持重试幂等。暂时性数据库错误持续退避重试；确定性损坏连续失败达到边界后进入 `quarantined`，后续正常 outbox 继续处理，master 崩溃后仍可继续领取过期 claim。

## 健康检查语义

- `/healthz` 只表示 edge 进程存活，不代表可以接收用户请求。
- `/readyz` 只有在 admission 开启、已存在最后一份验证并应用的策略、余额副本已初始化、账务状态可写、没有待恢复 staged settlement 且 settlement circuit 未打开时返回 `200`；策略 TTL 过期和本地上游瞬时可达性不参与整机 readiness。
- 应用新快照时先关闭数据面，在持有策略写锁的情况下原子替换投影，再恢复 readiness。请求完成本地余额 reservation 后会固定快照、价格和 balance revision，重试不能跨快照使用新的价格或路由策略。

## 配置与状态流向

```text
Master 管理后台
  -> 用户、令牌、渠道、模型、计费和节点策略
  -> Edge 本地自动投影

Edge 本地部署配置
  -> 节点身份、公开地址、CPA 凭证和运行环境
  -> Master 节点状态与公开节点列表

运行时观测
  -> Edge 上报负载；CPA 状态字段为空，仅作协议兼容
  -> Master 记录节点心跳和可用能力
  -> 可选公网探测只补充可达性和延迟
```

## 节点生命周期

### 首次部署

1. 在 master 配置快照签名密钥、正常渠道和计费策略；所有渠道默认进入 edge 快照。
2. 通过受 root 权限保护的 edge 管理接口创建节点；立即安全保存只返回一次的凭证 ID 和私钥。
3. 准备 edge 编排，把节点身份、master 地址、公开访问地址、SQLite 路径和需要启用的本地上游凭证写入私有部署配置。
4. 启动本地 CPA 或确认直连上游网络可达；随后启动 edge。
5. edge 使用节点凭证 bootstrap，原子生成 SQLite 投影、验证签名快照并开始心跳。

### 重装或迁移

1. 保留或安全迁移节点身份。
2. 未确认 usage、pending block 和 staged reservation 必须先结算，或恢复原 edge SQLite。
3. 在新环境设置新的公开访问地址和 CPA 凭证。
4. edge 重连后自动同步业务快照并更新公开地址声明。

### 地址变化

1. 修改 edge 本地公开访问地址。
2. edge 在下一次控制通信中签名上报。
3. master 直接更新地址并重新开始可达性探测。

### 节点下线

1. edge 停止接受新请求并上报剩余结算。
2. 等待在途 handler 完成 durable settlement，并上传本地 outbox。
3. master 禁用节点或吊销凭证。
4. 节点公开地址从可选节点列表中移除。

## 部署前检查

- master、edge 和 CPA 的时间必须由可靠时钟同步；允许偏差不能超过 `EDGE_CONTROL_CLOCK_SKEW_TOLERANCE_SECONDS`。
- `EDGE_MASTER_URL` 和四个 CPA base URL 必须为根 URL；CPA base URL 不能带 `/v1`。
- master 快照签名私钥与 edge 节点私钥必须属于不同密钥，并且都不能进入数据库、日志或版本库。
- edge SQLite 必须位于持久化存储；未确认 usage、staged settlement、outbox、pending block 或 active reservation 存在时不能删除或替换。
- 生产探针应使用 `/readyz` 决定是否接流量，`/healthz` 只用于判断进程是否需要重启。
- master 与 edge 的用户侧 AI 路由必须来自共享数据面注册表。Responses 压缩、多模态、Realtime 和异步任务不得由 edge 专用请求形状白名单拦截，也不得回源 master。
- 本地渠道 YAML 必须覆盖对应渠道所需的 CPA/上游地址和凭证；未配置本地执行条件的渠道保持禁用，不影响其他渠道继续服务。

## 运维边界

- 不在 edge 数据库中手工创建或编辑渠道。
- 不在每个节点维护 channel ID 到 CPA 地址的映射。
- 不从 master 下发 CPA OAuth 凭证。
- 不根据公网探测结果修改 edge 声明的公开地址。
- 不允许 CPA 通过公网绕过 edge 鉴权和计费。
- 节点失去信任时吊销节点凭证，不增加地址审批流程。
- 不把没有真实执行记录的 master/edge/CPA E2E 标记为已通过。
