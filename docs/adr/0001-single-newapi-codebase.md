# Master 与 Edge 使用同一个 New API 派生仓库

流量分布需要保留 New API 的鉴权、渠道、协议和计费语义，同时尽量跟随上游更新。决定保留上游默认 master 入口，并在同一个 Go module 中增加 edge 编译入口；两种运行时直接复用相同 package，不维护两个源码副本。

## Considered Options

- 两个独立 New API fork：职责直观，但相同修改会复制并逐渐漂移。
- CPA 插件直接提供完整 edge：部署轻，但会重新实现 New API 业务语义且缺少财务级 usage 持久性。
- 自写独立 gateway：边界自由，但最终会形成第二套协议、策略和计费网关。

## Consequences

公共启动流程需要从根入口提取为可复用 application package。edge 的新增代码集中在运行模式、同步、租约和 outbox；默认 master 行为必须保持上游兼容。
