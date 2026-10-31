# Centrifugo 合成 WSS 接入

复用官方 Centrifugo 6.10.2 的 PostgreSQL 消费器、WSS 和 Go SDK 0.13.0；版本、下载包 SHA-256 与许可见 [versions.lock.json](versions.lock.json)。当前开发契约为 OpenAPI info.version=0.8.0。论坛、帖子和固定频道由每次订阅的 HTTPS 代理授权；文字会话的个人收件箱是 user-limited channel，不经过该代理。频道 JWT 已不再使用。早期撤权结果见 [验证记录](../../../docs/成员撤权与消费者故障验证-2026-10-28.md)。

P2 后端 A01–A08 已完成。当前完整验收与交接入口是 [P2 验收](../../../docs/P2后端验收与交接-2026-10-29.md)，本文件保留运行和协议约定；正式上线指标与同事前端页面签收单独跟进。

## 重跑验证

在 backend 目录、具备本机网络和 Docker 权限的 PowerShell 7 中执行：

```powershell
pwsh -File deploy/postgres/test.ps1 -GoExecutable go -TestTags 'integration,realtime' -CentrifugoExecutable '<已校验的centrifugo.exe路径>'
```

省略 CentrifugoExecutable 时查找忽略的 runtime/backend/tools/centrifugo-6.10.2/centrifugo.exe。先核对官方压缩包完整 SHA-256，没有自动下载或全局安装步骤。真实实时测试在第二个专用数据库 odysseia_realtime_test 运行，其他 integration 测试使用 odysseia_test。

新增的可选双节点验证使用官方 Valkey 9.1.3 镜像，按 [平台摘要锁定](images.lock.json)。下列命令包含原有全部测试，并为双节点场景建立第三个隔离数据库 odysseia_cluster_test：

```powershell
pwsh -File deploy/postgres/test.ps1 -GoExecutable go -TestTags 'integration,realtime,cluster' -CentrifugoExecutable '<已校验的centrifugo.exe路径>'
```

cluster 必须与 integration,realtime 一起使用。脚本叠加 [compose.cluster.yaml](compose.cluster.yaml)，不修改旧项目或 Docker 全局设置。Valkey 只发布本机随机端口，TLS 仅 1.3、default 用户关闭、应用键／频道限定随机 prefix，管理命令拒绝；健康检查通过秘密文件和 VALKEYCLI_AUTH 认证，不把密码放入 argv。进程为非 root，文件系统只读，缓存使用临时内存文件系统并关闭持久化。它是单实例合成夹具，不是生产 Valkey 高可用配置。

最终收尾验证增加 -TestTags 'integration,realtime,cluster,acceptance'。包含全部前述场景，另真实暂停消费 65 秒以覆盖 60 秒历史／凭据到期，验证多批积压恢复、七页 HTTPS 与实时合并；脚本构建 realtime-status 并验证其真实退出码，测试超时调整为 180 秒。普通标签不运行这段等待。

夹具使用随机本机端口、短期证书、独立随机连接密钥和代理凭据，限制运行目录 Windows ACL，不输出 DSN／JWT／秘密。Go 清理原生子进程，PowerShell 移除专用容器与网络；敏感文件仍保留在忽略且受限的 runtime/backend-postgres 与 runtime/backend-realtime。

## 配置与权限

[config.example.json](config.example.json) 的尖括号均为占位符，不是自动环境变量展开。用受限配置或部署秘密注入真实值；连接签名密钥和私有代理凭据分别对应 ODYSSEIA_REALTIME_CONNECTION_SECRET、ODYSSEIA_REALTIME_PROXY_SECRET，彼此不同，各用至少 32 字节随机秘密。删除旧 subscription_token 验签配置；旧频道 JWT 不属于当前接入。

forum／topic／channel namespaces 启用 subscribe_proxy_enabled，代理地址必须是校验证书的 HTTPS。示例通过 http.tls.server_ca_pem 指定 API CA。代理的 X-Odysseia-Proxy-Key 只放 http.static_headers；http_headers、emulated_headers 保持空，不能让客户端覆盖它。不安装系统 CA、不关闭证书检查，生产还应隔离内部路由并限制来源。

`inbox` namespace 使用 `allow_user_limited_channels`。客户端订阅 `inbox:#` 加上连接 JWT 的 `sub`（本地用户 UUID）。该频道不走订阅代理。`allow_history_for_subscriber` 只让当前订阅者读取 64 条／60 秒历史，供短恢复；权威历史仍是 HTTPS。不要打开 `allow_history_for_client` 或 `allow_subscribe_for_client`。第十项迁移不要求排空，但要先装上这个 namespace，再让 API 写会话。

连接 JWT 只认证本地用户，不能授权频道；客户端以无频道 token 的方式订阅，代理复用当前 GetForum／GetTopic 读取权限。data、meta、labels 不提供权限，API／数据库失败不默认放行。namespace 默认禁止客户端 publish／直接读 WSS history，正文写入与持久历史仍走授权 HTTPS。

[shared-engine.example.json](shared-engine.example.json) 是可合入完整配置顶层的 engine 片段，不是独立完整配置；占位符需要替换。两个节点采用相同的共享 prefix、账号、连接签名密钥、代理凭据、namespace 及消费者 name／num_partitions，并使用不同监听端口。消费名称相同，官方 advisory lock 才会按同一分区协调；不按节点自动加后缀。Redis engine 在这里连接 Valkey，启用 CA／主机名验证；服务器端另限制 TLS 1.3。移除 cluster 标签时仍运行原有 memory engine 夹具，业务接口不依赖 Valkey。

消费者使用独立非超级用户身份，只连接目标库并拥有：

```sql
GRANT USAGE ON SCHEMA odysseia TO odysseia_realtime;
GRANT SELECT, DELETE ON odysseia.realtime_outbox TO odysseia_realtime;
```

API 仍只有 outbox INSERT，不能修改成员或读取操作审计；消费者不能读取正文表或成员。outbox 载荷含正文，同样受存储加密、访问和保留要求约束。当前消费者采用 5 个逻辑分区与 20ms 轮询；第七项迁移移除热写路径的逐条通知，避免跨流提交被全局对象锁串行化；分区数量和 64 条／60 秒实时历史缓存仍只是本机模板；测试缓存缩为两条验证溢出，没有生产容量结论。

## 0.4.3 分区升级步骤

第六项迁移由数据库生成 partition：disconnect 为 0；publish 根据 scope_id 的规范 UUID 最后一个字节取模，路由到 1–4。相同作用域始终落在同一分区；不同作用域可能共享分区，不承诺跨分区全局顺序。控制与数据分开，使某个数据分区阻塞时仍可处理撤权；仍不构成故障撤权 SLA。消息表没有物理分区。

已有五项迁移的环境必须依次执行：

1. 暂停 API 和维护命令等全部 Outbox 写入者。
2. 保持旧消费者运行，使用 realtime-status 确认队列排空。
3. 停止全部旧消费者；不得混用 1 分区和 5 分区消费者。
4. 用维护身份执行第六项及后续第七项迁移；第六项遇非空队列会明确失败，不自动丢弃或改派积压。
5. 全部消费者统一设置 num_partitions=5，保持同一消费者 name 和 20ms 轮询，不再配置 partition_notification_channel。
6. 启动消费者，确认健康后恢复写入并检查积压。

降级同样先停写、排空、停消费者，再回退迁移与统一配置。第六项原本增加通知，第七项移除该可选优化，改由原生轮询消费；没有另写后台 worker。实际阻塞分区、独立数据投递及控制撤权测试与压测见 [记录](../../../docs/缓存分区与准入修复-2026-10-29.md)。

## 0.4.5 从六项升级到七项

先确认全部消费者仍为五个相同逻辑分区／同名消费者且保留 20ms 轮询，再在短维护窗口执行显式 migrate up。第七项只删通知触发器和函数，不改路由或待投递记录，因此不要求为这一步排空；DDL 取锁超过 5 秒会失败并回滚，可避开长事务后重试。旧消费者监听通知的配置可以在迁移后移除，期间仍会轮询。只回退第七项会恢复通知提交锁；若还需要通知唤醒，也须恢复可选消费者通知配置。

从五项及更早升级仍先遵守上节停写／排空／全体切换要求。20ms 只是轮询间隔，不是投递或故障撤权 SLA；低负载退步、七轮对照及迁移回归见 [通知锁修复](../../../docs/独立进程压测与通知锁修复-2026-10-29.md)。

## 前端接入

四个 bodyless POST 均使用现有 Bearer Access Token，响应为 no-store，完整定义见 [OpenAPI](../../internal/httpapi/openapi.json)：

| 路径 | 响应 |
|---|---|
| /v1/realtime/connection-token | token、expires_at（Unix 秒）。身份凭据最多 60 秒且不延长原认证。 |
| /v1/forums/{forum_id}/subscription | 当前可读论坛的 channel。 |
| /v1/topics/{topic_id}/subscription | 当前可读帖子的 channel。 |
| /v1/channels/{channel_id}/subscription | 继承论坛成员的固定产品频道描述符。 |

1. 用连接 token 连接配置好的 wss:// 地址，不放 URL 或日志。订阅返回的 channel，不传频道 JWT；每次订阅和恢复都会回调代理检查当前成员。
2. SDK 请求连接刷新时重新调用连接凭据端点；401 结束认证，404 不再访问目标。代理权限拒绝码为 103；数据库失败暂时拒绝，不当成空历史。
3. 收到 4001／permissions changed 后 SDK 可重连，有权频道重新订阅；失去权限的频道不得继续重试取正文。
4. SDK 的 epoch／offset 用于短期恢复，业务事件 id 用于去重，scope_id／position 用于合并。offset 不是业务分页游标。
5. recovered=false 或缓存／进程状态丢失时，重新加载经授权的 HTTPS 历史，再合并缓冲事件；按消息 ID 去重。信封 position 与消息体 sequence 对应，均为字符串。真实重启／同 ID 重放结果、完整字段和合并顺序见 [事件接入契约](../../../docs/实时服务重启与事件接入契约-2026-10-29.md)。当前只有创建事件，未来编辑／删除需变更同步机制。

0.3.0 的两个 /subscription-token 路径已替换为 /subscription，没有旧频道 JWT 回退路径。此变更发生在开发切片，未声称已完成前端联调。

## 维护命令

先应用全部八项迁移，再由维护身份设置以下环境变量，运行 go run ./cmd/remove-forum-member：

| 变量 | 含义 |
|---|---|
| ODYSSEIA_MIGRATION_DATABASE_URL | 独立维护账号，verify-full。 |
| ODYSSEIA_OPERATION_ID | 本次操作的非零规范 UUID；重试使用同一个。 |
| ODYSSEIA_FORUM_ID／ODYSSEIA_FORUM_MEMBER_ID | 论坛、本地用户的非零规范 UUID。 |
| ODYSSEIA_OPERATOR／ODYSSEIA_CHANGE_REASON | 操作人标签和原因，分别非空且最多 200／500 个 Unicode 字符。 |

成员删除、操作审计与 disconnect 同事务提交；相同操作 UUID 的不同内容冲突失败。命令成功表示已提交并排队，不能当成全客户端完成的确认。当前断开该用户的全部实时连接，其他仍有权频道短暂重连；不按帖子逐条扫描或调用网络。直接 SQL 删除成员不经过此流程，不能作为运营撤权入口。

## 验证范围

已验证持久消息投递、SDK 恢复、缓存外 HTTPS 加载、当前代理鉴权、两台已有连接主动撤权、迟到订阅响应、旧 JWT 重订阅拒绝及另一个用户不受影响。消费者 SELECT 故障后补投、DELETE 失败后真实重试去重、成员操作 12 路幂等与失败回滚均通过；连接到期由忽略刷新的原始 WSS 客户端验证。

2026-10-29 新增原生进程两次真实停止／同地址重启：SDK 自动重订阅、epoch 变化、积压补投及 HTTPS 补历史通过；失去内存去重缓存后实际收到同 event ID／内容的重放，正文仍唯一。跨重启可能重复是已复现行为，不能按同进程临时去重推断全程只投递一次。

同日双节点共享 Valkey 验证也已通过：六条消息在两节点有序同 ID 投递，两个消费者均实际重试同一未确认事件且没有重复广播；一个节点停止时另一节点继续投递，显式换节点和原地址重启都恢复同一 epoch 的遗漏消息。跨两节点的三个已有连接被定向撤权，旧 JWT 重订阅以 103 拒绝，另一成员继续收取消息。命令、来源和限制见 [双节点记录](../../../docs/双节点实时与共享代理验证-2026-10-29.md)。

主动断连在 outbox 消费后发生，存在异步窗口；故障／积压会延迟。未刷新连接会按凭据到期关闭，但持续刷新的已有连接不能据此获得统一撤权上限；正式故障 SLA 待验。共享短期幂等缓存也不保证缓存丢失或超期无重复，客户端始终按事件 ID 去重。当前真实 65 秒中断、历史过期、七页补读与实时合并通过；小时级积压、缓存高可用、网络分区、乱序注入、生产集中告警和容量在 P6 验收。第五项迁移拒绝不合法命令，realtime-status 以独立观察账号读取聚合；具体权限、退出码和永久错误处置见 [运行手册](../../../docs/实时积压检查与故障处置.md)。

Go API／JWKS／Go 数据库客户端要求 TLS 1.3，WSS 测试也要求 TLS 1.3 并验证 CA／主机名。Centrifugo [原生 TLS](https://github.com/centrifugal/centrifugo/blob/v6.10.2/internal/configtypes/tls.go)最低为 1.2；生产严格 TLS 1.3 网关与网络隔离尚未完成。真实 IdP、存储／备份加密、密钥管理和完整管理员审计也未验收。

## 0.5.0 模式与产品频道升级

第八项增加产品频道和 chat 帖子模式。恢复写入前，全部消费者必须先认识 channel 命名空间并开启同样的订阅代理／history／recovery；模板已有该配置。维护身份运行显式迁移并授予运行角色 SELECT、INSERT ON odysseia.channels。五分区及轮询不变，原积压不会被重分配；第六项原停写／排空屏障仍适用于更早版本。已有 P3 数据／事件时禁止直接 Down，不自动删除或改模式。[完整顺序和验收](../../../docs/P3讨论模式与固定频道-2026-10-30.md)。

## 0.5.1 兼容边界

本版仅合并数据库单请求写批次并调节 API 写预算；仍为 v1 事件信封、五消费分区与八项迁移，既有 channel 命名、代理授权和恢复契约保持。升级不要求重排分区；第六／七／八项迁移的原升级屏障仍须遵守。Outbox 为零不能证明客户端签收，原版长时压测的一轮投递缺口及当前诊断见 [整体对照](../../../docs/整体优化与写预算对照-2026-10-30.md)。
