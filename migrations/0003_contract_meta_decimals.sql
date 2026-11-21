-- 0003_contract_meta_decimals.sql
-- F4：金额换算要用每个合约自己的小数位。
--
-- 之前前端写死 USDC 的 6 位，索引其他代币时金额会差 10^N。把它落到元信息里，
-- 接口按合约返回，前端不再猜。
--
-- 用 smallint 而不是 integer：ERC-20 的 decimals 是 uint8（0..255）。
-- 更窄的类型让「不该出现负数与超大值」由 CHECK 约束表达，而不是靠代码自觉。
-- 允许 NULL 表示「还没取到」，这点很重要：NULL 必须与 0 区分开，
-- 前者是未知，后者是「整数型代币」。
BEGIN;

ALTER TABLE contract_meta
    ADD COLUMN decimals smallint
        CHECK (decimals IS NULL OR (decimals >= 0 AND decimals <= 255));

COMMIT;
