-- 0001_init.sql
-- ChainLens 初始数据模型，逐字对应 HANDOFF.md 第 6 节。
-- 执行：psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/0001_init.sql

BEGIN;

-- 索引游标（单行表）
CREATE TABLE index_cursor (
    id              smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    chain_id        bigint      NOT NULL,
    next_block      bigint      NOT NULL,
    finalized_block bigint      NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- 块头，用于 reorg 检测
CREATE TABLE block_header (
    number       bigint      PRIMARY KEY,
    hash         bytea       NOT NULL,
    parent_hash  bytea       NOT NULL,
    block_time   timestamptz NOT NULL,
    is_finalized boolean     NOT NULL DEFAULT false
);

-- 原始日志（不可变事实）
CREATE TABLE chain_log (
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
    PRIMARY KEY (tx_hash, log_index)
);
CREATE INDEX idx_log_block  ON chain_log (block_number);
CREATE INDEX idx_log_topic1 ON chain_log (topic1);
CREATE INDEX idx_log_topic2 ON chain_log (topic2);

-- 解码结果（可与 chain_log 合并，先分表便于对照调试）
CREATE TABLE decoded_event (
    tx_hash    bytea   NOT NULL,
    log_index  integer NOT NULL,
    event_name text    NOT NULL,
    payload    jsonb   NOT NULL,
    PRIMARY KEY (tx_hash, log_index)
);

-- 合约元信息缓存
CREATE TABLE contract_meta (
    address        bytea PRIMARY KEY,
    implementation bytea,
    abi            jsonb,
    abi_source     text,          -- etherscan | manual | fallback
    resolved_at    timestamptz NOT NULL DEFAULT now()
);

-- 调试视图
CREATE VIEW logs_hex AS
SELECT block_number, '0x'||encode(tx_hash,'hex') AS tx_hash, log_index,
       '0x'||encode(contract,'hex') AS contract,
       '0x'||encode(topic0,'hex')   AS topic0
FROM chain_log;

COMMIT;
