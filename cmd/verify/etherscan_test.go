package main

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/dyyyyy-1/chainlens/internal/store"
)

func TestParseQuantity(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want uint64
		bad  bool
	}{
		{"十六进制", "0x1935854", 0x1935854, false},
		{"大写前缀", "0X10", 16, false},
		{"十进制", "26433620", 26433620, false},
		{"前后空白", " 0x2 ", 2, false},
		{"空串", "", 0, true},
		{"只有前缀", "0x", 0, true},
		{"非法字符", "0xzz", 0, true},
		{"负十六进制", "0x-1", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseQuantity(tc.in)
			if tc.bad {
				if err == nil {
					t.Fatalf("parseQuantity(%q) 应报错，得到 %d", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseQuantity(%q) 失败：%v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("parseQuantity(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsBlankQuantity(t *testing.T) {
	blank := []string{"", "  ", "0x", "0X"}
	for _, raw := range blank {
		if !isBlankQuantity(raw) {
			t.Fatalf("%q 应判定为空数量", raw)
		}
	}
	for _, raw := range []string{"0x0", "0", "0x2"} {
		if isBlankQuantity(raw) {
			t.Fatalf("%q 不应判定为空数量", raw)
		}
	}
}

func TestEtherscanLogToTypesLog(t *testing.T) {
	entry := etherscanLog{
		Address:         "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48",
		Topics:          []string{"0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"},
		Data:            "0x0000000000000000000000000000000000000000000000000000000000904bd0",
		BlockNumber:     "0x1935854",
		TimeStamp:       "0x6afc127f",
		LogIndex:        "0x",
		TransactionHash: "0xf757c8ebf7640bd4972450a53fdf2beeedb58caab966c10aa8df710f8790c264",
	}

	parsed, blockTime, err := entry.toTypesLog()
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}

	if parsed.BlockNumber != 0x1935854 {
		t.Fatalf("blockNumber = %d, want %d", parsed.BlockNumber, 0x1935854)
	}
	if parsed.Address != common.HexToAddress(entry.Address) {
		t.Fatalf("address = %s", parsed.Address.Hex())
	}
	if parsed.TxHash != common.HexToHash(entry.TransactionHash) {
		t.Fatalf("txHash = %s", parsed.TxHash.Hex())
	}
	if len(parsed.Topics) != 1 || parsed.Topics[0] != common.HexToHash(entry.Topics[0]) {
		t.Fatalf("topics = %v", parsed.Topics)
	}
	if len(parsed.Data) != 32 {
		t.Fatalf("data 长度 = %d, want 32", len(parsed.Data))
	}

	// 空 logIndex 必须是 indexUnknown，不能退化成 0——0 是合法的块内索引。
	if parsed.Index != indexUnknown {
		t.Fatalf("index = %d, want indexUnknown", parsed.Index)
	}
	if blockTime.Unix() != 0x6afc127f {
		t.Fatalf("blockTime = %d, want %d", blockTime.Unix(), 0x6afc127f)
	}
}

func TestEtherscanLogRejectsBadData(t *testing.T) {
	cases := []etherscanLog{
		{Address: "not-an-address", Data: "0x"},
		{Address: "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", Data: "0xzz"},
		{Address: "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", Data: "0x", BlockNumber: "0xzz"},
	}
	for i, entry := range cases {
		if _, _, err := entry.toTypesLog(); err == nil {
			t.Fatalf("第 %d 个用例应报错", i)
		}
	}
}

func TestSampleRanges(t *testing.T) {
	t.Run("区间小于抽样总量时整段返回", func(t *testing.T) {
		got := sampleRanges(100, 104, 10, 3)
		if len(got) != 1 || got[0] != [2]uint64{100, 104} {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("均匀抽样且不越界", func(t *testing.T) {
		got := sampleRanges(100, 199, 10, 3)
		if len(got) != 10 {
			t.Fatalf("段数 = %d, want 10", len(got))
		}
		if got[0] != [2]uint64{100, 102} {
			t.Fatalf("首段 = %v, want [100 102]", got[0])
		}
		if got[9] != [2]uint64{190, 192} {
			t.Fatalf("末段 = %v, want [190 192]", got[9])
		}
		for _, r := range got {
			if r[0] > r[1] || r[1] > 199 {
				t.Fatalf("段 %v 越界", r)
			}
		}
	})

	t.Run("末段在区间尾部收敛", func(t *testing.T) {
		got := sampleRanges(100, 112, 10, 5)
		last := got[len(got)-1]
		if last[1] > 112 {
			t.Fatalf("末段 %v 越过区间末尾", last)
		}
	})

	t.Run("参数非法返回空", func(t *testing.T) {
		if got := sampleRanges(10, 9, 5, 1); got != nil {
			t.Fatalf("start > end 应返回 nil，得到 %v", got)
		}
		if got := sampleRanges(1, 10, 0, 1); got != nil {
			t.Fatalf("segments=0 应返回 nil，得到 %v", got)
		}
		if got := sampleRanges(1, 10, 2, 0); got != nil {
			t.Fatalf("width=0 应返回 nil，得到 %v", got)
		}
	})
}

func TestReconcileIndexless(t *testing.T) {
	address := testContract

	t.Run("没有无索引记录时原样返回", func(t *testing.T) {
		chainLogs := []types.Log{chainLog(1, 0, 0x01)}
		dbLogs := []store.LogRecord{dbLog(1, 0, 0x01)}

		kept, remaining, matched, missing, err := reconcileIndexless(chainLogs, dbLogs, address)
		if err != nil {
			t.Fatalf("不应报错：%v", err)
		}
		if len(kept) != 1 || len(remaining) != 1 || matched != 0 || missing != 0 {
			t.Fatalf("kept=%d remaining=%d matched=%d missing=%d", len(kept), len(remaining), matched, missing)
		}
	})

	t.Run("唯一候选被摘除", func(t *testing.T) {
		chainLogs := []types.Log{chainLog(1, indexUnknown, 0x01)}
		dbLogs := []store.LogRecord{dbLog(1, 0, 0x01)}

		kept, remaining, matched, missing, err := reconcileIndexless(chainLogs, dbLogs, address)
		if err != nil {
			t.Fatalf("不应报错：%v", err)
		}
		if len(kept) != 0 {
			t.Fatalf("匹配上的无索引记录不应留在 kept，得到 %d 条", len(kept))
		}
		if len(remaining) != 0 {
			t.Fatalf("库内对应行应被摘除，剩余 %d 条", len(remaining))
		}
		if matched != 1 || missing != 0 {
			t.Fatalf("matched=%d missing=%d, want 1/0", matched, missing)
		}
	})

	t.Run("零候选计为缺失", func(t *testing.T) {
		chainLogs := []types.Log{chainLog(2, indexUnknown, 0x02)}
		dbLogs := []store.LogRecord{dbLog(1, 0, 0x01)}

		kept, remaining, matched, missing, err := reconcileIndexless(chainLogs, dbLogs, address)
		if err != nil {
			t.Fatalf("不应报错：%v", err)
		}
		if len(kept) != 0 || len(remaining) != 1 {
			t.Fatalf("kept=%d remaining=%d", len(kept), len(remaining))
		}
		if matched != 0 || missing != 1 {
			t.Fatalf("matched=%d missing=%d, want 0/1", matched, missing)
		}
	})

	t.Run("多候选无法判定时报错", func(t *testing.T) {
		chainLogs := []types.Log{chainLog(1, indexUnknown, 0x01)}
		dbLogs := []store.LogRecord{dbLog(1, 0, 0x01), dbLog(1, 1, 0x01)}

		if _, _, _, _, err := reconcileIndexless(chainLogs, dbLogs, address); err == nil {
			t.Fatal("同交易同主题同数据的多条候选应报错，而不是猜一条")
		}
	})
}

func TestIndexUnknownDoesNotCollideWithZero(t *testing.T) {
	// indexUnknown 必须与所有合法块内索引不同。块内日志数远小于 2^32，
	// 但仍显式断言一次，避免将来改成小常量后与真实的 0 混淆。
	if indexUnknown == 0 {
		t.Fatal("indexUnknown 不能为 0")
	}
	// 单块日志数受 gas 上限约束，现实中不可能超过 65535。
	if indexUnknown <= 65535 {
		t.Fatalf("indexUnknown=%d 太小，可能与合法索引冲突", indexUnknown)
	}
}
