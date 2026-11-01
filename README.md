# ODYSSEIA Backend

ODYSSEIA 社区产品的**后端仓库**。前端由其他同事独立开发；本仓库交付 HTTPS API、OpenAPI 契约，以及基于 Centrifugo 的实时事件接入。

当前实现语言为 **Go**。仓库目录名里的 `bbsgo` 只是历史遗留，与 BBS-GO 无关。

## 现在做到哪

| 状态 | 内容 |
| --- | --- |
| 已完成 | P2：成员论坛、同帖引用、分页、幂等、实时推送／断线恢复／成员撤权 |
| 已完成 | P3 最小闭环：贴吧式／聊天式帖子、继承论坛成员的固定即时频道（写路径仍为 HTTP 0.5.1 的批次与预算） |
| 已完成 | 协作面首个切片（HTTP **0.6.0**）：有效 JWT 引导本地用户、模拟 OIDC issuer、身份组与论坛成员／有效权限 |
| 已完成 | P4-a 文字私聊与群聊（HTTP **0.7.0**） |
| 已完成 | P4-b 会话图片与语音（HTTP **0.8.0**），以及约 7 天附件清扫（HTTP **0.8.1**）。API 同步写入 R2，下载为短时预签名 GET。不含已读回执和病毒扫描 |
| 已完成 | P4-c 自定义表情（HTTP **0.9.0**）。全站和论坛两级，png／apng／gif／webp，公开桶与会话桶分开 |
| 未做／未验收 | 会话编辑删除、作品关联、作者／平台权限覆盖、真实 ZITADEL、作品文件、R18、生产高可用与加密、约 3 万同时在线容量（P6）、前端正式联调签收、真实 R2 复验 |

更细的进度与接续方向见 [`docs/当前状态与接续.md`](docs/当前状态与接续.md)。产品要求以 [`docs/产品需求基线.md`](docs/产品需求基线.md) 为准。

## 技术栈（当前）

- Go（`go.mod` 要求 ≥ 1.27.3）
- PostgreSQL（嵌入式 goose 迁移，现有 12 项）
- Centrifugo（WSS 实时；双节点试验另用 Valkey）
- JWT／JWKS 身份校验（有效访问令牌引导本地用户；开发环境可运行模拟 OIDC issuer）

依赖明细与命令说明见 [`backend/README.md`](backend/README.md)。

## 仓库结构

```text
.
├─ backend/          Go 工程（API、迁移、集成测试、本地部署夹具）
├─ docs/             需求、验收、压测与交接文档
├─ deploy/           部署相关说明（不含历史 IM 试验配置）
├─ AGENTS.md         给自动化助手的接手约定（可选读）
└─ upstreams.lock.json
```

本仓库**不包含**历史候选源码（BBS-GO／OpenIM 等）与本地 `runtime/` 数据。

## 同事怎么上手

1. 读 [`docs/当前状态与接续.md`](docs/当前状态与接续.md) 和 [`docs/产品需求基线.md`](docs/产品需求基线.md)。
2. 联调或对接 API：以 [`backend/internal/httpapi/openapi.json`](backend/internal/httpapi/openapi.json) 为准；身份与错误语义见 [`docs/身份接入与API契约.md`](docs/身份接入与API契约.md)。
3. 本地跑后端：按 [`backend/README.md`](backend/README.md) 与 [`backend/.env.example`](backend/.env.example) 配置 HTTPS、数据库与 JWKS；**不要**把真实 `.env`、证书私钥提交进仓库。
4. 论坛切片与实时相关说明：[`docs/普通论坛切片与扩展边界.md`](docs/普通论坛切片与扩展边界.md)、[`docs/P2后端验收与交接-2026-10-29.md`](docs/P2后端验收与交接-2026-10-29.md)、[`docs/P3讨论模式与固定频道-2026-10-30.md`](docs/P3讨论模式与固定频道-2026-10-30.md)。

## 协作约定

- 只改后端职责范围内的内容；前端仓库另行维护。
- 对外契约变更请同步更新 OpenAPI，并在 `docs/` 留下简短说明。
- 压测与性能对照材料放在 `docs/`（含 `docs/性能基线/`），根 README 不堆版本流水账。
- 密钥、证书、本地数据库卷与运行时日志保持在仓库外。
