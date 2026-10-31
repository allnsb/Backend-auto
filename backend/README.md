# ODYSSEIA Go 后端

当前已完成 P2 和 [P3 最小后端切片](../docs/P3讨论模式与固定频道-2026-10-30.md)；[验收与交接](../docs/P2后端验收与交接-2026-10-29.md)记录 A01–A08 证据与后续门槛，实施步骤见 [里程碑](../docs/后端边界与首个里程碑.md)。用户选择 Go；go.mod 声明最低 Go 1.27.3，本轮实际使用 Go 1.27.3 验证。普通论坛可独立使用，作品后续通过明确关联接入。HTTPS 使用标准库，协议、数据库和迁移复用成熟库。

| 已引入依赖 | 用途与来源 |
|---|---|
| pgx v5.11.0 | PostgreSQL 驱动与连接池；[官方发行](https://github.com/jackc/pgx/releases/tag/v5.11.0)，MIT。 |
| x/sync v0.24.0 | 已锁定的 Weighted semaphore 与 singleflight，分别用于写入准入和同正文键在途查询合并，BSD-3-Clause；[官方 API](https://pkg.go.dev/golang.org/x/sync@v0.24.0)。版本沿用，未升级。 |
| Ristretto v2.4.2 | 进程内有界消息页缓存，Apache-2.0；核对并锁定当日最新稳定发行，[官方发布](https://github.com/dgraph-io/ristretto/releases/tag/v2.4.2)。授权不进缓存，采用边界见 [本轮记录](../docs/缓存分区与准入修复-2026-10-29.md)。 |
| goose v3.29.0 | 嵌入式 SQL 迁移、版本记录与 PostgreSQL 会话锁；[官方发行](https://github.com/pressly/goose/releases/tag/v3.29.0)，MIT。 |
| golang-jwt/jwt v5.3.1 | Access Token 验签与 claim 验证，MIT。 |
| keyfunc v3.8.0／jwkset v0.11.3 | JWKS 缓存、刷新和轮换，Apache-2.0。 |
| kin-openapi v0.152.0 | 仅用于测试，验证 OpenAPI 3.2.1 文档与响应，MIT。 |
| google/uuid v1.6.0 | 标识生成与解析，已核对当日官方最新稳定发行，BSD-3-Clause。 |
| aws-sdk-go-v2 v1.47.5／config v1.33.13／credentials v1.20.13／s3 v1.114.6／imds v1.20.5 | Cloudflare R2 的 S3 兼容客户端和本地预签名，Apache-2.0。请求校验和设为 WHEN_REQUIRED，避免默认 CRC32 被 R2 拒绝。见 [附件记录](../docs/P4图片与语音附件-2026-10-31.md)。 |
| golang.org/x/image v0.47.0 | 仅用 WebP `DecodeConfig` 读取宽高，不展开像素，BSD-3-Clause。 |
| centrifuge-go v0.13.0 | 仅集成测试使用的官方实时 SDK，MIT；实际服务器为 Centrifugo 6.10.2，Apache-2.0，见 [实时版本锁定](deploy/realtime/versions.lock.json)。 |

本轮身份组件的官方发行、对照候选与取舍见 [复用记录](../../research/identity-api-reuse-2026-10-28.md)。持续联网查找复用是每个功能的开发步骤，不仅是初始选型。

双节点测试另复用官方 Valkey 9.1.3 容器，通过 Centrifugo 原有共享 engine 接入；[镜像摘要](deploy/realtime/images.lock.json)、[来源及许可](../../research/realtime-cluster-reuse-2026-10-29.md)均有记录。它只属于可选集成夹具，没有加入 Go API 的运行时依赖，也不表示生产高可用拓扑已确定。

精确模块及传递依赖见 [go.mod](go.mod)，校验记录见 [go.sum](go.sum)，上游许可保留在模块缓存；没有将依赖源码复制进手写业务目录。下载时使用 HTTPS 模块代理并保持 Go 校验机制启用。测试数据库镜像按 [images.lock.json](deploy/postgres/images.lock.json) 中的官方摘要固定，已核对 linux/amd64 实际 image ID。

## 当前能力

- HTTP 0.8.0 在 0.7.0 的文字私聊和群聊之上，增加图片与语音附件。频道仍继承当前论坛成员。帖子与频道的写入授权仍是成员关系，并且仍要求正文。有效访问令牌会建立本地用户。见 [附件记录](../docs/P4图片与语音附件-2026-10-31.md)、[P4 文字记录](../docs/P4文字私聊与群聊-2026-10-31.md)、[协作面记录](../docs/协作面与模拟身份-2026-10-31.md) 和 [P3 契约与证据](../docs/P3讨论模式与固定频道-2026-10-30.md)。
- 固定内存的分段耗时／拒绝原因计量，供进程内聚合与测试控制管道读取；没有新增公网诊断端点，也不记录正文、令牌、SQL 参数或动态资源标签。

- 从进程环境读取明确配置，不隐式加载 .env。
- HTTPS 监听，最低 TLS 1.3；证书／密钥必须提供且有效，不回退到明文业务服务。
- GET／HEAD /healthz 返回 204，只证明进程能处理请求，不代表数据库、身份或实时依赖已经就绪。
- 启动时通过校验证书与主机名的 TLS 1.3 连接 PostgreSQL；不接受 disable／allow／prefer／require／verify-ca 等降低验证的连接配置。
- GET／HEAD /readyz 在 2 秒内检查数据库连接；成功 204、失败 503，响应不携带数据库错误。当前只检查连接，不冒充业务表或尚未接入服务的就绪检查。
- 独立 migrate up 命令执行嵌入式 SQL 迁移；并发由 PostgreSQL 会话锁协调。API 启动与普通查询不执行迁移。
- GET／HEAD /v1/me 根据校验后的 issuer＋subject 返回本地用户，只含本地 UUID 和创建时间。该身份第一次出现时插入一行；重复请求复用同一 UUID 和创建时间。
- 独立 provision-user 维护命令仍可显式创建身份映射；重复／并发创建保持同一 UUID。API 账号在 users 上需要 SELECT 和 INSERT。UPDATE 与 DELETE 留给维护账号。论坛读写授权见协作面记录。
- GET /openapi.json 提供嵌入的 OpenAPI 3.2.1 契约；身份接口错误使用 application/problem+json。
- 成员论坛 API 支持帖子创建／读取、一层子回复、同帖引用、带作用域的分页、并发幂等，以及正文与待发布事件同事务写入。完整路径和准备命令见 [普通论坛切片](../docs/普通论坛切片与扩展边界.md)。
- 开帖／建频道／回复入队前检查权限，每计数器最多 1 个活动写者、12 个活动／等待命令，再申请全局写名额；0.5.1 自动预算留下约三分之一共享池余量，池 16 对应 10 个写事务，总活动／等待 256，两段等待共用 2 秒。ODYSSEIA_MAX_WRITE_TRANSACTIONS=0 或未设置使用自动值，正整数可覆盖但不能超过该 API 池上限。它不是物理读取池或全站配额，所有实例／消费者／运维连接要合计。事务内重新授权，503／Retry-After: 1 保留；分池、借用与动态份额未采用，见 [最新对照](../docs/整体优化与写预算对照-2026-10-30.md)。
- 每次消息分页先查当前成员与流位置，再复用追加型正文快照；Ristretto 的估计成本上限 64MiB、TTL 5 分钟，singleflight 合并同键未完成的内容加载。编辑／删除前必须增加版本或失效机制；维护命令沿用无缓存构造器，API 显式创建并在停机后关闭缓存。
- 第六项迁移生成 Outbox 分区：控制 0、发布 1–4，同一 scope 保持同一数据分区；第七项移除导致跨流提交串行的逐条通知，原生消费者使用已有 20ms 轮询。已有实例应用第六项时须按 [操作步骤](deploy/realtime/README.md)停写、排空并统一消费者；从六项升级第七项不改路由或积压，先确认轮询；没有给消息表做物理分区。第九项只增加身份组，不改分区。第十项放宽发布检查以接受 `inbox:#<用户>`，scope 仍是会话 ID，不要求排空；先更新 Centrifugo 的 inbox namespace，再打开会话写入。第十一项增加附件表，并把空正文限制在带附件的会话消息上，不改 Outbox，不要求排空。
- HTTPS 签发短期连接凭据，订阅代理检查当前读取权限，维护移除与审计／断连同事务。真实 WSS、恢复、主动撤权、消费失败重试及原生服务重启通过；跨重启会重放相同事件 ID，合并要求见 [事件接入契约](../docs/实时服务重启与事件接入契约-2026-10-29.md)。
- 请求头读取 5 秒、其它请求完整读取 10 秒，请求头预算 32 KiB；真实慢请求超时返回 408，大请求头由服务器返回 431。附件上传把该连接的读写截止时间和处理 context 放宽到 2 分钟。
- Ctrl+C／SIGTERM 触发停机，最多等待 5 秒让正在处理的请求完成，超时关闭连接并明确报错。

当前有身份引导、身份组、论坛成员、固定频道、文字私聊／群聊、会话图片／语音和实时接入 API。第九项迁移增加身份组、用户与组的关系，以及论坛成员的 role_id，默认角色为 member。第十项增加会话和会话成员，并放宽 Outbox 发布检查。第十一项增加 `attachments` 与 `message_attachments`。前八项的模式、频道、审计、实时命令、只读积压、Outbox 分区和通知移除保持原样。OpenAPI info.version 为 0.8.0。realtime-status 维护命令使用仅能读取聚合视图的观察账号，返回积压量／年龄及警戒退出码，不读取载荷。双节点故障、真实 65 秒中断、缓存过期和分页补读／实时合并通过。仍只处理合成数据；主动断连经 outbox 异步执行，不能据身份 JWT 的短有效期推断持续刷新连接的统一故障撤权上限。会话可以带图片和语音；没有自定义表情或已读回执。尚无作品 API；真实 R2、真实 ZITADEL、作者／平台覆盖、正式高可用／SLA、容量和生产隐私基础设施仍待落实。

## 目录

```text
backend/
├─ cmd/api/           API 进程入口与装配
├─ cmd/mock-idp/      开发用 OIDC 发现与 JWKS
├─ cmd/migrate/       独立迁移命令
├─ cmd/provision-user/ 独立测试账号创建命令
├─ cmd/provision-forum/ 独立成员论坛准备命令
├─ cmd/remove-forum-member/ 有审计和幂等的维护撤权命令
├─ cmd/realtime-status/ 仅查询聚合视图的积压检查命令
├─ internal/config/   API 配置读取与格式检查
├─ internal/httpapi/  HTTPS、身份接口、OpenAPI 与生命周期
├─ internal/identity/ 供应商无关的外部身份、本地用户和身份组判定
├─ internal/mockidp/  模拟 issuer 的发现文档、JWKS 和访问令牌
├─ internal/message/  通用消息类型，不依赖作品或论坛
├─ internal/forum/    论坛、帖子、楼层与引用规则
├─ internal/tokenauth/ JWT Access Token 与可信 JWKS 适配
├─ internal/realtime/  只签发身份连接凭据，不授予频道
├─ internal/postgres/ 连接、嵌入式 SQL 迁移与数据库集成测试
├─ deploy/postgres/   隔离合成数据测试夹具
├─ deploy/realtime/   官方实时组件版本、配置与接入约定
└─ examples/          不含真实凭据的 HTTPS 请求样例
```

按实际功能新增模块，不预建空的业务层／接口层。标准 net/http 足以支撑当前端点，后续业务路由需要时再引入 chi。

## 运行

使用 Go 1.27.3 或按 [版本策略](../docs/加密边界与版本策略-2026-10-28.md)验证后的工具链。在本目录运行；Go 未加入 PATH 时使用 go 可执行文件的完整路径。

配置参考 [.env.example](.env.example)，将值放入启动进程环境：

| 变量 | 含义 |
|---|---|
| ODYSSEIA_HTTPS_ADDR | 默认 127.0.0.1:8443，仅监听本机。 |
| ODYSSEIA_TLS_CERT_FILE | 必填，TLS 证书链文件路径。 |
| ODYSSEIA_TLS_KEY_FILE | 必填，对应私钥文件路径。 |
| ODYSSEIA_DATABASE_URL | API 必填，普通运行账号的 PostgreSQL URL，必须验证 CA 和主机名。 |
| ODYSSEIA_MIGRATION_DATABASE_URL | 迁移／账号维护命令必填，使用独立维护账号；不回退使用 API 账号。 |
| ODYSSEIA_AUTH_ISSUER | API／账号维护命令必填；可信 HTTPS issuer，验证时精确匹配。 |
| ODYSSEIA_AUTH_AUDIENCE | API 必填；为本 API 配置的 audience。 |
| ODYSSEIA_AUTH_JWKS_URL | API 必填；明确配置的可信 HTTPS 公钥端点。 |
| ODYSSEIA_AUTH_CA_FILE | 可选；只替换 JWKS 客户端信任根，空时使用系统信任根。 |
| ODYSSEIA_MOCK_IDP_ADDR／ISSUER／AUDIENCE／TLS_CERT_FILE／TLS_KEY_FILE | 仅 mock-idp 进程。监听地址必须是回环，默认 127.0.0.1:9443。issuer 默认 https://127.0.0.1:9443，JWKS 为该源加上 `/oauth/v2/keys`。 |
| ODYSSEIA_REALTIME_CONNECTION_SECRET | API 必填；至少 32 字节随机秘密，对应 Centrifugo 连接验签配置。 |
| ODYSSEIA_REALTIME_PROXY_SECRET | API 必填；另一把至少 32 字节随机秘密，仅供 Centrifugo 私有 HTTPS 代理认证，与签名密钥不同。 |
| ODYSSEIA_R2_ACCOUNT_ID／ACCESS_KEY_ID／SECRET_ACCESS_KEY／BUCKET | API 必填。Cloudflare R2 账号、S3 访问密钥和私有桶。秘密不进客户端、日志或命令行回显。 |
| ODYSSEIA_R2_ENDPOINT／REGION | 可选 endpoint，空则用 `https://<account>.r2.cloudflarestorage.com`。region 默认 `auto`。 |
| ODYSSEIA_ATTACHMENT_* | 附件大小、边长、像素、语音时长、每分钟次数、每用户配额、预签名时长和每条消息个数。默认值和天花板见 [附件记录](../docs/P4图片与语音附件-2026-10-31.md) 与 [.env.example](.env.example)。 |
| ODYSSEIA_PROVISION_SUBJECT | 仅账号维护命令必填；来自身份供应商的 subject，不用 email 替代。 |
| ODYSSEIA_FORUM_TITLE | 仅论坛维护命令必填；本次新论坛标题。 |
| ODYSSEIA_FORUM_MEMBER_IDS | 仅论坛维护命令必填；逗号分隔的本地用户 UUID。 |
| ODYSSEIA_OPERATION_ID／ODYSSEIA_FORUM_ID／ODYSSEIA_FORUM_MEMBER_ID | 仅移除成员维护命令必填，规范非零 UUID；操作 UUID 用于重试幂等。 |
| ODYSSEIA_OPERATOR／ODYSSEIA_CHANGE_REASON | 仅移除成员维护命令必填，非空操作标签和原因；详见 [运行步骤](deploy/realtime/README.md)。 |
| ODYSSEIA_OBSERVER_DATABASE_URL／ODYSSEIA_BACKLOG_WARN_AFTER | 仅 realtime-status 命令使用；独立只读聚合身份和正时长警戒值（默认 30s），详见 [诊断与处置](../docs/实时积压检查与故障处置.md)。 |

提供本地证书，客户端信任其 CA，证书名称与访问名称匹配。数据库 URL 使用 sslmode=verify-full 和 sslrootcert，例如下列占位形式；证书路径等 URL 参数需要编码。私钥及真实连接凭据放在受限运行目录，不提交版本库、不复制进日志或命令参数。

```text
postgresql://<运行账号>:<密码>@<证书匹配的主机>:5432/<数据库>?sslmode=verify-full&sslrootcert=<编码后的CA路径>
```

数据库和账号须事先准备；迁移账号拥有目标库的建表权限，运行账号只授予当前业务所需权限。分别在相应进程环境提供账号配置后，先显式迁移，再运行 API：

```powershell
go run ./cmd/migrate up
go run ./cmd/api
```

API 启动还要求可信 JWKS 首次加载成功，不会在身份配置缺失或公钥不可验证时开放匿名身份路径。运行账号需要 users 的 SELECT 与 INSERT。令牌格式见 [身份接入与 API 契约](../docs/身份接入与API契约.md)。

开发用模拟 issuer 与 API 分开启动，并且只监听回环地址。`0.0.0.0`、空主机和其他非回环地址会拒绝启动。`POST /mock/token` 再检查客户端地址，非回环返回 403。证书名称必须等于 issuer 主机名。发现文档位于 `{ODYSSEIA_MOCK_IDP_ISSUER}/.well-known/openid-configuration`，公钥位于 `{ODYSSEIA_MOCK_IDP_ISSUER}/oauth/v2/keys`。把这两个值分别写入 `ODYSSEIA_AUTH_ISSUER` 和 `ODYSSEIA_AUTH_JWKS_URL`，并把 `ODYSSEIA_AUTH_AUDIENCE` 设成 `ODYSSEIA_MOCK_IDP_AUDIENCE`。

```powershell
go run ./cmd/mock-idp
```

`POST /mock/token` 接受 `{"subject":"...","alg":"RS256"}` 或 `ES256`，audience 固定为 `ODYSSEIA_MOCK_IDP_AUDIENCE`，请求体不能另写。返回五分钟的 `at+jwt`。`POST /oauth/v2/token` 在模拟进程中返回 501，把该路径留给以后的 ZITADEL。

以证书包含 localhost、客户端持有对应 CA 证书为例，可检查：

```text
curl --cacert <CA证书路径> https://localhost:8443/healthz
```

不需要关闭证书验证。Go 进程提供 TLS 1.3，WSS 测试客户端也要求 TLS 1.3；Centrifugo 原生服务最低 TLS 1.2，详见 [已核对的边界](deploy/realtime/README.md)。Caddy、生产入口的 TLS 1.3 限制、域名证书自动化和 HTTP/3 尚未接入。

## 验证

```powershell
go test -count=1 -timeout=180s ./...
go vet ./...
go build ./...
```

默认测试使用合成请求和内存中的测试证书，并检查不安全数据库模式、错误脱敏；HTTPS 测试需要本机临时端口。真实 PostgreSQL 测试通过 [隔离夹具](deploy/postgres/README.md) 运行：

```powershell
pwsh -File deploy/postgres/test.ps1 -GoExecutable go
```

夹具生成短期 CA／服务器证书和随机密码，启动独立 PostgreSQL 18.6，以分离的测试角色运行带 integration 标签的测试，最后清理自己的项目。增加 -TestTags 'integration,realtime' 可运行 [真实 WSS 验证](deploy/realtime/README.md)，需提供校验过的 Centrifugo 可执行文件，消费者使用第三个最小授权角色和第二个隔离数据库。不会接入旧 NodeBB／IM 数据库。若执行环境禁止回环连接或 Docker 管道，应在允许本机网络与 Docker 的环境执行同一测试，不跳过协议断言。

增加 -TestTags 'integration,realtime,cluster' 可运行双节点共享 Valkey 场景，使用第三个隔离数据库；全部命令与配置见 [实时运行说明](deploy/realtime/README.md)。普通 integration／integration,realtime 保持原有隔离配置。

完整收尾使用 -TestTags 'integration,realtime,cluster,acceptance'，包含真实 65 秒故障和 60 秒缓存／连接到期，不缩短测试时钟。2026-10-29 全部验收标签测试（PostgreSQL 包 78.228s）、默认测试、全标签 go vet、模块校验及六个 Linux／amd64 命令交叉构建通过。Go／原生实时程序实际在 Windows 执行，PostgreSQL／Valkey 在 Docker Linux 执行，未在 Linux 运行 Go 命令。govulncheck v1.9.0 对全标签含测试调用图未命中当时已知漏洞，不代替原生服务和镜像供应链审计。入队约束、只读角色、编译后诊断命令 0→2→0 状态和完整 HTTPS／WSS 交接链路均有真实验证，测试资源已清理。前述单节点／双节点报告保留当时范围，当前完成状态以 [P2 验收](../docs/P2后端验收与交接-2026-10-29.md)为准。

前轮 0.4.4 将流锁／幂等查询放入同一个 pgx.Batch，但仍是顺序执行的两条 READ COMMITTED 语句；并发提交快照有真实锁等待实验。引用读取仅取消息 ID／根楼归属，重试结果一次查询完整返回。本轮确认减少驱动调用组和数据读取，旧二进制末尾回测也出现明显漂移，稳定吞吐增幅未证实。六项迁移和所有依赖版本保持，详见 [同类社区策略与优化](../docs/同类社区策略与优化-2026-10-29.md)。

## 性能基线

首次 [本地 API 基线](../docs/本地性能基线-2026-10-29.md)已记录 HTTP/2／TLS 1.3、实际 JWT 验签和数据库读写的吞吐／P50／P95／P99、池等待及深分页执行计划。使用独立 integration,performance 标签重跑，命令与条件见 [夹具说明](deploy/postgres/README.md)；它选择性能场景，功能／故障验收仍使用各自命令。当前主要限制是热点帖的持锁事务和并发下的连接池等待；没有据此宣称生产容量或三万活跃 WSS 通过。

后续 [算法适配研究](../../research/algorithm-selection-2026-10-29.md)比较排序、分页、幂等、Outbox、缓存和状态同步。首轮 [热点回复优化与对照](../docs/热点写入优化与对照-2026-10-29.md)已将小 CTE／pgx.Batch 用于回复持久化，保留权限、顺序和事务原子性；六轮同条件测量显示局部吞吐／P95 收益，也记录热点 P50 退步。当前性能命令在负载后增加独立串行写入探针，JSON 含 write_samples；不为此添加生产配置或端点。其他候选的当前采用状态见算法研究和下方最新记录。

真实 SQL 性质研究位于 [experiments/sql_algorithms](experiments/sql_algorithms/README.md)，用 integration,algorithms 标签重跑；它不计量吞吐，也不修改业务表。首次性能数字保留其历史测量范围，不能继续作为当前回复路径的调用次数或容量结论。

此前 0.4.1 的 [混合持续负载与写预算](../docs/混合负载与写入准入-2026-10-29.md)验证查询保护，同时暴露普通写入排队。此前 0.4.2 的 [按流隔离与原因分析](../docs/按流写入隔离与原因分析-2026-10-29.md)补充分段计时、重复对照、局部准入和权限回归，并公开额外预检查成本。原性能命令加 -PerformanceProfile mixed，可测普通流量的固定到达率、客户端未发出的需求、HTTP 全程延迟、开始事务前／事务内时长及数据库 Lock／池等待；计时仅用于合成测试，不记录正文或标识。生产容量仍需后续验证。

此前 [多热帖与 WSS 扇出](../docs/多热帖与实时扇出压测-2026-10-29.md)与 [0.4.3 缓存／分区](../docs/缓存分区与准入修复-2026-10-29.md)保留各自时点。前轮 0.4.4 的 [同类策略评估与逐步对照](../docs/同类社区策略与优化-2026-10-29.md)记录批量读取、引用元数据和借用／份额候选的否决；原／仅 SQL／最终版的短 API 和多人 WSS 均归档。负载命令不变；新增 latency_successful／latency_capacity_refused，避免将快速拒绝误作成功延迟。该轮 P3 即时式帖子和产品频道尚未实现；0.5.0 现已完成其最小后端闭环，P6 容量仍未验收。

0.4.5 的 [独立进程测量与通知锁修复](../docs/独立进程压测与通知锁修复-2026-10-29.md)记录数据库全局通知对象锁的实证、引用读批次和单活动写者取舍。去掉通知会增加空闲时的轮询等待；最高档仍有容量拒绝。独立进程仍共用物理机器，不能据此承诺生产吞吐。升级第七项前确认所有消费者保留轮询；从五项及更早升级仍需遵守第六项停写／排空屏障。

## P3 混合负载

`pwsh -File deploy/postgres/test.ps1 -GoExecutable go -TestTags 'integration,realtime,performance' -PerformanceProfile discussions -DatabaseStorage disk` 使用独立新磁盘卷，960 个真实 WSS 客户端、三形态各五个热点，每档 60 秒，结束后清理夹具。全标签编译／静态检查使用 integration,realtime,cluster,acceptance,algorithms,performance；当前 unit、完整故障回归、vet、模块校验、六命令 Linux/amd64 交叉构建及漏洞核查均见 [本轮记录](../docs/P3讨论模式与固定频道-2026-10-30.md)，Linux 产物未执行。

## 0.5.1 整体优化

流锁查询取得最新位置和数据库事务时间，由原有 Go 编码器生成事件，把计数器／消息、可选关系及 Outbox 放入同一 pgx.Batch；回调核对 RETURNING 并排空所有错误，外层事务随后提交。锁后幂等／引用保持独立语句的新快照。没有跨请求微批、应用时钟或弱化持久性；依赖与八项迁移不变。

启动变量 ODYSSEIA_MAX_WRITE_TRANSACTIONS 见 [.env.example](.env.example)；未设置／0 使用自动值，正数不能超过该 API 的 pool_max_conns，非法值启动失败。每实例总池、消费者和运维连接均须纳入数据库预算。新增有界 atomic_message_batch 计时包含正文／关系／Outbox，不能与旧 message_write 单段直接比较；仅测量夹具导出统计，没有新增公网诊断接口。

[研究、候选及真实对照](../docs/整体优化与写预算对照-2026-10-30.md)保留全部拒绝、未发出和原版的一轮 25 次投递缺口；后续成功没有证明其原因或修复。P6 容量／生产门槛仍未通过。
