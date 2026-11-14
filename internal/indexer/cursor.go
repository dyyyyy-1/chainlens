// Package indexer 实现索引器主循环：确定可扫区间、拉取日志、落库、推进游标。
package indexer

// Range 是一次扫描覆盖的闭区间区块范围。
type Range struct {
	From uint64
	To   uint64
}

// nextRange 计算本轮可扫描的区间。
//
// 上界受两个独立约束：单批最大跨度 batch，以及确认数水位 head-confirmations。
// 因此 To 恒不超过 head-confirmations，凡是写入的数据都已越过确认数门槛。
//
// 返回 ok=false 表示追平或无可用区间，调用方应等待下一轮。
func nextRange(nextBlock, head, confirmations, batch uint64) (Range, bool) {
	if batch == 0 {
		batch = 1
	}
	if confirmations >= head {
		return Range{}, false
	}

	safe := head - confirmations
	if nextBlock > safe {
		return Range{}, false
	}

	// 先比较可用跨度再相加，避免 nextBlock+batch-1 溢出。
	to := safe
	if safe-nextBlock >= batch {
		to = nextBlock + batch - 1
	}

	return Range{From: nextBlock, To: to}, true
}
