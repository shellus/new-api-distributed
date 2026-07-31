# 分布式余额复制与有界超卖设计

## 结论

余额复制采用“master 权威余额向量 + 每节点确认 revision + 心跳服务端 diff + edge 本地事务账本 + 原有异步结算链”的闭环。master 不记录余额变更日志，也不向上游钱包、token 或订阅写路径增加插桩；每次心跳直接读取当前余额事实，与该节点最后确认的完整向量比较，只传变化条目。edge 的普通请求不访问 master。

本设计由 [ADR 0004](./adr/0004-replicated-balances-and-bounded-oversell.md) 替代 ADR 0002，并由 [ADR 0006](./adr/0006-zero-admission-and-bounded-settlement.md) 把原单一负下限拆成零余额 admission 与独立 Settlement Floor。实施阶段不得恢复租约作为兼容回退。

## 现有实现基线

以下事实决定了替换边界：

- `EdgeLeaseFunding.PreConsume` 先尝试本地租约，不足时同步向 master 申请新租约；默认申请额度为 100,000 quota（`service/edge/funding.go:26`、`service/edge/funding.go:88`、`service/edge/funding.go:227`）。master 再把单租约限制为默认 500,000 quota（`service/edge/control_accounting.go:187`）。
- master 在签发租约时已经扣减钱包或订阅，并同时扣减有限 token；结算阶段只增加租约消费量和统计，不再次扣余额（`service/edge/master_lease.go:214`、`service/edge/master_lease.go:239`、`service/edge/master_lease.go:257`、`service/edge/master_lease.go:322`、`model/edge_lease_accounting.go:222`）。
- edge 当前通过 `NoopTokenQuotaAccounting` 避免重复扣 token（`service/edge/funding.go:56`、`service/token_quota_accounting.go:106`），而本地鉴权上下文无条件把 token 标成 unlimited，并把用户额度设为 `common.MaxQuota`（`middleware/edge_token_auth.go:100`、`middleware/edge_token_auth.go:107`、`middleware/edge_token_auth.go:122`）。删除租约后，有限 token 余额必须进入新余额数据集，否则 token 限额会失效。
- master 钱包资金来源是 `users.quota`，token 限额是 `tokens.remain_quota` 与 `tokens.unlimited_quota`（`model/user.go:23`、`model/token.go:14`）。普通 `BillingSession` 对每个请求同时处理资金来源和 token 额度（`service/billing_session.go:75`、`service/billing_session.go:95`、`service/billing_session.go:262`、`service/billing_session.go:270`）。
- 订阅权威余额由 `amount_total - amount_used` 表示，`amount_total == 0` 表示不限量；订阅还携带下一次重置时间和钱包回退策略（`model/subscription.go:253`、`model/subscription.go:267`、`model/subscription.go:276`、`model/edge_lease_accounting.go:157`）。
- 现有 edge 结算已经先持久化 staged usage，再在一个 SQLite 事务中完成 reservation、usage event、outbox 和连续序号（`model/edge_local_accounting.go:15`、`model/edge_local_accounting.go:118`、`model/edge_local_accounting.go:218`、`model/edge_local_accounting.go:250`、`model/edge_local_accounting.go:266`）。结算区块会持久化完全相同的请求并按连续序号组块，确认时原子推进本地游标（`model/edge_local_accounting.go:361`、`model/edge_local_accounting.go:387`、`model/edge_local_accounting.go:511`）。
- master 通过 block ID、幂等键、链式摘要、连续事件序号和唯一 usage event 约束提供 exactly-once 受理，再在同一事务写消费日志 outbox（`service/edge/master_lease.go:354`、`service/edge/master_lease.go:362`、`service/edge/master_lease.go:370`、`service/edge/master_lease.go:476`、`service/edge/master_lease.go:523`）。这些机制与租约额度分配无关，应保留。
- 当前 readiness 明确要求策略快照尚未过期：`EdgeControlReady` 比较快照过期时间，启动恢复也拒绝过期快照，reservation 事务再次检查快照 TTL（`service/edge/control_loops.go:73`、`service/edge/control_loops.go:271`、`model/edge_local.go:1397`）。该行为必须按 ADR 0004 改为“过期冻结策略变化，不关闭数据面”。

## 余额向量

### 账户维度

余额向量必须同时包含三类账户，不能只复制 token 级或只复制 user 级余额：

1. **用户钱包**：以 `user_id` 为键，`remain_quota` 来自 `users.quota`。钱包选择 user 级，而不是把钱包复制到每个 token，因为 master 的钱包资金来源本身按用户扣减；复制到 token 会把同一钱包重复成多份并放大超卖。
2. **token 限额**：以 `token_id` 为键，包含 `user_id`、`remain_quota` 和 `unlimited_quota`。有限 token 与钱包或订阅是并行限制，单次请求必须同时预占资金账户和有限 token 账户。`unlimited_quota=true` 时 `remain_quota` 固定为 0，仅表示不执行 token 额度扣减，不表示钱包或订阅免费。
3. **用户订阅**：以 `subscription_id` 为键，包含 `user_id`、`total_quota`、`remain_quota`、`unlimited_quota`、`next_reset_at_unix_milli`、`expires_at_unix_milli` 和 `allow_wallet_overflow`。`remain_quota = amount_total - amount_used`，允许为负；`amount_total == 0` 映射为 `unlimited_quota=true` 且 `total_quota=remain_quota=0`。

订阅到达 `next_reset_at_unix_milli` 后，edge 不自行创造下一周期额度。该订阅停止参与新 reservation，直到 master 完成权威重置并下发带有新 `remain_quota` 与新重置时间的条目。这样既不会在 master 宕机时伪造多轮订阅额度，也能避免继续消费已经跨周期的旧余额。`next_reset_at_unix_milli=0` 表示没有周期重置。

### 规范化与删除

master 每次心跳从主数据库读取非软删除用户、token 和仍可能参与计费的订阅，构造按账户类型、主键升序排列的规范向量。比较字段包含上述全部余额和生命周期字段；任一字段变化都产生 delta。某条记录不再存在或不再可参与计费时发送 tombstone，而不是依赖 edge 猜测删除。

单个账户的额度、归属或时间字段超出协议安全范围时，该账户从目标向量省略并记录错误日志；相对上一 revision 的 diff 会为它生成 tombstone，限制影响范围为对应用户、token 或订阅。主数据库查询失败、pending vector 损坏或 revision 状态错误仍中止本次 heartbeat，不能伪装成账户删除。

余额数据集是 `edge-control.v2` 的独立可变 dataset，不进入 `EdgeSnapshotManifestV1`，不参与策略快照编译、内容摘要或 detached signature。传输仍位于现有经过节点 Ed25519 认证、HTTPS 保护且带 request correlation 的心跳请求/响应内；不新增连接或轮询。策略快照继续只承载低频鉴权、分组、渠道、定价和 token 启停信息。

## 协议 v2 与 DTO 草案

命名、JSON 字段和 `Validate` 风格沿用 `dto/edge_control_v1.go` 的显式类型、正整数 ID、非负 revision、Unix 毫秒和有界数组规则（现有 heartbeat DTO 位于 `dto/edge_control_v1.go:631`，校验位于 `dto/edge_control_v1.go:1049`）。实现仍可放在该文件，类型使用 `V2` 后缀，避免把 v2 语义伪装成 v1。

```go
const (
    EdgeControlProtocolVersionV2 = "edge-control.v2"
    EdgeControlMaxBalanceItemsV2 = 100_000
)

type EdgeBalanceDatasetV2 string

const EdgeBalanceDatasetBalancesV2 EdgeBalanceDatasetV2 = "balances"

type EdgeWalletBalanceV2 struct {
    UserID      int64 `json:"user_id"`
    RemainQuota int64 `json:"remain_quota"`
    Deleted     bool  `json:"deleted,omitempty"`
}

type EdgeTokenBalanceV2 struct {
    TokenID        int64 `json:"token_id"`
    UserID         int64 `json:"user_id"`
    RemainQuota    int64 `json:"remain_quota"`
    UnlimitedQuota bool  `json:"unlimited_quota"`
    Deleted         bool  `json:"deleted,omitempty"`
}

type EdgeSubscriptionBalanceV2 struct {
    SubscriptionID      int64 `json:"subscription_id"`
    UserID              int64 `json:"user_id"`
    TotalQuota          int64 `json:"total_quota"`
    RemainQuota         int64 `json:"remain_quota"`
    UnlimitedQuota      bool  `json:"unlimited_quota"`
    NextResetAtUnixMilli int64 `json:"next_reset_at_unix_milli,omitempty"`
    ExpiresAtUnixMilli   int64 `json:"expires_at_unix_milli"`
    AllowWalletOverflow bool  `json:"allow_wallet_overflow"`
    Deleted             bool  `json:"deleted,omitempty"`
}

type EdgeBalanceDeltaV2 struct {
    Dataset                         EdgeBalanceDatasetV2        `json:"dataset"`
    BaseRevision                    int64                       `json:"base_revision"`
    Revision                        int64                       `json:"revision"`
    Full                            bool                        `json:"full"`
    SettlementAppliedThroughSequence int64                     `json:"settlement_applied_through_sequence"`
    Wallets                         []EdgeWalletBalanceV2       `json:"wallets,omitempty"`
    Tokens                          []EdgeTokenBalanceV2        `json:"tokens,omitempty"`
    Subscriptions                   []EdgeSubscriptionBalanceV2 `json:"subscriptions,omitempty"`
}

type EdgeHeartbeatRequestV1 struct {
    // existing fields...
    BalanceRevision int64 `json:"balance_revision,omitempty"`
}

type EdgeHeartbeatResponseV1 struct {
    // existing fields...
    BalanceDelta *EdgeBalanceDeltaV2 `json:"balance_delta,omitempty"`
}
```

v2 校验规则固定如下：

- `dataset` 必须为 `balances`；`balance_revision`、`base_revision`、`revision` 和结算水位不得为负；有 delta 时 `revision > base_revision`。
- 增量响应必须以请求的 `balance_revision` 为 `base_revision`；全量响应允许跨过未知 base，但仍携带 edge 报告的 base 供审计。
- full 响应不得包含 tombstone；增量内同类主键不得重复，所有数组使用主键升序规范排列。
- `unlimited_quota=true` 时对应 `remain_quota` 必须为 0；有限账户允许负数，但不得小于 `-common.MaxQuota` 或大于 `common.MaxQuota`。
- tombstone 只保留主键和归属 `user_id`，其他额度及时间字段必须为零值。

bootstrap 请求按优先顺序声明 `supported_protocol_versions=["edge-control.v2","edge-control.v1"]`。master 只允许协商 v2；只声明 v1 的旧 edge 返回 `unsupported_protocol`，`expected.protocol_versions` 明确给出 v2。字段使用 `omitempty` 只保证 JSON 解码兼容，不代表运行时允许回退租约。现有节点模型和 mutation 入口都硬编码只接受 v1（`model/edge_control.go:113`、`service/edge/control_mutation.go:66`），Phase B 必须先把该检查改为显式协商并持久化选中的版本。

新 edge bootstrap 后立即发送一次 heartbeat 获取 full 余额集；在 full 余额原子应用之前 admission 保持关闭，不等待正常 30 秒 ticker。

## revision 与服务端 diff

revision 是**每节点、每 generation**的交付序号，不是全局余额版本，也不是数据库变更序号。无变化心跳不递增 revision，不发送余额条目。

master 为每个节点保存一行复制状态：

| 字段 | 语义 |
|------|------|
| `node_id`、`node_generation` | 节点代次边界 |
| `confirmed_revision` | edge 已在后续心跳中确认应用的 revision |
| `confirmed_vector_payload` | 与 confirmed revision 对应的完整规范向量 JSON |
| `confirmed_settlement_sequence` | 该向量已经包含的本节点 settlement 水位 |
| `pending_revision` | 已下发但尚未由 edge 确认的 revision；最多一个 |
| `pending_vector_payload` | pending revision 对应的完整目标向量 |
| `pending_delta_payload` | 重试时必须原样重发的 full/delta |
| `pending_settlement_sequence` | pending 向量包含的 settlement 水位 |

状态机固定如下：

1. 请求 revision 等于 `pending_revision`：先把 pending 提升为 confirmed，再基于新的权威向量计算下一份 diff。
2. 请求 revision 等于 `confirmed_revision` 且存在 pending：原样重发 pending，不重新编号、不用更新后的数据库内容改写同一 revision。
3. 请求 revision 等于 `confirmed_revision` 且没有 pending：读取完整权威向量；无变化则返回空，有变化则创建 `confirmed_revision + 1` 的 pending delta。
4. 请求 revision 为 0、落后于 confirmed、超前于 pending、跨 generation，或 master 缺失与该 revision 对应的向量：判定断档，废弃旧 pending，以 `max(request_revision, confirmed_revision, pending_revision)+1` 创建 full 响应。
5. master 只在下一次 heartbeat 看见 edge 已应用 revision 后推进 confirmed。发送成功、HTTP 连接关闭或 edge 本地事务失败都不能提前确认。

复制状态只在创建 pending、确认 pending 或 generation 变化时写入；稳态无余额变化时，除既有 heartbeat 观测更新外不新增余额复制写入或传输条目。

## Edge 本地账本

### 表结构

新增 `edge_local_balance_accounts`，统一保存三类账户，避免为钱包、token、订阅复制三套事务逻辑：

| 字段 | 语义 |
|------|------|
| `account_type`、`account_id` | 复合主键；`wallet/user_id`、`token/token_id`、`subscription/subscription_id` |
| `user_id` | 账户归属用户 |
| `replicated_quota` | 最近已应用 master revision 的绝对余额 |
| `unlimited_quota` | 不参与额度扣减；不绕过另一资金或 token 维度 |
| `reserved_quota` | active reservation 的本地预占总额 |
| `unsettled_quota` | 已完成并写入 usage event、但尚未包含在 master 余额水位中的消费总额 |
| `total_quota`、`next_reset_at_unix_milli`、`expires_at_unix_milli`、`allow_wallet_overflow` | 订阅专用字段，其他账户为零值 |
| `deleted` | tombstone 已应用；禁止新 reservation，但有本地 overlay 时暂不物理删除 |
| `balance_revision`、`updated_at_unix_milli` | 最近应用版本与更新时间 |

`edge_local_control_state` 增加 `balance_revision`、`balance_initialized` 和 `balance_settlement_sequence`。现有 `edge_local_quota_reservations` 保留 reservation/staged/finalized 状态机，但把 `lease_id` 替换为以下固定事实：

- `funding_account_type`、`funding_account_id`；
- `token_account_id` 与 `token_unlimited_quota`；
- `snapshot_id`、`snapshot_revision`、`pricing_revision`；
- `balance_revision`；
- `reserved_quota`、`charged_quota`。

`edge_local_usage_events` 与其 payload 保存同样的 funding/source、subscription ID、策略版本和 balance revision，供 master 结算与本地水位清理使用。现有 usage event、outbox、settlement block 和控制游标表继续承担 durable exactly-once 链路。

### 可用额度、Admission Floor 与 Settlement Floor

有限账户的实时可用额度定义为：

```text
available_quota = replicated_quota - reserved_quota - unsettled_quota
```

每个请求同时检查一个资金账户和一个 token 账户；token 为 unlimited 时只检查资金账户。正额度 reservation 后每个被扣减的有限账户都必须满足 `available_quota >= 0`，否则在访问 CPA 前返回额度不足。master 失联时，reservation 与 unsettled overlay 继续扣减最后同步的正余额；单个 edge 不能主动把余额消费为负数。

`EDGE_BALANCE_SETTLEMENT_FLOOR_QUOTA` 默认 `-10_000_000`（约 -$20），有效范围 `-common.MaxQuota..0`。它只用于已经执行上游请求后的最终结算：实际 charge 高于 reservation 时，只要结算后的有限账户仍不低于 Settlement Floor，就持久化真实 usage 并完成账务；账户变负后不再接受新的正额度 reservation。旧变量 `EDGE_BALANCE_NEGATIVE_FLOOR_QUOTA` 仅作为兼容别名。

免费请求仍创建零额度 reservation、usage event 和 settlement 序列，但不会修改有限账户余额。

### BillingSession 事务衔接

现有 `ReserveEdgeLocalQuota` 的幂等 reservation、staged settlement 和恢复思路保留，但底层从“移动 lease 的 remaining/reserved/consumed”改为“移动 balance account 的 reserved/unsettled”：

1. **PreConsume**：按用户的 `billing_preference` 选择钱包或订阅，再与 token 账户在同一 SQLite 事务增加 `reserved_quota` 并创建 reservation。
2. **Reserve(delta)**：正 delta 同时扩展资金账户与有限 token 的预占；负 delta 只回滚本次扩展。任一账户更新失败时整个事务回滚。
3. **Refund**：在没有 staged usage 时，把 reservation 的预占从两个账户原子释放，并把 reservation 标记为 refunded。
4. **Settle**：先原样 staged 精确 usage；随后在一个事务中从 `reserved_quota` 释放原预占，把实际 charge 加入 `unsettled_quota`，完成 reservation，写 usage event/outbox 并推进事件序号。实际 charge 高于预占时，额外部分必须通过 Settlement Floor；该下限不参与 PreConsume 或 Reserve(delta)。
5. **Recovery**：已 staged 的 reservation 只从 durable payload 重试，并在恢复完成前关闭全局 accounting readiness。无 owner、active 且未 staged 的孤儿 reservation 保留现场并隔离其用户与 token，不自动退款或猜测 charge；其他 subject 继续服务。数据库不可用、reservation 身份不完整或账务结构损坏仍全局 fail closed。该边界由 [ADR 0008](./adr/0008-quarantine-unstaged-accounting-by-subject.md) 固定。

当前 `EdgeUserSettingV1` 明确排除了 `billing_preference`，因为资金选择由 master 租约承担（`dto/edge_control_v1.go:321`）。删除租约后必须把规范化后的 `billing_preference` 加入低频用户策略快照；它不属于余额 dataset。订阅按 `end_at_unix_milli, subscription_id` 升序选择，严格保持 `subscription_only`、`wallet_only`、`wallet_first`、`subscription_first` 和 `allow_wallet_overflow` 的现有语义（`service/billing_session.go:428`、`service/billing_session.go:492`、`model/subscription.go:1309`）。

### 应用 diff 与避免覆盖本地消费

余额响应不能直接用 master 绝对值覆盖 edge 当前可用余额，否则“结算已落 master、心跳回冲期间又发生的新消费”会被覆盖。`SettlementAppliedThroughSequence` 用于区分已经进入 master 权威余额的本地消费：

1. master 在构造目标向量的同一事务读取节点 `LastEventSeq`，把该水位写入 delta。
2. edge 在一个 SQLite 事务应用 delta/full，并按 usage event 的账户归属汇总 `sequence <= SettlementAppliedThroughSequence` 的额度，从对应账户 `unsettled_quota` 中移除。
3. 对 delta 中出现的账户更新 `replicated_quota`；未变化账户保留原绝对值。full 中缺失的旧账户按 tombstone 处理。tombstone 立即禁止新 reservation，但存在 active reservation 或 unsettled usage 时保留账户行，overlay 清零后再清理。
4. `sequence` 高于水位的已完成消费和所有 active reservation 保持为本地 overlay，不会被 master 绝对余额覆盖。
5. 最后原子推进 `balance_revision` 与 `balance_settlement_sequence`。重复应用同一 revision 直接返回成功；base 不匹配时拒绝整批并在下一心跳请求 full。

## 结算闭环

### 保留内容

以下机制原样保留：edge staged usage、连续 event sequence、durable usage/outbox、单个 pending block、previous block digest、block digest、HTTP 幂等键、master block/event 唯一约束、ack 游标，以及 master consume-log outbox 与 billing event key。请求 ID 不进入 block canonical digest，允许安全 HTTP 重试而不改变账务内容（`pkg/edgesettlement/edgesettlement.go:26`）。

“原样保留 settlement block”指保留上述容器、顺序、摘要和 exactly-once 语义，不指保留强制 `lease_id` 的 v1 event 字节结构。v2 usage event 必须删除 `lease_id`，增加 `snapshot_id`、`snapshot_revision`、`balance_revision`、`funding_source`、`user_subscription_id` 和 `token_unlimited_quota`；否则 master 在删除 lease 表后无法确认资金来源或复核固定价格版本。现有 v1 校验强制 `lease_id`，master 也通过 lease 解析用户、token、快照和资金表（`dto/edge_control_v1.go:1907`、`service/edge/master_lease.go:399`、`service/edge/master_lease.go:419`、`service/edge/master_lease.go:444`）。

### Master 权威落账

master 对一个区块执行两遍处理：

1. 第一遍验证 block 链、事件唯一性、节点/代次、固定策略快照、用户/token/订阅归属并重新计算每个 charge；同时构造本区块的权威扣账明细。
2. 在通过节点滑动窗口熔断后，第二遍为每个事件建立独立保存点，实际扣减 `users.quota` 或增加 `user_subscriptions.amount_used`，并扣减有限 `tokens.remain_quota`。余额不足不拒绝已经发生的可信 edge 消费，权威余额允许为负；所有 quota 运算继续使用项目的 int32 安全边界。
3. 当前用户、token 或订阅已经缺失、改属、改变 quota 模式或超出安全数值范围时，只回滚该事件保存点并写入 Settlement Skip；当前订阅套餐标题缺失不阻断扣账。渠道和消费统计属于辅助投影，其状态差异只记录警告，不回滚权威余额与 usage。
4. 同一事务写 accepted block、已应用 usage、Settlement Skip 和 consume-log outbox，并按区块总事件数推进节点事件水位。重复区块只返回原 ack，不再次扣账或重复写 skip。

usage event 固定的订阅 ID 和快照 ID 都是账务外键，而不是可随生命周期清理的缓存键。已取消或过期的 `user_subscriptions` 行必须保留并排除在新 reservation 之外；所有曾发布的编译快照也必须继续可供 settlement 复核。当前协议没有全局“最老未结算引用”水位，因此不能仅根据订阅状态或快照 TTL 物理删除这些记录。

历史行应尽量保留，但系统可用性不再依赖所有当前业务表永久保留。若维护或历史操作已经物理删除引用，可信事件会形成 Settlement Skip；该 charge 仍参与节点滑动窗口，edge 在后续余额水位回冲时清除相应 unsettled overlay。

master 普通钱包请求已经在用户余额 `<= 0` 时拒绝，有限 token 也在 `remain_quota <= 0` 时失效（`service/billing_session.go:436`、`model/token.go:216`）。订阅 `remain_quota <= 0` 后不能满足下一次订阅预扣；若策略允许钱包回退且钱包仍为正，仍可按既有规则切换资金来源（`service/billing_session.go:524`）。因此负值会阻止对应钱包、订阅或有限 token 继续消费，而不会擅自改变既有 funding fallback 语义。

### 回冲时序

```text
edge 本地 reservation/settle
  -> unsettled_quota 增加 + usage/outbox 落盘
  -> settlement block 上传
  -> master exactly-once 扣权威余额并推进 LastEventSeq
  -> 后续 heartbeat 读取新权威向量和 LastEventSeq
  -> edge 原子应用 diff，并清除该水位以前的 unsettled overlay
  -> 本地 available_quota 收敛到 master 余额减去更新的本地未结算/预占
```

## 节点滑动窗口熔断

### 定义

master 使用以下配置：

| 变量 | 默认值 | 语义 |
|------|------|------|
| `EDGE_NODE_SETTLEMENT_WINDOW_SECONDS` | `300` | 每节点事件时间滑动窗口长度，范围 `10..86400` 秒 |
| `EDGE_NODE_SETTLEMENT_WINDOW_QUOTA` | `50_000_000` | 同一节点代次在任一窗口内允许受理的最大 charge，范围 `1..common.MaxQuota` |

窗口按 usage event 的 `finished_at_unix_milli` 计算，而不是按上传时间计算。master 先重新计算本区块全部 charge，再读取覆盖候选最早时间减去窗口长度到最晚时间的既有 accepted usage events，将两者按完成时间排序并用双指针检查每个闭区间 `(t-window, t]`。这样 master 宕机后的大批回放不会仅因为上传突发而误触发；如果历史上某个真实消费窗口本身超过阈值，恢复回放仍会触发熔断。

### 触发状态

任一候选窗口超过阈值时，整个区块拒收，不写 usage、不扣余额、不推进事件水位。节点保留 `status=active` 以继续心跳，但写入独立熔断字段：`settlement_circuit_open=true`、`settlement_circuit_opened_at`、`settlement_circuit_reason`、`settlement_circuit_epoch`。控制响应带 `settlement_circuit_open`，edge 立即关闭新 admission，但继续 heartbeat 和保留本地 outbox。

当前 `ExecuteControlMutation` 会对所有 4xx domain rejection 回滚 savepoint 内的权威状态（`service/edge/control_mutation.go:127`），因此 Phase D 必须增加仅限熔断的“提交节点熔断标记和拒绝 receipt、但不提交结算账务”结果类型，不能依赖普通 domain rejection 顺便更新节点。

### 恢复手续

熔断不自动恢复。管理员必须完成以下步骤：

1. 核对触发 block、事件时间窗口、节点本地 outbox 和实际流量，排除凭证泄漏或错误计费。
2. 必要时调整窗口阈值，但不得修改或删除 edge durable usage。
3. 通过节点管理接口清除 circuit 并递增 `settlement_circuit_epoch`；节点凭证和 generation 不变。
4. edge 在 heartbeat 看到新 epoch 后，为同一 durable pending block 生成新的 HTTP request ID 后重试。block ID、事件、序号和 digest 不变；request ID 本来就不属于 canonical digest（`pkg/edgesettlement/edgesettlement.go:26`）。旧拒绝 receipt 保留审计，不阻塞新的幂等键。

## 策略快照 TTL 与 readiness

数据面 readiness 改为同时满足：

- admission 开启；
- 至少成功验证并完整应用过一份鉴权/策略快照，且本地鉴权索引存在；
- 已成功应用 v2 full 余额集；
- accounting 可写、没有待恢复 staged settlement、没有人工介入锁；
- 节点没有被禁用、吊销或 settlement circuit 熔断。

快照 `expires_at` 仅用于控制新快照是否还能被下载和应用，不再参与每请求 `EdgeControlReady` 或 reservation 条件。过期后继续使用最后一份已验证策略，禁止把未验证或部分下载的新策略混入数据面。token 记录自身的 `expires_at_unix_milli` 仍按本地时间生效（`middleware/edge_token_auth.go:59`）。master 结算继续按事件固定的不可变快照复核；现有结算查询已经允许 published 或 retired 快照，不要求仍在 TTL 内（`model/edge_snapshot_query.go:34`）。

该取舍意味着 master 宕机期间的新 token 撤销、用户禁用、价格和渠道变化会被冻结，直到控制面恢复；这是 ADR 0004 明确接受的 consequence，不得用恢复租约或强制快照 TTL 停服规避。

## 删除与迁移清单

### Master/共享代码

- 删除 lease acquire/close 路由、controller、client 方法和 `ProcessLeaseAcquire`、`ProcessLeaseClose`；保留并改写 `ProcessSettlementBlock`（当前入口见 `router/edge-control-router.go:12`、`controller/edge_control.go:49`、`service/edge/control_accounting.go:16`）。
- 删除 `AcquireMasterQuotaLeaseTx`、`CloseMasterQuotaLeaseTx`、`RevokeMasterQuotaLeaseTx`、`ForceCloseMasterQuotaLeaseTx`、lease funding 选择、租约 TTL/上限/续租阈值和相关 cache invalidation。`master_lease.go`、`master_lease_helpers.go` 中仍有价值的结算链、价格复核和 usage 规范化逻辑迁移到不含 lease 命名的 settlement 文件。
- 删除 `EdgeQuotaLease`、`EdgeLeaseFunding` 模型和 `edge_quota_leases`、`edge_lease_fundings` 活跃表；`EdgeSettlementBlock`、`EdgeUsageEvent`、`EdgeConsumeLogOutbox` 拆到独立文件继续保留（当前混在 `model/edge_lease.go:63`、`model/edge_lease.go:115`）。
- 删除 `EdgeLeaseSubjectV1`、`EdgeLeaseAcquireRequestV1`、`EdgeQuotaLeaseV1`、`EdgeLeaseAcquireResponseV1`、`EdgeLeaseClose*`、`EdgeLeaseRuntimeStateV1`、lease status/error code，以及 heartbeat 的 `leases` 字段；v2 usage event 移除 `lease_id`。
- 删除所有 `EDGE_LEASE_*` 环境变量和节点 `MaxOutstandingQuota` 的租约含义；节点风险改由 settlement window 配置与 circuit 字段表达。

### Edge 本地代码

- `EdgeLeaseFunding` 改为 `EdgeBalanceFunding`，删除 singleflight、同步 acquire、renew、close 和所有 lease 查找；保留 `BillingSession` funding 契约和 staged settlement。
- 删除 `service/edge/lease_maintenance.go` 的补租/关租逻辑；staged recovery 移入独立 accounting maintenance loop。
- 删除 `DrainEdgeControl` 的 close lease 阶段，只保留 settlement flush（当前两个阶段见 `service/edge/drain.go:47`）。
- 删除 `EdgeLocalQuotaLease`、`EdgeLocalLeaseAcquireIntent`、`edge_local_quota_leases`、`edge_local_lease_acquire_intents`；reservation 的 `lease_id` 改为资金账户、token 账户、策略和余额版本字段。
- 删除 `LeaseRuntimeStates` store 接口和 heartbeat payload；新增 balance revision/apply 接口。

### 历史数据处置

切换前必须先用旧 v1 版本停止 admission、完成 staged recovery、上传全部 settlement、确认 outbox 全部 ack，并关闭所有租约以归还未用额度。master 迁移若发现 active/closing/revoked lease，必须中止 v2 启用，不能猜测消费或直接删除预留。

干净的 `edge.db` 升级时执行一次 SQLite 迁移：确认不存在 active reservation、staged usage、pending/in-block outbox 和 pending settlement block后，删除本地 lease/acquire-intent 表，重建 reservation/usage 表以移除 lease 字段，再通过第一次 v2 heartbeat full 余额集初始化账本。已 ack 的历史 usage/block 可以保留只读审计，也可以在备份后按现有清理策略删除；它们不得转换成新的可用余额。

若旧 edge 无法完成 drain，只允许恢复旧 v1 master 完成结算，或显式放弃该节点 generation 并人工核账。不得把含未确认账务的旧 `edge.db` 直接导入 v2。

## 待决问题与代码事实出入

以下问题已经技术负责人裁决，实施必须按裁决执行：

1. **接受批量更新扩大 diff 延迟，不关闭 batch update。** master 可启用 `BATCH_UPDATE_ENABLED=true`，此时用户和 token 余额先进入进程内 batch，默认 5 秒后才写主数据库（`internal/app/app.go:230`、`model/utils.go:33`、`common/init.go:108`）。heartbeat 只读取已写入数据库的权威事实，因此实际双花上界固定表述为“batch 延迟 + heartbeat 周期”；关闭 batch update 时 batch 延迟为零。
2. **“settlement block 原样保留”指保留机制，不保留 v1 event 字节结构。** 连续序号、链式摘要、幂等、exactly-once、durable outbox、ack 游标和 consume-log outbox 全部保留；v2 usage event 移除 `lease_id`，增加资金来源、订阅、token 限额、策略版本和余额版本字段。
3. **批准受限的 committed rejection 语义。** 熔断拒收结算时，在同一控制 mutation 中提交节点熔断标记和拒绝 receipt，但不提交结算账务、usage、事件水位或 consume-log outbox。该结果类型只允许用于结算熔断，不得把普通 4xx domain rejection 扩展为可提交状态。

## 验证门禁

- diff：新增、修改、删除、无变化零条目、pending 原样重发、ack 推进、revision 断档 full。
- 本地账本：钱包/订阅优先级、有限与 unlimited token、双账户零余额原子预占、退款、实际 charge 高于预占、Settlement Floor 边界，以及结算变负后禁止下一次正额度 reservation。
- overlay：结算受理后、balance diff 应用前继续消费，不得被绝对余额覆盖；重复 delta 不得重复清除 unsettled。
- 结算：重复 block 最多扣一次权威钱包/订阅和 token，最多写一次 usage、consume log 和 `quota_data`。
- 熔断：事件时间窗口边界、恢复期批量回放、触发后拒收且节点继续心跳、人工恢复后同一 block 使用新 request ID 成功。
- readiness：master 断开且策略快照 TTL 已过时，只要鉴权索引、余额副本和 accounting 正常，数据面继续服务到本地正余额耗尽；没有 full 余额集的旧 edge 明确失败。
