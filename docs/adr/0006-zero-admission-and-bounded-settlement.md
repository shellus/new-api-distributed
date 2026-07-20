# Edge 使用零余额准入与有界负余额结算

ADR 0004 使用同一个负余额下限控制新请求预占和最终结算，使 edge 可以在余额同步完成后仍持续把该下限作为信用额度消费。决定把两种语义分离：新 reservation 后每个有限资金账户和 token 账户必须保持非负；只有已经执行上游请求、且实际 charge 超过 reservation 时，才允许在独立 Settlement Floor 内完成 durable settlement。master 的 `PreConsumedQuota` 作为签名计费策略的一部分下发，edge 不再依赖自己的编译默认值。

## Considered Options

- 把现有负下限直接改为零：可以阻止透支 admission，但实际 charge 超过预扣时会留下无法完成的 staged settlement，并关闭整个 edge 的 accounting readiness。
- 继续保留单一负下限：结算最稳健，但负余额会成为持续可用的业务信用额度，而不是同步窗口风险。
- 恢复逐请求 master 授权或配额租约：可以限制多节点双花，但重新引入跨地域控制面依赖，并推翻 ADR 0004 的离线数据面目标。

## Consequences

master 离线时，edge 仍可消费最后同步的正余额，并用 reservation 与 unsettled overlay 防止同一节点重复使用额度；新请求不能主动把本地可用余额降到零以下。不同 edge 对同一份尚未回冲的正余额仍可能在 heartbeat 或 master 失联窗口内重复消费，该风险继续由余额 revision 和 settlement circuit 约束。

Settlement Floor 只保护已经发生的真实消费。结算把账户推为负数后，后续 reservation 会因 Admission Floor 为零而立即拒绝。签名计费策略新增可选的 `pre_consumed_quota`；滚动升级期间 master 必须在全部 edge 支持该字段后才开始下发。
