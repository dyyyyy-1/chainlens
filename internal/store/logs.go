package store

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

// stagingTable 是本批日志的中转表。用 TEMP + ON COMMIT DROP，生命周期不超出本事务。
//
// 存在的理由：COPY 协议不支持 ON CONFLICT，而幂等写入要求主键冲突时静默跳过。
// 因此先 COPY 进无主键的临时表，再用 INSERT ... ON CONFLICT DO NOTHING 合并进 chain_log。
const stagingTable = "chain_log_staging"

// logColumns 与 chain_log 的列顺序保持一致。
var logColumns = []string{
	"block_number", "block_time", "tx_hash", "log_index", "contract",
	"topic0", "topic1", "topic2", "topic3", "data",
}

const (
	createStagingSQL = `CREATE TEMP TABLE IF NOT EXISTS ` + stagingTable + ` (LIKE chain_log) ON COMMIT DROP`
	clearStagingSQL  = `TRUNCATE ` + stagingTable

	// mergeStagingSQL 把临时表合并进正式表，重复的 (tx_hash, log_index) 静默跳过。
	mergeStagingSQL = `
INSERT INTO chain_log (block_number, block_time, tx_hash, log_index, contract, topic0, topic1, topic2, topic3, data)
SELECT block_number, block_time, tx_hash, log_index, contract, topic0, topic1, topic2, topic3, data
FROM chain_log_staging
ON CONFLICT (tx_hash, log_index) DO NOTHING`
)

// InsertLogs 以 COPY 批量写入日志，返回实际新增的行数（冲突跳过的行不计入）。
//
// 返回值为 0 即代表本批数据此前已全部落库，这是幂等重扫的判定依据（HANDOFF.md 8.4）。
func (s *Store) InsertLogs(ctx context.Context, tx pgx.Tx, records []LogRecord) (int64, error) {
	if len(records) == 0 {
		return 0, nil
	}

	if _, err := tx.Exec(ctx, createStagingSQL); err != nil {
		return 0, fmt.Errorf("store: create staging table: %w", err)
	}
	if _, err := tx.Exec(ctx, clearStagingSQL); err != nil {
		return 0, fmt.Errorf("store: clear staging table: %w", err)
	}

	copied, err := tx.CopyFrom(ctx, pgx.Identifier{stagingTable}, logColumns, pgx.CopyFromSlice(len(records), func(i int) ([]any, error) {
		return logRecordRow(records[i]), nil
	}))
	if err != nil {
		return 0, fmt.Errorf("store: copy %d logs into staging: %w", len(records), err)
	}

	tag, err := tx.Exec(ctx, mergeStagingSQL)
	if err != nil {
		return 0, fmt.Errorf("store: merge %d staged logs: %w", copied, err)
	}

	return tag.RowsAffected(), nil
}

func logRecordRow(record LogRecord) []any {
	return []any{
		int64(record.BlockNumber),
		record.BlockTime,
		record.TxHash.Bytes(),
		int32(record.LogIndex),
		record.Contract.Bytes(),
		topicBytes(record.Topic0),
		topicBytes(record.Topic1),
		topicBytes(record.Topic2),
		topicBytes(record.Topic3),
		record.Data,
	}
}

// topicBytes 把可空 topic 转为 bytea；缺失的 topic 写入 NULL。
func topicBytes(topic *common.Hash) any {
	if topic == nil {
		return nil
	}
	return topic.Bytes()
}
