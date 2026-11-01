# PostgreSQL TLS 集成测试

使用独立 Compose 项目 odysseia-backend-test，只创建合成测试身份与数据。与旧 NodeBB／IM 数据库隔离，不复用它们的容器、配置、密码或卷。

使用 PostgreSQL 18.6 官方镜像，以 SHA-256 摘要固定；[images.lock.json](images.lock.json) 记录官方来源、索引／平台摘要和已核对的实际 image ID。

## 测试内容

- P5-a 第十四项迁移：作者私有作品草稿与最小列级授权；两个独立存储预算下 12 路幂等创建、12 路 revision 更新只有一次成功、无操作不改时间、编辑后创建重放、隐藏读取、真实 HTTPS／OpenAPI、游标、共享写预算及有数据 Down 拒绝均有集成验证。见 [P5-a 记录](../../../docs/P5-a作品私有草稿-2026-11-01.md)。

- 客户端验证 CA 与主机名，协商 TLS 1.3。
- 不可信 CA、主机名不匹配被拒绝，数据库服务端拒绝明文 TCP。
- 打开运行时连接池不会创建 schema 或迁移版本表。
- 两个迁移执行者首次并发运行，每个迁移只应用一次；再次运行不重复执行。
- 运行时身份不能创建 schema，迁移使用单独身份。
- 12 个并发 Ensure 只产生一个本地 UUID，不同 issuer 的同名 subject 不合并；运行身份可以 INSERT 用户，不能 DELETE。
- HTTPS 请求经过真实 RS256 Access Token 验签；第一次有效令牌插入本地用户，再次请求返回同一 UUID。
- 无作品记录的成员论坛：一层回复与同帖引用、数据库约束、12 路并发幂等、8 路不同回复序号、越权和撤销后新请求拒绝。
- 强制 outbox 插入失败后正文、帖子、消息流和计数均回滚；HTTPS 论坛响应对照 OpenAPI，页间插入和跨作用域游标也有验证。
- 可选 integration,realtime 标签在第二个隔离库启动真实 Centrifugo：验证投递／恢复／缓存外加载、当前权限代理、两个已有连接的主动撤权、迟到订阅响应、连接过期、消费者短期故障补投及真实重试去重；见 [实时运行说明](../realtime/README.md)。消费者使用独立最小授权角色。
- 第四项迁移创建维护操作审计；12 路移除成员幂等、API 身份拒绝和强制 outbox 失败的完整回滚有真实数据库验证。
- 第五项迁移增加 outbox 命令约束和只读聚合视图，十类不合法命令被数据库拒绝；可选 acceptance 标签使用独立观察角色验证聚合读取、底表读取／删除拒绝和编译后 realtime-status 的 0→2→0 状态。
- 实时标签还会两次停止并同地址重启官方原生服务，验证 SDK 自动重订阅、epoch 改变、积压保留及同 ID 事件重放；PostgreSQL 在本轮不重启。结果和事件字段见 [重启记录](../../../docs/实时服务重启与事件接入契约-2026-10-29.md)。
- 可选 integration,realtime,cluster 标签在第三个隔离库和一个临时 Valkey 中运行两个原生实时进程，验证共享广播、两个消费者的真实重试去重、节点停止后的继续投递／恢复及三个连接的跨节点撤权。详见 [双节点记录](../../../docs/双节点实时与共享代理验证-2026-10-29.md)。普通 integration 配置不加载该 overlay。

2026-10-28 上述真实 PostgreSQL 集成测试已通过，测试容器与专用网络已清理；该结果仅覆盖列出的范围。

2026-10-29 全部 integration,realtime,cluster 测试通过，两个原生实时子进程、专用 PostgreSQL／Valkey 容器和网络已清理。此轮没有重启数据库或验证缓存服务高可用。

P2 收尾命令为 -TestTags 'integration,realtime,cluster,acceptance'，须包含四个标签；会实际暂停消费 65 秒，Go 测试超时为 180 秒。全套已通过，覆盖真实缓存过期、未刷新身份连接到期、240 条消息＋断连记录保留、页间再插入后的 241 条补投及七页 HTTPS 合并，详见 [P2 验收](../../../docs/P2后端验收与交接-2026-10-29.md)。资源同样自动清理，未验证数据库重启或缓存高可用。

0.4.3 新增当前权限检查后的正文页缓存验证：跨成员共享内容、调用者修改隔离、其他实例追加后未满页刷新、闭合前缀复用和热缓存撤权拒绝。第六项迁移将 Outbox 改为 1 控制＋4 数据逻辑分区并增加事务通知；非空队列迁移失败屏障、非法命令与生成分区不可覆盖均有验证。cluster 标签还真实锁住一条消费确认，验证另一个数据分区和控制撤权继续推进，见 [最新报告](../../../docs/缓存分区与准入修复-2026-10-29.md)。

## 运行

要求 Windows PowerShell 7、Docker Desktop 的 Linux 引擎、Git for Windows OpenSSL 和 Go 工具链。Go 不在 PATH 时传入其完整路径。在 backend 目录执行：

```powershell
pwsh -File deploy/postgres/test.ps1 -GoExecutable go
```

脚本会检查专用项目没有现有容器，生成本轮测试资料，启动数据库，运行带 integration 标签的测试，最后停止并移除本轮专用项目。若专用项目已经存在，会明确退出，不重置该项目；磁盘档还拒绝已有专用项目卷，确保每轮新数据。其他项目不在操作范围内。可加 -VerboseTests 保留非性能回归的逐测试阶段日志。

单独测量本地 API 基线时运行：

```powershell
pwsh -File deploy/postgres/test.ps1 -GoExecutable go -TestTags 'integration,performance'
```

该标签只选择 TestPerformanceBaseline：预置 28225 条 1KiB 合成消息，使用真实 HTTP/2／TLS 1.3、RS256 验签和业务 SQL，分别以 1／8／32 并发测首页、深分页、分散写与热点写。超时为 180 秒，热身不计入样本，JSON 结果写入受限且忽略的 runtime/backend-postgres/performance.json。负载结束后另用单连接池采集无引用／带引用各 16 次串行探针，每类预热 1 次；共另写入 34 条消息，记录到 write_samples，不计入 API 吞吐和探针前的 outbox_pending。客户端和 API 同进程、数据库为临时内存存储；首次 [性能记录](../../../docs/本地性能基线-2026-10-29.md)保留历史范围，首轮短事务实现及六轮对照见 [热点优化](../../../docs/热点写入优化与对照-2026-10-29.md)。功能／故障验证继续使用它们对应的标签；此场景不启动实时消费者，也不测 WSS 广播。当前 JSON 的 succeeded／capacity_rejected 分开记录成功与符合 code／Retry-After 契约的写容量拒绝，errors 仍记录协议／传输等异常；吞吐只计成功，延迟包含全部已发出请求。无退避的短突发可能迅速收到大量 503，不能用其低延迟解释为成功写入提速。

运行持续混合档：

```powershell
pwsh -File deploy/postgres/test.ps1 -GoExecutable go -TestTags 'integration,performance' -PerformanceProfile mixed
```

该档选择 TestPerformanceMixedLoad，超时 240 秒，输出 runtime/backend-postgres/performance-mixed.json。普通读／写固定 100／25 次每秒，分别使用 32／8 个客户端工作者，未能发出的计划请求记 dropped；热点另用 32／128 工作者闭环施压。每组合预热 3 秒、计量 20 秒，采样使用独立运行角色连接、97ms 间隔的聚合活动视图。共有普通前后控制、纯热点和两种混合阶段；0.4.1 准入取舍见 [前轮记录](../../../docs/混合负载与写入准入-2026-10-29.md)，0.4.2 的局部准入和重复对照见 [原因分析](../../../docs/按流写入隔离与原因分析-2026-10-29.md)。性能档仅运行指定 postgres 测试；功能／故障档继续运行各自全部包。

混合档的 write_timings／warmup_write_timings 按合成流量类型记录处理器全程、处理器开始至 BEGIN 发起、BEGIN 发起至 COMMIT／ROLLBACK 返回的分位数。事务前时长包含身份查询、权限预检查、准入与取连接，不能直接命名为纯排队时间。标记只在测试包装器／pgx tracer 中生效，不采集正文、SQL 参数、用户或资源标识；最终夹具逐请求核对区间非负及不超过处理器全程，输出 clock_samples／invalid_clocks，非法区间使测试失败。聚合分位数不能相减求另一阶段的分位数。

运行多个热帖与真实 WSS 扇出档：

```powershell
pwsh -File deploy/postgres/test.ps1 -GoExecutable go -TestTags 'integration,realtime,performance' -PerformanceProfile fanout
```

该档选择 TestPerformanceHotTopicsFanout，超时 420 秒，输出 runtime/backend-postgres/performance-fanout.json。使用隔离的实时数据库、原生 Centrifugo 6.10.2 和 SDK 0.13.0；4 个热帖各 128 个独立身份／WSS 连接，或 16 个热帖各 64 个连接。每帖分别计划 25／100 次写入每秒，32 个写工作者；普通帖保持读 100／秒、写 25／秒与原工作者数。每档预热 3 秒、计量 12 秒；输入按到达率独立调度，未发出需求和 HTTP 503 分别保留。

实际请求轮换主楼、引用主楼的子回复、引用子回复的子回复，均为当前 threaded 业务。按发起请求核对数据库作者／顺序／正文／根楼及引用，再以规范化载荷、事件 ID 和消息序号核对每个订阅者；发送到接收采用同一个 Go 单调时钟。PUB/SUB 组件 channel 不等于 P3 产品固定频道，此档没有实现或验收即时式帖子与固定即时频道。

停止发起后先等 HTTP 结束、核对数据库，再最多等 30 秒收敛 outbox 和客户端回调；delivery_drain_ms 只计最后这段等待。报告完整性不代表所有计划输入都成功，须同时查看 dropped、failures 和普通帖结果。接收分位数只统计实际收到的消息，并同时列出目标／收到／缺口。观察账号只读既有聚合视图，API 权限不扩大。场景结束立即释放接收映射，JSON 只保留聚合数据。完整结果、硬件和限制见 [多热帖报告](../../../docs/多热帖与实时扇出压测-2026-10-29.md)。

证书两天有效，密码随机生成；CA、私钥、bootstrap SQL 和 fixture.json 位于忽略的 runtime/backend-postgres。Windows ACL 限制为当前用户、SYSTEM 和 Administrators；CA 不安装进系统信任库，CA 私钥不挂载进数据库容器。

## 夹具边界

默认 PostgreSQL 数据位于临时内存文件系统，项目停止后数据消失。-DatabaseStorage disk 则叠加 compose.disk.yaml，使用全新专用 Docker 卷，finally 清理容器、网络和本轮卷；不复用业务数据库。它是 Docker Desktop 虚拟机上的磁盘路径，仍受缓存影响，不是生产磁盘／存储加密验收。容器只发布本机随机端口，TCP 仅允许 TLS 和 SCRAM；容器内 Unix socket 的 trust 仅用于这个合成数据夹具的初始化。此配置不是生产部署。

运行时角色与迁移角色均没有 SUPERUSER、CREATEDB、CREATEROLE；迁移角色只拥有专用测试数据库。维护身份在迁移后授予 API 当前需要的身份查询、论坛读取、消息／帖子插入及位置更新权限，不授予创建用户／论坛或修改成员的能力；实际授权见 [论坛切片](../../../docs/普通论坛切片与扩展边界.md)。

start.sh 只处理 Windows 挂载测试证书在容器内所需的所有者与权限，然后交给官方 PostgreSQL entrypoint 初始化，不重写数据库初始化器。

本测试不证明生产存储、备份、密钥管理或管理员审计已经完成。生产加密边界见 [加密要求](../../../docs/加密边界与版本策略-2026-10-28.md)。

0.4.3 同条件复测保留旧版、仅缓存／分区候选、准入修复后两轮及短 API 对照，JSON 位于 [结果快照](../../../docs/性能基线/2026-10-29-cache-partition.json)。fanout 另记录每阶段 page_cache 命中／未命中／实际正文加载数；所有调用仍查当前成员／流位置。固定场景并非生产命中率，HTTP 容量 503 也核对错误码与 Retry-After。

0.4.4 新增第五个 SQL 性质实验：在同一 pgx.Batch 中先取得真实行锁，再读到等待期间另一事务提交的记录，避免把批量发送误作同条 CTE。借用／份额实验虽通过边界回归，但持续负载退步，最终撤去该生产改动和专用测试、恢复原准入回归；实验源码保留在忽略审计目录。性能档新增 latency_successful／latency_capacity_refused，按成功／容量拒绝分别统计，并保留全尝试延迟和客户端未发出需求；探针支持批内锁取得计时。逐步同条件结果见 [本轮报告](../../../docs/同类社区策略与优化-2026-10-29.md)。

0.4.5 的 fanout 已改为 API 独立子进程：父进程只持有生成器／SDK 和验证连接，API 的连接池、正文缓存、GC／堆和时段统计从私有控制管道读取。旧 go_process_* 字段现在明确表示生成器；api_process、api_before／api_after 表示 API，api_database_wait_events 和 outbox_by_partition 记录实际等待及分区积压。代码没有给生产 API 新增诊断路径。ODYSSEIA_TEST_API_BINARY 可选择兼容的已冻结测试二进制，仅用于受控原／新对照。

使用磁盘档多热帖：在前述 fanout 命令后加 -DatabaseStorage disk。需要剖析时先建立受限目录，将 ODYSSEIA_TEST_API_PROFILE_DIR 设为该目录；按计量阶段保存 API 和生成器两套 CPU pprof，剖析轮与无剖析对照分开。ODYSSEIA_TEST_PROCESS_OUTPUT 可选择无秘密的三个 PID 文件供 OS CPU／RSS 采样。两种方式仍共用机器，不等于独立硬件测量。

第七项迁移与积压保留的往返验证、真实锁等待后的新引用可见性／幂等冲突、单活动写者的排队撤权与全局名额验证均随实现更新。采用理由、低负载代价、全部成功与失败需求见 [最新报告](../../../docs/独立进程压测与通知锁修复-2026-10-29.md)。

## P3 三形态持续混合档

`-TestTags 'integration,realtime,performance' -PerformanceProfile discussions -DatabaseStorage disk` 运行 TestPerformanceDiscussionModes，覆盖 threaded／chat 帖子和固定产品频道，每种 16 个流、5 个热点，每热点 64 个真实 WSS 客户端。每档预热 3 秒、计量 60 秒，热点每流 10／25／50/s，普通读 100/s、普通写 25/s。API 独立子进程、同一总池／写预算；每次运行新磁盘卷并清理。结果保存于受限 runtime/backend-postgres/performance-discussions.json，需在后续运行覆盖前归档。分段直方图与内部拒绝原因通过私有控制管道读取，低档成功不替代高档失败或生产容量验收。[本轮数据与解释](../../../docs/P3讨论模式与固定频道-2026-10-30.md)。

## 0.5.1 写预算与对照

运行 API 时可设置 ODYSSEIA_MAX_WRITE_TRANSACTIONS；0／未设置为自动预算，约三分之一共享池留在写事务名额之外，池 16 对应 10。正值不能超过数据库 URL 中该 API 的 pool_max_conns；增加 API 实例须合计所有池和消费者／运维连接，不能用业务在线人数直接设置数据库连接数。没有新增迁移或关闭持久性。

三形态性能场景默认每档 60 秒；ODYSSEIA_TEST_PHASE_SECONDS 仅测试时允许 10–60 秒。原生实时缺口单独失败，不用补读或延长排空掩盖；失败报告补有界位置样本及连接／订阅诊断。固定总连接的分池和写预算历史候选使用已冻结的实验二进制，当前 API 不提供分池开关。完整条件见 [整体对照](../../../docs/整体优化与写预算对照-2026-10-30.md)。
