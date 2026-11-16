package indexer

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5"

	"github.com/dyyyyy-1/chainlens/internal/decoder"
	"github.com/dyyyyy-1/chainlens/internal/store"
)

// LogFetcher 由索引器声明、由 internal/chain 实现（接口定义在使用方，HANDOFF.md 第 11 节）。
type LogFetcher interface {
	Fetch(ctx context.Context, from, to uint64, addr common.Address) ([]types.Log, error)
}

// HeaderFetcher 提供块头与链头高度。
type HeaderFetcher interface {
	LatestBlockNumber(ctx context.Context) (uint64, error)
	HeadersByRange(ctx context.Context, numbers []uint64) (map[uint64]*types.Header, error)
}

// LogStore 是索引器所需的持久化能力。写方法接受调用方事务，以便与游标推进同事务提交。
type LogStore interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	EnsureCursor(ctx context.Context, chainID, startBlock uint64) error
	LoadCursor(ctx context.Context) (store.Cursor, error)
	InsertHeaders(ctx context.Context, tx pgx.Tx, records []store.HeaderRecord) (int64, error)
	InsertLogs(ctx context.Context, tx pgx.Tx, records []store.LogRecord) (int64, error)
	InsertDecoded(ctx context.Context, tx pgx.Tx, records []store.DecodedRecord) (int64, error)
	Advance(ctx context.Context, tx pgx.Tx, nextBlock, finalizedBlock uint64) error
}

// EventDecoder 把原始日志解码为结构化事件，由 internal/decoder 实现。
type EventDecoder interface {
	Decode(entry types.Log) decoder.Event
}

// Config 是索引器运行参数，取自 HANDOFF.md 第 10 节的环境变量。
type Config struct {
	ChainID       uint64
	Contract      common.Address
	StartBlock    uint64
	Confirmations uint64
	BatchBlocks   uint64
	PollInterval  time.Duration
}

// Scanner 驱动索引主循环。
type Scanner struct {
	logs    LogFetcher
	headers HeaderFetcher
	store   LogStore
	decoder EventDecoder
	cfg     Config
	logger  *slog.Logger
}

// New 构造索引器。logger 为 nil 时回落到 slog 默认实例。
func New(logs LogFetcher, headers HeaderFetcher, st LogStore, dec EventDecoder, cfg Config, logger *slog.Logger) *Scanner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scanner{logs: logs, headers: headers, store: st, decoder: dec, cfg: cfg, logger: logger}
}

// Run 持续扫描直到 ctx 结束。
//
// 单批失败不终止进程：记录错误后等待下一轮重试。游标未推进，因此重试不会产生重复数据。
func (s *Scanner) Run(ctx context.Context) error {
	if err := s.store.EnsureCursor(ctx, s.cfg.ChainID, s.cfg.StartBlock); err != nil {
		return err
	}

	for {
		if err := s.scanOnce(ctx); err != nil {
			if ctx.Err() != nil {
				s.logger.Info("indexer stopped", "reason", ctx.Err())
				return nil
			}
			s.logger.Error("scan batch failed", "err", err)
		}

		select {
		case <-ctx.Done():
			s.logger.Info("indexer stopped", "reason", ctx.Err())
			return nil
		case <-time.After(s.cfg.PollInterval):
		}
	}
}

// scanOnce 完成一个批次的完整流程：取区间、拉日志、补块头、落库、推进游标。
func (s *Scanner) scanOnce(ctx context.Context) error {
	cursor, err := s.store.LoadCursor(ctx)
	if err != nil {
		return err
	}
	if cursor.ChainID != s.cfg.ChainID {
		return fmt.Errorf("indexer: cursor chain id %d does not match configured chain id %d", cursor.ChainID, s.cfg.ChainID)
	}

	head, err := s.headers.LatestBlockNumber(ctx)
	if err != nil {
		return err
	}

	blockRange, hasWork := nextRange(cursor.NextBlock, head, s.cfg.Confirmations, s.cfg.BatchBlocks)
	if !hasWork {
		return nil
	}

	started := time.Now()

	rawLogs, err := s.logs.Fetch(ctx, blockRange.From, blockRange.To, s.cfg.Contract)
	if err != nil {
		return err
	}

	headers, err := s.headers.HeadersByRange(ctx, blockNumbers(blockRange.From, blockRange.To))
	if err != nil {
		return err
	}

	logRecords := make([]store.LogRecord, 0, len(rawLogs))
	for _, raw := range rawLogs {
		header, hasHeader := headers[raw.BlockNumber]
		if !hasHeader {
			return fmt.Errorf("indexer: block %d returned logs but no header", raw.BlockNumber)
		}
		logRecords = append(logRecords, store.NewLogRecord(raw, blockTime(header)))
	}

	decodedRecords, undecoded := s.decodeAll(rawLogs)

	// ADR-1：区块数据与游标必须在同一事务内提交，否则崩溃会导致游标前进而数据缺失。
	tx, err := s.store.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	insertedHeaders, err := s.store.InsertHeaders(ctx, tx, headerRecords(headers, blockRange.To))
	if err != nil {
		return err
	}
	insertedLogs, err := s.store.InsertLogs(ctx, tx, logRecords)
	if err != nil {
		return err
	}
	insertedDecoded, err := s.store.InsertDecoded(ctx, tx, decodedRecords)
	if err != nil {
		return err
	}
	if err := s.store.Advance(ctx, tx, blockRange.To+1, blockRange.To); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("indexer: commit batch %d..%d: %w", blockRange.From, blockRange.To, err)
	}

	s.logger.Info("batch committed",
		"from", blockRange.From,
		"to", blockRange.To,
		"logs_fetched", len(rawLogs),
		"logs_inserted", insertedLogs,
		"decoded_inserted", insertedDecoded,
		"decoded_undecodable", undecoded,
		"headers_inserted", insertedHeaders,
		"elapsed_ms", time.Since(started).Milliseconds(),
	)

	return nil
}

// decodeAll 解码整批日志，返回待入库记录与未能解码的条数。
//
// 单条解码失败只影响该条的状态，不中断同批其它日志，也不影响入库（HANDOFF.md 8.3）。
func (s *Scanner) decodeAll(rawLogs []types.Log) ([]store.DecodedRecord, int) {
	records := make([]store.DecodedRecord, 0, len(rawLogs))
	undecoded := 0

	for _, raw := range rawLogs {
		event := s.decoder.Decode(raw)
		if !event.IsDecoded() {
			undecoded++
			s.logUndecoded(raw, event)
		}
		records = append(records, store.DecodedRecord{
			TxHash:    event.TxHash,
			LogIndex:  event.LogIndex,
			EventName: event.Name,
			Status:    event.Status,
			Payload:   event.Payload,
		})
	}

	return records, undecoded
}

// logUndecoded 按原因分级记录。
//
// 「ABI 里没有这个事件」是常态——绝大多数合约只把少数事件写进我们手上的 ABI——
// 用 Debug 避免刷屏；只有 ABI 命中却解不开（data 长度非法、indexed 超限）才是异常，用 Warn。
func (s *Scanner) logUndecoded(raw types.Log, event decoder.Event) {
	attributes := []any{
		"tx", raw.TxHash.Hex(),
		"log_index", raw.Index,
		"topics", len(raw.Topics),
		"data_len", len(raw.Data),
		"reason", event.Reason,
	}

	switch event.Reason {
	case decoder.ReasonMalformed, decoder.ReasonTooManyIndexed:
		s.logger.Warn("log not decodable", attributes...)
	default:
		s.logger.Debug("log not decodable", attributes...)
	}
}

// blockNumbers 生成闭区间内的全部块号，供批量拉取块头使用。
//
// 之所以写整段而不是只写「有日志的块」：reorg 检测要靠 parent_hash 把块头连成链
// （HANDOFF.md 8.1），缺块就会让链断开、无法判断分叉深度。对高频合约两者的请求数
// 几乎一致，对低频合约多出的几次批量调用可以忽略。
func blockNumbers(from, to uint64) []uint64 {
	if to < from {
		return nil
	}

	numbers := make([]uint64, 0, to-from+1)
	for number := from; number <= to; number++ {
		numbers = append(numbers, number)
	}

	return numbers
}

// headerRecords 把块头映射转换为待写入记录，按块号升序保证写入顺序稳定。
func headerRecords(headers map[uint64]*types.Header, finalizedUpTo uint64) []store.HeaderRecord {
	records := make([]store.HeaderRecord, 0, len(headers))
	for number, header := range headers {
		records = append(records, store.HeaderRecord{
			Number:      number,
			Hash:        header.Hash(),
			ParentHash:  header.ParentHash,
			BlockTime:   blockTime(header),
			IsFinalized: number <= finalizedUpTo,
		})
	}

	slices.SortFunc(records, func(a, b store.HeaderRecord) int {
		switch {
		case a.Number < b.Number:
			return -1
		case a.Number > b.Number:
			return 1
		default:
			return 0
		}
	})

	return records
}

// blockTime 把块头中的 unix 秒转为 UTC 时间。
func blockTime(header *types.Header) time.Time {
	return time.Unix(int64(header.Time), 0).UTC()
}
