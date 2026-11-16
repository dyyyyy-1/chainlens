// Package store 负责 ChainLens 的持久化：批量写入日志与块头、维护索引游标。
//
// 所有写方法都接受调用方提供的 pgx.Tx，游标推进与数据写入由上层保证在同一事务内提交
// （HANDOFF.md ADR-1：跨存储无法保证「游标前进但数据丢失」不发生）。
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store 是 PostgreSQL 连接池的持有者。
type Store struct {
	pool *pgxpool.Pool
}

// New 建立连接池并校验连通性。
func New(ctx context.Context, databaseURL string) (*Store, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("store: empty database URL")
	}

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping database: %w", err)
	}

	return &Store{pool: pool}, nil
}

// Close 释放连接池。
func (s *Store) Close() {
	s.pool.Close()
}

// Begin 开启一个事务，用于把区块数据与游标推进绑定在一起。
func (s *Store) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin transaction: %w", err)
	}
	return tx, nil
}

// LogRecord 是 chain_log 一行在内存中的形态。
type LogRecord struct {
	BlockNumber uint64
	BlockTime   time.Time
	TxHash      common.Hash
	LogIndex    uint32
	Contract    common.Address
	Topic0      *common.Hash
	Topic1      *common.Hash
	Topic2      *common.Hash
	Topic3      *common.Hash
	Data        []byte
}

// HeaderRecord 是 block_header 一行在内存中的形态。
type HeaderRecord struct {
	Number      uint64
	Hash        common.Hash
	ParentHash  common.Hash
	BlockTime   time.Time
	IsFinalized bool
}

// Cursor 是 index_cursor 单行表的快照。
//
// NextBlock 表示下一个待扫描的高度；FinalizedBlock 是已确认水位。
type Cursor struct {
	ChainID        uint64
	NextBlock      uint64
	FinalizedBlock uint64
}

// DecodedRecord 是 decoded_event 一行在内存中的形态。
//
// Status 取 decoded 或 undecodable；未解出参数的记录同样入库，payload 为空对象。
type DecodedRecord struct {
	TxHash    common.Hash
	LogIndex  uint32
	EventName string
	Status    string
	Payload   map[string]any
}

// ContractMeta 是 contract_meta 一行在内存中的形态。
//
// Implementation 为 nil 表示识别结果不是代理；ABI 为 nil 表示尚未获取。
type ContractMeta struct {
	Address        common.Address
	Implementation *common.Address
	ABI            []byte
	ABISource      string
	ResolvedAt     time.Time
}

// NewLogRecord 把 RPC 返回的原始日志转换为可写入 chain_log 的记录。
//
// topic 数量可变（0~4），超出部分置空；data 为 nil 时写入空串以满足 NOT NULL 约束。
func NewLogRecord(raw types.Log, blockTime time.Time) LogRecord {
	var topics [4]*common.Hash
	for i := range min(len(raw.Topics), len(topics)) {
		topic := raw.Topics[i]
		topics[i] = &topic
	}

	data := raw.Data
	if data == nil {
		data = []byte{}
	}

	return LogRecord{
		BlockNumber: raw.BlockNumber,
		BlockTime:   blockTime,
		TxHash:      raw.TxHash,
		LogIndex:    uint32(raw.Index), // 单块日志数远小于 2^32，截断不可能发生
		Contract:    raw.Address,
		Topic0:      topics[0],
		Topic1:      topics[1],
		Topic2:      topics[2],
		Topic3:      topics[3],
		Data:        data,
	}
}
