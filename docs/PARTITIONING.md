# 分区与归档方案（P3）

> 结论先行：**按 `block_number` 一级 RANGE 分区，每 100,000 块一个分区；`decoded_event` 同步分区并增加 `block_number` 列。**
> 现在数据量（450 万行 / 1.6 GB）还不需要，但**必须在全量索引之前落定**——迁移成本随数据量线性增长，
> 而 schema 变更（主键含分区键）需要与代码同一窗口发布。
>
> 最后更新：2026-11-23。依据是当前库的实测值，不是估算。

---

## 1. 量级：从实测值外推

| 指标 | 实测（2026-11-23） |
|---|---|
| `chain_log` 行数 | 4,908,080 |
| 已索引区间 | 26,422,714 .. 26,475,854（53,140 块） |
| `chain_log` 大小 | 1786 MB（heap 1198 + 索引 588）= 381 字节/行 |
| `decoded_event` 大小 | 1447 MB = 309 字节/行 |
| 死元组 | 233（无膨胀，autovacuum 跟得上） |
| 全量区间 | 6,082,467 .. 26,475,559（20,393,092 块） |

**全量行数不是「当前密度 × 全部块」。** USDC 在 2018–2020 年几乎无人使用，
用 91.6 行/块外推整段历史会高估 4–5 倍。按块区间采样 10 段（每段 3 点 × 20 块）实测后积分：

| 段起点块 | 每块日志数 | 段起点块 | 每块日志数 |
|---|---|---|---|
| 6.08M | 0.17 | 16.28M | 6.83 |
| 8.12M | 0.93 | 18.32M | 11.55 |
| 10.16M | 6.73 | 20.36M | 18.30 |
| 12.20M | 12.55 | 22.40M | 53.47 |
| 14.24M | 14.97 | 24.44M | 51.60 |

积分结果：**约 3.61 亿行**（平均 17.7 条/块），不是 18.7 亿。
按 691 字节/行（381 + 309）算约 **250 GB**；加 `block_header` 3.9 GB，再留 20% 余量（膨胀、
索引重建、临时空间），**建议按 300–350 GB 规划磁盘**。

不确定性：近期密度上升很快（26.4M 处实测 91.6 条/块，是 24.4M 段的 1.8 倍），
若按末端密度外推尾段，总量会再高一成左右。300–350 GB 已包含这部分。

时间：全量 2,039 万块，publicnode 在 1000 块批下实测中位约 46 块/秒（见 `docs/BENCHMARK.md`），
纯拉取约 5 天；计入写库与块头，**估 6–8 天**。分区不解决吞吐，只解决存储与维护。

---

## 2. 为什么必须分区（而不是只加索引）

单表 19 亿行会遇到三个具体问题，都不是「加个索引」能解决的：

1. **VACUUM 与索引重建的时间不可控。** 19 亿行的 autovacuum 会长期占用 IO；`REINDEX` 单表要数小时且需要额外等量磁盘。
2. **删除历史数据只能 `DELETE`。** reorg 或归档时，`DELETE` 会产生大量死元组，还得靠 VACUUM 回收；分区表可以直接 `DROP`/`DETACH` 一个分区，O(1)。
3. **热数据与冷数据混在同一批数据页里。** 常用查询只碰最近几周，但缓冲区里塞的是七八年的数据，命中率被摊薄。

---

## 3. 分区键：`block_number`，粒度 100,000 块

`block_number` 是唯一能同时满足三件事的键：

- **查询能裁剪**：`/api/events` 的 `from`/`to`、`Coverage` 的 `min/max`、reorg 的「删除 >= N」都按块号；
- **单调递增**：新数据只落最新分区，写入没有热点分散问题；
- **区间稳定**：主网出块稳定在 12 秒，按块分区≈按时间分区，但不需要额外维护时间列。

粒度计算（每分区约 940 万行）：

| 粒度 | 全量分区数 | 单分区行数 | 单分区大小 |
|---|---|---|---|
| 50,000 块 | 408 | 470 万 | 约 1.8 GB |
| **100,000 块** | **204** | **940 万** | **约 3.6 GB** |
| 500,000 块 | 41 | 4,700 万 | 约 18 GB |

选 100,000 块的理由：单分区 3.6 GB 时 `REINDEX`/`VACUUM FULL` 在维护窗口内可完成；204 个分区的规划开销
在 PostgreSQL 里仍属正常范围（经验上限约 1,000 个）。50,000 块会让分区数翻倍而收益有限。

**不做二级按合约分区**：当前只有 1 个合约，多合约后也预计在 10 个以内。二级分区会把分区数乘以合约数，
而收益只是「按合约查询少扫几个分区」——`contract` 上的索引已经能解决。等合约数超过 20 再重新评估。

---

## 4. Schema 变更：主键必须含分区键

PostgreSQL 要求分区表的唯一约束**包含分区键**。这是整个方案里改动最大的一处：

```sql
-- 现在
PRIMARY KEY (tx_hash, log_index)
-- 分区后
PRIMARY KEY (block_number, tx_hash, log_index)
```

唯一性语义不变（一笔交易只属于一个块），但**所有 `ON CONFLICT` 子句都要跟着改**：

| 位置 | 现在 | 分区后 |
|---|---|---|
| `store/logs.go` 的 `mergeStagingSQL` | `ON CONFLICT (tx_hash, log_index) DO NOTHING` | `ON CONFLICT (block_number, tx_hash, log_index) DO NOTHING` |
| `store/decoded.go` 的 `insertDecodedSQL` / `upsertDecodedSQL` | `ON CONFLICT (tx_hash, log_index)` | 同上（见下） |

`decoded_event` 目前没有 `block_number`，只有 `(tx_hash, log_index)`。两个选择：

- **A. 加 `block_number` 并分区**（推荐）：多一列 8 字节，换来 `DeleteFrom` 从
  「`WHERE (tx_hash, log_index) IN (SELECT … FROM chain_log WHERE block_number >= $1)`」
  变成「`DELETE WHERE block_number >= $1`」，且能按块裁剪。
- B. 不分区：`decoded_event` 会长到 19 亿行，成为单表瓶颈，`DeleteFrom` 的子查询在大表上代价更高。

选 A。代价是写入时多带一个字段（索引器已经有 `raw.BlockNumber`，不需要额外查询）。

```sql
-- 目标 schema（节选）
CREATE TABLE chain_log (
    block_number bigint      NOT NULL,
    block_time   timestamptz NOT NULL,
    tx_hash      bytea       NOT NULL,
    log_index    integer     NOT NULL,
    contract     bytea       NOT NULL,
    topic0       bytea, topic1 bytea, topic2 bytea, topic3 bytea,
    data         bytea       NOT NULL,
    PRIMARY KEY (block_number, tx_hash, log_index)
) PARTITION BY RANGE (block_number);

CREATE TABLE decoded_event (
    block_number bigint  NOT NULL,
    tx_hash      bytea   NOT NULL,
    log_index    integer NOT NULL,
    event_name   text    NOT NULL,
    status       text    NOT NULL DEFAULT 'decoded'
        CHECK (status IN ('decoded', 'undecodable')),
    payload      jsonb   NOT NULL,
    PRIMARY KEY (block_number, tx_hash, log_index)
) PARTITION BY RANGE (block_number);
```

分区命名：`chain_log_p<起始块>`（如 `chain_log_p26400000`），便于按块号定位与管理。
**索引建在父表上**，PostgreSQL 会自动在每个分区上建对应的本地索引；`INSERT` 时自动路由到对应分区
（PG 11+ 有分区路由缓存，单行插入开销可忽略；索引器是批量 COPY 到临时表再 INSERT SELECT，路由按批进行）。

---

## 5. 查询影响：哪些能裁剪，哪些不能

| 查询 | 过滤条件 | 分区裁剪 | 说明 |
|---|---|---|---|
| `/api/events`（带 from/to） | `block_number BETWEEN` | ✅ 只扫相关分区 | 主要收益 |
| `/api/events`（无过滤） | `ORDER BY block_number DESC LIMIT n` | ✅ 只扫最新分区 | 倒序扫描凑够即停 |
| `/api/stats` 的 `min/max` | 聚合 | ⚠️ 需扫所有分区索引 | 每次多 204 次索引查找，仍是毫秒级 |
| `/api/address/{a}/balances` | `topic1/topic2` | ❌ 跨全部分区 | **这是分区的主要代价** |
| `/api/address/{a}/approvals` | `topic2` | ❌ 跨全部分区 | 同上 |
| reorg `DeleteFrom(n)` | `block_number >= n` | ✅ 只碰尾部 | O(1) 到尾部几个分区 |

画像查询没有块范围，会 append 所有分区的 `idx_log_topic1/2` 索引扫描。204 个分区时，
planner 要为每个分区生成一个 Append 子节点——计划时间从微秒级升到毫秒级，执行时间取决于各分区索引大小之和。

缓解手段（按优先级）：

1. **给画像接口加可选 `from`/`to`**（前端先按「最近 N 块」查，用户显式展开全区间时才扫全部）。
2. `statement_timeout` 已经存在（3s），最坏情况会返回 503 而不是拖垮库。
3. 若确实需要全区间画像，再考虑 `topic1` 上的 BRIN 或用 `decoded_event` 的物化投影（当前被「不预计算」约束禁止，需要重新权衡）。

**不要**为了画像查询去建二级合约分区，也不要给每个分区手工建不同的索引——两者都会让维护复杂度失控。

---

## 6. 迁移步骤（停机窗口约 10–20 分钟）

> 前提：索引器与 API 同时停止。主键变更必须与代码发布同窗口，否则索引器的 `ON CONFLICT` 会直接报错。

```sql
-- 1. 建父表与新分区（覆盖现有区间 + 预建未来 2 个分区）
--    见 scripts/partition/0001_partition_tables.sql（手工执行，不进 migrations）
-- 2. 搬迁数据（单事务，4.5M 行约 1–2 分钟）
INSERT INTO chain_log_new SELECT * FROM chain_log;
INSERT INTO decoded_event_new (block_number, tx_hash, log_index, event_name, status, payload)
SELECT l.block_number, d.tx_hash, d.log_index, d.event_name, d.status, d.payload
FROM decoded_event d JOIN chain_log l USING (tx_hash, log_index);

-- 3. 校验（必须相等，否则回滚）
SELECT (SELECT count(*) FROM chain_log) = (SELECT count(*) FROM chain_log_new);

-- 4. 切换（同一事务）
ALTER TABLE chain_log RENAME TO chain_log_pre_partition;
ALTER TABLE chain_log_new RENAME TO chain_log;
-- decoded_event 同理
```

回滚：把两张表的 `RENAME` 反过来即可，旧表在验证通过前不要 `DROP`。

**迁移后必须做的三件事**：

1. 改 `store/logs.go` 与 `store/decoded.go` 的 `ON CONFLICT` 列表（否则索引器第一批就失败）。
2. `store/decoded.go` 的 `InsertDecoded` 参数增加 `block_number`。
3. `store/headers.go` 的 `DeleteFrom` 改成 `DELETE WHERE block_number >= $1`，删掉子查询。

`cmd/verify` 与 `cmd/redecode` 不需要改：它们读的是 `store` 导出的方法。

---

## 7. 归档策略：先定规则，暂不实施

产品语义是「覆盖某个区间的完整历史」，**归档等于放弃该区间的查询能力**，所以这不是纯运维决定。

规则（建议）：

- **热数据**：最近 12 个月，保留在 `chainlens` 库的主分区里。
- **冷数据**：更早的分区 `DETACH`，`pg_dump` 压缩后存冷存储，然后 `DROP`。
- **不做**「同库冷表」：那只是把问题挪到另一张表，仍然占同一块磁盘。

执行方式（每年一到两次）：

```sql
ALTER TABLE chain_log DETACH PARTITION chain_log_p24000000;
-- 导出后
DROP TABLE chain_log_p24000000;
```

⚠️ 归档后 `/api` 的 `coverage.from_block` 会前移，前端会如实显示「已索引区间」的起点变化——
这正是 `coverage` 随响应返回的意义：空结果要能区分「没有记录」与「这段历史不在库里」。

---

## 8. 何时必须实施

不是「越早越好」，而是有明确的触发条件：

| 触发条件 | 当前值 | 阈值 |
|---|---|---|
| `chain_log` 单表大小 | 1.6 GB | 50 GB |
| `chain_log` 行数 | 450 万 | 5,000 万 |
| `/api/stats` 全表 count | 约 0.6 s（450 万行） | 3 s |
| 画像查询最坏耗时 | 约 200 ms | 1 s |

50 GB 约对应 1.3 亿行（按 381 字节/行算 `chain_log` 单表）。全量 3.6 亿行分摊到 20,393,092 块，
平均 17.7 条/块，但**近期段是 90+ 条/块**——按末端密度算，5,000 万行约 55 万块（约 2.5 个月）；
按全段平均算约 280 万块（约 13 个月）。取更保守的前者。

也就是说：**全量索引跑起来后，两个月到一年内一定撞线**。这就是「全量索引前必须先定」的原因——
到那时单表已是现在的几十倍，迁移窗口与风险都不再是现在这个量级。

---

## 9. 未决问题

1. **分区维护自动化**：谁来建下个月的分区？建议索引器启动时检查「未来 2 个分区是否存在」，缺则创建
   （需要 `CREATE TABLE` 权限）。不做「触发器自动建分区」——在批量 COPY 的事务里创建表会带来锁与回滚风险。
2. **`decoded_event` 的 payload 是否拆表**：Transfer 的 payload 只有 3 个字段，jsonb 的存储开销（约 120 字节）
   与解析成本都高于拆成列。但拆列会让不同事件的字段无处安放。当前选择保留 jsonb，把它视为「事件参数的可变容器」，
   等画像查询真的因此变慢再议。
3. **是否用 `pg_partman`**：能自动化分区创建与保留策略，但引入一个扩展依赖。当前项目坚持「只有 go-ethereum 与 pgx
   两个直接依赖」，数据库侧也应保持同样的克制。先手写维护逻辑。
