# Prompt Safety Policy 通过签名业务快照下发

edge 与 master 共用请求 relay，因此会执行提示词敏感词检查；但 edge 不初始化 master Option，若不显式同步策略，只会使用编译默认词表。决定把 Prompt Safety Policy 作为现有 `routing` 单记录聚合中的可选类型字段，由 master 编译进签名 Business Snapshot，edge 在策略写锁内与渠道亲和策略一起安装。

## Considered Options

- 在每台 edge 的环境变量或本地文件中维护敏感词：实现简单，但形成多份事实来源，后台 Option 更新无法自动收敛。
- 下发完整或任意 Option map：可以复用后台配置，但扩大控制面权限边界，允许无关甚至敏感配置进入 edge。
- 新增第八个快照数据集：语义最独立，但会同时改变 manifest 基数、数据集顺序、分页和旧 edge 校验，滚动升级面明显更大。
- 在现有 `routing` 聚合中增加可选类型字段：保持七数据集协议和签名、持久化、原子切换链路不变，同时限制只传递请求安全所需字段。

## Consequences

master 是 Prompt Safety Policy 的唯一事实来源；edge 不读取本地 Option，也不人工维护词表。字段包含敏感检查总开关、提示词检查开关、命中处理开关和词表，并受数量、单项字节数及总字节数限制。

由于 edge 对控制响应使用严格 JSON 解码，新字段由 `EDGE_PROMPT_SAFETY_SNAPSHOT_ENABLED` 控制。部署时先保持关闭并升级全部 edge，确认健康后再启用 master 发送。字段缺失表示旧协议语义，edge 安装编译默认策略；字段存在时，edge 在数据面策略写锁内原子替换完整策略，请求不会跨策略切换混用开关和词表。
