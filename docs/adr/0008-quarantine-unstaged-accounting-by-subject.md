# 未 staged 的不确定账务按 Subject 隔离

同步请求已经完成上游执行、但精确 usage event 仍无法写入 staged settlement 时，reservation 中已经持久化了用户、token、资金来源和预占额度，但最终实际用量不可再推断。决定保留该 reservation 等待人工核查，并同时隔离其用户与 token；其他 subject 继续使用同一 edge。只有数据库不可用、reservation 身份不完整、账务结构损坏或 durable staged settlement 尚未恢复时，才关闭全局 accounting readiness。

## Considered Options

- 继续全局永久 fail closed：账务边界最简单，但单个用户的一条不确定 reservation 会让整个节点永久 `503`。
- 自动退款或按预占额结算：可以自动恢复服务，但会猜测已经执行的上游结果，可能造成免费消费或错误扣费。
- 按用户与 token 隔离：不猜测账务结果，保留原 reservation 和预占现场，同时把服务能力损失限制在相关 subject。

## Consequences

运行时失败会立即登记 reservation quarantine；重启时由所有无 owner、active 且未 staged 的 reservation 重建 quarantine。被隔离用户或 token 的新请求在访问 CPA 前返回服务不可用，`/readyz` 不因单纯的 subject quarantine 失败。维护循环只在对应 reservation 被人工处置、进入 staged/finalized 状态或删除后解除隔离，不自动退款、补记 usage 或清除现场。
