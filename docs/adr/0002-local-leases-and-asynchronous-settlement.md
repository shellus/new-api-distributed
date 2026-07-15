# Edge 使用本地配额租约和异步结算

master 与 edge 可能跨国家通信并间歇性失联，逐请求鉴权、查余额或共享数据库会把控制面延迟和故障放大到用户请求。决定由 master 预留并签发有限 quota lease，edge 本地鉴权、预占和结算，再通过 durable outbox 异步提交 settlement block。

## Considered Options

- 共享 master 数据库或 Redis：数据实时，但跨地域请求延迟和失联可用性不可接受。
- edge 复制用户余额：请求独立，但多个节点会同时消费同一余额并造成超卖。
- 每个请求向 master 授权：容易保持真值，但直接违背 edge 离线工作目标。

## Consequences

master 在 lease 签发时承担额度预留，断线风险受未关闭 lease 限制。edge 必须拥有完整鉴权索引和可靠本地账务事务；没有 lease 且无法联系 master 时，请求必须在访问 CPA 前失败。
