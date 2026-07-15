# Edge 拥有节点运行配置，Master 拥有业务配置

逐节点在数据库中维护 CPA 地址、公开地址和渠道映射会使轻量部署复杂化。决定由 master 统一管理用户、渠道、模型和计费等业务配置；edge 自己管理公开地址、CPA 凭证和运行环境，并通过统一 CPA 服务别名生成本地渠道投影。

## Considered Options

- master 为每个 edge 编辑 CPA 地址和公开地址：集中可见，但产生大量节点特例和人工同步。
- edge 数据库手工维护 channel ID 映射：灵活，但难以重装、批量部署和自动恢复。
- authenticated edge 上报地址后由 master 人工审批：多一层状态，但在 edge 已被完全信任的前提下不增加实质安全性。

## Consequences

已认证 edge 对自己的公开地址具有最终声明权，master 的主动探测只提供可达性和延迟。CPA 使用标准 Docker 内部地址；edge SQLite 由同步流程自动生成，不作为人工配置源。节点失去信任时吊销凭证或禁用整个节点。
