# Master 数据切换与回滚

本文描述从上游兼容的原版运行时切换到分布式项目 master 入口时的演练、生产切换与回滚流程。所有环境路径、镜像标签、数据库账号和 Compose 项目名只写入私有 `.env`，不得提交到 Git。

生产执行步骤见 [原版 New API 切换至 newapi-distributed master SOP](./original-newapi-to-distributed-master-sop.md)。

## 结论

当原版和 master 使用同一套 PostgreSQL、Redis 和 `/data` 时，不存在“全量迁移”或“增量迁移”步骤。切换本质上是：

1. 停止所有数据库写入者；
2. 由 master 在原数据库上执行向前兼容的 Schema 迁移；
3. 用固定镜像 ID 启动 master；
4. 必要时用冻结的原版镜像重新启动。

因此它是同库原地升级，不是数据复制。新增表和新增列在回滚后继续保留，原版会忽略它们。

当前脚本按主业务库和日志库共用同一 PostgreSQL 的部署设计。预检检测到独立 `LOG_SQL_DSN` 时会拒绝执行，需要先为独立日志库补充单独的备份、Schema 和一致性检查。

只有在 master 使用另一套数据库时，才需要 `pg_dump`/`pg_restore` 全量复制。项目目前没有 CDC、双写或逻辑复制接入点，不能把“两套独立数据库”描述成可无缝追平的增量迁移。

## 已完成的克隆数据演练

使用在线 PostgreSQL 的 custom-format 一致性逻辑快照，在隔离网络和隔离数据目录中依次完成了：

- 冻结的原版镜像读取克隆数据并通过健康检查；
- master 启动并完成 Schema 迁移；
- master 停止后重新启动冻结的原版镜像；
- 使用生产同构 Compose 再次执行正式切换脚本的 `cutover` 和 `rollback`。

首轮测得原版启动 8 秒、切换 master 11 秒、回滚原版 7 秒。该时间只代表本次主机和数据快照，不是生产 SLA。

原版、master、回滚原版三个阶段的以下逻辑指标完全一致：用户、令牌、渠道、消费日志、额度统计、订阅和系统选项。生产同构脚本演练中的切换前、master 候选、回滚前、原版候选四份指标也完全一致。

master Schema 检查确认：

- 16 张 edge 控制、快照、租约和结算表全部存在；
- `logs.billing_event_key` 及其唯一索引存在；
- `quota_data.model_name` 从 64 扩展到 256。

回滚原版后，原版的 AutoMigrate 会把 `quota_data.model_name` 缩回 64。本次数据中的模型名没有超过该长度，因此回滚成功。正式回滚前必须重复检查这一条件。

## 脚本

脚本位于 `ops/master-switch/`：

- `drill.sh`：克隆在线数据库，在隔离环境中执行原版、master、原版的完整演练；
- `cutover.sh`：执行生产候选验证、停写快照、正式切换和受门禁保护的回滚；
- `db-audit.sh`：复用业务指标、master Schema 和 edge 回滚阻塞项检查；
- `docker-compose.image-override.yml`：只替换 `new-api` 镜像，明确清除原 Compose 的 `build` 配置，并在首次切换时关闭分布式控制面。

`drill.sh` 支持两种构建模式：

- `docker`：使用项目 Dockerfile 完整构建，正式发布优先使用；
- `overlay`：在宿主机完成前后端和 Go 构建，把新二进制叠加到冻结原版的相同运行时基底。它适合镜像仓库短时不可用时继续做数据兼容演练，但正式发布仍需固定镜像 ID 并完成同等测试。

私有演练环境文件准备好后执行：

```bash
SWITCH_ENV_FILE=/private/drill.env ops/master-switch/drill.sh all
```

也可以逐步执行 `clone`、`start-original`、`switch-master`、`rollback` 和 `report`，便于在每个门禁处人工检查。

## 生产切换保护

`cutover.sh cutover` 的顺序是：

1. 校验私有环境文件权限、Compose override、数据库、磁盘空间和 master 镜像 ID；
2. 将当前正在运行的原版容器镜像冻结到独立标签并记录真实镜像 ID；
3. 停止配置的全部数据库写入服务；
4. 在停写状态生成 custom-format PostgreSQL 快照，并验证归档目录和 SHA-256；
5. 采集业务逻辑指标；
6. 启动没有正式服务网络别名的 master 候选容器；
7. 验证候选容器镜像 ID、健康状态、Schema 和业务指标；
8. 候选验证通过后才重建正式 `new-api` 服务并恢复配套服务；
9. 任一步失败时自动尝试恢复冻结的原版镜像。

生产切换命令需要两个显式确认：

```bash
CONFIRM_PRODUCTION_SWITCH=YES \
MASTER_PARITY_APPROVED=YES \
CUTOVER_ENV_FILE=/private/cutover.env \
ops/master-switch/cutover.sh cutover
```

首次切换强制 `EDGE_DISTRIBUTED_ENABLED=false`。先验证 master 作为上游兼容入口稳定运行，再单独启用 edge 控制面和边缘节点。这样首次回滚不涉及远端未上报用量。

## 回滚门禁

普通回滚复用同一数据库，不恢复备份。脚本先停止写入，再用不公开的原版候选容器验证兼容性和业务指标，最后重建正式服务。

回滚必须同时满足：

- `quota_data.model_name` 当前数据最大长度不超过 64；
- 没有非终态 edge 租约；
- 没有 `reserved` 资金预留；
- 没有未发布的消费日志 outbox；
- 没有 `processing` 状态的 edge 请求回执；
- 如果数据库中已经存在 edge 节点，已在外部完成停止准入、等待在途请求结束和结算 drain，并显式确认。

```bash
CONFIRM_PRODUCTION_ROLLBACK=YES \
CUTOVER_ENV_FILE=/private/cutover.env \
ops/master-switch/cutover.sh rollback
```

一旦真实 edge 流量已经发生，不能只看 master 数据库中的计数：远端节点本地仍可能有未上传 outbox 或在途请求。必须先从流量入口停止 edge 准入，等待所有节点完成上传和确认，再允许回滚。

数据库快照是异常恢复的最后保险，不是普通回滚的默认步骤。只有同库数据已损坏、镜像回滚也无法恢复时，才应在完整停写和人工确认下重建数据库并执行 `pg_restore`。

## 正式切换前仍需完成的门禁

数据和 Schema 兼容演练已经通过，但当前不能仅凭该结果直接切生产。还需要：

- 将现网原版分支中的审计对接、对话归集候选字段、Seedance 时长计费与视频直链三个本地提交完整同步到 master，并做功能回归；
- 发布一个不可变的 master 镜像，记录其镜像 ID，并让私有环境文件中的 `EXPECTED_MASTER_IMAGE_ID` 精确匹配；
- 确认生产 Compose 中所有会写数据库的配套服务都列入 `QUIESCE_SERVICES`；
- 在启用 edge 之前，把实际 CPA 池（包括 VIP 池）纳入快照编译配置并完成端到端验证；
- 确认停写窗口、维护通知、监控和人工负责人。

`MASTER_PARITY_APPROVED=YES` 是故意设置的人工门禁：上述功能对齐和发布验证没有完成前，不应设置它。
