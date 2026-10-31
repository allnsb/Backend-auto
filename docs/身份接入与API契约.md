# 最小身份接入与 API 契约

更新时间：2026-10-29。本文记录身份侧合成验证，正式身份供应商尚未选定／接入。代码入口见 [backend](../backend/README.md)，机器可读契约见 [openapi.json](../backend/internal/httpapi/openapi.json)，运行中的同一文档由 HTTPS GET /openapi.json 提供。

P2 后端切片已完成，[普通论坛接口](普通论坛切片与扩展边界.md)复用本文的身份入口；本文只维护身份侧规则，正式登录供应商仍未接入。当前全部迁移为五项，含实时命令约束与只读聚合；论坛、审计及授权见相应文档，当前阶段见 [P2 验收](P2后端验收与交接-2026-10-29.md)。

## 当前身份边界

前端从可信授权服务器取得 Access Token，再使用 Authorization: Bearer 调用 API。当前后端承担资源服务器的令牌验证与本地身份映射；没有实现登录回调、密码注册、刷新令牌、账号关联或即时注销，不把测试维护命令定义为正式注册准入政策。

业务层使用本地随机 UUID。外部身份按 issuer＋subject 精确匹配，二者组成数据库唯一键；当前一个外部身份对应一个本地用户。相同 subject 来自不同 issuer 时不会合并，也不按 email 自动关联。未来多身份绑定需单独设计，不能直接扩大当前维护命令的权限。

HTTP 传输只接受 Authorization 头中的一个 Bearer 凭据。请求正文、查询参数、Cookie 和 X-User-ID 等身份头不能指定当前作者或账号；没有有效令牌时不访问账号存储。返回用户信息仅含 id、created_at，不暴露 subject、email 或原始令牌。

## 令牌与公钥配置

配置变量见 [.env.example](../backend/.env.example)：ODYSSEIA_AUTH_ISSUER、ODYSSEIA_AUTH_AUDIENCE、ODYSSEIA_AUTH_JWKS_URL 必填；ODYSSEIA_AUTH_CA_FILE 可选。URL 必须为没有凭据、查询与片段的 HTTPS 地址。issuer 不做小写化或去斜杠等重写，必须与签发值一致；JWKS 地址由配置确定，不读取令牌中的 jku／x5u 等地址。

当前 profile 基于 [RFC 9068](https://www.rfc-editor.org/rfc/rfc9068.html)：

- 必须是签名 JWT Access Token，typ 为 at+jwt 或 application/at+jwt；ID Token 和 opaque token 不接受。
- 支持 RS256、ES256、EdDSA；拒绝 none、HMAC 及其他未列出的算法。
- 校验签名、issuer、API audience 和有效期；要求非空 sub、jti、client_id 以及 exp、iat，检查未来 iat、nbf 及 exp 晚于 iat。当前无额外时钟宽限。
- 额外要求非空 kid，以及对应 JWKS 公钥明确标记 use=sig；不支持自定义 crit 扩展。令牌上限 16 KiB。

JWKS 使用 TLS 1.3，验证 CA／主机名、拒绝重定向，5 秒超时。可选 CA 文件仅用于该客户端，不安装系统信任、不关闭证书检查。首次加载失败则 API 启动失败；缓存每分钟刷新，未知 kid 触发刷新由上游限流为每五分钟一次，等待上限一秒。供应商轮换时应提前发布新公钥并保留过渡重叠，不在切换后才依赖每个 API 进程临时发现。

有效 JWT 的本地校验不能证明其未被单独撤销；缓存刷新失败也不自动清空已有可信公钥。用户封禁、会话注销与令牌即时撤销需要后续状态／introspection 方案。本轮没有把这些标记为已实现。

实时接入保留已验证 Access Token 的原始 exp：稳定身份仍是 issuer＋subject，Authentication／Actor 单独携带认证到期时间，不写进映射或 /v1/me 响应。只签发连接 JWT，exp 取原 exp 与当前时间＋60 秒的较早值，且无频道授权。频道改由受服务凭据保护的 HTTPS 订阅代理检查当前成员；移除成员可通过事务 outbox 主动断连。它不提供 IdP 会话即时撤销，也不承诺零延迟，具体 [接入与限制](../backend/deploy/realtime/README.md)单独维护。

## 显式准备测试账号

先准备独立数据库和迁移／运行身份，通过迁移命令应用 SQL。当前迁移创建 schema 和 users 表，不硬编码部署环境的数据库角色名。运行身份只需该 schema 的 USAGE 和 users 的 SELECT；以夹具角色名为例，由维护身份在迁移后执行：

```sql
GRANT USAGE ON SCHEMA odysseia TO odysseia_runtime;
GRANT SELECT ON odysseia.users TO odysseia_runtime;
```

本地或隔离测试中，在维护进程环境中设置 ODYSSEIA_MIGRATION_DATABASE_URL、ODYSSEIA_AUTH_ISSUER 和 ODYSSEIA_PROVISION_SUBJECT，随后运行：

```powershell
go run ./cmd/provision-user
```

命令只操作配置的数据库，不读取请求声明；首次创建本地 UUID，重复／并发执行复用已有 UUID 和创建时间。连接凭据与 subject 由环境提供，不放进命令参数；输出只记录本地 user_id，不记录 subject 或令牌。API 进程无需持有维护账号。

独立 [集成测试脚本](../backend/deploy/postgres/README.md) 自动生成合成角色、证书与数据，验证后清理自己的容器；不会提供持久的正式测试登录站点或迁移旧用户。

## 已实现端点

| 方法与路径 | 行为 |
|---|---|
| GET /healthz | 204，进程存活。 |
| GET /readyz | 检查数据库连接，204 或 503；不证明业务表或身份供应商全部就绪。 |
| GET /openapi.json | 无需身份凭据，提供嵌入的 OpenAPI 3.2.1 文档。 |
| GET /v1/me | 200 返回当前本地用户；401 表示缺少／无效令牌，404 表示该可信身份尚未创建本地账号，503 表示账号查询不可用。GET 不写库。 |

Go 的 GET 路由同时支持 HEAD；写入方法不能调用当前身份读取路径。/v1/me 的成功与错误均返回 Cache-Control: no-store。身份错误使用 application/problem+json，稳定扩展字段 code 为 authentication_required、invalid_token、account_not_provisioned 或 dependency_unavailable；错误不返回数据库／验签内部信息。401 带 WWW-Authenticate Bearer challenge。

接口当前按同源或同源代理部署使用，未开放任意来源 CORS。前端从其他域直接访问时，需要与实际部署域名一起确定允许来源；不会为调试全局放开身份接口。

## 实际验证

默认测试验证令牌类型／字段／签名／算法拒绝、可信 TLS JWKS、缓存命中、轮换后移除旧密钥、公钥用途、OpenAPI 文档与响应；真实 PostgreSQL 测试验证首次并发迁移、12 个并发账号创建只产生一行、issuer 隔离、运行账号禁止创建用户，以及 HTTPS 请求经过 RS256 验签到真实数据库的查询链路。2026-10-28 均通过。

这些验证不等于正式供应商登录／注册流程、完整权限或上线隐私验收。存储与备份加密、管理审计和容量仍按 [实施里程碑](后端边界与首个里程碑.md) 推进。
