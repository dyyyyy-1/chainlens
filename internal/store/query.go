package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

// 本文件是 chain_log / decoded_event / index_cursor 的**只读**查询面，
// 与写路径（logs.go / headers.go / decoded.go）分开：
// 写路径服务于索引器的事务语义，读路径服务于查询服务，两者的事务要求不同。
//
// 只有一个文件而不是 events.go + stats.go：三个方法都是对同一批表的聚合，
// 拆开会让「覆盖区间」这种被多处复用的查询无处安放。

// EventQuery 是事件检索的过滤条件。字段为零值表示该维度不限。
type EventQuery struct {
	Contract  *common.Address
	FromBlock *uint64
	ToBlock   *uint64
	// Cursor 是上一页最后一行，走键集分页；为空表示从最新开始。
	// 与 Offset 互斥，Cursor 优先。
	Cursor *EventCursor
	Limit  int
	Offset int
}

// EventCursor 是键集分页的游标，对应 chain_log 排序键 (block_number, log_index)。
//
// 用键集而不是 OFFSET：OFFSET 让数据库先数过前面所有行，翻到第 1 万页时
// 前 1 万页的行都要被扫出来。键集的代价恒定，且不会因并发写入而跳行。
type EventCursor struct {
	BlockNumber uint64
	LogIndex    uint32
}

// Event 是 chain_log 一行与解码结果的组合读模型。
//
// EventName / Status / Payload 在解码记录缺失时为零值，因此调用方应看 Status
// 而不是假定 EventName 一定有值。
type Event struct {
	BlockNumber uint64
	BlockTime   time.Time
	TxHash      common.Hash
	LogIndex    uint32
	Contract    common.Address
	EventName   string
	Status      string
	Payload     []byte
}

// Coverage 是当前已索引的区间与水位，用来给每个响应标注「这份数据覆盖到哪里」。
//
// 没有它，部分索引的数据会被读成「该地址没有历史」，这是本项目最容易出现的误读。
type Coverage struct {
	FromBlock       uint64
	ToBlock         uint64
	FinalizedBlock  uint64
	NextBlock       uint64
	LatestBlockTime time.Time
}

// Stats 是索引器的运行快照。
type Stats struct {
	Coverage
	TotalLogs        int64
	DecodedCount     int64
	UndecodableCount int64
}

const listEventsBase = `
SELECT l.block_number, l.block_time, l.tx_hash, l.log_index, l.contract,
       coalesce(d.event_name, ''), coalesce(d.status, ''), coalesce(d.payload, '{}'::jsonb)
FROM chain_log l
LEFT JOIN decoded_event d ON d.tx_hash = l.tx_hash AND d.log_index = l.log_index`

// ListEvents 按过滤条件检索事件，按区块与日志序号倒序返回。
//
// 条件按需拼接而不是写成 `($1 IS NULL OR ...)`：后者会让规划器无法使用
// idx_log_block 的倒序扫描，本该提前停止的查询变成全表扫描。
//
// 排序与索引方向一致，配合 LIMIT 时规划器可以走索引倒序扫描、凑够条数即停；
// 一旦指定 from/to，就会先按 block_number 位图定位再排序。OFFSET 深翻页会退化，
// 这是已知取舍（游标分页留到需要时再做）。
func (s *Store) ListEvents(ctx context.Context, q EventQuery) ([]Event, error) {
	query, args := buildEventQuery(q)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}
	defer rows.Close()

	events := make([]Event, 0, q.Limit)
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}

	return events, nil
}

// StreamEvents 逐行读取事件并交给 fn，不在内存里攒整批结果。
//
// 导出用：200k 行 CSV 约 40 MB，先攒进切片再写响应会让进程内存随导出行数上升。
// 回调返回错误即停止读取（例如客户端断开）。
func (s *Store) StreamEvents(ctx context.Context, q EventQuery, fn func(Event) error) error {
	query, args := buildEventQuery(q)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: stream events: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return err
		}
		if err := fn(event); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: stream events: %w", err)
	}
	return nil
}

// CountEvents 返回符合过滤条件的行数。
//
// 导出前先数一遍，是为了在写响应头之前就能拒绝「范围过大」——
// 一旦开始流式输出，就没法再改状态码，只能给用户一个不完整的文件。
func (s *Store) CountEvents(ctx context.Context, q EventQuery) (int64, error) {
	conditions, args := buildEventConditions(q)

	query := "SELECT count(*) FROM chain_log l"
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}

	var count int64
	if err := s.pool.QueryRow(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count events: %w", err)
	}
	return count, nil
}

// buildEventQuery 把过滤条件拼成完整 SQL，供分页与导出共用。
//
// 共用同一个构造函数，是为了让导出与页面看到完全相同的数据与排序：
// 两处各写一份条件，迟早会出现「页面少一条、导出多一条」这种难以解释的差异。
func buildEventQuery(q EventQuery) (string, []any) {
	conditions, args := buildEventConditions(q)

	query := listEventsBase
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}

	order := " ORDER BY l.block_number DESC, l.log_index DESC"
	if q.Cursor != nil {
		args = append(args, q.Limit)
		return query + order + fmt.Sprintf(" LIMIT $%d", len(args)), args
	}

	args = append(args, q.Limit, q.Offset)
	return query + order + fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args
}

// buildEventConditions 按需拼接 WHERE 条件与参数。
func buildEventConditions(q EventQuery) ([]string, []any) {
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 5)

	if q.Contract != nil {
		args = append(args, q.Contract.Bytes())
		conditions = append(conditions, fmt.Sprintf("l.contract = $%d", len(args)))
	}
	if q.FromBlock != nil {
		args = append(args, int64(*q.FromBlock))
		conditions = append(conditions, fmt.Sprintf("l.block_number >= $%d", len(args)))
	}
	if q.ToBlock != nil {
		args = append(args, int64(*q.ToBlock))
		conditions = append(conditions, fmt.Sprintf("l.block_number <= $%d", len(args)))
	}
	if q.Cursor != nil {
		// 行比较 (block_number, log_index) < (cursor)：与倒序排序键同向，
		// 规划器可以从游标位置继续沿索引倒序扫描，不需要回退。
		args = append(args, int64(q.Cursor.BlockNumber), int32(q.Cursor.LogIndex))
		conditions = append(conditions, fmt.Sprintf("(l.block_number, l.log_index) < ($%d, $%d)", len(args)-1, len(args)))
	}

	return conditions, args
}

// scanEvent 读取一行事件。列顺序与 listEventsBase 绑定，两处必须同步改。
func scanEvent(rows pgx.Rows) (Event, error) {
	var (
		event   Event
		txHash  []byte
		payload []byte
	)
	if err := rows.Scan(
		&event.BlockNumber, &event.BlockTime, &txHash, &event.LogIndex, &event.Contract,
		&event.EventName, &event.Status, &payload,
	); err != nil {
		return Event{}, fmt.Errorf("store: scan event: %w", err)
	}
	event.TxHash = common.BytesToHash(txHash)
	event.Payload = payload
	return event, nil
}

// coverageSQL 一次取回水位与区间边界。
//
// min/max 走 idx_log_block 的两端，是 O(1) 级别的索引查找，与表大小无关。
const coverageSQL = `
SELECT i.next_block, i.finalized_block,
       coalesce((SELECT min(block_number) FROM chain_log), 0),
       coalesce((SELECT max(block_number) FROM chain_log), 0),
       (SELECT block_time FROM chain_log ORDER BY block_number DESC LIMIT 1)
FROM index_cursor i
WHERE i.id = 1`

// Coverage 返回已索引区间。数据库为空时区间为 0，LatestBlockTime 为零值。
func (s *Store) Coverage(ctx context.Context) (Coverage, error) {
	var (
		coverage Coverage
		latest   *time.Time
	)
	err := s.pool.QueryRow(ctx, coverageSQL).Scan(
		&coverage.NextBlock, &coverage.FinalizedBlock,
		&coverage.FromBlock, &coverage.ToBlock, &latest,
	)
	if err != nil {
		return Coverage{}, fmt.Errorf("store: load coverage: %w", err)
	}
	if latest != nil {
		coverage.LatestBlockTime = *latest
	}
	return coverage, nil
}

// logsByRangeSQL 按区块区间读取原始日志。
const logsByRangeSQL = `
SELECT block_number, block_time, tx_hash, log_index, contract,
       topic0, topic1, topic2, topic3, data
FROM chain_log
WHERE block_number BETWEEN $1 AND $2
ORDER BY block_number, log_index`

// LogsByRange 读取区间内的原始日志，供对拍与导出使用。
//
// 这是唯一一条可能一次返回大量行的查询：调用方必须自己控制区间宽度
// （cmd/verify 默认 500 块一批）。按 (block_number, log_index) 排序，
// 走 idx_log_block 的区间扫描。
func (s *Store) LogsByRange(ctx context.Context, from, to uint64) ([]LogRecord, error) {
	if from > to {
		return nil, fmt.Errorf("store: invalid block range %d..%d", from, to)
	}

	rows, err := s.pool.Query(ctx, logsByRangeSQL, int64(from), int64(to))
	if err != nil {
		return nil, fmt.Errorf("store: logs by range %d..%d: %w", from, to, err)
	}
	defer rows.Close()

	records := make([]LogRecord, 0, 1024)
	for rows.Next() {
		record, err := scanLogRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: logs by range %d..%d: %w", from, to, err)
	}

	return records, nil
}

// scanLogRecord 读取一行 chain_log 的列布局。
//
// 与 logsByRangeSQL / undecodableLogsSQL 的 SELECT 列顺序绑定：两处查询必须保持
// 同样的列顺序，否则会静默把 topic 装错列。
func scanLogRecord(rows pgx.Rows) (LogRecord, error) {
	var (
		record                 LogRecord
		txHash, contract       []byte
		topic0, topic1, topic2 []byte
		topic3, data           []byte
		logIndex               int32
	)
	if err := rows.Scan(
		&record.BlockNumber, &record.BlockTime, &txHash, &logIndex, &contract,
		&topic0, &topic1, &topic2, &topic3, &data,
	); err != nil {
		return LogRecord{}, fmt.Errorf("store: scan log: %w", err)
	}

	record.TxHash = common.BytesToHash(txHash)
	record.LogIndex = uint32(logIndex)
	record.Contract = common.BytesToAddress(contract)
	record.Topic0 = hashPtr(topic0)
	record.Topic1 = hashPtr(topic1)
	record.Topic2 = hashPtr(topic2)
	record.Topic3 = hashPtr(topic3)
	record.Data = data

	return record, nil
}

// hashPtr 把可空 topic 列转成指针：NULL 与空字节都视为缺失。
func hashPtr(raw []byte) *common.Hash {
	if len(raw) == 0 {
		return nil
	}
	hash := common.BytesToHash(raw)
	return &hash
}

// countSQL 统计日志与解码状态。
//
// 这是全表聚合，代价随表线性增长。当前量级（百万行以内）在几十毫秒，
// 全量索引 USDC 后会退化成秒级；届时应改成计数表或物化视图，
// 但那与「不预计算」的约束冲突，等真到那一步再权衡。
const countSQL = `
SELECT (SELECT count(*) FROM chain_log),
       (SELECT count(*) FROM decoded_event WHERE status = 'decoded'),
       (SELECT count(*) FROM decoded_event WHERE status = 'undecodable')`

// Stats 返回索引水位与事件计数。
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var stats Stats

	coverage, err := s.Coverage(ctx)
	if err != nil {
		return Stats{}, err
	}
	stats.Coverage = coverage

	if err := s.pool.QueryRow(ctx, countSQL).Scan(
		&stats.TotalLogs, &stats.DecodedCount, &stats.UndecodableCount,
	); err != nil {
		return Stats{}, fmt.Errorf("store: load stats: %w", err)
	}

	return stats, nil
}
