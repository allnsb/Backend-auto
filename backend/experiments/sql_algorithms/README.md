# SQL 算法性质研究夹具

使用当前已锁定的 PostgreSQL 18.6 和 pgx v5.11.0，验证消息排序／幂等优化时需要保留的数据库性质。它属于 experiments，只在 integration 与 algorithms 标签同时启用时编译，不提供业务端点。

在 backend 目录运行：

```powershell
pwsh -File deploy/postgres/test.ps1 -GoExecutable go -TestTags 'integration,algorithms'
```

该命令也运行原有 integration 回归。夹具只接受 localhost 的 odysseia_test 合成库，使用迁移角色及校验证书的 TLS；在自己生成的随机 schema 中建研究表，不修改 odysseia 业务表、迁移或授权。测试清理自己的 schema，外层脚本移除专用容器和网络。

当前五个子测试分别验证：

- 普通 sequence 的分配顺序不保证提交顺序，直接推进读取游标会漏迟到提交。
- 事务计数器使后续写者实际等待，且失败回滚不会消费业务序号。
- READ COMMITTED 下的 INSERT／DO NOTHING＋同条 CTE 查询可能读不到并发赢家，后续独立查询能读到。
- 同一 pgx.Batch 中先等待行锁再执行另一条读取，后者能看到等待期间的并发提交；不能据此将两条 SQL 改成单条 CTE。
- pgx.Batch 在末尾约束失败时回滚前面已经执行的更新。

并发实验通过 pg_stat_activity 观察实际 Lock 等待后才释放另一事务，避免用固定睡眠猜测调度。所有并发查询返回到测试主 goroutine 检查，失败也清理事务。

2026-10-29 最初四项实验和完整 integration 回归通过，研究包用时约 0.341 秒，全标签 go vet 通过。此夹具不测吞吐，不意味着小 CTE、Batch 或其他候选已经在完整业务路径中优化成功；结论与采用顺序见 [算法适配报告](../../../../research/algorithm-selection-2026-10-29.md)。

同日 0.4.4 增加第五项批内快照实验，integration,algorithms 与完整故障回归均通过；实际业务采用及逐步性能对照见 [记录](../../../docs/同类社区策略与优化-2026-10-29.md)。SQL 性质通过不等于吞吐或生产容量达标。
