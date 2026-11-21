package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

// insertDecodedSQL 用 unnest 一次写入多行，payload 以 JSON 文本传入后在库内转 jsonb。
//
// DO NOTHING 是索引器的语义：同一条日志只解一次，重扫不应改写已有结果。
const insertDecodedSQL = `
INSERT INTO decoded_event (tx_hash, log_index, event_name, status, payload)
SELECT tx_hash, log_index, event_name, status, payload::jsonb
FROM unnest($1::bytea[], $2::integer[], $3::text[], $4::text[], $5::text[])
     AS staged(tx_hash, log_index, event_name, status, payload)
ON CONFLICT (tx_hash, log_index) DO NOTHING`

// upsertDecodedSQL 与 insertDecodedSQL 的唯一差别是冲突动作：这里覆盖。
//
// 覆盖是重解的语义：同一行日志的 ABI 变了，旧结果就是过期数据，
// 必须被新结果替换，否则「重解」等于没做。
const upsertDecodedSQL = `
INSERT INTO decoded_event (tx_hash, log_index, event_name, status, payload)
SELECT tx_hash, log_index, event_name, status, payload::jsonb
FROM unnest($1::bytea[], $2::integer[], $3::text[], $4::text[], $5::text[])
     AS staged(tx_hash, log_index, event_name, status, payload)
ON CONFLICT (tx_hash, log_index) DO UPDATE
SET event_name = EXCLUDED.event_name,
    status     = EXCLUDED.status,
    payload    = EXCLUDED.payload`

// InsertDecoded 批量写入解码结果，返回实际新增的行数。
//
// undecodable 的记录同样会写入，只是 payload 为空 —— 这样「哪些日志没解出来」
// 可以直接用 status 列查出，不需要靠 chain_log 与 decoded_event 的左连接反推。
func (s *Store) InsertDecoded(ctx context.Context, tx pgx.Tx, records []DecodedRecord) (int64, error) {
	columns, err := decodedColumns(records)
	if err != nil || len(records) == 0 {
		return 0, err
	}

	tag, err := tx.Exec(ctx, insertDecodedSQL,
		columns.txHashes, columns.logIndexes, columns.names, columns.statuses, columns.payloads)
	if err != nil {
		return 0, fmt.Errorf("store: insert %d decoded events: %w", len(records), err)
	}
	return tag.RowsAffected(), nil
}

// UpsertDecoded 批量覆盖解码结果，返回受影响的行数。
//
// 供 cmd/redecode 使用：行已存在，InsertDecoded 的 DO NOTHING 会让重解静默无效。
// 受影响行数包含冲突更新，因此调用方不能用它判断「新增了多少」。
func (s *Store) UpsertDecoded(ctx context.Context, tx pgx.Tx, records []DecodedRecord) (int64, error) {
	columns, err := decodedColumns(records)
	if err != nil || len(records) == 0 {
		return 0, err
	}

	tag, err := tx.Exec(ctx, upsertDecodedSQL,
		columns.txHashes, columns.logIndexes, columns.names, columns.statuses, columns.payloads)
	if err != nil {
		return 0, fmt.Errorf("store: upsert %d decoded events: %w", len(records), err)
	}
	return tag.RowsAffected(), nil
}

// decodedColumns 把记录摊平成 unnest 所需的并行数组。
func decodedColumns(records []DecodedRecord) (stagedDecoded, error) {
	if len(records) == 0 {
		return stagedDecoded{}, nil
	}

	columns := stagedDecoded{
		txHashes:   make([][]byte, len(records)),
		logIndexes: make([]int32, len(records)),
		names:      make([]string, len(records)),
		statuses:   make([]string, len(records)),
		payloads:   make([]string, len(records)),
	}

	for i, record := range records {
		payload, err := json.Marshal(record.Payload)
		if err != nil {
			return stagedDecoded{}, fmt.Errorf("store: marshal payload of %s#%d: %w", record.TxHash.Hex(), record.LogIndex, err)
		}
		columns.txHashes[i] = record.TxHash.Bytes()
		columns.logIndexes[i] = int32(record.LogIndex)
		columns.names[i] = record.EventName
		columns.statuses[i] = record.Status
		columns.payloads[i] = string(payload)
	}

	return columns, nil
}

type stagedDecoded struct {
	txHashes   [][]byte
	logIndexes []int32
	names      []string
	statuses   []string
	payloads   []string
}

// undecodableSummarySQL 统计存量未解码的规模与分布。
//
// 不走 (block_number) 索引：过滤条件是 decoded_event.status，而该列没有索引。
// 这是运维命令的一次性全表扫描（百万行量级为数百毫秒），不值得为它加索引。
const undecodableSummarySQL = `
SELECT count(*),
       count(DISTINCT cl.block_number),
       coalesce(min(cl.block_number), 0),
       coalesce(max(cl.block_number), 0)
FROM decoded_event de
JOIN chain_log cl ON cl.tx_hash = de.tx_hash AND cl.log_index = de.log_index
WHERE de.status <> 'decoded'
  AND cl.contract = $1`

// UndecodableSummary 是存量未解码日志的分布概况。
type UndecodableSummary struct {
	Rows     int64
	Blocks   int64
	MinBlock uint64
	MaxBlock uint64
}

// UndecodableSummary 返回某个合约未解码日志的条数、涉及块数与块号区间。
//
// 按合约统计而不是全表：多合约索引后每个合约的 ABI 不同，
// 「用谁的 ABI 重解」必须先按合约分开，否则会把一个合约的日志交给另一个的 ABI。
func (s *Store) UndecodableSummary(ctx context.Context, contract common.Address) (UndecodableSummary, error) {
	var summary UndecodableSummary
	if err := s.pool.QueryRow(ctx, undecodableSummarySQL, contract.Bytes()).Scan(
		&summary.Rows, &summary.Blocks, &summary.MinBlock, &summary.MaxBlock,
	); err != nil {
		return UndecodableSummary{}, fmt.Errorf("store: undecodable summary: %w", err)
	}
	return summary, nil
}

// undecodableLogsSQL 取出区间内仍未解码的原始日志。
//
// 只取 undecodable 的行：已解码的行重解没有意义，把它们拉回内存只是浪费带宽。
const undecodableLogsSQL = `
SELECT cl.block_number, cl.block_time, cl.tx_hash, cl.log_index, cl.contract,
       cl.topic0, cl.topic1, cl.topic2, cl.topic3, cl.data
FROM chain_log cl
JOIN decoded_event de ON de.tx_hash = cl.tx_hash AND de.log_index = cl.log_index
WHERE de.status <> 'decoded'
  AND cl.block_number BETWEEN $1 AND $2
  AND cl.contract = $3
ORDER BY cl.block_number, cl.log_index`

// UndecodableLogs 返回闭区间内某合约未解码的原始日志，按 (block_number, log_index) 升序。
//
// 调用方必须自己控制区间宽度：这是可能一次返回大量行的查询，与 LogsByRange 同理。
func (s *Store) UndecodableLogs(ctx context.Context, from, to uint64, contract common.Address) ([]LogRecord, error) {
	if from > to {
		return nil, fmt.Errorf("store: invalid block range %d..%d", from, to)
	}

	rows, err := s.pool.Query(ctx, undecodableLogsSQL, int64(from), int64(to), contract.Bytes())
	if err != nil {
		return nil, fmt.Errorf("store: undecodable logs %d..%d: %w", from, to, err)
	}
	defer rows.Close()

	records := make([]LogRecord, 0, 256)
	for rows.Next() {
		record, err := scanLogRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: undecodable logs %d..%d: %w", from, to, err)
	}

	return records, nil
}
