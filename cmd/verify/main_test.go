package main

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/dyyyyy-1/chainlens/internal/store"
)

var (
	testContract = common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48")
	testBlockAt  = time.Date(2026, 11, 17, 6, 39, 35, 0, time.UTC)
)

func chainLog(tx byte, index uint, data byte) types.Log {
	return types.Log{
		BlockNumber: 26433029,
		TxHash:      common.Hash{31: tx},
		Index:       index,
		Address:     testContract,
		Topics:      []common.Hash{{31: 0xaa}, {31: 0xbb}, {31: 0xcc}},
		Data:        []byte{data},
	}
}

func dbLog(tx byte, index uint32, data byte) store.LogRecord {
	topic0 := common.Hash{31: 0xaa}
	topic1 := common.Hash{31: 0xbb}
	topic2 := common.Hash{31: 0xcc}
	return store.LogRecord{
		BlockNumber: 26433029,
		BlockTime:   testBlockAt,
		TxHash:      common.Hash{31: tx},
		LogIndex:    index,
		Contract:    testContract,
		Topic0:      &topic0,
		Topic1:      &topic1,
		Topic2:      &topic2,
		Data:        []byte{data},
	}
}

func headers() map[uint64]*types.Header {
	return map[uint64]*types.Header{
		26433029: {Number: common.Big0, Time: uint64(testBlockAt.Unix())},
	}
}

// TestDiffBatchDetectsNothing 确定「一致」的基线。
func TestDiffBatchDetectsNothing(t *testing.T) {
	got := diffBatch(
		[]types.Log{chainLog(0x01, 7, 0x10)},
		[]store.LogRecord{dbLog(0x01, 7, 0x10)},
		testContract,
		headers(),
	)

	if got.missing != 0 || got.extra != 0 || got.mismatch != 0 || got.timeMismatch != 0 {
		t.Fatalf("相同数据被误判为有差异：%+v", got)
	}
	if got.dbCount != 1 {
		t.Fatalf("dbCount = %d, want 1", got.dbCount)
	}
}

// TestDiffBatchDetectsEveryKind 证明工具真的能发现差异。
//
// 没有这组用例，「0 差异」的结论是不可信的——一个永远返回 0 的比对函数
// 也能得出同样的汇总。
func TestDiffBatchDetectsEveryKind(t *testing.T) {
	cases := []struct {
		name         string
		chainLogs    []types.Log
		dbLogs       []store.LogRecord
		wantMissing  int
		wantExtra    int
		wantMismatch int
		wantTime     int
	}{
		{
			name:        "链上有库里没有",
			chainLogs:   []types.Log{chainLog(0x01, 7, 0x10)},
			dbLogs:      nil,
			wantMissing: 1,
		},
		{
			name:      "库里有链上没有",
			dbLogs:    []store.LogRecord{dbLog(0x02, 3, 0x20)},
			wantExtra: 1,
		},
		{
			name:         "data 被改动",
			chainLogs:    []types.Log{chainLog(0x01, 7, 0x10)},
			dbLogs:       []store.LogRecord{dbLog(0x01, 7, 0x99)},
			wantMismatch: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := diffBatch(tc.chainLogs, tc.dbLogs, testContract, headers())
			if got.missing != tc.wantMissing || got.extra != tc.wantExtra || got.mismatch != tc.wantMismatch || got.timeMismatch != tc.wantTime {
				t.Fatalf("差异计数不符：got %+v, want missing=%d extra=%d mismatch=%d time=%d",
					got, tc.wantMissing, tc.wantExtra, tc.wantMismatch, tc.wantTime)
			}
		})
	}
}

func TestDiffBatchDetectsBlockTimeDrift(t *testing.T) {
	drifted := dbLog(0x01, 7, 0x10)
	drifted.BlockTime = testBlockAt.Add(time.Hour)

	got := diffBatch([]types.Log{chainLog(0x01, 7, 0x10)}, []store.LogRecord{drifted}, testContract, headers())
	if got.timeMismatch != 1 {
		t.Fatalf("block_time 漂移未被发现：%+v", got)
	}
	// 时间不一致不应被算成字段不一致，否则汇总里两类差异会互相掩盖。
	if got.mismatch != 0 {
		t.Fatalf("block_time 漂移被误计入 mismatch：%+v", got)
	}
}

func TestDiffBatchIgnoresOtherContracts(t *testing.T) {
	other := dbLog(0x03, 1, 0x30)
	other.Contract = common.HexToAddress("0x1111111111111111111111111111111111111111")

	got := diffBatch(nil, []store.LogRecord{other}, testContract, nil)
	if got.extra != 0 || got.dbCount != 0 {
		t.Fatalf("非目标合约的日志不应算作差异：%+v", got)
	}
}

func TestResolveRange(t *testing.T) {
	coverage := store.Coverage{FromBlock: 100, ToBlock: 200}

	cases := []struct {
		name    string
		from    uint64
		to      uint64
		wantErr bool
	}{
		{"缺省取库内区间", 0, 0, false},
		{"子区间", 120, 180, false},
		{"起点早于库内", 99, 200, true},
		{"终点晚于库内", 100, 201, true},
		{"起点大于终点", 180, 120, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end, err := resolveRange(tc.from, tc.to, coverage)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，得到 %d..%d", start, end)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外报错：%v", err)
			}
			if tc.from == 0 && (start != coverage.FromBlock || end != coverage.ToBlock) {
				t.Fatalf("缺省区间 = %d..%d，want %d..%d", start, end, coverage.FromBlock, coverage.ToBlock)
			}
		})
	}
}
