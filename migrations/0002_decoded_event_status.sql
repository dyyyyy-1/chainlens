-- 0002_decoded_event_status.sql
-- §8.3 要求把无法解码的日志标记为 undecodable，而 §6 的 decoded_event 没有状态列。
-- 决策：状态单独成列，不塞进 payload，避免污染业务字段（W3 的 API 直接消费 payload）。
-- 执行：psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/0002_decoded_event_status.sql

BEGIN;

ALTER TABLE decoded_event
    ADD COLUMN status text NOT NULL DEFAULT 'decoded'
        CHECK (status IN ('decoded', 'undecodable'));

COMMIT;
