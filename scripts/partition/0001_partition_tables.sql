-- 分区改造：建新表 → 搬数据 → 校验 → 切换。
--
-- ⚠️ 不要放进 migrations/：migrations 会被 `make migrate` / 测试自动执行，
--    而这份脚本要求「索引器与 API 已停止」，且只有在新代码（ON CONFLICT 含 block_number）
--    准备就绪时才可切换。它是维护窗口里手工执行的脚本，不是可重复的迁移。
--
-- 用法：
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f scripts/partition/0001_partition_tables.sql
--
-- 参数（按实际库调整）：
--   下面的 26400000 / 100000 / 3 个分区覆盖 26,400,000..26,700,000。
--   现有数据落在 26,422,714.. 起，全量索引前应把起点调到 USDC 部署块 6,082,467。
--
-- 步骤与文档见 docs/PARTITIONING.md 第 6 节。

BEGIN;

-- ---------------------------------------------------------------------------
-- 1. 新表（分区父表）
-- ---------------------------------------------------------------------------

CREATE TABLE chain_log_new (
    block_number bigint      NOT NULL,
    block_time   timestamptz NOT NULL,
    tx_hash      bytea       NOT NULL,
    log_index    integer     NOT NULL,
    contract     bytea       NOT NULL,
    topic0       bytea,
    topic1       bytea,
    topic2       bytea,
    topic3       bytea,
    data         bytea       NOT NULL,
    -- 分区表要求唯一约束包含分区键。唯一性语义不变：一笔交易只属于一个块。
    PRIMARY KEY (block_number, tx_hash, log_index)
) PARTITION BY RANGE (block_number);

CREATE TABLE decoded_event_new (
    -- 新增 block_number：让 reorg 删除与分区裁剪都不必回查 chain_log。
    block_number bigint  NOT NULL,
    tx_hash      bytea   NOT NULL,
    log_index    integer NOT NULL,
    event_name   text    NOT NULL,
    status       text    NOT NULL DEFAULT 'decoded'
        CHECK (status IN ('decoded', 'undecodable')),
    payload      jsonb   NOT NULL,
    PRIMARY KEY (block_number, tx_hash, log_index)
) PARTITION BY RANGE (block_number);

-- 父表上的索引会自动在每个分区上建成本地索引。
CREATE INDEX idx_log_block_new   ON chain_log_new (block_number);
CREATE INDEX idx_log_topic1_new  ON chain_log_new (topic1);
CREATE INDEX idx_log_topic2_new  ON chain_log_new (topic2);

-- ---------------------------------------------------------------------------
-- 2. 分区：每 100,000 块一个，命名带起始块号便于定位
-- ---------------------------------------------------------------------------

DO $$
DECLARE
    first_block bigint := 26400000;
    step        bigint := 100000;
    parts       int    := 3;
    i           int;
    lo          bigint;
    hi          bigint;
BEGIN
    FOR i IN 0..(parts - 1) LOOP
        lo := first_block + i * step;
        hi := lo + step;

        EXECUTE format(
            'CREATE TABLE %I PARTITION OF chain_log_new FOR VALUES FROM (%s) TO (%s)',
            'chain_log_p' || lo, lo, hi);
        EXECUTE format(
            'CREATE TABLE %I PARTITION OF decoded_event_new FOR VALUES FROM (%s) TO (%s)',
            'decoded_event_p' || lo, lo, hi);
    END LOOP;
END $$;

-- ---------------------------------------------------------------------------
-- 3. 搬迁现有数据（显式列名，不依赖列顺序）
-- ---------------------------------------------------------------------------

INSERT INTO chain_log_new (
    block_number, block_time, tx_hash, log_index, contract,
    topic0, topic1, topic2, topic3, data
)
SELECT block_number, block_time, tx_hash, log_index, contract,
       topic0, topic1, topic2, topic3, data
FROM chain_log;

-- decoded_event 原本没有 block_number，只能通过 chain_log 反查补上。
INSERT INTO decoded_event_new (block_number, tx_hash, log_index, event_name, status, payload)
SELECT l.block_number, d.tx_hash, d.log_index, d.event_name, d.status, d.payload
FROM decoded_event d
JOIN chain_log l ON l.tx_hash = d.tx_hash AND l.log_index = d.log_index;

-- ---------------------------------------------------------------------------
-- 4. 校验：数量必须完全相等，否则 MANUAL ROLLBACK 后排查
-- ---------------------------------------------------------------------------

DO $$
DECLARE
    old_logs  bigint;
    new_logs  bigint;
    old_decs  bigint;
    new_decs  bigint;
BEGIN
    SELECT count(*) INTO old_logs FROM chain_log;
    SELECT count(*) INTO new_logs FROM chain_log_new;
    SELECT count(*) INTO old_decs FROM decoded_event;
    SELECT count(*) INTO new_decs FROM decoded_event_new;

    IF old_logs <> new_logs OR old_decs <> new_decs THEN
        RAISE EXCEPTION '搬迁数量不一致：chain_log % → %，decoded_event % → %',
            old_logs, new_logs, old_decs, new_decs;
    END IF;

    RAISE NOTICE '校验通过：chain_log % 行，decoded_event % 行', new_logs, new_decs;
END $$;

-- ---------------------------------------------------------------------------
-- 5. 切换（旧表改名保留，验证通过后再 DROP）
--
-- 执行到这里之前，确认新代码已就绪：store/logs.go 与 store/decoded.go 的
-- ON CONFLICT 必须包含 block_number，否则索引器第一批就会报错。
-- ---------------------------------------------------------------------------

ALTER TABLE chain_log       RENAME TO chain_log_pre_partition;
ALTER TABLE decoded_event   RENAME TO decoded_event_pre_partition;
ALTER TABLE chain_log_new   RENAME TO chain_log;
ALTER TABLE decoded_event_new RENAME TO decoded_event;

-- 索引名在同一 schema 内必须唯一：旧表还占着 idx_log_* ，先把它们挪开，
-- 再让新索引接管标准名。顺序反过来会直接报 duplicate index name。
ALTER INDEX idx_log_block  RENAME TO idx_log_block_pre_partition;
ALTER INDEX idx_log_topic1 RENAME TO idx_log_topic1_pre_partition;
ALTER INDEX idx_log_topic2 RENAME TO idx_log_topic2_pre_partition;

ALTER INDEX idx_log_block_new  RENAME TO idx_log_block;
ALTER INDEX idx_log_topic1_new RENAME TO idx_log_topic1;
ALTER INDEX idx_log_topic2_new RENAME TO idx_log_topic2;

COMMIT;

-- 回滚（在新表验证失败时执行；旧表在 DROP 之前一直可用）：
--   BEGIN;
--   ALTER TABLE chain_log RENAME TO chain_log_partitioned;
--   ALTER TABLE chain_log_pre_partition RENAME TO chain_log;
--   ALTER TABLE decoded_event RENAME TO decoded_event_partitioned;
--   ALTER TABLE decoded_event_pre_partition RENAME TO decoded_event;
--   COMMIT;
