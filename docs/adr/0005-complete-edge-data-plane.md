# Edge 承载完整数据面并使用通用计费收据

首期 edge 只注册 Chat Completions 和 Responses，并在 relay 前使用请求形状白名单拒绝图片、音频、内置工具、阶梯计费、异步任务和 Realtime。这些限制保护了首期结算协议，但也让同一份 New API relay 代码在 master 与 edge 上表现不同；每增加一种请求格式都需要维护额外的 edge 例外，最终会形成第二套网关边界。

决定把 edge 的能力边界改为完整数据面：master 与 edge 从同一个路由注册表暴露 New API 已实现的用户侧 AI API，复用同一套请求解析、协议转换、渠道选择、上游执行和计费代码。edge 不同步调用、代理或回退到 master；master 只保留策略、余额、结算和统计权威职责。master 尚未实现的占位路由不属于完整数据面。

请求路径或请求体形状不再作为 edge 能力白名单。模型和渠道是否可用由签名业务快照、共享 relay/adaptor 和本地 CPA 能力共同决定。新增上游请求字段或新的合法内容类型，只要共享 relay 能处理且计费能形成收据，就不需要增加 edge 专用放行规则。

异步结算使用通用 Billing Receipt。收据固定价格策略身份、归一化 usage、请求派生计费因子、阶梯表达式结果和最终 charge 所需输入；不携带用户 prompt、完整请求体或上游凭证。master 依据对应快照中的价格策略和共享计算器复核策略身份、输入范围、表达式哈希与计费算术。请求条件本身和 provider usage 一样属于可信 edge 上报的事实，master 不要求重放原始用户请求。

## Considered Options

- 继续扩展请求白名单：单次改动小，但每种新工具、模态或字段都需要 edge 专用维护，master 与 edge 会持续漂移。
- edge 遇到不支持的请求时回源 master：可以快速提高兼容性，但把跨地域延迟和 master 故障重新放回用户请求路径，也违背 edge 独立数据面的目标。
- 为 edge 复制一套完整 router 和计费分支：可以绕开现有边界，但会形成第二套网关实现，增加上游合并和财务一致性风险。
- 在结算事件中携带完整请求体供 master 重放：可以复核任意请求条件，但会扩大敏感数据面、outbox 体积和长期存储责任。

## Consequences

路由注册必须拆成共享的数据面注册表，master 与 edge 只注入不同的鉴权、admission 和运行模式中间件。edge 专用 `EdgeTextBoundary` 不再参与请求处理，模型投影也不再按文本模型或固定 endpoint 枚举裁剪。

业务快照必须表达完整计费策略，包括多模态 token 倍率、固定价格请求倍率、工具调用价格和 tiered expression。所有完成计费的同步、流式、Realtime 和任务请求都必须在本地先持久化 Billing Receipt，再更新余额 overlay 和进入 settlement outbox。

异步任务状态由提交请求的 edge 本地持久化和轮询；用户查询不依赖 master。任务产生的最终收费、退款或差额仍通过同一有序结算链上报，master 只负责权威账务和统计，不参与任务执行。

完整数据面会增加 edge 本地数据库迁移、后台任务和测试范围。以后上游新增已实现的用户侧 AI 路由时，默认应同时出现在 master 与 edge；只有缺少可持久化计费事实或必须依赖中心私有状态时，才允许通过新的 ADR 明确排除。
